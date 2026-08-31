package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	portfolioIntelligenceVersion = "portfolio-intelligence-v1"
	portfolioPolicyVersion       = "portfolio-policy-v1"
	portfolioRiskPositionCap     = 25 // mirrors analysis.portfolio_risk.MAX_RISK_POSITIONS
)

type PortfolioThesisContext struct {
	ID                    string `json:"id"`
	Claim                 string `json:"claim"`
	ActiveThesisCount     int    `json:"activeThesisCount"`
	UserConfidence        *int   `json:"userConfidence"`
	PrimaryRisk           string `json:"primaryRisk,omitempty"`
	UpdatedAt             int64  `json:"updatedAt"`
	LatestCheckAt         *int64 `json:"latestCheckAt,omitempty"`
	LatestCheckVerdict    string `json:"latestCheckVerdict,omitempty"`
	LatestCheckSummary    string `json:"latestCheckSummary,omitempty"`
	LatestCheckConfidence *int   `json:"latestCheckConfidence,omitempty"`
}

type PortfolioTargetStatus struct {
	Kind          string   `json:"kind"`
	Key           string   `json:"key"`
	CurrentWeight *float64 `json:"currentWeight,omitempty"`
	TargetWeight  *float64 `json:"targetWeight,omitempty"`
	MinWeight     *float64 `json:"minWeight,omitempty"`
	MaxWeight     *float64 `json:"maxWeight,omitempty"`
	Drift         *float64 `json:"drift,omitempty"`
	State         string   `json:"state"` // within_range | below_range | above_range | no_range | unavailable
}

type PortfolioPositionIntelligence struct {
	Ticker          string                  `json:"ticker"`
	Quantity        float64                 `json:"quantity"`
	AverageCost     *float64                `json:"averageCost,omitempty"`
	Sector          string                  `json:"sector,omitempty"`
	Industry        string                  `json:"industry,omitempty"`
	Price           *float64                `json:"price,omitempty"`
	MarketValue     *float64                `json:"marketValue,omitempty"`
	Weight          *float64                `json:"weight,omitempty"`
	ValuationSource string                  `json:"valuationSource"` // quote | manual | unavailable
	QuoteSource     string                  `json:"quoteSource,omitempty"`
	QuoteAsOf       string                  `json:"quoteAsOf,omitempty"`
	SourceSynthetic bool                    `json:"sourceIsSynthetic"`
	Thesis          *PortfolioThesisContext `json:"thesis,omitempty"`
	Target          *PortfolioTargetStatus  `json:"target,omitempty"`
}

type PortfolioExposure struct {
	Kind   string  `json:"kind"` // sector | industry
	Key    string  `json:"key"`
	Weight float64 `json:"weight"`
}

type PortfolioConcentration struct {
	LargestTicker string   `json:"largestTicker,omitempty"`
	LargestWeight *float64 `json:"largestWeight,omitempty"`
	TopTwoWeight  *float64 `json:"topTwoWeight,omitempty"`
	TopFiveWeight *float64 `json:"topFiveWeight,omitempty"`
	HHI           *float64 `json:"hhi,omitempty"`
}

type PortfolioFinding struct {
	Code     string         `json:"code"`
	Severity string         `json:"severity"` // attention | information
	Subject  string         `json:"subject"`
	Summary  string         `json:"summary"`
	Evidence map[string]any `json:"evidence"`
}

