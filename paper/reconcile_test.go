package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// reconcile_test.go — determinism (fix 1) and the three-store contract (fixes 2 and 3).
//
// The two things under test here are the two ways this service could be quietly wrong about its own
// experiment: an equity curve that depends on the order `CONFIGS` is written in, and a book that
// silently lost a fill nothing ever retried.

// --------------------------------------------------------------------------- multi-config harness

var (
	nvdaCfg  = PaperCfg{Ticker: "NVDA", Timeframe: "1D", Horizon: 5}
	googlCfg = PaperCfg{Ticker: "GOOGL", Timeframe: "1D", Horizon: 5}
)

// harnessWith wires an engine over an explicit config list, in the order given. The ORDER is the
// point of the determinism test, so it is never sorted here.
func harnessWith(t *testing.T, f *fakes, dir string, cfgs []PaperCfg) (*Engine, *Store) {
	t.Helper()
	cfg := Config{
		PredictionURL: f.predict.URL, AnalysisURL: f.analysis.URL, JournalURL: f.journal.URL,
		Configs: cfgs, DataDir: dir,
		MaxBarAgeSessions: 3, MaxModelAgeSessions: 10, StartingCash: defaultStartingCash,
	}
	store, err := openStore(dir, cfg.Configs)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	ledger, lerr := openLedger(dir, cfg.StartingCash)
	if lerr != nil {
		t.Fatalf("openLedger: %v", lerr)
	}
	return newEngine(cfg, store, newClients(cfg), ledger, nil), store
}

// tickerBar is a real completed bar for one ticker at one close.
func tickerBar(ticker string, day time.Time, close float64) map[string]any {
	return map[string]any{
		"ticker": ticker, "timeframe": "1D", "source": "tiingo", "sourceIsSynthetic": false,
		"bars": []any{map[string]any{"time": day.UTC().Format("2006-01-02"), "close": close}},
	}
}

func tickerQuote(ticker string, now time.Time, price float64) map[string]any {
	return map[string]any{
		"symbol": ticker, "price": price, "source": "tiingo",
		"asOf": now.UTC().Format(time.RFC3339),
	}
}

// setBars points every configured ticker at ITS OWN bar and quote for the session ending `bar`.
// Different prices per ticker is what makes an ordering bug observable at all.
func setBars(f *fakes, bar, now time.Time, prices map[string]float64) {
	f.set(func(f *fakes) {
		f.barBy = map[string]map[string]any{}
		f.quoteBy = map[string]map[string]any{}
		for ticker, p := range prices {
			f.barBy[ticker] = tickerBar(ticker, bar, p)
			f.quoteBy[ticker] = tickerQuote(ticker, now, p)
		}
	})
}

// --------------------------------------------------------------------------- fix 1: determinism

