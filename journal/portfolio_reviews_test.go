package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type llmPortfolioFixture struct {
	mu            sync.Mutex
	reviewCalls   int
	scenarioCalls int
	contexts      []map[string]any
	server        *httptest.Server
}

func newLLMPortfolioFixture(t *testing.T) *llmPortfolioFixture {
	t.Helper()
	fixture := &llmPortfolioFixture{}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		context, _ := body["context"].(map[string]any)
		fixture.mu.Lock()
		fixture.contexts = append(fixture.contexts, context)
		switch r.URL.Path {
		case "/portfolio-review":
			fixture.reviewCalls++
			fixture.mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{
				"contextVersion": "untrusted-downstream-echo",
				"structured": map[string]any{
					"posture":       "Recorded portfolio context is available.",
					"supports":      []string{"A cash balance is recorded."},
					"threats":       []string{"A configured finding remains open."},
					"invalidations": []string{"A context change would invalidate this review."},
					"attention":     []any{}, "summary": "Review the supplied research context.",
				},
				"modelUsed": "stub:offline", "warnings": []string{}, "retried": false,
				"disclaimer": "Research explanation only.",
			})
		case "/portfolio-scenario":
			fixture.scenarioCalls++
			fixture.mu.Unlock()
			writeJSON(w, http.StatusOK, map[string]any{
				"contextVersion": "untrusted-downstream-echo", "question": "changed by downstream",
				"structured": map[string]any{
					"scenario": body["question"], "overallExposure": "unclear",
					"mostExposed": []any{}, "secondaryEffects": []any{}, "mitigants": []any{},
					"uncertainties": []string{"Model offline."}, "invalidations": []string{"Rerun later."},
					"summary": "No qualitative reasoning was performed.",
				},
				"modelUsed": "stub:offline", "warnings": []string{}, "retried": false,
				"disclaimer": "Hypothetical research only.",
			})
		default:
			fixture.mu.Unlock()
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fixture.server.Close)
	return fixture
}