type PortfolioIntelligence struct {
	PortfolioID        string                          `json:"portfolioId"`
	PortfolioVersion   int                             `json:"portfolioVersion"`
	CalculationVersion string                          `json:"calculationVersion"`
	PolicyVersion      string                          `json:"policyVersion"`
	ContextVersion     string                          `json:"contextVersion"`
	AsOf               string                          `json:"asOf"`
	BaseCurrency       string                          `json:"baseCurrency"`
	Profile            PortfolioProfile                `json:"profile"`
	TotalValue         *float64                        `json:"totalValue,omitempty"`
	KnownValue         float64                         `json:"knownValue"`
	CashValue          float64                         `json:"cashValue"`
	CashWeight         *float64                        `json:"cashWeight,omitempty"`
	ValuationComplete  bool                            `json:"valuationComplete"`
	UnconvertedCash    []PortfolioCash                 `json:"unconvertedCash"`
	Positions          []PortfolioPositionIntelligence `json:"positions"`
	Exposures          []PortfolioExposure             `json:"exposures"`
	UnclassifiedWeight *float64                        `json:"unclassifiedWeight,omitempty"`
	Concentration      PortfolioConcentration          `json:"concentration"`
	HistoricalRisk     map[string]any                  `json:"historicalRisk"`
	Targets            []PortfolioTargetStatus         `json:"targets"`
	Findings           []PortfolioFinding              `json:"findings"`
	Degraded           []string                        `json:"degraded"`
}

func rounded(v float64) float64 { return math.Round(v*1e8) / 1e8 }
func floatPtr(v float64) *float64 {
	v = rounded(v)
	return &v
}

func portfolioTheses(theses []Thesis) map[string]*PortfolioThesisContext {
	active := map[string][]Thesis{}
	for _, thesis := range theses {
		if thesis.Status == statusActive {
			active[strings.ToUpper(thesis.Ticker)] = append(active[strings.ToUpper(thesis.Ticker)], thesis)
		}
	}
	out := map[string]*PortfolioThesisContext{}
	for ticker, rows := range active {
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].UpdatedAt != rows[j].UpdatedAt {
				return rows[i].UpdatedAt > rows[j].UpdatedAt
			}
			return rows[i].ID < rows[j].ID
		})
		latest := rows[0]
		ctx := &PortfolioThesisContext{
			ID: latest.ID, Claim: latest.Claim, ActiveThesisCount: len(rows),
			UserConfidence: latest.Confidence, UpdatedAt: latest.UpdatedAt,
		}
		if len(latest.Risks) > 0 {
			ctx.PrimaryRisk = latest.Risks[0].Text
		}
		if latest.LastCheck != nil {
			at := latest.LastCheck.At
			ctx.LatestCheckAt = &at
			ctx.LatestCheckVerdict = latest.LastCheck.Verdict
			ctx.LatestCheckSummary = latest.LastCheck.Summary
			ctx.LatestCheckConfidence = latest.LastCheck.Confidence
		}
		out[ticker] = ctx
	}
	return out
}