// The day's equity snapshot must not depend on the order the configs happen to be listed in.
//
// The engine used to mark and then reconcile each config in turn, so the ledger settled the date's
// snapshot on the LAST config's mark — after every earlier config had already traded that bar, and
// before every later one had. Reordering CONFIGS therefore moved fills across the snapshot boundary
// and produced a different equity curve from the same market data. The tick is now two-phase: every
// mark first, every decision second, so the snapshot is the PRE-TRADE mark of that bar's close
// whatever the order (contract §5.3).
//
// The two configs are given DIFFERENT validated costs on purpose. With identical costs the two
// orderings are symmetric and the old bug hides behind its own arithmetic: whichever config went
// first charged the same fee, so the snapshot came out the same either way and a green test would
// have meant nothing.
func TestSnapshotsAreIdenticalUnderEitherConfigOrder(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)

	run := func(order []PaperCfg) []byte {
		t.Helper()
		f := newFakes(t, now)
		dir := t.TempDir()
		e, _ := harnessWith(t, f, dir, order)

		setBars(f, now.AddDate(0, 0, -1), now, map[string]float64{"NVDA": 100, "GOOGL": 200})
		f.set(func(f *fakes) {
			f.predBy = map[string]map[string]any{
				"NVDA":  predictAtCost(now, "Buy", 6),
				"GOOGL": predictAtCost(now, "Buy", 200),
			}
		})
		e.tick(context.Background(), now)

		for _, c := range order {
			if !e.ledger.HasLot(c.Key()) {
				t.Fatalf("the fixture must open %s, or it proves nothing about ordering", c.Key())
			}
		}
		b, err := os.ReadFile(dir + "/snapshots.jsonl")
		if err != nil {
			t.Fatalf("read snapshots: %v", err)
		}
		return b
	}

	forward := run([]PaperCfg{nvdaCfg, googlCfg})
	reverse := run([]PaperCfg{googlCfg, nvdaCfg})

	if len(forward) == 0 {
		t.Fatal("the fixture produced no snapshots at all — the test proves nothing")
	}
	if string(forward) != string(reverse) {
		t.Fatalf("the snapshot depends on CONFIGS ordering.\nNVDA-first:  %s\nGOOGL-first: %s",
			forward, reverse)
	}

	// And it is the PRE-TRADE mark of the bar close: the snapshot is taken before either config's
	// opening fill, so it is exactly the opening balance and no fee has been charged into it.
	var snap Snapshot
	line, _, _ := strings.Cut(string(forward), "\n")
	if err := json.Unmarshal([]byte(line), &snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if snap.Equity != defaultStartingCash || snap.Cash != defaultStartingCash {
		t.Errorf("the daily snapshot is the PRE-TRADE mark of that bar's close: want equity=cash=%.2f, "+
			"got equity=%.2f cash=%.2f", defaultStartingCash, snap.Equity, snap.Cash)
	}
	if snap.Marks["NVDA:1D:5"] != 100 || snap.Marks["GOOGL:1D:5"] != 200 {
		t.Errorf("both configs' closes must be in the snapshot, got %+v", snap.Marks)
	}
}

// predictAtCost is a clean /predict response at a specific validated cost, so two configs in one
// tick can be given different fees.
func predictAtCost(now time.Time, direction string, costBps float64) map[string]any {
	p := cleanPredict(now, direction)
	bt := p["backtest"].(map[string]any)
	bt["costBps"] = costBps
	return p
}

// --------------------------------------------------------------------------- fix 2: pending bookings

// brokenLedger makes the NEXT ledger write fail, by pointing the append-only fills file at a
// directory. It is the cheapest honest way to reproduce "the journal took the trade and the book
// would not take the fill" without a fake ledger that could drift from the real one.
func breakLedgerWrites(t *testing.T, dir string) func() {
	t.Helper()
	path := dir + "/fills.jsonl"
	_ = os.Remove(path)
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("break ledger: %v", err)
	}
	return func() {
		if err := os.Remove(path); err != nil {
			t.Fatalf("unbreak ledger: %v", err)
		}
	}
}

func TestALedgerFailureBecomesAPendingBookingAndIsRetried(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	f := newFakes(t, now)
	e, store := harnessWith(t, f, dir, []PaperCfg{testCfg})
	ctx := context.Background()

	restore := breakLedgerWrites(t, dir)
	e.evalConfig(ctx, testCfg, now)

	// The journal took the trade; the book refused the fill.
	if got := f.journalCalls(); len(got) != 1 || got[0] != "POST /trades" {
		t.Fatalf("the position must still be opened in the journal, calls = %v", got)
	}
	if st := store.StateFor(testCfg); st.Side != "long" {
		t.Fatalf("the engine holds the position it opened, got %+v", st)
	}
	if e.ledger.HasLot(testCfg.Key()) {
		t.Fatal("the fixture did not actually break the ledger write")
	}
	pending := store.PendingBookingsFor(testCfg.Key())
	if len(pending) != 1 || pending[0].Kind != fillOpen || pending[0].TradeID != "t1" {
		t.Fatalf("the lost fill must become a DURABLE pending booking, got %+v", pending)
	}
	if pending[0].Price != 100 || pending[0].Side != "long" || pending[0].Qty != 1000 ||
		pending[0].Notional != 100000 || pending[0].N != 1 {
		t.Errorf("the pending booking must carry the fill it owes, got %+v", pending[0])
	}

	// While it is owed, the config is desynced and refuses to change its position.
	if s := e.syncFor(testCfg.Key()); s.Consistent {
		t.Error("a fill the book is still owed is not agreement")
	}

	// The book comes back; the next tick books what it owes.
	restore()
	// Change current equity before the retry. Recovery must still use the original 100,000/1,000
	// intent, not recompute equity/N from this later book.
	other := PaperCfg{Ticker: "GOOGL", Timeframe: "1D", Horizon: 5}
	if _, err := e.ledger.Open(other, "long", "2026-08-19", "other", 200, 100, 2, now); err != nil {
		t.Fatalf("change book before retry: %v", err)
	}
	next := now.AddDate(0, 0, 1)
	f.set(func(f *fakes) { f.bar = cleanBar(next) })
	e.evalConfig(ctx, testCfg, next)

	if len(store.PendingBookingsFor(testCfg.Key())) != 0 {
		t.Fatalf("the retry should have cleared the pending booking, got %+v",
			store.PendingBookingsFor(testCfg.Key()))
	}
	lot := e.ledger.LotFor(testCfg.Key())
	if lot == nil || lot.TradeID != "t1" || lot.EntryPrice != 100 || lot.Qty != 1000 ||
		lot.EntryNotional != 100000 || lot.NAtEntry != 1 {
		t.Fatalf("the book must hold the fill it was owed, got %+v", lot)
	}
	if s := e.syncFor(testCfg.Key()); !s.Consistent {
		t.Errorf("the stores agree again once the fill is booked, got %+v", s)
	}
}

