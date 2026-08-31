package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// engine_test.go — the per-bar allocation rule and the four hard gates
// (docs/PAPER_EXECUTION_CONTRACT.md §3 and §4).
//
// GAPS.md said the fill simulation itself was untested and set the trigger explicitly: "whenever the
// simulation's fill model is next changed." This is that change — the engine stopped opening once and
// holding H bars and started reconciling a target position every bar — so this is that suite.
//
// Everything upstream is a stdlib httptest fake (prediction, analysis, journal), like the alerts
// service's tests. No network, no model, no clock dependence: `now` is passed in.

// --------------------------------------------------------------------------- fakes

type fakes struct {
	mu       sync.Mutex
	analysis *httptest.Server
	predict  *httptest.Server
	journal  *httptest.Server

	bar      map[string]any // GET /candles/{ticker}
	quote    map[string]any // GET /quote/{ticker}
	pred     map[string]any // GET /predict/{ticker}
	barErr   bool
	quoteErr bool
	predErr  bool

	// PER-TICKER overrides, for the multi-config fixtures. A single shared bar/quote/pred is enough
	// for one config, but a tick over several configs is only an interesting test when the configs
	// see DIFFERENT prices — otherwise every ordering question is hidden by symmetry. Empty by
	// default, so every existing fixture behaves exactly as before.
	barBy   map[string]map[string]any
	quoteBy map[string]map[string]any
	predBy  map[string]map[string]any

	// GET /trades?mode=paper&status=... — what the journal believes it holds. Empty by default; a
	// non-empty open list is how an ORPHAN (a trade the engine has no memory of) is simulated.
	openTrades   []any
	closedTrades []any
	listErr      bool
	// PATCH /trades/{id} outcome. 0 = accept; otherwise the status to answer with (404 = the trade
	// is gone, 503 = the journal is unreachable — two failures with opposite consequences).
	patchStatus int
	// Trade ids DELETE must refuse. /paper/reset deletes every paper trade before it wipes anything,
	// so "one of them would not delete" is a case with its own required behaviour.
	deleteFail map[string]bool

	calls  []string // ordered journal MUTATIONS, e.g. "PATCH /trades/t1" (reads are not recorded)
	nextID int
}

