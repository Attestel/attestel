package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// eval_test.go — the scheduled evaluator: scheduled reviews, synthetic-state propagation, duplicate
// suppression, and the hard guarantee that no model is ever called from the evaluation loop (§4.6).

const testThesisID = "9f2c1b7a4e0d5a83"

// upstream is a stub for BOTH the analysis service and the gateway. It records every path it is
// asked for, which is how the no-LLM test proves a negative.
type upstream struct {
	mu     sync.Mutex
	paths  []string
	source string // price source label -> drives the dataState assertion
	price  float64
}

func newUpstream(t *testing.T, source string, price float64) (*upstream, *httptest.Server) {
	t.Helper()
	u := &upstream{source: source, price: price}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.paths = append(u.paths, r.URL.Path)
		u.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasPrefix(r.URL.Path, "/quote/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"price": u.price, "changePct": 1.0, "source": u.source})
		case strings.HasPrefix(r.URL.Path, "/analysis/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"source": u.source, "sourceIsSynthetic": u.source == "synthetic",
				"price":      map[string]any{"lastClose": u.price, "changePct": 1.0},
				"indicators": map[string]any{"latest": map[string]any{"rsi": 55.0, "macdHist": 0.1}},
				"regime":     map[string]any{"trend": "uptrend", "macdState": "bullish"},
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{})
		}
	}))
	t.Cleanup(srv.Close)
	return u, srv
}

func (u *upstream) seen() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.paths...)
}

func testEvaluator(t *testing.T, up *httptest.Server) (*Evaluator, *Store) {
	t.Helper()
	store, err := openStore(t.TempDir())
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	cfg := Config{
		AnalysisURL: up.URL, GatewayURL: up.URL,
		EvalInterval: time.Minute, Tickers: []string{"NVDA"},
	}
	return newEvaluator(cfg, store, newNotifier(cfg)), store
}

// ---------------------------------------------------------------- scheduled review

func TestScheduledReviewFiresWhenDue(t *testing.T) {
	_, up := newUpstream(t, "tiingo", 130)
	e, store := testEvaluator(t, up)

	// Created 40 days ago with a 30-day cadence: due.
	created := store.AddRule(Rule{
		UserID: "u1", Ticker: "NVDA", Timeframe: "1D", Type: TypeThesisReview,
		ThesisID: testThesisID, Intent: IntentThesisReview, Active: true, CooldownSec: 3600,
		Params: map[string]any{"everyDays": 30.0},
	})
	seedRuleCreatedAt(t, store, created.ID, time.Now().Unix()-40*86400)

	e.evalAll(context.Background())

	events, _ := store.ListEvents("u1", 10)
	if len(events) != 1 {
		t.Fatalf("a review 40 days into a 30-day cadence must fire once, got %d events", len(events))
	}
	ev := events[0]
	if ev.ThesisID != testThesisID {
		t.Errorf("the event must carry the thesis, got %q", ev.ThesisID)
	}
	if ev.ResearchAction != ActionReviewThesis {
		t.Errorf("a review must ask the user to review the thesis, got %q", ev.ResearchAction)
	}
	if ev.Bearing != nil {
		t.Errorf("a scheduled review changes nothing about the world, so it carries no bearing; got %q", *ev.Bearing)
	}
	if ev.ResearchLink == nil || ev.ResearchLink.Hash != "#research/thesis" {
		t.Errorf("the event must link to Research, got %+v", ev.ResearchLink)
	}
	if !strings.Contains(strings.ToLower(ev.Message), "review due") {
		t.Errorf("the message must say a review is due, got %q", ev.Message)
	}
}

func TestScheduledReviewStaysQuietBeforeItIsDue(t *testing.T) {
	_, up := newUpstream(t, "tiingo", 130)
	e, store := testEvaluator(t, up)

	store.AddRule(Rule{
		UserID: "u1", Ticker: "NVDA", Timeframe: "1D", Type: TypeThesisReview,
		ThesisID: testThesisID, Intent: IntentThesisReview, Active: true, CooldownSec: 3600,
		Params: map[string]any{"everyDays": 30.0},
	})
	e.evalAll(context.Background())

	if events, _ := store.ListEvents("u1", 10); len(events) != 0 {
		t.Fatalf("a review created today must not be due, got %d events", len(events))
	}
}

