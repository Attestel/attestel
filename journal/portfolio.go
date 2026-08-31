package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

// portfolio.go owns the authenticated portfolio source of truth described by the Portfolio
// Intelligence plan. It is deliberately a research record, not a brokerage account: no order,
// broker, routing, execution or money-movement field exists anywhere in this model.

const (
	portfolioSchemaVersion = 1
	maxPortfoliosPerUser   = 20
	maxPortfolioPositions  = 100
	maxPortfolioCashRows   = 20
	maxPortfolioTargets    = 200
)

var (
	currencyCodeRE    = regexp.MustCompile(`^[A-Z]{3}$`)
	portfolioTickerRE = regexp.MustCompile(`^[A-Z0-9][A-Z0-9.\-]{0,15}$`)
)

type PortfolioPosition struct {
	Ticker      string   `json:"ticker"`
	Quantity    float64  `json:"quantity"`
	AverageCost *float64 `json:"averageCost,omitempty"`
	ManualValue *float64 `json:"manualValue,omitempty"`
	Sector      string   `json:"sector,omitempty"`
	Industry    string   `json:"industry,omitempty"`
}

type PortfolioCash struct {
	Currency string  `json:"currency"`
	Amount   float64 `json:"amount"`
}

// PortfolioTarget is a user-authored review band. Weights are decimal fractions in [0,1]. Kind is
// ticker, sector, or cash; key is the ticker/sector name and is normalized by kind.
type PortfolioTarget struct {
	Kind         string   `json:"kind"`
	Key          string   `json:"key"`
	TargetWeight *float64 `json:"targetWeight,omitempty"`
	MinWeight    *float64 `json:"minWeight,omitempty"`
	MaxWeight    *float64 `json:"maxWeight,omitempty"`
}

type PortfolioConstraints struct {
	NoLeverage            bool     `json:"noLeverage"`
	ExcludedSectors       []string `json:"excludedSectors"`
	MinimumCashWeight     *float64 `json:"minimumCashWeight,omitempty"`
	MaximumPositionWeight *float64 `json:"maximumPositionWeight,omitempty"`
}

type PortfolioProfile struct {
	Objective     string               `json:"objective,omitempty"`
	Horizon       string               `json:"horizon,omitempty"`
	LossTolerance string               `json:"lossTolerance,omitempty"`
	Constraints   PortfolioConstraints `json:"constraints"`
}

type Portfolio struct {
	ID            string              `json:"id"`
	SchemaVersion int                 `json:"schemaVersion"`
	Name          string              `json:"name"`
	BaseCurrency  string              `json:"baseCurrency"`
	Positions     []PortfolioPosition `json:"positions"`
	Cash          []PortfolioCash     `json:"cash"`
	Targets       []PortfolioTarget   `json:"targets"`
	Profile       PortfolioProfile    `json:"profile"`
	Version       int                 `json:"version"`
	CreatedAt     int64               `json:"createdAt"`
	UpdatedAt     int64               `json:"updatedAt"`
}

var validPortfolioObjective = map[string]bool{
	"": true, "capital_preservation": true, "balanced": true, "growth": true,
	"aggressive_growth": true,
}
var validPortfolioHorizon = map[string]bool{
	"": true, "under_1_year": true, "1_3_years": true, "3_5_years": true, "5_plus_years": true,
}
var validLossTolerance = map[string]bool{"": true, "low": true, "medium": true, "high": true}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func normalizePortfolio(p *Portfolio) {
	p.Name = strings.TrimSpace(p.Name)
	p.BaseCurrency = strings.ToUpper(strings.TrimSpace(p.BaseCurrency))
	if p.BaseCurrency == "" {
		p.BaseCurrency = "USD"
	}
	if p.Positions == nil {
		p.Positions = []PortfolioPosition{}
	}
	for i := range p.Positions {
		p.Positions[i].Ticker = strings.ToUpper(strings.TrimSpace(p.Positions[i].Ticker))
		p.Positions[i].Sector = strings.TrimSpace(p.Positions[i].Sector)
		p.Positions[i].Industry = strings.TrimSpace(p.Positions[i].Industry)
	}
	if p.Cash == nil {
		p.Cash = []PortfolioCash{}
	}
	for i := range p.Cash {
		p.Cash[i].Currency = strings.ToUpper(strings.TrimSpace(p.Cash[i].Currency))
	}
	if p.Targets == nil {
		p.Targets = []PortfolioTarget{}
	}
	for i := range p.Targets {
		p.Targets[i].Kind = strings.ToLower(strings.TrimSpace(p.Targets[i].Kind))
		p.Targets[i].Key = strings.TrimSpace(p.Targets[i].Key)
		if p.Targets[i].Kind == "ticker" || p.Targets[i].Kind == "cash" {
			p.Targets[i].Key = strings.ToUpper(p.Targets[i].Key)
		}
	}
	p.Profile.Objective = strings.ToLower(strings.TrimSpace(p.Profile.Objective))
	p.Profile.Horizon = strings.ToLower(strings.TrimSpace(p.Profile.Horizon))
	p.Profile.LossTolerance = strings.ToLower(strings.TrimSpace(p.Profile.LossTolerance))
	sectors := make([]string, 0, len(p.Profile.Constraints.ExcludedSectors))
	seen := map[string]bool{}
	for _, raw := range p.Profile.Constraints.ExcludedSectors {
		sector := strings.TrimSpace(raw)
		key := strings.ToLower(sector)
		if sector != "" && !seen[key] {
			seen[key] = true
			sectors = append(sectors, sector)
		}
	}
	p.Profile.Constraints.ExcludedSectors = sectors
}