func reviewPortfolio(t *testing.T, e *portfolioTestEnv) Portfolio {
	t.Helper()
	p := Portfolio{
		Name: "Review", BaseCurrency: "USD",
		Positions: []PortfolioPosition{{Ticker: "NVDA", Quantity: 2, ManualValue: ptr(500), Sector: "Technology"}},
		Cash:      []PortfolioCash{{Currency: "USD", Amount: 500}},
	}
	if apiErr := validatePortfolio(&p); apiErr != nil {
		t.Fatal(apiErr)
	}
	created, err := e.srv.portfolios.Add("alice", p)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func TestPortfolioReviewIsCachedByServerContextVersion(t *testing.T) {
	e := newPortfolioTestEnv(t)
	analysis := newAnalysisFixture(t)
	llm := newLLMPortfolioFixture(t)
	e.srv.cfg.AnalysisURL = analysis.server.URL
	e.srv.cfg.LLMURL = llm.server.URL
	e.srv.http = analysis.server.Client()
	p := reviewPortfolio(t, e)

	firstRec := e.request(http.MethodPost, "/portfolios/"+p.ID+"/review", "alice", nil)
	if firstRec.Code != http.StatusCreated {
		t.Fatalf("first status=%d body=%s", firstRec.Code, firstRec.Body.String())
	}
	var first struct {
		Review PortfolioReview `json:"review"`
		Reused bool            `json:"reused"`
	}
	_ = json.Unmarshal(firstRec.Body.Bytes(), &first)
	if first.Reused || first.Review.ContextVersion == "" || first.Review.ContextVersion == "untrusted-downstream-echo" {
		t.Fatalf("review=%+v reused=%v", first.Review, first.Reused)
	}

	secondRec := e.request(http.MethodPost, "/portfolios/"+p.ID+"/review", "alice", nil)
	if secondRec.Code != http.StatusOK {
		t.Fatalf("second status=%d body=%s", secondRec.Code, secondRec.Body.String())
	}
	var second struct {
		Review PortfolioReview `json:"review"`
		Reused bool            `json:"reused"`
	}
	_ = json.Unmarshal(secondRec.Body.Bytes(), &second)
	if !second.Reused || second.Review.ID != first.Review.ID {
		t.Fatalf("cache miss: first=%s second=%s reused=%v", first.Review.ID, second.Review.ID, second.Reused)
	}
	llm.mu.Lock()
	calls := llm.reviewCalls
	llm.mu.Unlock()
	if calls != 1 {
		t.Fatalf("review model calls=%d want 1", calls)
	}

	listRec := e.request(http.MethodGet, "/portfolios/"+p.ID+"/reviews", "alice", nil)
	if listRec.Code != http.StatusOK || !strings.Contains(listRec.Body.String(), first.Review.ID) {
		t.Fatalf("list status=%d body=%s", listRec.Code, listRec.Body.String())
	}
	foreign := e.request(http.MethodGet, "/portfolios/"+p.ID+"/reviews", "bob", nil)
	if foreign.Code != http.StatusNotFound {
		t.Fatalf("foreign status=%d want 404", foreign.Code)
	}
}

func TestPortfolioContextCacheIgnoresRenameButInvalidatesFinancialInput(t *testing.T) {
	e := newPortfolioTestEnv(t)
	analysis := newAnalysisFixture(t)
	llm := newLLMPortfolioFixture(t)
	e.srv.cfg.AnalysisURL = analysis.server.URL
	e.srv.cfg.LLMURL = llm.server.URL
	e.srv.http = analysis.server.Client()
	p := reviewPortfolio(t, e)
	first := e.request(http.MethodPost, "/portfolios/"+p.ID+"/review", "alice", nil)
	if first.Code != http.StatusCreated {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}

	p.Name = "Renamed only"
	p, ok, err := e.srv.portfolios.Replace("alice", p)
	if err != nil || !ok {
		t.Fatalf("rename err=%v ok=%v", err, ok)
	}
	renamed := e.request(http.MethodPost, "/portfolios/"+p.ID+"/review", "alice", nil)
	if renamed.Code != http.StatusOK || !strings.Contains(renamed.Body.String(), `"reused":true`) {
		t.Fatalf("rename invalidated context status=%d body=%s", renamed.Code, renamed.Body.String())
	}

	p.Cash[0].Amount = 750
	_, ok, err = e.srv.portfolios.Replace("alice", p)
	if err != nil || !ok {
		t.Fatalf("cash edit err=%v ok=%v", err, ok)
	}
	changed := e.request(http.MethodPost, "/portfolios/"+p.ID+"/review", "alice", nil)
	if changed.Code != http.StatusCreated || !strings.Contains(changed.Body.String(), `"reused":false`) {
		t.Fatalf("financial change did not invalidate status=%d body=%s", changed.Code, changed.Body.String())
	}
	llm.mu.Lock()
	calls := llm.reviewCalls
	llm.mu.Unlock()
	if calls != 2 {
		t.Fatalf("review calls=%d want 2", calls)
	}
}

func TestPortfolioScenarioUsesCurrentOwnedContextAndNeverTrustsDownstreamIdentity(t *testing.T) {
	e := newPortfolioTestEnv(t)
	analysis := newAnalysisFixture(t)
	llm := newLLMPortfolioFixture(t)
	e.srv.cfg.AnalysisURL = analysis.server.URL
	e.srv.cfg.LLMURL = llm.server.URL
	e.srv.http = analysis.server.Client()
	p := reviewPortfolio(t, e)
	question := "What happens if rates stay restrictive?"
	rec := e.request(http.MethodPost, "/portfolios/"+p.ID+"/scenario", "alice", map[string]any{"question": question})
	if rec.Code != http.StatusOK {
		t.Fatalf("scenario status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Scenario portfolioLLMResponse `json:"scenario"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body.Scenario.ContextVersion == "" || body.Scenario.ContextVersion == "untrusted-downstream-echo" || body.Scenario.Question != question {
		t.Fatalf("scenario identity=%+v", body.Scenario)
	}
	llm.mu.Lock()
	contexts, calls := append([]map[string]any(nil), llm.contexts...), llm.scenarioCalls
	llm.mu.Unlock()
	if calls != 1 || len(contexts) != 1 {
		t.Fatalf("scenario calls=%d contexts=%d", calls, len(contexts))
	}
	positions := contexts[0]["positions"].([]any)
	if len(positions) != 1 || positions[0].(map[string]any)["ticker"] != "NVDA" {
		t.Fatalf("context positions=%v", positions)
	}

	empty := e.request(http.MethodPost, "/portfolios/"+p.ID+"/scenario", "alice", map[string]any{"question": " "})
	if empty.Code != http.StatusBadRequest {
		t.Fatalf("empty status=%d body=%s", empty.Code, empty.Body.String())
	}
	foreign := e.request(http.MethodPost, "/portfolios/"+p.ID+"/scenario", "bob", map[string]any{"question": question})
	if foreign.Code != http.StatusNotFound {
		t.Fatalf("foreign status=%d want 404", foreign.Code)
	}
}
