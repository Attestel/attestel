package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type analysisFixture struct {
	mu        sync.Mutex
	prices    map[string]float64
	riskCalls []map[string]any
	server    *httptest.Server
}

func newAnalysisFixture(t *testing.T) *analysisFixture {
	t.Helper()
	fixture := &analysisFixture{prices: map[string]float64{"NVDA": 100}}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/quote/"):
			ticker := strings.TrimPrefix(r.URL.Path, "/quote/")
			price, ok := fixture.prices[ticker]
			if !ok {
				http.Error(w, "missing", http.StatusBadGateway)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"symbol": ticker, "price": price, "asOf": "2026-08-22T12:00:00Z", "source": "fixture",
			})
		case r.URL.Path == "/portfolio-risk":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			fixture.mu.Lock()
			fixture.riskCalls = append(fixture.riskCalls, body)
			fixture.mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{
				"modelVersion": "portfolio-risk-v1", "available": true, "complete": true,
				"annualizedVolatility": 0.22, "beta": 1.1, "maximumDrawdown": 0.18,
				"sourceIsSynthetic": false, "observations": 252,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fixture.server.Close)
	return fixture
}

func ptr(v float64) *float64 { return &v }

func addPortfolioThesis(t *testing.T, store *ThesisStore, uid string) Thesis {
	t.Helper()
	now := time.Now().Unix()
	created, apiErr := store.Create(uid, Thesis{
		Ticker: "NVDA", Claim: "Data-center demand remains durable", Text: "Data-center demand remains durable",
		Status: statusActive, Confidence: func() *int { v := 80; return &v }(),
		Risks: []ThesisItem{{ID: "rsk_test", Text: "Export controls widen", CreatedAt: now}},
	}, now)
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	checkConfidence := 65
	updated, apiErr := store.Mutate(uid, created.ID, now+1, func(cur Thesis, _ []Thesis) (thesisMutation, *apiError) {
		cur.LastCheck = &ThesisCheck{
			At: now + 1, Verdict: "challenged", Summary: "New evidence challenges one assumption.",
			Confidence: &checkConfidence,
		}
		return thesisMutation{next: cur, version: false, bumpUpdated: false}, nil
	})
	if apiErr != nil {
		t.Fatal(apiErr)
	}
	return updated
}

func TestPortfolioIntelligenceCalculatesValuationExposureConcentrationDriftAndPolicy(t *testing.T) {
	e := newPortfolioTestEnv(t)
	analysis := newAnalysisFixture(t)
	e.srv.cfg.AnalysisURL = analysis.server.URL
	e.srv.http = analysis.server.Client()
	thesis := addPortfolioThesis(t, e.srv.theses, "alice")

	portfolio := Portfolio{
		Name: "Intelligence", BaseCurrency: "USD",
		Positions: []PortfolioPosition{
			{Ticker: "NVDA", Quantity: 4, Sector: "Technology", Industry: "Semiconductors"},
			{Ticker: "AMD", Quantity: 2, ManualValue: ptr(600), Sector: "Technology", Industry: "Semiconductors"},
		},
		Cash: []PortfolioCash{{Currency: "USD", Amount: 1000}},
		Targets: []PortfolioTarget{
			{Kind: "ticker", Key: "NVDA", TargetWeight: ptr(.1), MaxWeight: ptr(.15)},
			{Kind: "ticker", Key: "MSFT", TargetWeight: ptr(.1), MinWeight: ptr(.05)},
			{Kind: "sector", Key: "Technology", TargetWeight: ptr(.4), MaxWeight: ptr(.45)},
			{Kind: "cash", Key: "USD", TargetWeight: ptr(.6), MinWeight: ptr(.55)},
		},
		Profile: PortfolioProfile{Constraints: PortfolioConstraints{
			MinimumCashWeight: ptr(.55), MaximumPositionWeight: ptr(.25), ExcludedSectors: []string{"technology"},
		}},
	}
	if apiErr := validatePortfolio(&portfolio); apiErr != nil {
		t.Fatal(apiErr)
	}
	created, err := e.srv.portfolios.Add("alice", portfolio)
	if err != nil {
		t.Fatal(err)
	}

	intel, err := e.srv.buildPortfolioIntelligence(t.Context(), "alice", created)
	if err != nil {
		t.Fatal(err)
	}
	if !intel.ValuationComplete || intel.TotalValue == nil || *intel.TotalValue != 2000 {
		t.Fatalf("valuation=%+v", intel)
	}
	if intel.CashWeight == nil || *intel.CashWeight != .5 {
		t.Fatalf("cashWeight=%v", intel.CashWeight)
	}
	byTicker := map[string]PortfolioPositionIntelligence{}
	for _, position := range intel.Positions {
		byTicker[position.Ticker] = position
	}
	if byTicker["NVDA"].Weight == nil || *byTicker["NVDA"].Weight != .2 || byTicker["NVDA"].ValuationSource != "quote" {
		t.Fatalf("NVDA=%+v", byTicker["NVDA"])
	}
	if byTicker["AMD"].Weight == nil || *byTicker["AMD"].Weight != .3 || byTicker["AMD"].ValuationSource != "manual" {
		t.Fatalf("AMD=%+v", byTicker["AMD"])
	}
	if got := byTicker["NVDA"].Thesis; got == nil || got.ID != thesis.ID || got.UserConfidence == nil || *got.UserConfidence != 80 || got.LatestCheckConfidence == nil || *got.LatestCheckConfidence != 65 || got.LatestCheckVerdict != "challenged" {
		t.Fatalf("thesis context=%+v", got)
	}
	if intel.Concentration.LargestTicker != "AMD" || *intel.Concentration.LargestWeight != .3 || *intel.Concentration.TopTwoWeight != .5 || *intel.Concentration.HHI != .13 {
		t.Fatalf("concentration=%+v", intel.Concentration)
	}
	if len(intel.Exposures) != 2 || intel.Exposures[0].Weight != .5 || intel.Exposures[1].Weight != .5 {
		t.Fatalf("exposures=%+v", intel.Exposures)
	}
	states := map[string]string{}
	for _, target := range intel.Targets {
		states[target.Kind+":"+target.Key] = target.State
	}
	if states["ticker:NVDA"] != "above_range" || states["ticker:MSFT"] != "below_range" || states["sector:Technology"] != "above_range" || states["cash:USD"] != "below_range" {
		t.Fatalf("target states=%v", states)
	}
	codes := make([]string, 0, len(intel.Findings))
	for _, finding := range intel.Findings {
		codes = append(codes, finding.Code)
	}
	sort.Strings(codes)
	for _, want := range []string{"excluded_sector_present", "maximum_position_exceeded", "minimum_cash_not_met", "target_range_above", "target_range_below"} {
		found := false
		for _, got := range codes {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing finding %q in %v", want, codes)
		}
	}
	if intel.HistoricalRisk["beta"] != 1.1 || intel.ContextVersion == "" {
		t.Fatalf("risk/context=%+v %q", intel.HistoricalRisk, intel.ContextVersion)
	}
	analysis.mu.Lock()
	if len(analysis.riskCalls) != 1 {
		t.Fatalf("risk calls=%d", len(analysis.riskCalls))
	}
	positions := analysis.riskCalls[0]["positions"].([]any)
	analysis.mu.Unlock()
	if len(positions) != 2 {
		t.Fatalf("risk request positions=%v", positions)
	}

	again, _ := e.srv.buildPortfolioIntelligence(t.Context(), "alice", created)
	if again.ContextVersion != intel.ContextVersion {
		t.Fatalf("unchanged inputs changed context version: %s != %s", again.ContextVersion, intel.ContextVersion)
	}
	analysis.prices["NVDA"] = 110
	changed, _ := e.srv.buildPortfolioIntelligence(t.Context(), "alice", created)
	if changed.ContextVersion == intel.ContextVersion {
		t.Fatal("price change did not change context version")
	}
}