func TestOneShotReviewFiresOnItsDate(t *testing.T) {
	_, up := newUpstream(t, "tiingo", 130)
	e, store := testEvaluator(t, up)

	store.AddRule(Rule{
		UserID: "u1", Ticker: "NVDA", Timeframe: "1D", Type: TypeThesisReview,
		ThesisID: testThesisID, Intent: IntentAssumptionReview, ThesisItemID: "asm_1111111111111111",
		Active: true, CooldownSec: 3600, ReviewAt: time.Now().Unix() - 60,
	})
	e.evalAll(context.Background())

	events, _ := store.ListEvents("u1", 10)
	if len(events) != 1 {
		t.Fatalf("a one-shot review whose date has passed must fire, got %d", len(events))
	}
	if events[0].ThesisItemID != "asm_1111111111111111" {
		t.Errorf("the event must name the assumption it watches, got %q", events[0].ThesisItemID)
	}
}

// ---------------------------------------------------------------- synthetic labelling

// TestSyntheticStatePropagates is invariant #1's visible half at the event layer: a firing computed
// from synthetic demo bars must be unmistakably labelled everywhere it lands.
func TestSyntheticStatePropagates(t *testing.T) {
	for _, c := range []struct{ source, want string }{
		{"synthetic", DataStateSynthetic},
		{"tiingo", DataStateLive},
		{"yfinance", DataStateLive},
		{"seed", DataStateSeed},
	} {
		t.Run(c.source, func(t *testing.T) {
			_, up := newUpstream(t, c.source, 200)
			e, store := testEvaluator(t, up)
			store.AddRule(Rule{
				UserID: "u1", Ticker: "NVDA", Timeframe: "1D", Type: TypePriceCross, Active: true,
				CooldownSec: 3600, Params: map[string]any{"level": 130.0, "direction": "above"},
			})
			e.evalAll(context.Background())

			events, _ := store.ListEvents("u1", 10)
			if len(events) != 1 {
				t.Fatalf("expected one firing, got %d", len(events))
			}
			if events[0].DataState != c.want {
				t.Errorf("source %q must stamp dataState %q, got %q", c.source, c.want, events[0].DataState)
			}
		})
	}
}

// TestSyntheticEventIsLabelledOutOfApp — the notification body is often read away from the app,
// where the synthetic banner is not there to qualify it.
func TestSyntheticEventIsLabelledOutOfApp(t *testing.T) {
	body := notificationBody(Event{
		Ticker: "NVDA", Message: "NVDA price crossed above 130.00", DataState: DataStateSynthetic})
	if !strings.Contains(body, "SYNTHETIC") {
		t.Errorf("a synthetic-triggered notification must say so, got %q", body)
	}
}

// TestNotificationNamesTheCompany — D-19 is open, so an out-of-app link lands on the right SECTION
// of the ACTIVE company. The words are what make the notification unambiguous (§4.5).
func TestNotificationNamesTheCompany(t *testing.T) {
	body := notificationBody(Event{
		Ticker: "NVDA", Message: "condition met", ThesisID: testThesisID,
		ResearchAction: ActionReviewEvidence, DataState: DataStateLive,
		ResearchLink: researchLinkFor("NVDA", testThesisID),
	})
	if !strings.Contains(body, "NVDA") {
		t.Errorf("the notification body must name the company in text, got %q", body)
	}
	for _, banned := range []string{"buy", "sell", "should you"} {
		if strings.Contains(strings.ToLower(body), banned) {
			t.Errorf("notification body must carry no trade language, found %q in %q", banned, body)
		}
	}
}

// ---------------------------------------------------------------- duplicate suppression

func TestCooldownSuppressesDuplicateEvents(t *testing.T) {
	_, up := newUpstream(t, "tiingo", 200)
	e, store := testEvaluator(t, up)
	store.AddRule(Rule{
		UserID: "u1", Ticker: "NVDA", Timeframe: "1D", Type: TypePriceCross, Active: true,
		CooldownSec: 3600, Params: map[string]any{"level": 130.0, "direction": "above"},
	})

	e.evalAll(context.Background())
	e.evalAll(context.Background())
	e.evalAll(context.Background())

	events, _ := store.ListEvents("u1", 10)
	if len(events) != 1 {
		t.Fatalf("a persistent condition must fire once, not once per tick; got %d events", len(events))
	}
}