func validateWeight(field string, v *float64) *apiError {
	if v == nil {
		return nil
	}
	if !finite(*v) || *v < 0 || *v > 1 {
		return badField(field, field+" must be between 0 and 1")
	}
	return nil
}

func validatePortfolio(p *Portfolio) *apiError {
	normalizePortfolio(p)
	if p.Name == "" || len([]rune(p.Name)) > 120 {
		return badField("name", "name is required and must be at most 120 characters")
	}
	if !currencyCodeRE.MatchString(p.BaseCurrency) {
		return badField("baseCurrency", "baseCurrency must be a three-letter currency code")
	}
	if len(p.Positions) > maxPortfolioPositions {
		return badField("positions", fmt.Sprintf("at most %d positions are allowed", maxPortfolioPositions))
	}
	seenTickers := map[string]bool{}
	for i, position := range p.Positions {
		field := fmt.Sprintf("positions[%d]", i)
		if !portfolioTickerRE.MatchString(position.Ticker) {
			return badField(field+".ticker", "ticker must use 1-16 uppercase letters, digits, dots, or hyphens")
		}
		if seenTickers[position.Ticker] {
			return badField(field+".ticker", "each ticker may appear only once")
		}
		seenTickers[position.Ticker] = true
		if !finite(position.Quantity) || position.Quantity <= 0 {
			return badField(field+".quantity", "quantity must be greater than zero")
		}
		if position.AverageCost != nil && (!finite(*position.AverageCost) || *position.AverageCost <= 0) {
			return badField(field+".averageCost", "averageCost must be greater than zero")
		}
		if position.ManualValue != nil && (!finite(*position.ManualValue) || *position.ManualValue < 0) {
			return badField(field+".manualValue", "manualValue must be zero or greater")
		}
		if len([]rune(position.Sector)) > 100 || len([]rune(position.Industry)) > 120 {
			return badField(field, "sector and industry labels are too long")
		}
	}
	if len(p.Cash) > maxPortfolioCashRows {
		return badField("cash", fmt.Sprintf("at most %d cash balances are allowed", maxPortfolioCashRows))
	}
	seenCurrency := map[string]bool{}
	for i, cash := range p.Cash {
		field := fmt.Sprintf("cash[%d]", i)
		if !currencyCodeRE.MatchString(cash.Currency) {
			return badField(field+".currency", "currency must be a three-letter currency code")
		}
		if seenCurrency[cash.Currency] {
			return badField(field+".currency", "each cash currency may appear only once")
		}
		seenCurrency[cash.Currency] = true
		if !finite(cash.Amount) || cash.Amount < 0 {
			return badField(field+".amount", "cash amount must be zero or greater")
		}
	}
	if len(p.Targets) > maxPortfolioTargets {
		return badField("targets", fmt.Sprintf("at most %d targets are allowed", maxPortfolioTargets))
	}
	seenTargets := map[string]bool{}
	for i, target := range p.Targets {
		field := fmt.Sprintf("targets[%d]", i)
		if target.Kind != "ticker" && target.Kind != "sector" && target.Kind != "cash" {
			return badField(field+".kind", "target kind must be ticker, sector, or cash")
		}
		if target.Key == "" || len([]rune(target.Key)) > 120 {
			return badField(field+".key", "target key is required and must be at most 120 characters")
		}
		if target.Kind == "ticker" && !portfolioTickerRE.MatchString(target.Key) {
			return badField(field+".key", "ticker target must use 1-16 uppercase letters, digits, dots, or hyphens")
		}
		if target.Kind == "cash" && !currencyCodeRE.MatchString(target.Key) {
			return badField(field+".key", "cash target must use a three-letter currency code")
		}
		key := target.Kind + ":" + strings.ToLower(target.Key)
		if seenTargets[key] {
			return badField(field+".key", "each target kind/key pair may appear only once")
		}
		seenTargets[key] = true
		for suffix, value := range map[string]*float64{
			"targetWeight": target.TargetWeight, "minWeight": target.MinWeight, "maxWeight": target.MaxWeight,
		} {
			if err := validateWeight(field+"."+suffix, value); err != nil {
				return err
			}
		}
		if target.TargetWeight == nil && target.MinWeight == nil && target.MaxWeight == nil {
			return badField(field, "a target needs targetWeight, minWeight, or maxWeight")
		}
		if target.MinWeight != nil && target.MaxWeight != nil && *target.MinWeight > *target.MaxWeight {
			return badField(field, "minWeight cannot exceed maxWeight")
		}
		if target.TargetWeight != nil && target.MinWeight != nil && *target.TargetWeight < *target.MinWeight {
			return badField(field+".targetWeight", "targetWeight cannot be below minWeight")
		}
		if target.TargetWeight != nil && target.MaxWeight != nil && *target.TargetWeight > *target.MaxWeight {
			return badField(field+".targetWeight", "targetWeight cannot exceed maxWeight")
		}
	}
	if !validPortfolioObjective[p.Profile.Objective] {
		return badField("profile.objective", "objective is not supported")
	}
	if !validPortfolioHorizon[p.Profile.Horizon] {
		return badField("profile.horizon", "horizon is not supported")
	}
	if !validLossTolerance[p.Profile.LossTolerance] {
		return badField("profile.lossTolerance", "lossTolerance must be low, medium, or high")
	}
	if err := validateWeight("profile.constraints.minimumCashWeight", p.Profile.Constraints.MinimumCashWeight); err != nil {
		return err
	}
	if err := validateWeight("profile.constraints.maximumPositionWeight", p.Profile.Constraints.MaximumPositionWeight); err != nil {
		return err
	}
	return nil
}