func TestPortfolioIntelligenceWithholdsWeightsAndRiskWhenValuationIsIncomplete(t *testing.T) {
	e := newPortfolioTestEnv(t)
	analysis := newAnalysisFixture(t)
	e.srv.cfg.AnalysisURL = analysis.server.URL
	e.srv.http = analysis.server.Client()
	p := Portfolio{
		Name: "Incomplete", BaseCurrency: "USD",
		Positions: []PortfolioPosition{{Ticker: "MISSING", Quantity: 3}},
		Cash:      []PortfolioCash{{Currency: "EUR", Amount: 100}},
	}
	if apiErr := validatePortfolio(&p); apiErr != nil {
		t.Fatal(apiErr)
	}
	created, _ := e.srv.portfolios.Add("alice", p)
	intel, err := e.srv.buildPortfolioIntelligence(t.Context(), "alice", created)
	if err != nil {
		t.Fatal(err)
	}
	if intel.ValuationComplete || intel.TotalValue != nil || intel.Positions[0].Weight != nil || intel.CashWeight != nil {
		t.Fatalf("incomplete valuation exposed complete metrics: %+v", intel)
	}
	if len(intel.UnconvertedCash) != 1 || len(intel.Degraded) != 2 {
		t.Fatalf("degraded=%v unconverted=%v", intel.Degraded, intel.UnconvertedCash)
	}
	analysis.mu.Lock()
	calls := len(analysis.riskCalls)
	analysis.mu.Unlock()
	if calls != 0 {
		t.Fatalf("risk was called for incomplete valuation: %d", calls)
	}
}

func TestPortfolioIntelligenceRouteIsOwnerScoped(t *testing.T) {
	e := newPortfolioTestEnv(t)
	analysis := newAnalysisFixture(t)
	e.srv.cfg.AnalysisURL = analysis.server.URL
	e.srv.http = analysis.server.Client()
	p := Portfolio{Name: "Route", BaseCurrency: "USD", Cash: []PortfolioCash{{Currency: "USD", Amount: 100}}}
	if apiErr := validatePortfolio(&p); apiErr != nil {
		t.Fatal(apiErr)
	}
	created, _ := e.srv.portfolios.Add("alice", p)

	owner := e.request(http.MethodGet, "/portfolios/"+created.ID+"/intelligence", "alice", nil)
	if owner.Code != http.StatusOK || !strings.Contains(owner.Body.String(), `"totalValue":100`) {
		t.Fatalf("owner status=%d body=%s", owner.Code, owner.Body.String())
	}
	foreign := e.request(http.MethodGet, "/portfolios/"+created.ID+"/intelligence", "bob", nil)
	if foreign.Code != http.StatusNotFound {
		t.Fatalf("foreign status=%d want 404", foreign.Code)
	}
}