func newFakes(t *testing.T, now time.Time) *fakes {
	t.Helper()
	f := &fakes{
		bar:   cleanBar(now),
		quote: cleanQuote(now),
		pred:  cleanPredict(now, "Buy"),
	}

	amux := http.NewServeMux()
	amux.HandleFunc("GET /candles/{ticker}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.barErr {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(pick(f.barBy, r.PathValue("ticker"), f.bar))
	})
	amux.HandleFunc("GET /quote/{ticker}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.quoteErr {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(pick(f.quoteBy, r.PathValue("ticker"), f.quote))
	})
	f.analysis = httptest.NewServer(amux)

	f.predict = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.predErr {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		ticker := strings.TrimPrefix(r.URL.Path, "/predict/")
		_ = json.NewEncoder(w).Encode(pick(f.predBy, ticker, f.pred))
	}))

	jmux := http.NewServeMux()
	jmux.HandleFunc("POST /trades", func(w http.ResponseWriter, r *http.Request) {
		var trade map[string]any
		_ = json.NewDecoder(r.Body).Decode(&trade)
		f.mu.Lock()
		defer f.mu.Unlock()
		f.nextID++
		id := fmt.Sprintf("t%d", f.nextID)
		body, _ := json.Marshal(map[string]any{"trade": map[string]any{"id": id}})
		f.calls = append(f.calls, "POST /trades")
		// THE FAKE JOURNAL REMEMBERS. It used to answer with an id and forget the trade, so
		// `GET /trades?status=open` never listed anything the engine had just opened. That was
		// invisible until the three-store reconciliation (reconcile.go) started comparing the
		// engine's position against the journal's open trades — at which point every fixture read as
		// "the engine holds a trade the journal has never heard of", which is a defect in the fake,
		// not in the engine. A journal that does not hold what it accepted is not a journal.
		if trade == nil {
			trade = map[string]any{}
		}
		trade["id"] = id
		trade["status"] = "open"
		f.openTrades = append(f.openTrades, trade)
		_, _ = w.Write(body)
	})
	jmux.HandleFunc("PATCH /trades/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, "PATCH /trades/"+id)
		if f.patchStatus != 0 {
			w.WriteHeader(f.patchStatus)
			_, _ = w.Write([]byte(`{"error":"nope"}`))
			return
		}
		f.closeTradeLocked(id)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	jmux.HandleFunc("DELETE /trades/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, "DELETE /trades/"+id)
		if f.deleteFail[id] {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"cannot delete"}`))
			return
		}
		f.closeTradeLocked(id)
		f.closedTrades = dropTrade(f.closedTrades, id)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	jmux.HandleFunc("GET /trades", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.listErr {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		trades := f.openTrades
		if r.URL.Query().Get("status") == "closed" {
			trades = f.closedTrades
		}
		if trades == nil {
			trades = []any{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"trades": trades})
	})
	f.journal = httptest.NewServer(jmux)

	t.Cleanup(func() {
		f.analysis.Close()
		f.predict.Close()
		f.journal.Close()
	})
	return f
}

// closeTradeLocked moves a trade from the open list to the closed one, the way an exit PATCH does.
func (f *fakes) closeTradeLocked(id string) {
	for _, t := range f.openTrades {
		if m, ok := t.(map[string]any); ok && m["id"] == id {
			m["status"] = "closed"
			f.closedTrades = append(f.closedTrades, m)
		}
	}
	f.openTrades = dropTrade(f.openTrades, id)
}

func dropTrade(trades []any, id string) []any {
	out := make([]any, 0, len(trades))
	for _, t := range trades {
		if m, ok := t.(map[string]any); ok && m["id"] == id {
			continue
		}
		out = append(out, t)
	}
	return out
}

// pick returns the per-ticker override when one is registered, and the shared fixture otherwise.
func pick(by map[string]map[string]any, ticker string, def map[string]any) map[string]any {
	if v, ok := by[ticker]; ok {
		return v
	}
	return def
}

func (f *fakes) journalCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakes) set(mutate func(*fakes)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	mutate(f)
}

// barLabel is the bar a decision taken at `now` may act on: the last COMPLETED daily session, i.e.
// YESTERDAY'S. Today's bar is still forming — its close is not its close — and the engine refuses to
// decide on it (contract §2). Every fixture here is built around that, because that is the real
// shape of the data the engine sees during a session.
func barLabel(now time.Time) string {
	return now.AddDate(0, 0, -1).UTC().Format("2006-01-02")
}

// cleanBar is a real, COMPLETED bar as of `now`.
func cleanBar(now time.Time) map[string]any {
	return map[string]any{
		"ticker": "NVDA", "timeframe": "1D", "source": "tiingo", "sourceIsSynthetic": false,
		"bars": []any{map[string]any{"time": barLabel(now), "close": 100.0}},
	}
}

// formingBar is the newest bar DURING the session it names: dated today, not yet closed.
func formingBar(now time.Time) map[string]any {
	return map[string]any{
		"ticker": "NVDA", "timeframe": "1D", "source": "tiingo", "sourceIsSynthetic": false,
		"bars": []any{map[string]any{"time": now.UTC().Format("2006-01-02"), "close": 100.0}},
	}
}

// cleanQuote is a real quote, dated now — never older than the bar it reconciles.
func cleanQuote(now time.Time) map[string]any {
	return map[string]any{
		"symbol": "NVDA", "price": 100.0, "source": "tiingo",
		"asOf": now.UTC().Format(time.RFC3339),
	}
}

// cleanPredict is a /predict response that passes ALL FOUR gates — the only state in which the
// engine is allowed to open anything.
func cleanPredict(now time.Time, direction string) map[string]any {
	return map[string]any{
		"ticker": "NVDA", "timeframe": "1D", "horizon": 5,
		"modelVersion": "model-v1", "strategyVersion": "sv1-abcdef0123456789",
		"signal":             map[string]any{"direction": direction, "probUp": 0.62, "confidence": 0.31},
		"backtest":           map[string]any{"passed": true, "costBps": 6.0, "allowShort": true, "hitRate": 0.55},
		"trainedOnSynthetic": false,
		"dataThrough":        now.UTC().Format("2006-01-02"),
		"currentData":        map[string]any{"source": "tiingo", "synthetic": false},
		"evaluation": map[string]any{
			"verdict": "EDGE", "evaluatedAt": "2026-08-20T00:00:00Z",
			"strategyVersion": "sv1-abcdef0123456789", "method": "portfolio-v3",
			"current": true, "evidenceCurrent": true,
		},
	}
}

var testCfg = PaperCfg{Ticker: "NVDA", Timeframe: "1D", Horizon: 5}

// harness wires an engine against the fakes with a fresh store under `dir`.
func harness(t *testing.T, f *fakes, dir string) (*Engine, *Store) {
	t.Helper()
	cfg := Config{
		PredictionURL: f.predict.URL, AnalysisURL: f.analysis.URL, JournalURL: f.journal.URL,
		Configs: []PaperCfg{testCfg}, DataDir: dir,
		MaxBarAgeSessions: 3, MaxModelAgeSessions: 10,
		StartingCash: defaultStartingCash,
	}
	store, err := openStore(dir, cfg.Configs)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	// A REAL ledger, not a nil one. Every test in this file therefore drives the fake-money book as
	// well as the decision rule, which is what makes "the engine opened a position" and "the book
	// recorded the fill" the same assertion rather than two hopes.
	ledger, lerr := openLedger(dir, cfg.StartingCash)
	if lerr != nil {
		t.Fatalf("openLedger: %v", lerr)
	}
	return newEngine(cfg, store, newClients(cfg), ledger, nil), store
}

// harnessNoLedger is the DEPRECATED-FALLBACK shape: an engine with no book at all, which must still
// decide, still refuse, and still say that it is sizing with a constant.
func harnessNoLedger(t *testing.T, f *fakes, dir string) (*Engine, *Store) {
	t.Helper()
	e, store := harness(t, f, dir)
	e.ledger = nil
	e.ledgerErr = errors.New("ledger disabled for this test")
	return e, store
}

func lastDecision(t *testing.T, store *Store) *Decision {
	t.Helper()
	st := store.StateFor(testCfg)
	if st.LastDecision == nil {
		t.Fatal("engine recorded no decision — a decision nobody can see is not a decision")
	}
	return st.LastDecision
}

// --------------------------------------------------------------------------- new-bar detection

func TestActsOncePerBar(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	e, store := harness(t, f, t.TempDir())
	ctx := context.Background()

	e.evalConfig(ctx, testCfg, now)
	e.evalConfig(ctx, testCfg, now.Add(5*time.Minute))
	e.evalConfig(ctx, testCfg, now.Add(10*time.Minute))

	if got := f.journalCalls(); len(got) != 1 || got[0] != "POST /trades" {
		t.Fatalf("EVAL_INTERVAL is a polling rate, not a decision rate; journal calls = %v", got)
	}
	if store.StateFor(testCfg).Side != "long" {
		t.Errorf("expected a long position, state = %+v", store.StateFor(testCfg))
	}
}

func TestANewBarIsDecidedOnAgain(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	e, store := harness(t, f, t.TempDir())
	ctx := context.Background()

	e.evalConfig(ctx, testCfg, now) // opens long on bar 08-20
	next := now.AddDate(0, 0, 1)
	f.set(func(f *fakes) {
		f.bar = cleanBar(next)
		f.pred = cleanPredict(next, "Hold") // the target goes flat on the new bar
	})
	e.evalConfig(ctx, testCfg, next)

	want := []string{"POST /trades", "PATCH /trades/t1"}
	if got := f.journalCalls(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("journal calls = %v, want %v", got, want)
	}
	if st := store.StateFor(testCfg); st.Side != "" {
		t.Errorf("expected flat after a Hold target, got %q", st.Side)
	}
	if d := lastDecision(t, store); d.Bar != barLabel(next) {
		t.Errorf("decision recorded against bar %q, want %q", d.Bar, barLabel(next))
	}
}

// A restart mid-bar must not act twice: the bar cursor is persisted, not held in memory.
func TestRestartMidBarIsIdempotent(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	f := newFakes(t, now)

	e1, _ := harness(t, f, dir)
	e1.evalConfig(context.Background(), testCfg, now)

	// Fresh store from the SAME directory = a process restart.
	e2, store2 := harness(t, f, dir)
	e2.evalConfig(context.Background(), testCfg, now.Add(time.Minute))

	if got := f.journalCalls(); len(got) != 1 {
		t.Fatalf("a restart within one bar must not re-act; journal calls = %v", got)
	}
	if st := store2.StateFor(testCfg); st.Side != "long" || st.TradeID != "t1" {
		t.Errorf("the position must survive the restart, got %+v", st)
	}
}

// An unreachable upstream is NOT a decision: the bar must stay un-consumed so the engine retries.
func TestTransportFailureDoesNotConsumeTheBar(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	e, store := harness(t, f, t.TempDir())
	ctx := context.Background()

	f.set(func(f *fakes) { f.predErr = true })
	e.evalConfig(ctx, testCfg, now)
	if st := store.StateFor(testCfg); st.LastBarActedOn != "" {
		t.Fatalf("a failed fetch must not consume the bar, cursor = %q", st.LastBarActedOn)
	}
	if d := lastDecision(t, store); !strings.Contains(d.Reason, "prediction unavailable") {
		t.Errorf("reason = %q", d.Reason)
	}

	f.set(func(f *fakes) { f.predErr = false })
	e.evalConfig(ctx, testCfg, now.Add(time.Minute)) // same bar, retried
	if got := f.journalCalls(); len(got) != 1 {
		t.Fatalf("the retry within the same bar should have acted, calls = %v", got)
	}
}

func TestNoBarMeansNoDecision(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	e, store := harness(t, f, t.TempDir())

	f.set(func(f *fakes) { f.barErr = true })
	e.evalConfig(context.Background(), testCfg, now)

	if got := f.journalCalls(); len(got) != 0 {
		t.Fatalf("nothing may be written without a bar, calls = %v", got)
	}
	d := lastDecision(t, store)
	if d.Gate != "data" || !strings.Contains(d.Reason, "latest bar unavailable") {
		t.Errorf("decision = %+v", d)
	}
}

// PAPER_BAR_SECONDS > 0 is the demo override: every tick counts as a new bar.
func TestFastForwardCountsEveryTickAsABar(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	e, _ := harness(t, f, t.TempDir())
	e.cfg.BarSeconds = 1

	e.evalConfig(context.Background(), testCfg, now)
	f.set(func(f *fakes) { f.pred = cleanPredict(now, "Hold") })
	e.evalConfig(context.Background(), testCfg, now)

	want := []string{"POST /trades", "PATCH /trades/t1"}
	if got := f.journalCalls(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("fast-forward should have decided twice on the same real bar, calls = %v", got)
	}
}

// --------------------------------------------------------------------------- reconciliation

func TestReconcileOpensFromFlat(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	e, store := harness(t, f, t.TempDir())

	e.evalConfig(context.Background(), testCfg, now)

	st := store.StateFor(testCfg)
	if st.Side != "long" || st.TradeID != "t1" || st.EntryPrice != 100.0 {
		t.Fatalf("state = %+v", st)
	}
	if st.EntryBar != barLabel(now) {
		t.Errorf("the entry must record the bar it was decided on, got %q", st.EntryBar)
	}
	if st.EntrySynthetic {
		t.Error("a new open can never be synthetic — gate 1 rejects, it does not mark")
	}
	if d := lastDecision(t, store); d.Action != "open" || d.From != "flat" || d.Target != "long" {
		t.Errorf("decision = %+v", d)
	}
}

func TestReconcileHoldsAnUnchangedTarget(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	e, store := harness(t, f, t.TempDir())
	ctx := context.Background()

	e.evalConfig(ctx, testCfg, now) // long
	next := now.AddDate(0, 0, 1)
	f.set(func(f *fakes) { f.bar = cleanBar(next); f.pred = cleanPredict(next, "Buy") })
	e.evalConfig(ctx, testCfg, next)

	if got := f.journalCalls(); len(got) != 1 {
		t.Fatalf("holding a position must write nothing (and cost nothing), calls = %v", got)
	}
	if d := lastDecision(t, store); d.Action != "hold" {
		t.Errorf("decision = %+v", d)
	}
}

func TestReconcileClosesToFlat(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	e, store := harness(t, f, t.TempDir())
	ctx := context.Background()

	e.evalConfig(ctx, testCfg, now)
	next := now.AddDate(0, 0, 1)
	f.set(func(f *fakes) { f.bar = cleanBar(next); f.pred = cleanPredict(next, "Hold") })
	e.evalConfig(ctx, testCfg, next)

	if got := f.journalCalls(); got[len(got)-1] != "PATCH /trades/t1" {
		t.Fatalf("calls = %v", got)
	}
	if st := store.StateFor(testCfg); st.Side != "" || st.TradeID != "" {
		t.Errorf("state after a close = %+v", st)
	}
	if d := lastDecision(t, store); d.Action != "close" || d.Target != "flat" {
		t.Errorf("decision = %+v", d)
	}
}

// A flip is close-then-open, in that order, at one reconciliation.
func TestReconcileFlipsClosingBeforeOpening(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	e, store := harness(t, f, t.TempDir())
	ctx := context.Background()

	e.evalConfig(ctx, testCfg, now) // long, t1
	next := now.AddDate(0, 0, 1)
	f.set(func(f *fakes) { f.bar = cleanBar(next); f.pred = cleanPredict(next, "Sell") })
	e.evalConfig(ctx, testCfg, next)

	want := []string{"POST /trades", "PATCH /trades/t1", "POST /trades"}
	if got := f.journalCalls(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("a flip must close then open; calls = %v, want %v", got, want)
	}
	st := store.StateFor(testCfg)
	if st.Side != "short" || st.TradeID != "t2" {
		t.Errorf("state after a flip = %+v", st)
	}
	if d := lastDecision(t, store); d.Action != "flip" || d.From != "long" || d.Target != "short" {
		t.Errorf("decision = %+v", d)
	}
}

// A short is refused when the record never backtested shorting, even if a Sell somehow arrives.
func TestShortRefusedWhenShortingWasNeverBacktested(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	e, store := harness(t, f, t.TempDir())

	f.set(func(f *fakes) {
		p := cleanPredict(now, "Sell")
		p["backtest"].(map[string]any)["allowShort"] = false
		f.pred = p
	})
	e.evalConfig(context.Background(), testCfg, now)

	if got := f.journalCalls(); len(got) != 0 {
		t.Fatalf("calls = %v", got)
	}
	if d := lastDecision(t, store); !strings.Contains(d.Reason, "never backtested shorting") {
		t.Errorf("reason = %q", d.Reason)
	}
}

func TestNoSignalIsAnAbsenceNotAFlatTarget(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	e, store := harness(t, f, t.TempDir())
	ctx := context.Background()

	e.evalConfig(ctx, testCfg, now) // long
	next := now.AddDate(0, 0, 1)
	f.set(func(f *fakes) {
		f.bar = cleanBar(next)
		p := cleanPredict(next, "Buy")
		p["signal"] = nil
		p["reason"] = "insufficient validation"
		f.pred = p
	})
	e.evalConfig(ctx, testCfg, next)

	if got := f.journalCalls(); len(got) != 1 {
		t.Fatalf("a silent prediction service must not close a position, calls = %v", got)
	}
	if st := store.StateFor(testCfg); st.Side != "long" {
		t.Errorf("the position must be left alone, got %q", st.Side)
	}
	d := lastDecision(t, store)
	if d.Gate != "signal" || !strings.Contains(d.Reason, "insufficient validation") {
		t.Errorf("decision = %+v", d)
	}
	if st := store.StateFor(testCfg); st.LastBarActedOn != barLabel(next) {
		t.Error("an absent signal IS a decision for the bar and must consume it")
	}
}

// A quote the engine cannot use blocks the action but not the bar: it retries.
func TestUnusableQuoteDoesNotConsumeTheBar(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	e, store := harness(t, f, t.TempDir())

	f.set(func(f *fakes) { f.quoteErr = true })
	e.evalConfig(context.Background(), testCfg, now)

	if got := f.journalCalls(); len(got) != 0 {
		t.Fatalf("calls = %v", got)
	}
	if st := store.StateFor(testCfg); st.LastBarActedOn != "" {
		t.Errorf("cursor = %q, want un-consumed", st.LastBarActedOn)
	}
	if d := lastDecision(t, store); d.Gate != "quote" {
		t.Errorf("decision = %+v", d)
	}
}

// --------------------------------------------------------------------------- legacy state

// A position from before exact sizing evidence existed is migrated for visibility but frozen. Its
// old price alone is not enough to invent the quantity/notional/N that the ledger never recorded.
func TestLegacyOpenPositionWithoutSizingEvidenceIsMigratedAndBlocked(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	legacy := `{
	  "configs": [{"ticker":"NVDA","timeframe":"1D","horizon":5}],
	  "open": {"NVDA:1D:5": {"tradeId":"old1","ticker":"NVDA","timeframe":"1D","horizon":5,
	           "side":"long","entryPrice":90,"entryTime":1,"dueAt":99999999999,"entrySynthetic":true}}
	}`
	if err := writeFile(dir+"/state.json", legacy); err != nil {
		t.Fatal(err)
	}
	f := newFakes(t, now)
	// The JOURNAL still holds the legacy trade open — it is the source of truth for trades, and the
	// engine's migrated state is only bookkeeping about it. Without this the fixture would describe
	// a journal that lost a trade it accepted, which the three-store reconciliation would (rightly)
	// call a desync.
	f.set(func(f *fakes) {
		f.openTrades = []any{map[string]any{
			"id": "old1", "ticker": "NVDA", "side": "long", "status": "open", "mode": "paper",
			"entry":          map[string]any{"price": 90.0, "timeframe": "1D"},
			"attachedSignal": map[string]any{"horizon": 5},
		}}
	})
	e, store := harness(t, f, dir)

	if st := store.StateFor(testCfg); st.Side != "long" || st.TradeID != "old1" {
		t.Fatalf("the legacy position must migrate, got %+v", st)
	}
	f.set(func(f *fakes) { f.pred = cleanPredict(now, "Hold") })
	e.evalConfig(context.Background(), testCfg, now)

	if got := f.journalCalls(); len(got) != 0 {
		t.Fatalf("the legacy position must not move on a reconstructed fill, calls = %v", got)
	}
	if d := lastDecision(t, store); d.Gate != "sync" {
		t.Fatalf("the missing historical lot must be a visible sync refusal, got %+v", d)
	}
}

func writeFile(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o644)
}

// --------------------------------------------------------------------------- bar completeness
//
// "Strictly newer than the last bar acted on" is necessary but not sufficient (contract §2).
// `/candles?limit=1` serves the newest bar the provider HAS, and during a session that bar is still
// FORMING: its close is not its close. Acting on it decides at a price that has not happened yet —
// and then never re-decides, because the cursor has moved past it.

func TestAFormingBarIsNotActedOn(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	e, store := harness(t, f, t.TempDir())

	f.set(func(f *fakes) { f.bar = formingBar(now) }) // dated today: the session is still open
	e.evalConfig(context.Background(), testCfg, now)

	if got := f.journalCalls(); len(got) != 0 {
		t.Fatalf("a forming bar must not produce a fill, calls = %v", got)
	}
	st := store.StateFor(testCfg)
	if st.LastBarActedOn != "" || st.LastBarUnix != 0 {
		t.Errorf("the cursor must not advance past a bar that has not closed, got %q", st.LastBarActedOn)
	}
	if st.LastDecision != nil {
		t.Errorf("an incomplete bar is 'no new bar yet', not a decision; got %+v", st.LastDecision)
	}

	// ...and the SAME bar, once the session is over, is acted on normally.
	e.evalConfig(context.Background(), testCfg, now.AddDate(0, 0, 1))
	if got := f.journalCalls(); len(got) != 1 || got[0] != "POST /trades" {
		t.Fatalf("the completed bar should have been decided on, calls = %v", got)
	}
	if st := store.StateFor(testCfg); st.LastBarActedOn != now.UTC().Format("2006-01-02") {
		t.Errorf("cursor = %q, want the now-completed bar", st.LastBarActedOn)
	}
}

func TestFastForwardBypassesTheCompletenessCheck(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	e, _ := harness(t, f, t.TempDir())
	e.cfg.BarSeconds = 1 // PAPER_BAR_SECONDS: demo/testing only, documented as such

	f.set(func(f *fakes) { f.bar = formingBar(now) })
	e.evalConfig(context.Background(), testCfg, now)

	if got := f.journalCalls(); len(got) != 1 {
		t.Fatalf("fast-forward is the documented demo bypass; calls = %v", got)
	}
}

func TestBarCompleteness(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	daily := func(d string) *latestBar {
		return &latestBar{Time: barTime{Label: d, Unix: mustDay(t, d).Unix()}, Source: "tiingo"}
	}
	intraday := func(at time.Time) *latestBar {
		return &latestBar{Time: barTime{Label: at.Format(time.RFC3339), Unix: at.Unix()}, Source: "tiingo"}
	}
	cases := []struct {
		name      string
		bar       *latestBar
		timeframe string
		want      bool
	}{
		{"yesterday's daily bar is closed", daily("2026-08-19"), "1D", true},
		{"today's daily bar is still forming", daily("2026-08-20"), "1D", false},
		{"a future-dated daily bar is not complete", daily("2026-08-21"), "1D", false},
		{"an hourly bar an hour old is closed", intraday(now.Add(-time.Hour)), "1H", true},
		{"an hourly bar 30 minutes old is forming", intraday(now.Add(-30 * time.Minute)), "1H", false},
		{"a 5m bar 6 minutes old is closed", intraday(now.Add(-6 * time.Minute)), "5m", true},
		{"a 15m bar 5 minutes old is forming", intraday(now.Add(-5 * time.Minute)), "15m", false},
		{"a nil bar is never complete", nil, "1D", false},
		{"an unreadable daily label is never complete", &latestBar{Time: barTime{Label: "??"}}, "1D", false},
	}
	for _, c := range cases {
		if got := barComplete(c.bar, c.timeframe, now); got != c.want {
			t.Errorf("%s: barComplete = %v, want %v", c.name, got, c.want)
		}
	}
}

func mustDay(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatal(err)
	}
	return v.UTC()
}

// --------------------------------------------------------------------------- execution quote

// A fill dated BEFORE the bar it reconciles is a price from before the decision existed.
func TestStaleQuoteAsOfIsRefusedAndTheBarIsRetried(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	e, store := harness(t, f, t.TempDir())

	f.set(func(f *fakes) {
		q := cleanQuote(now)
		q["asOf"] = "2026-08-10T00:00:00Z" // older than the bar (2026-08-19) it would reconcile
		f.quote = q
	})
	e.evalConfig(context.Background(), testCfg, now)

	if got := f.journalCalls(); len(got) != 0 {
		t.Fatalf("a fill older than its bar is not a fill, calls = %v", got)
	}
	d := lastDecision(t, store)
	if d.Gate != "quote" || !strings.Contains(d.Reason, "BEFORE the bar") {
		t.Errorf("decision = %+v", d)
	}
	if st := store.StateFor(testCfg); st.LastBarActedOn != "" {
		t.Error("a stale quote is retryable — the bar must not be consumed")
	}

	// A fresh quote within the same bar is acted on.
	f.set(func(f *fakes) { f.quote = cleanQuote(now) })
	e.evalConfig(context.Background(), testCfg, now.Add(5*time.Minute))
	if got := f.journalCalls(); len(got) != 1 {
		t.Fatalf("the retry within the same bar should have acted, calls = %v", got)
	}
}

func TestQuoteWithoutAnAsOfIsRefused(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	e, store := harness(t, f, t.TempDir())

	f.set(func(f *fakes) { f.quote = map[string]any{"symbol": "NVDA", "price": 100.0, "source": "tiingo"} })
	e.evalConfig(context.Background(), testCfg, now)

	if got := f.journalCalls(); len(got) != 0 {
		t.Fatalf("calls = %v", got)
	}
	if d := lastDecision(t, store); !strings.Contains(d.Reason, "no asOf") {
		t.Errorf("reason = %q", d.Reason)
	}
}

// The quote's provenance is RECORDED on the trade, not discarded.
func TestTheTradeRecordsTheQuotesSourceAndAsOf(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	var body map[string]any
	f := newFakes(t, now)
	// Re-point the journal at a server that captures the POSTed trade.
	js := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(map[string]any{"trade": map[string]any{"id": "t1"}})
	}))
	defer js.Close()

	e, _ := harness(t, f, t.TempDir())
	e.cfg.JournalURL = js.URL
	e.clients = newClients(e.cfg)
	e.evalConfig(context.Background(), testCfg, now)

	sig, _ := body["attachedSignal"].(map[string]any)
	if sig == nil {
		t.Fatalf("trade = %+v", body)
	}
	if sig["quoteSource"] != "tiingo" || sig["quoteAsOf"] != now.UTC().Format(time.RFC3339) {
		t.Errorf("the fill's provenance must travel with the trade, got %+v", sig)
	}
	if sig["decidedOnBar"] != barLabel(now) {
		t.Errorf("decidedOnBar = %v, want %v", sig["decidedOnBar"], barLabel(now))
	}
	if sig["horizon"] != 5.0 {
		t.Errorf("horizon = %v — the comparison groups on it", sig["horizon"])
	}
	if sig["fillKind"] != fillOpen || sig["nAtEntry"] != 1.0 {
		t.Errorf("the crash-recovery identity must travel with the trade, got %+v", sig)
	}
	if sig["modelVersion"] != "model-v1" || sig["strategyVersion"] != "sv1-abcdef0123456789" {
		t.Errorf("the immutable model lineage must travel with the trade, got %+v", sig)
	}
}

// --------------------------------------------------------------------------- crash windows

// A trade the journal holds and the engine has forgotten (a crash between journalCreate returning
// and state being persisted) is ADOPTED, not duplicated.
func TestAnOrphanJournalTradeIsAdoptedNotDuplicated(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	e, store := harness(t, f, t.TempDir())

	f.set(func(f *fakes) {
		f.openTrades = []any{map[string]any{
			"id": "orphan1", "ticker": "NVDA", "side": "long", "status": "open", "mode": "paper",
			"entry": map[string]any{"date": "2026-08-19", "price": 97.5, "size": 100.0, "timeframe": "1D"},
			"attachedSignal": map[string]any{
				"horizon": 5, "decidedOnBar": "2026-08-19", "costBps": 6.0,
				"notional": 9750.0, "nAtEntry": 10,
			},
		}}
	})
	e.evalConfig(context.Background(), testCfg, now) // target is Buy = long, which the orphan already is

	if got := f.journalCalls(); len(got) != 0 {
		t.Fatalf("the orphan IS the position — nothing new may be opened, calls = %v", got)
	}
	st := store.StateFor(testCfg)
	if st.Side != "long" || st.TradeID != "orphan1" || st.EntryPrice != 97.5 {
		t.Fatalf("the orphan must be adopted into state, got %+v", st)
	}
	lot := e.ledger.LotFor(testCfg.Key())
	if lot == nil || lot.Qty != 100 || lot.EntryNotional != 9750 || lot.NAtEntry != 10 || lot.CostBps != 6 {
		t.Fatalf("orphan recovery must replay the original fill exactly, got %+v", lot)
	}
	if d := lastDecision(t, store); d.Action != "hold" {
		t.Errorf("decision = %+v, want a hold of the adopted position", d)
	}
}

func TestAnOrphanIsNotStolenFromAnotherHorizon(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	e, store := harness(t, f, t.TempDir())

	f.set(func(f *fakes) {
		f.openTrades = []any{map[string]any{
			"id": "other", "ticker": "NVDA", "side": "long", "status": "open", "mode": "paper",
			"entry":          map[string]any{"date": "2026-08-19", "price": 97.5, "size": 100.0, "timeframe": "1D"},
			"attachedSignal": map[string]any{"horizon": 10}, // a DIFFERENT config's position
		}}
	})
	e.evalConfig(context.Background(), testCfg, now)

	if st := store.StateFor(testCfg); st.TradeID == "other" {
		t.Fatal("NVDA:1D:5 must not adopt NVDA:1D:10's position")
	}
	if got := f.journalCalls(); len(got) != 1 || got[0] != "POST /trades" {
		t.Fatalf("this config had nothing of its own, so it opens; calls = %v", got)
	}
}

func TestALegacyOrphanIsLeftAloneWhenAmbiguous(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	dir := t.TempDir()
	f.set(func(f *fakes) {
		f.openTrades = []any{map[string]any{
			"id": "legacy", "ticker": "NVDA", "side": "long", "status": "open", "mode": "paper",
			"entry": map[string]any{"date": "2026-08-19", "price": 97.5, "size": 100.0, "timeframe": "1D"},
			// no attachedSignal at all — written before the provenance block existed
		}}
	})
	e, store := harness(t, f, dir)
	// Two configs share (NVDA, 1D): the legacy trade could belong to either.
	two := []PaperCfg{testCfg, {Ticker: "NVDA", Timeframe: "1D", Horizon: 10}}
	store.SetConfigs(two)
	e.cfg.Configs = two

	e.evalConfig(context.Background(), testCfg, now)
	if st := store.StateFor(testCfg); st.TradeID == "legacy" {
		t.Error("an unattributable orphan must be left alone, not guessed at")
	}
}

// A journal blip must never orphan a live paper trade: the position is KEPT and retried.
func TestACloseTimeoutKeepsTheOpenPosition(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	e, store := harness(t, f, t.TempDir())
	ctx := context.Background()

	e.evalConfig(ctx, testCfg, now) // open long, t1
	next := now.AddDate(0, 0, 1)
	f.set(func(f *fakes) {
		f.bar = cleanBar(next)
		f.pred = cleanPredict(next, "Hold") // target goes flat
		f.patchStatus = http.StatusServiceUnavailable
	})
	e.evalConfig(ctx, testCfg, next)

	st := store.StateFor(testCfg)
	if st.Side != "long" || st.TradeID != "t1" {
		t.Fatalf("an unreachable journal must not drop a position the journal still holds, got %+v", st)
	}
	if st.LastBarActedOn == barLabel(next) {
		t.Error("the bar must stay un-consumed so the close is retried")
	}
	if d := lastDecision(t, store); !strings.Contains(d.Reason, "KEPT") {
		t.Errorf("reason = %q", d.Reason)
	}

	// The journal comes back; the retry within the same bar closes it.
	f.set(func(f *fakes) { f.patchStatus = 0 })
	e.evalConfig(ctx, testCfg, next.Add(5*time.Minute))
	if st := store.StateFor(testCfg); st.Side != "" {
		t.Errorf("the retry should have closed the position, got %+v", st)
	}
}

// ...but a trade that is genuinely GONE is dropped, because our bookkeeping is the wrong half.
func TestACloseOfAVanishedTradeDropsTheBookkeeping(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	e, store := harness(t, f, t.TempDir())
	ctx := context.Background()

	e.evalConfig(ctx, testCfg, now)
	next := now.AddDate(0, 0, 1)
	f.set(func(f *fakes) {
		f.bar = cleanBar(next)
		f.pred = cleanPredict(next, "Hold")
		f.patchStatus = http.StatusNotFound
	})
	e.evalConfig(ctx, testCfg, next)

	if st := store.StateFor(testCfg); st.Side != "" || st.TradeID != "" {
		t.Fatalf("a 404 means the trade is gone; the stale state must go too, got %+v", st)
	}
	if d := lastDecision(t, store); !strings.Contains(d.Reason, "gone from the journal") {
		t.Errorf("reason = %q", d.Reason)
	}
}

// The open is persisted BEFORE the tick finishes, so a crash cannot lose it.
func TestAnOpenIsPersistedImmediately(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	f := newFakes(t, now)
	e, _ := harness(t, f, dir)

	bar, err := e.clients.latestBar(context.Background(), testCfg.Ticker, testCfg.Timeframe)
	if err != nil {
		t.Fatal(err)
	}
	pred, err := e.clients.predict(context.Background(), testCfg)
	if err != nil {
		t.Fatal(err)
	}
	q, err := e.clients.quote(context.Background(), testCfg.Ticker)
	if err != nil {
		t.Fatal(err)
	}
	st := e.store.StateFor(testCfg)
	if err := e.openPosition(context.Background(), testCfg, st, bar, pred, q, "long", now, false); err != nil {
		t.Fatal(err)
	}

	// A SEPARATE store over the same directory = what a restarted process would read. openPosition
	// alone (no record(), no reconcile()) must already have written the trade id.
	reloaded, err := openStore(dir, []PaperCfg{testCfg})
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.StateFor(testCfg); got.TradeID != "t1" || got.Side != "long" {
		t.Fatalf("the position must survive a crash right after journalCreate, got %+v", got)
	}
}