// Booking is idempotent by (trade id, kind): the same fill can never be recorded twice, so a retry
// after a crash between the ledger accepting it and the pending record being cleared is safe.
func TestADuplicateBookingIsRefused(t *testing.T) {
	dir := t.TempDir()
	l, err := openLedger(dir, 100000)
	if err != nil {
		t.Fatalf("openLedger: %v", err)
	}
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)

	if _, err := l.Open(testCfg, "long", "2026-08-19", "t1", 100, 6, 1, now); err != nil {
		t.Fatalf("first open: %v", err)
	}
	if _, err := l.Close(testCfg, "2026-08-20", 110, now, ""); err != nil {
		t.Fatalf("close: %v", err)
	}
	// The lot is gone, so the "already holds a lot" guard cannot catch this. The IDENTITY guard must.
	_, err = l.Open(testCfg, "long", "2026-08-19", "t1", 100, 6, 1, now)
	if !errors.Is(err, errDuplicateFill) {
		t.Fatalf("re-booking (t1, open) must be refused as a duplicate, got %v", err)
	}
	if !l.HasBooked("t1", fillOpen) || !l.HasBooked("t1", fillClose) {
		t.Error("both legs should be recorded in the idempotency set")
	}
	if l.HasBooked("", fillOpen) {
		t.Error("a fill with no trade id has no identity and can never be 'already booked'")
	}
	// A book replayed from the append-only files carries the same set — otherwise the guard would
	// evaporate at exactly the moment it is needed, which is after a crash.
	if err := os.Remove(dir + "/ledger.json"); err != nil {
		t.Fatal(err)
	}
	replayed, err := openLedger(dir, 100000)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if !replayed.HasBooked("t1", fillOpen) {
		t.Error("the idempotency set must survive a replay from fills.jsonl")
	}
}

// A desynced config refuses to CHANGE its position, names the mismatch, and keeps marking.
func TestADesyncedConfigRefusesToTradeAndSaysWhy(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	f := newFakes(t, now)
	e, store := harnessWith(t, f, dir, []PaperCfg{testCfg})
	ctx := context.Background()

	e.evalConfig(ctx, testCfg, now) // opens long, all three stores agree
	if !e.ledger.HasLot(testCfg.Key()) {
		t.Fatal("setup: the position should be open in the book")
	}

	// Now make the LEDGER hold a different trade than the engine does — the exact shape of the
	// disagreement the old log line claimed /paper/status reported, and did not.
	st := store.StateFor(testCfg)
	st.TradeID = "t-something-else"
	store.SaveState(st)

	next := now.AddDate(0, 0, 1)
	f.set(func(f *fakes) {
		f.bar = cleanBar(next)
		f.pred = cleanPredict(next, "Hold") // the signal says close
	})
	nSnapsBefore := len(e.ledger.Payload(0)["equityCurve"].([]float64))
	e.evalConfig(ctx, testCfg, next)

	if got := f.journalCalls(); len(got) != 1 {
		t.Fatalf("a desynced config must not move its position, calls = %v", got)
	}
	d := lastDecision(t, store)
	if d.Gate != "sync" {
		t.Fatalf("the refusal must be surfaced like a gate refusal, got %+v", d)
	}
	if !strings.Contains(d.Reason, "the ledger's lot is trade t1") {
		t.Errorf("the reason must NAME the mismatch, got %q", d.Reason)
	}
	if store.StateFor(testCfg).LastBarActedOn == barLabel(next) {
		t.Error("a desync must leave the bar un-consumed so the decision can still be acted on")
	}
	// Marking continues: the book keeps measuring while the bookkeeping is repaired.
	if n := len(e.ledger.Payload(0)["equityCurve"].([]float64)); n <= nSnapsBefore {
		t.Errorf("marking and snapshots must continue while a config is desynced (%d -> %d)",
			nSnapsBefore, n)
	}
}