type portfolioPatch struct {
	Name         *string              `json:"name"`
	BaseCurrency *string              `json:"baseCurrency"`
	Positions    *[]PortfolioPosition `json:"positions"`
	Cash         *[]PortfolioCash     `json:"cash"`
	Targets      *[]PortfolioTarget   `json:"targets"`
	Profile      *PortfolioProfile    `json:"profile"`
}

func (s *Server) registerPortfolioRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /portfolios", s.optionalAuth(s.handleListPortfolios))
	mux.HandleFunc("POST /portfolios", s.requireAuth(s.handleCreatePortfolio))
	mux.HandleFunc("GET /portfolios/{id}", s.requireAuth(s.handleGetPortfolio))
	mux.HandleFunc("PATCH /portfolios/{id}", s.requireAuth(s.handleUpdatePortfolio))
	mux.HandleFunc("DELETE /portfolios/{id}", s.requireAuth(s.handleDeletePortfolio))
	mux.HandleFunc("GET /portfolios/{id}/intelligence", s.requireAuth(s.handlePortfolioIntelligence))
	mux.HandleFunc("GET /portfolios/{id}/snapshots", s.requireAuth(s.handleListPortfolioSnapshots))
	mux.HandleFunc("POST /portfolios/{id}/snapshots", s.requireAuth(s.handleCreatePortfolioSnapshot))
	mux.HandleFunc("GET /portfolios/{id}/reviews", s.requireAuth(s.handleListPortfolioReviews))
	mux.HandleFunc("POST /portfolios/{id}/review", s.requireAuth(s.handleCreatePortfolioReview))
	mux.HandleFunc("POST /portfolios/{id}/scenario", s.requireAuth(s.handlePortfolioScenario))
	// Phase 3. Which scheduled events bear on this portfolio's holdings, with the exposed weight
	// computed here in code. A READ: it starts nothing, changes nothing and calls no model.
	mux.HandleFunc("GET /portfolios/{id}/event-impact", s.requireAuth(s.handlePortfolioEventImpact))
}