func (s *Server) portfolioQuoteBatch(ctx context.Context, positions []PortfolioPosition) map[string]*quoteResp {
	out := map[string]*quoteResp{}
	var mu sync.Mutex
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for _, position := range positions {
		if position.ManualValue != nil {
			continue
		}
		ticker := position.Ticker
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			quote, err := s.fetchQuote(ctx, ticker)
			mu.Lock()
			if err == nil {
				out[ticker] = quote
			} else {
				out[ticker] = nil
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

func (s *Server) postJSON(ctx context.Context, endpoint string, body any, target any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	responseBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("POST %s -> %d", endpoint, resp.StatusCode)
	}
	return json.Unmarshal(responseBody, target)
}

func (s *Server) fetchPortfolioRisk(ctx context.Context, positions []PortfolioPositionIntelligence) (map[string]any, error) {
	riskPositions := make([]map[string]any, 0, len(positions))
	for _, position := range positions {
		if position.Weight != nil && *position.Weight > 0 {
			riskPositions = append(riskPositions, map[string]any{"ticker": position.Ticker, "weight": *position.Weight})
		}
	}
	var risk map[string]any
	err := s.postJSON(ctx, s.cfg.AnalysisURL+"/portfolio-risk", map[string]any{
		"positions": riskPositions, "benchmark": "SPY", "lookbackDays": 520,
	}, &risk)
	return risk, err
}

func targetStatus(target PortfolioTarget, current *float64) PortfolioTargetStatus {
	status := PortfolioTargetStatus{
		Kind: target.Kind, Key: target.Key, CurrentWeight: current,
		TargetWeight: target.TargetWeight, MinWeight: target.MinWeight, MaxWeight: target.MaxWeight,
		State: "unavailable",
	}
	if current == nil {
		return status
	}
	if target.TargetWeight != nil {
		status.Drift = floatPtr(*current - *target.TargetWeight)
	}
	status.State = "no_range"
	if target.MinWeight != nil && *current < *target.MinWeight {
		status.State = "below_range"
	} else if target.MaxWeight != nil && *current > *target.MaxWeight {
		status.State = "above_range"
	} else if target.MinWeight != nil || target.MaxWeight != nil {
		status.State = "within_range"
	}
	return status
}

func addTargetFinding(findings *[]PortfolioFinding, status PortfolioTargetStatus) {
	if status.State != "below_range" && status.State != "above_range" {
		return
	}
	*findings = append(*findings, PortfolioFinding{
		Code: "target_range_" + strings.TrimSuffix(status.State, "_range"), Severity: "attention",
		Subject: status.Key, Summary: "Current allocation is " + strings.ReplaceAll(status.State, "_", " ") + ".",
		Evidence: map[string]any{
			"kind": status.Kind, "currentWeight": status.CurrentWeight,
			"minWeight": status.MinWeight, "maxWeight": status.MaxWeight,
		},
	})
}

func (s *Server) buildPortfolioIntelligence(ctx context.Context, uid string, portfolio Portfolio) (PortfolioIntelligence, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	positions := sortedPortfolioPositions(portfolio.Positions)
	quotes := s.portfolioQuoteBatch(ctx, positions)
	thesisMap := portfolioTheses(s.theses.List(uid))
	intel := PortfolioIntelligence{
		PortfolioID: portfolio.ID, PortfolioVersion: portfolio.Version,
		CalculationVersion: portfolioIntelligenceVersion, PolicyVersion: portfolioPolicyVersion,
		AsOf: time.Now().UTC().Format(time.RFC3339), BaseCurrency: portfolio.BaseCurrency,
		Profile:   portfolio.Profile,
		Positions: []PortfolioPositionIntelligence{}, Exposures: []PortfolioExposure{},
		HistoricalRisk: map[string]any{"available": false, "modelVersion": "portfolio-risk-v1"},
		Targets:        []PortfolioTargetStatus{}, Findings: []PortfolioFinding{},
		UnconvertedCash: []PortfolioCash{}, Degraded: []string{}, ValuationComplete: true,
	}

	for _, cash := range portfolio.Cash {
		if cash.Currency == portfolio.BaseCurrency {
			intel.CashValue += cash.Amount
		} else if cash.Amount != 0 {
			intel.UnconvertedCash = append(intel.UnconvertedCash, cash)
			intel.ValuationComplete = false
			intel.Degraded = append(intel.Degraded, "cash:fx_unavailable:"+cash.Currency)
		}
	}
	intel.KnownValue = intel.CashValue
	for _, position := range positions {
		row := PortfolioPositionIntelligence{
			Ticker: position.Ticker, Quantity: position.Quantity, AverageCost: position.AverageCost,
			Sector: position.Sector, Industry: position.Industry, ValuationSource: "unavailable",
			Thesis: thesisMap[position.Ticker],
		}
		if position.ManualValue != nil {
			row.MarketValue = floatPtr(*position.ManualValue)
			row.ValuationSource = "manual"
			intel.KnownValue += *position.ManualValue
		} else if quote := quotes[position.Ticker]; quote != nil && quote.Price != nil && *quote.Price >= 0 {
			row.Price = floatPtr(*quote.Price)
			row.MarketValue = floatPtr(position.Quantity * *quote.Price)
			row.ValuationSource = "quote"
			row.QuoteSource = quote.Source
			row.QuoteAsOf = quote.AsOf
			row.SourceSynthetic = strings.Contains(strings.ToLower(quote.Source), "synthetic")
			intel.KnownValue += *row.MarketValue
		} else {
			intel.ValuationComplete = false
			intel.Degraded = append(intel.Degraded, "valuation:unavailable:"+position.Ticker)
		}
		intel.Positions = append(intel.Positions, row)
	}
	intel.KnownValue = rounded(intel.KnownValue)
	intel.CashValue = rounded(intel.CashValue)

	if intel.ValuationComplete {
		intel.TotalValue = floatPtr(intel.KnownValue)
		if intel.KnownValue > 0 {
			intel.CashWeight = floatPtr(intel.CashValue / intel.KnownValue)
			for i := range intel.Positions {
				intel.Positions[i].Weight = floatPtr(*intel.Positions[i].MarketValue / intel.KnownValue)
			}
		}
	}

	targets := portfolioTargetsByKey(portfolio.Targets)
	evaluatedTargets := map[string]bool{}
	sectorWeights := map[string]float64{}
	industryWeights := map[string]float64{}
	unclassified := 0.0
	weights := make([]PortfolioPositionIntelligence, 0, len(intel.Positions))
	for i := range intel.Positions {
		row := &intel.Positions[i]
		if row.Weight != nil {
			weights = append(weights, *row)
			if row.Sector != "" {
				sectorWeights[row.Sector] += *row.Weight
			} else {
				unclassified += *row.Weight
			}
			if row.Industry != "" {
				industryWeights[row.Industry] += *row.Weight
			}
		}
		if target, ok := targets["ticker:"+strings.ToLower(row.Ticker)]; ok {
			status := targetStatus(target, row.Weight)
			row.Target = &status
			intel.Targets = append(intel.Targets, status)
			addTargetFinding(&intel.Findings, status)
			evaluatedTargets["ticker:"+strings.ToLower(row.Ticker)] = true
		}
	}
	for key, weight := range sectorWeights {
		intel.Exposures = append(intel.Exposures, PortfolioExposure{Kind: "sector", Key: key, Weight: rounded(weight)})
	}
	for key, weight := range industryWeights {
		intel.Exposures = append(intel.Exposures, PortfolioExposure{Kind: "industry", Key: key, Weight: rounded(weight)})
	}
	sort.Slice(intel.Exposures, func(i, j int) bool {
		if intel.Exposures[i].Kind != intel.Exposures[j].Kind {
			return intel.Exposures[i].Kind < intel.Exposures[j].Kind
		}
		if intel.Exposures[i].Weight != intel.Exposures[j].Weight {
			return intel.Exposures[i].Weight > intel.Exposures[j].Weight
		}
		return intel.Exposures[i].Key < intel.Exposures[j].Key
	})
	if intel.ValuationComplete && intel.KnownValue > 0 {
		intel.UnclassifiedWeight = floatPtr(unclassified)
	}

	for _, target := range portfolio.Targets {
		key := target.Kind + ":" + strings.ToLower(target.Key)
		if evaluatedTargets[key] {
			continue
		}
		var current *float64
		switch target.Kind {
		case "ticker":
			if intel.ValuationComplete && intel.KnownValue > 0 {
				current = floatPtr(0) // a user target can name a currently unheld ticker
			}
		case "cash":
			current = intel.CashWeight
		case "sector":
			if intel.ValuationComplete && intel.KnownValue > 0 {
				for sector, weight := range sectorWeights {
					if strings.EqualFold(sector, target.Key) {
						current = floatPtr(weight)
					}
				}
				if current == nil {
					current = floatPtr(0)
				}
			}
		}
		status := targetStatus(target, current)
		intel.Targets = append(intel.Targets, status)
		addTargetFinding(&intel.Findings, status)
	}
	sort.Slice(intel.Targets, func(i, j int) bool {
		if intel.Targets[i].Kind != intel.Targets[j].Kind {
			return intel.Targets[i].Kind < intel.Targets[j].Kind
		}
		return intel.Targets[i].Key < intel.Targets[j].Key
	})

	if len(weights) > 0 {
		sort.Slice(weights, func(i, j int) bool {
			if *weights[i].Weight != *weights[j].Weight {
				return *weights[i].Weight > *weights[j].Weight
			}
			return weights[i].Ticker < weights[j].Ticker
		})
		intel.Concentration.LargestTicker = weights[0].Ticker
		intel.Concentration.LargestWeight = floatPtr(*weights[0].Weight)
		topTwo, topFive, hhi := 0.0, 0.0, 0.0
		for i, position := range weights {
			weight := *position.Weight
			hhi += weight * weight
			if i < 2 {
				topTwo += weight
			}
			if i < 5 {
				topFive += weight
			}
		}
		intel.Concentration.TopTwoWeight = floatPtr(topTwo)
		intel.Concentration.TopFiveWeight = floatPtr(topFive)
		intel.Concentration.HHI = floatPtr(hhi)
	}

	constraints := portfolio.Profile.Constraints
	if constraints.MaximumPositionWeight != nil {
		for _, position := range intel.Positions {
			if position.Weight != nil && *position.Weight > *constraints.MaximumPositionWeight {
				intel.Findings = append(intel.Findings, PortfolioFinding{
					Code: "maximum_position_exceeded", Severity: "attention", Subject: position.Ticker,
					Summary:  "Position weight exceeds the user's maximum-position constraint.",
					Evidence: map[string]any{"currentWeight": position.Weight, "maximumWeight": constraints.MaximumPositionWeight},
				})
			}
		}
	}
	if constraints.MinimumCashWeight != nil && intel.CashWeight != nil && *intel.CashWeight < *constraints.MinimumCashWeight {
		intel.Findings = append(intel.Findings, PortfolioFinding{
			Code: "minimum_cash_not_met", Severity: "attention", Subject: "CASH",
			Summary:  "Cash weight is below the user's minimum-cash constraint.",
			Evidence: map[string]any{"currentWeight": intel.CashWeight, "minimumWeight": constraints.MinimumCashWeight},
		})
	}
	for _, excluded := range constraints.ExcludedSectors {
		for sector, weight := range sectorWeights {
			if strings.EqualFold(excluded, sector) && weight > 0 {
				intel.Findings = append(intel.Findings, PortfolioFinding{
					Code: "excluded_sector_present", Severity: "attention", Subject: sector,
					Summary:  "Portfolio includes a sector the user marked as excluded.",
					Evidence: map[string]any{"weight": rounded(weight)},
				})
			}
		}
	}

	if intel.ValuationComplete && len(intel.Positions) > 0 && len(intel.Positions) <= portfolioRiskPositionCap {
		risk, err := s.fetchPortfolioRisk(ctx, intel.Positions)
		if err != nil {
			intel.Degraded = append(intel.Degraded, "risk:unavailable")
		} else {
			intel.HistoricalRisk = risk
		}
	} else if len(intel.Positions) > portfolioRiskPositionCap {
		intel.Degraded = append(intel.Degraded, "risk:position_cap")
	}

	sort.Slice(intel.Findings, func(i, j int) bool {
		if intel.Findings[i].Severity != intel.Findings[j].Severity {
			return intel.Findings[i].Severity < intel.Findings[j].Severity
		}
		if intel.Findings[i].Code != intel.Findings[j].Code {
			return intel.Findings[i].Code < intel.Findings[j].Code
		}
		return intel.Findings[i].Subject < intel.Findings[j].Subject
	})
	sort.Strings(intel.Degraded)

	versionPayload := map[string]any{
		"calculationVersion": intel.CalculationVersion, "policyVersion": intel.PolicyVersion,
		"baseCurrency": intel.BaseCurrency, "profile": intel.Profile,
		"positions": intel.Positions, "cashValue": intel.CashValue,
		"cashWeight": intel.CashWeight, "unconvertedCash": intel.UnconvertedCash,
		"historicalRisk": intel.HistoricalRisk, "targets": intel.Targets, "findings": intel.Findings,
	}
	raw, _ := json.Marshal(versionPayload)
	sum := sha256.Sum256(raw)
	intel.ContextVersion = "pctx_" + hex.EncodeToString(sum[:16])
	return intel, nil
}

func (s *Server) handlePortfolioIntelligence(w http.ResponseWriter, r *http.Request) {
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
	intel, err := s.buildPortfolioIntelligence(r.Context(), uid, portfolio)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "portfolio intelligence is unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"intelligence": intel})
}