// The sync verdict has to reach the JSON, per config, or it does not exist.
func TestStatusCarriesThePerConfigSyncBlock(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	f := newFakes(t, now)
	e, store := harnessWith(t, f, dir, []PaperCfg{testCfg})
	e.evalConfig(context.Background(), testCfg, now)

	api := newAPI(e.cfg, store, e.clients, e)
	var body struct {
		Configs []struct {
			Config string    `json:"config"`
			Sync   syncState `json:"sync"`
		} `json:"configs"`
		Reconciliation struct {
			DesyncedConfigs []string `json:"desyncedConfigs"`
			PendingBookings int      `json:"pendingBookings"`
			Stores          []string `json:"stores"`
		} `json:"reconciliation"`
	}
	rec := httptest.NewRecorder()
	api.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/paper/status", nil))
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Configs) != 1 {
		t.Fatalf("configs = %+v", body.Configs)
	}
	s := body.Configs[0].Sync
	if !s.Consistent || !s.JournalChecked || !s.LedgerChecked {
		t.Fatalf("all three stores agree and all three were compared, got %+v", s)
	}
	if s.Detail == "" {
		t.Error("the sync block must say what it compared, in words")
	}
	if len(body.Reconciliation.Stores) != 3 || body.Reconciliation.PendingBookings != 0 {
		t.Errorf("reconciliation roll-up = %+v", body.Reconciliation)
	}
	if len(body.Reconciliation.DesyncedConfigs) != 0 {
		t.Errorf("nothing is desynced here, got %v", body.Reconciliation.DesyncedConfigs)
	}
}

// An unreachable journal is NOT agreement — it is a comparison that did not happen, and it says so.
func TestAnUnreachableJournalIsReportedAsNotCompared(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	e, _ := harnessWith(t, f, t.TempDir(), []PaperCfg{testCfg})
	f.set(func(f *fakes) { f.listErr = true })

	s := e.reconcileStores(context.Background())[testCfg.Key()]
	if s.JournalChecked {
		t.Fatal("the journal was down; it cannot have been checked")
	}
	if s.Consistent || !s.blocked() {
		t.Fatalf("an unverified journal must block movement, got %+v", s)
	}
	if !strings.Contains(s.Detail, "could not be reached") {
		t.Errorf("detail must say why the journal was not compared, got %q", s.Detail)
	}
}

// --------------------------------------------------------------------------- fix 3: config + reset