func (s *Server) handleListPortfolios(w http.ResponseWriter, r *http.Request) {
	uid := userID(r)
	if uid == "" {
		writeJSON(w, http.StatusOK, map[string]any{"portfolios": []Portfolio{}})
		return
	}
	items, err := s.portfolios.List(uid)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "portfolio store is unreadable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"portfolios": items})
}

func (s *Server) handleGetPortfolio(w http.ResponseWriter, r *http.Request) {
	portfolio, ok, err := s.portfolios.Get(userID(r), r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "portfolio store is unreadable"})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "portfolio not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"portfolio": portfolio})
}

func (s *Server) handleCreatePortfolio(w http.ResponseWriter, r *http.Request) {
	uid := userID(r)
	items, err := s.portfolios.List(uid)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "portfolio store is unreadable"})
		return
	}
	if len(items) >= maxPortfoliosPerUser {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "portfolio limit reached", "cap": maxPortfoliosPerUser})
		return
	}
	var portfolio Portfolio
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&portfolio); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON: " + err.Error()})
		return
	}
	portfolio.ID = ""
	portfolio.SchemaVersion = portfolioSchemaVersion
	portfolio.Version = 0
	portfolio.CreatedAt = 0
	portfolio.UpdatedAt = 0
	if apiErr := validatePortfolio(&portfolio); apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}
	created, err := s.portfolios.Add(uid, portfolio)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "portfolio could not be persisted"})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"portfolio": created})
}

func (s *Server) handleUpdatePortfolio(w http.ResponseWriter, r *http.Request) {
	uid := userID(r)
	portfolio, ok, err := s.portfolios.Get(uid, r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "portfolio store is unreadable"})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "portfolio not found"})
		return
	}
	var patch portfolioPatch
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&patch); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON: " + err.Error()})
		return
	}
	if patch.Name != nil {
		portfolio.Name = *patch.Name
	}
	if patch.BaseCurrency != nil {
		portfolio.BaseCurrency = *patch.BaseCurrency
	}
	if patch.Positions != nil {
		portfolio.Positions = append([]PortfolioPosition(nil), (*patch.Positions)...)
	}
	if patch.Cash != nil {
		portfolio.Cash = append([]PortfolioCash(nil), (*patch.Cash)...)
	}
	if patch.Targets != nil {
		portfolio.Targets = append([]PortfolioTarget(nil), (*patch.Targets)...)
	}
	if patch.Profile != nil {
		portfolio.Profile = *patch.Profile
	}
	if apiErr := validatePortfolio(&portfolio); apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}
	updated, ok, err := s.portfolios.Replace(uid, portfolio)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "portfolio could not be persisted"})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "portfolio not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"portfolio": updated})
}

func (s *Server) handleDeletePortfolio(w http.ResponseWriter, r *http.Request) {
	ok, err := s.portfolios.Delete(userID(r), r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "portfolio could not be persisted"})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "portfolio not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// portfolioTargetsByKey is shared by the intelligence layer and tests. A copied map is returned so
// calculations cannot mutate the user's record.
func portfolioTargetsByKey(targets []PortfolioTarget) map[string]PortfolioTarget {
	out := make(map[string]PortfolioTarget, len(targets))
	for _, target := range targets {
		out[target.Kind+":"+strings.ToLower(target.Key)] = target
	}
	return out
}

func sortedPortfolioPositions(positions []PortfolioPosition) []PortfolioPosition {
	out := append([]PortfolioPosition(nil), positions...)
	sort.Slice(out, func(i, j int) bool { return out[i].Ticker < out[j].Ticker })
	return out
}

func newPortfolioForTest(name string) Portfolio {
	now := time.Now().Unix()
	return Portfolio{
		ID: "pf_" + newID(), SchemaVersion: portfolioSchemaVersion, Name: name, BaseCurrency: "USD",
		Positions: []PortfolioPosition{}, Cash: []PortfolioCash{}, Targets: []PortfolioTarget{},
		Version: 1, CreatedAt: now, UpdatedAt: now,
	}
}