func TestEventsCarryADedupeKey(t *testing.T) {
	_, up := newUpstream(t, "tiingo", 200)
	e, store := testEvaluator(t, up)
	store.AddRule(Rule{
		UserID: "u1", Ticker: "NVDA", Timeframe: "1D", Type: TypePriceCross, Active: true,
		CooldownSec: 3600, ThesisID: testThesisID,
		Params: map[string]any{"level": 130.0, "direction": "above"},
	})
	e.evalAll(context.Background())

	events, _ := store.ListEvents("u1", 10)
	if len(events) != 1 || events[0].DedupeKey == "" {
		t.Fatalf("every event must carry a dedupe key so the change set can collapse repeats: %+v", events)
	}
}

// ---------------------------------------------------------------- no LLM in the scheduler

// TestSchedulerNeverCallsTheLLM is §4.6, proved two ways at once.
//
// Behaviourally: every rule type is evaluated against a stub that records each path it is asked for,
// and the recorded set must contain only deterministic endpoints — no /chat, /lens, /brief,
// /assistant, /committee, /thesis-check, /digest, /scenario or /read.
//
// Structurally: alerts' Config has no LLM URL at all, so there is nothing for the loop to call even
// if a future edit tried. The source scan below keeps that true.
func TestSchedulerNeverCallsTheLLM(t *testing.T) {
	up, srv := newUpstream(t, "tiingo", 200)
	e, store := testEvaluator(t, srv)

	// One rule of every type, so no evaluator branch is left unexercised.
	for _, r := range []Rule{
		{Type: TypePriceCross, Params: map[string]any{"level": 130.0, "direction": "above"}},
		{Type: TypePctMove, Params: map[string]any{"pct": 0.5, "window": "day"}},
		{Type: TypeRSIThreshold, Params: map[string]any{"level": 50.0, "direction": "above"}},
		{Type: TypeMACDCross, Params: map[string]any{"direction": "bullish"}},
		{Type: TypeTrendFlip, Params: map[string]any{}},
		{Type: TypeConfluenceConflict, Params: map[string]any{"mode": "appears"}},
		{Type: TypeVWAPCross, Params: map[string]any{"direction": "above"}},
		{Type: TypeNew8K, Params: map[string]any{}},
		{Type: TypeCalendarEvent, Params: map[string]any{"kind": "earnings", "lead": 3.0}},
		{Type: TypeThesisReview, ThesisID: testThesisID, Intent: IntentThesisReview,
			Params: map[string]any{"everyDays": 30.0}},
	} {
		rule := r
		rule.UserID, rule.Ticker, rule.Timeframe, rule.Active, rule.CooldownSec = "u1", "NVDA", "1D", true, 3600
		store.AddRule(rule)
	}

	e.evalAll(context.Background())

	banned := []string{"/chat", "/lens", "/brief", "/assistant", "/committee",
		"/thesis-check", "/digest", "/scenario", "/read", "/transcript", "llm"}
	for _, p := range up.seen() {
		for _, b := range banned {
			if strings.Contains(strings.ToLower(p), b) {
				t.Errorf("the evaluation loop reached a model endpoint: %q (matched %q)", p, b)
			}
		}
	}
	if len(up.seen()) == 0 {
		t.Fatal("the stub was never called — the test would pass vacuously")
	}
}

// TestAlertsHasNoModelClientInSource is the structural half of §4.6: it fails if a future edit
// introduces an LLM URL or an Ollama client anywhere in the service.
func TestAlertsHasNoModelClientInSource(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	banned := []string{"LLM_URL", "LLMURL", "ollama", "OLLAMA", "/chat", "qwen"}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, b := range banned {
			if strings.Contains(string(src), b) {
				t.Errorf("%s references %q — the alerts service must contain no model client (§4.6)", name, b)
			}
		}
	}
}

// seedRuleCreatedAt backdates a stored rule so a cadence test can place it in the past without
// sleeping. CreatedAt is server-owned (AddRule stamps it), so the test reaches into the store
// directly rather than pretending a client could set it.
func seedRuleCreatedAt(t *testing.T, store *Store, id string, createdAt int64) {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	for i := range store.rules {
		if store.rules[i].ID == id {
			store.rules[i].CreatedAt = createdAt
			store.persistRulesLocked()
			return
		}
	}
	t.Fatalf("rule %s not found", id)
}