func TestRemovingAConfigThatHoldsAPositionIsRefused(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	f := newFakes(t, now)
	e, store := harnessWith(t, f, dir, []PaperCfg{testCfg, googlCfg})
	setBars(f, now.AddDate(0, 0, -1), now, map[string]float64{"NVDA": 100, "GOOGL": 200})
	e.tick(context.Background(), now)

	if !e.ledger.HasLot(testCfg.Key()) {
		t.Fatal("setup: NVDA should be holding a lot")
	}
	cfg := e.cfg
	cfg.AuthSecret, cfg.CookieName, cfg.SystemUID = "s", "nvda_session", "paper-engine"
	api := newAPI(cfg, store, e.clients, e)

	req := httptest.NewRequest(http.MethodPost, "/paper/config",
		strings.NewReader(`{"configs":"GOOGL:1D:5"}`)) // NVDA dropped while it holds a position
	req.AddCookie(sessionCookie("s", cfg.SystemUID, time.Now().Add(time.Hour)))
	rec := httptest.NewRecorder()
	api.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("removing a config that holds a position must be 409, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "NVDA:1D:5") {
		t.Errorf("the refusal must NAME the configs, got %s", rec.Body.String())
	}
	if len(store.Configs()) != 2 {
		t.Errorf("nothing may be removed on a refusal, configs = %+v", store.Configs())
	}

	// Closing the position first makes the same request succeed.
	next := now.AddDate(0, 0, 1)
	setBars(f, now, next, map[string]float64{"NVDA": 100, "GOOGL": 200})
	f.set(func(f *fakes) { f.pred = cleanPredict(next, "Hold") })
	e.tick(context.Background(), next)
	if e.ledger.HasLot(testCfg.Key()) {
		t.Fatal("setup: NVDA should have closed to flat")
	}

	req2 := httptest.NewRequest(http.MethodPost, "/paper/config", strings.NewReader(`{"configs":"GOOGL:1D:5"}`))
	req2.AddCookie(sessionCookie("s", cfg.SystemUID, time.Now().Add(time.Hour)))
	rec2 := httptest.NewRecorder()
	api.routes().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("a config holding nothing may be removed, got %d: %s", rec2.Code, rec2.Body.String())
	}
}

// A reset that cannot delete every journal trade wipes NOTHING.
func TestResetWithAFailedDeletionWipesNothing(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	f := newFakes(t, now)
	e, store := harnessWith(t, f, dir, []PaperCfg{testCfg})
	e.evalConfig(context.Background(), testCfg, now)

	// A second paper trade the journal will refuse to delete.
	f.set(func(f *fakes) {
		f.openTrades = append(f.openTrades, map[string]any{
			"id": "undeletable", "ticker": "NVDA", "side": "long", "status": "open", "mode": "paper",
			"entry": map[string]any{"price": 100.0, "timeframe": "1D"},
		})
		f.deleteFail = map[string]bool{"undeletable": true}
	})

	cfg := e.cfg
	cfg.AuthSecret, cfg.CookieName, cfg.SystemUID = "s", "nvda_session", "paper-engine"
	api := newAPI(cfg, store, e.clients, e)
	api.now = func() time.Time { return now }
	req := httptest.NewRequest(http.MethodPost, "/paper/reset?confirm=true", nil)
	req.AddCookie(sessionCookie("s", cfg.SystemUID, time.Now().Add(time.Hour)))
	rec := httptest.NewRecorder()
	api.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("a failed deletion must abort the reset with 502, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Deleted          []string         `json:"deletedPaperTrades"`
		Failed           []map[string]any `json:"failedPaperTrades"`
		EngineStateReset bool             `json:"engineStateReset"`
		LedgerReset      bool             `json:"ledgerReset"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Failed) != 1 || body.Failed[0]["tradeId"] != "undeletable" {
		t.Errorf("the response must list WHAT failed, got %+v", body.Failed)
	}
	if body.EngineStateReset || body.LedgerReset {
		t.Error("neither the engine state nor the book may be touched on an aborted reset")
	}
	if st := store.StateFor(testCfg); st.Side != "long" {
		t.Errorf("the engine's state must be intact, got %+v", st)
	}
	if !e.ledger.HasLot(testCfg.Key()) {
		t.Error("the book must be intact")
	}

	// With the journal healthy, the same request completes and empties all three stores.
	f.set(func(f *fakes) { f.deleteFail = nil })
	req2 := httptest.NewRequest(http.MethodPost, "/paper/reset?confirm=true", nil)
	req2.AddCookie(sessionCookie("s", cfg.SystemUID, time.Now().Add(time.Hour)))
	rec2 := httptest.NewRecorder()
	api.routes().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("a clean deletion must proceed, got %d: %s", rec2.Code, rec2.Body.String())
	}
	if st := store.StateFor(testCfg); st.Side != "" {
		t.Errorf("engine state should be cleared, got %+v", st)
	}
	if e.ledger.HasLot(testCfg.Key()) {
		t.Error("the book should be cleared")
	}
	if len(store.PendingBookings()) != 0 {
		t.Error("pending bookings are owed against trades that no longer exist — they go too")
	}
}
