package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func scoutFixtureCandidate(ticker string, rank int, score float64, related ...string) scoutCandidate {
	return scoutCandidate{
		Ticker: ticker, Rank: rank, AttentionScore: score, AttentionBand: "monitor",
		Components:     map[string]any{"eventAttention": score},
		WhyNow:         "Fresh product launch evidence: a cited company announcement.",
		Evidence:       []map[string]any{{"kind": "canonical_event", "id": "evt_" + ticker}},
		RelatedTickers: related, LatestEvidenceAt: "2026-08-26T18:00:00Z",
		SourceTiers: []string{"official"}, DataState: "live",
	}
}

func TestPersonalizeScoutReranksRelationshipsAndExcludesCoveredCompanies(t *testing.T) {
	follow := testFollowSet("NVDA")
	holdings := map[string]bool{"MSFT": true}
	candidates := []scoutCandidate{
		scoutFixtureCandidate("ORCL", 1, 0.60),
		scoutFixtureCandidate("AMD", 2, 0.55, "NVDA"),
		scoutFixtureCandidate("CRM", 3, 0.54, "MSFT"),
		scoutFixtureCandidate("NVDA", 4, 0.90),
		scoutFixtureCandidate("MSFT", 5, 0.85),
	}

	got := personalizeScout(candidates, follow, holdings, 20)
	if len(got) != 3 {
		t.Fatalf("candidates = %d, want 3 after followed/held companies are excluded", len(got))
	}
	if got[0].Ticker != "AMD" || got[1].Ticker != "CRM" || got[2].Ticker != "ORCL" {
		t.Fatalf("personalized order = %v, want AMD, CRM, ORCL", []string{
			got[0].Ticker, got[1].Ticker, got[2].Ticker,
		})
	}
	if got[0].WhyYouAreSeeingThis == "" || got[0].WhyNow == "" || got[0].Disclaimer == "" {
		t.Fatal("a surfaced Scout lead is missing its explanation or disclosure")
	}
	if got[0].BaseRank != 2 || got[0].Rank != 1 {
		t.Fatalf("AMD ranks = base %d personalized %d, want 2 -> 1", got[0].BaseRank, got[0].Rank)
	}
}

func TestPersonalizeScoutUsesSectorOverlapOnlyAsTheWeakerRung(t *testing.T) {
	got := personalizeScout(
		[]scoutCandidate{scoutFixtureCandidate("AMD", 1, 0.50)},
		testFollowSet("NVDA"), map[string]bool{}, 10,
	)
	if len(got) != 1 {
		t.Fatal("AMD disappeared")
	}
	want := scoutBaseWeight*0.50 + scoutRelationshipWeight*0.50
	if got[0].AttentionScore != want {
		t.Fatalf("score = %v, want weaker sector rung %v", got[0].AttentionScore, want)
	}
	if got[0].WhyYouAreSeeingThis != "AMD shares the semiconductors coverage group with NVDA." {
		t.Fatalf("why = %q", got[0].WhyYouAreSeeingThis)
	}
}

func TestScoutHandlerReadsStoredCandidatesAndPortfolioContextWithoutCallingLLM(t *testing.T) {
	events := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/scout" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"runId": "sct_1", "scoreVersion": "scout@2",
			"universeVersion": "scout-universe@1", "asOf": "2026-08-26T20:00:00Z",
			"coverage": map[string]any{"state": "ok"},
			"candidates": []scoutCandidate{
				scoutFixtureCandidate("AMD", 1, 0.55, "NVDA"),
				scoutFixtureCandidate("CRM", 2, 0.52, "MSFT"),
			},
			"degraded": []string{},
		})
	}))
	defer events.Close()

	journal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/subscriptions":
			writeJSON(w, http.StatusOK, map[string]any{
				"subscriptions": []map[string]any{{"ticker": "NVDA"}},
			})
		case "/portfolios":
			writeJSON(w, http.StatusOK, map[string]any{
				"portfolios": []map[string]any{{
					"positions": []map[string]any{{"ticker": "MSFT", "quantity": 1}},
				}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer journal.Close()

	llmCalls := 0
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		llmCalls++
		http.Error(w, "must not be reached", http.StatusInternalServerError)
	}))
	defer llm.Close()

	cfg := loadConfig()
	cfg.EventsURL = events.URL
	cfg.JournalURL = journal.URL
	cfg.LLMURL = llm.URL
	cfg.Secret = "test-secret"
	cfg.CookieName = "nvda_session"
	srv := &Server{cfg: cfg, cache: newCache(), http: http.DefaultClient}
	mux := http.NewServeMux()
	srv.registerEventRoutes(mux)

	req := httptest.NewRequest(http.MethodGet, "/api/scout", nil)
	req.AddCookie(&http.Cookie{
		Name: cfg.CookieName, Value: testSessionToken(cfg.Secret, "alice"),
	})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body scoutResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.RunID == nil || *body.RunID != "sct_1" || len(body.Candidates) != 2 {
		t.Fatalf("body = %+v", body)
	}
	if len(body.PortfolioTickers) != 1 || body.PortfolioTickers[0] != "MSFT" {
		t.Fatalf("portfolio tickers = %v", body.PortfolioTickers)
	}
	if llmCalls != 0 {
		t.Fatalf("Scout page load made %d model calls", llmCalls)
	}
}

func TestScoutHandlerFailsClosedWhenEventStoreIsUnavailable(t *testing.T) {
	cfg := loadConfig()
	cfg.EventsURL = "http://127.0.0.1:1"
	cfg.JournalURL = "http://127.0.0.1:1"
	srv := &Server{cfg: cfg, cache: newCache(), http: http.DefaultClient}
	mux := http.NewServeMux()
	srv.registerEventRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/scout", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body scoutResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.Candidates) != 0 || !hasString(body.Degraded, degradedEventsUnreachable) {
		t.Fatalf("candidates=%d degraded=%v, want empty + events:unreachable",
			len(body.Candidates), body.Degraded)
	}
}

func TestScoutDoesNotCacheMissingPersonalContext(t *testing.T) {
	for _, marker := range []string{
		degradedEventsUnreachable, degradedEventsUnconfigured,
		degradedSubscriptionsUnreachable, degradedScoutPortfolioUnreachable,
	} {
		if scoutResponseCacheable([]string{marker}) {
			t.Fatalf("degraded marker %q was cacheable", marker)
		}
	}
	if !scoutResponseCacheable([]string{"scout:no-runs"}) {
		t.Fatal("a valid empty Scout snapshot should remain cacheable")
	}
}
