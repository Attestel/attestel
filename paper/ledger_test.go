package main

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ledger_test.go — the fake-money book (docs/PAPER_EXECUTION_CONTRACT.md §5).
//
// The cross-language guarantee lives in accounting_fixtures_test.go: that the ledger reproduces the
// contract's accounting atom from its own fills and marks. This file covers the rest of the book —
// sizing, fees, marking, durability, reset, and the HTTP surface — including the cases where the
// honest answer is "no number": a synthetic bar is a GAP, and a Sharpe below the sample floor is
// NULL.

var (
	cfgA = PaperCfg{Ticker: "AAA", Timeframe: "1D", Horizon: 5}
	cfgB = PaperCfg{Ticker: "BBB", Timeframe: "1D", Horizon: 5}
)

func newTestLedger(t *testing.T, dir string) *Ledger {
	t.Helper()
	l, err := openLedger(dir, defaultStartingCash)
	if err != nil {
		t.Fatalf("openLedger: %v", err)
	}
	return l
}

var ledgerNow = time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)

// --------------------------------------------------------------------------- sizing

// A position takes equity/N, and N is read AT THE MOMENT OF ENTRY — so adding configs changes what
// the next position gets without touching what an open one already holds.
func TestSizingIsEquityOverNAndFollowsAChangeInN(t *testing.T) {
	l := newTestLedger(t, t.TempDir())

	// Two configs enabled: the first position takes half the book.
	first, err := l.Open(cfgA, "long", "2026-08-19", "t1", 100, 0, 2, ledgerNow)
	if err != nil {
		t.Fatalf("open A: %v", err)
	}
	if math.Abs(first.Notional-defaultStartingCash/2) > 1e-9 {
		t.Errorf("first notional %.4f, want equity/2 = %.4f", first.Notional, defaultStartingCash/2)
	}
	if first.Qty != first.Notional/100 {
		t.Errorf("qty %.6f is not notional/price", first.Qty)
	}
	if first.NAtEntry != 2 {
		t.Errorf("the fill must record the N it was sized against, got %d", first.NAtEntry)
	}

	// The book's equity has not moved (a zero-cost fill is value-neutral), but N has: four configs
	// are now enabled, so the NEXT position takes a quarter.
	if eq := l.Equity(); math.Abs(eq-defaultStartingCash) > 1e-9 {
		t.Fatalf("a zero-cost fill changed equity: %.6f", eq)
	}
	second, err := l.Open(cfgB, "long", "2026-08-19", "t2", 50, 0, 4, ledgerNow)
	if err != nil {
		t.Fatalf("open B: %v", err)
	}
	if math.Abs(second.Notional-defaultStartingCash/4) > 1e-9 {
		t.Errorf("second notional %.4f, want equity/4 = %.4f", second.Notional, defaultStartingCash/4)
	}
	if second.NAtEntry != 4 {
		t.Errorf("second fill nAtEntry = %d, want 4", second.NAtEntry)
	}

	// The first position is NOT resized by the change in N. Rebalancing an open position would be
	// turnover the backtest never charged (contract §5.2).
	if lot := l.LotFor(cfgA.Key()); lot == nil || math.Abs(lot.EntryNotional-defaultStartingCash/2) > 1e-9 {
		t.Errorf("the open lot was resized by a change in N: %+v", lot)
	}
}

// A fee is charged on the TRADED notional at the cost the model was validated under, on both the
// entry and the exit — and the realized P&L is net of both, never gross with a footnote.
func TestFeesAreChargedOnBothLegsAndLandInRealizedPnl(t *testing.T) {
	l := newTestLedger(t, t.TempDir())
	const cost = 6.0 // bps

	in, err := l.Open(cfgA, "long", "2026-08-19", "t1", 100, cost, 1, ledgerNow)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	wantEntryFee := in.Notional * cost / 10000
	if math.Abs(in.Fee-wantEntryFee) > 1e-9 {
		t.Errorf("entry fee %.6f, want %.6f", in.Fee, wantEntryFee)
	}

	out, err := l.Close(cfgA, "2026-08-20", 110, ledgerNow, "")
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	wantExitFee := in.Qty * 110 * cost / 10000
	if math.Abs(out.Fee-wantExitFee) > 1e-9 {
		t.Errorf("exit fee %.6f, want %.6f (cost_bps on the notional ACTUALLY traded)", out.Fee, wantExitFee)
	}
	wantRealized := in.Qty*110 - in.Notional - in.Fee - out.Fee
	if math.Abs(out.Realized-wantRealized) > 1e-9 {
		t.Errorf("realized %.6f, want %.6f — both fees belong in realized P&L", out.Realized, wantRealized)
	}
	if eq := l.Equity(); math.Abs(eq-(defaultStartingCash+wantRealized)) > 1e-6 {
		t.Errorf("equity %.6f != startingCash + realized %.6f", eq, defaultStartingCash+wantRealized)
	}
}

// --------------------------------------------------------------------------- the flip

// A flip is TWO fills and TWO fees, at one reconciliation — driven through the real engine, so this
// covers the wiring and not just the ledger method.
func TestAFlipBooksTwoFillsWithTwoFees(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	e, _ := harness(t, f, t.TempDir())
	ctx := context.Background()

	e.evalConfig(ctx, testCfg, now) // opens long
	next := now.AddDate(0, 0, 1)
	f.set(func(f *fakes) { f.bar = cleanBar(next); f.pred = cleanPredict(next, "Sell") })
	e.evalConfig(ctx, testCfg, next) // flips to short

	fills, err := e.ledger.readFills(0)
	if err != nil {
		t.Fatalf("readFills: %v", err)
	}
	if len(fills) != 3 {
		t.Fatalf("a flip after an open must leave 3 fills (open, flip-close, flip-open), got %d: %+v",
			len(fills), fills)
	}
	if fills[0].Kind != fillOpen || fills[1].Kind != fillFlipClose || fills[2].Kind != fillFlipOpen {
		t.Fatalf("fill kinds = %q, %q, %q — the close leg must be booked BEFORE the open leg",
			fills[0].Kind, fills[1].Kind, fills[2].Kind)
	}
	flipBar := fills[1].Bar
	if fills[2].Bar != flipBar {
		t.Errorf("both legs of a flip belong to the same bar, got %q and %q", fills[1].Bar, fills[2].Bar)
	}
	for _, f := range fills[1:] {
		if f.Fee <= 0 {
			t.Errorf("flip leg %q was not charged a fee (%+v) — a flip pays 2x, not 1x", f.Kind, f)
		}
	}
	if fills[1].Position != "long" || fills[2].Position != "short" {
		t.Errorf("the flip closed a %q and opened a %q", fills[1].Position, fills[2].Position)
	}
	// A trade is a transition INTO a nonzero position: the open and the flip's open leg, not the close.
	if got := countEpisodes(fills); got != 2 {
		t.Errorf("counted %d trades across an open and a flip, want 2", got)
	}
}

// --------------------------------------------------------------------------- marking

// A synthetic bar is not a mark. It produces a recorded GAP — never a substituted price, and never
// a snapshot that would put an invented number on the equity curve.
func TestASyntheticBarProducesAGapNotASnapshot(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	e, _ := harness(t, f, t.TempDir())
	f.set(func(f *fakes) {
		f.bar = map[string]any{
			"ticker": "NVDA", "timeframe": "1D", "source": "synthetic", "sourceIsSynthetic": true,
			"bars": []any{map[string]any{"time": barLabel(now), "close": 100.0}},
		}
	})
	e.evalConfig(context.Background(), testCfg, now)

	want := barLabel(now)
	if len(e.ledger.st.Snapshots) != 0 {
		t.Errorf("a synthetic bar must not produce a snapshot, got %+v", e.ledger.st.Snapshots)
	}
	found := false
	for _, d := range e.ledger.st.GapDates {
		if d == want {
			found = true
		}
	}
	if !found {
		t.Errorf("the synthetic bar's date %q is not recorded as a gap (gaps = %v) — a missing "+
			"measurement has to be visible", want, e.ledger.st.GapDates)
	}
}

// A real bar with no close is equally not a mark: the ledger marks at the bar's CLOSE, and a bar
// that does not carry one cannot be marked at all.
func TestABarWithNoCloseIsAGapToo(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	e, _ := harness(t, f, t.TempDir())
	f.set(func(f *fakes) {
		f.bar = map[string]any{
			"ticker": "NVDA", "timeframe": "1D", "source": "tiingo", "sourceIsSynthetic": false,
			"bars": []any{map[string]any{"time": barLabel(now)}},
		}
	})
	e.evalConfig(context.Background(), testCfg, now)
	if len(e.ledger.st.GapDates) != 1 || e.ledger.st.GapDates[0] != barLabel(now) {
		t.Errorf("gaps = %v, want exactly %q", e.ledger.st.GapDates, barLabel(now))
	}
}

// A date only becomes a snapshot when EVERY enabled config has marked it. One config marking alone
// leaves it pending — a book valued on a partial set of closes is valued on a portfolio nobody held.
func TestASnapshotWaitsForEveryEnabledConfig(t *testing.T) {
	l := newTestLedger(t, t.TempDir())
	keys := []string{cfgA.Key(), cfgB.Key()}

	l.Mark(keys, cfgA.Key(), "2026-08-19", 100, true)
	if len(l.st.Snapshots) != 0 {
		t.Fatalf("one of two configs marked and the book already snapshotted: %+v", l.st.Snapshots)
	}
	l.Mark(keys, cfgB.Key(), "2026-08-19", 50, true)
	if len(l.st.Snapshots) != 1 {
		t.Fatalf("both configs marked and there is no snapshot: %+v", l.st.Snapshots)
	}
	if got := l.st.Snapshots[0].Marks; got[cfgA.Key()] != 100 || got[cfgB.Key()] != 50 {
		t.Errorf("the snapshot must carry the marks it was taken at, got %v", got)
	}

	// A date the book moves past while still incomplete becomes a gap rather than pending forever.
	l.Mark(keys, cfgA.Key(), "2026-08-20", 101, true)
	l.Mark(keys, cfgA.Key(), "2026-08-21", 102, true)
	l.Mark(keys, cfgB.Key(), "2026-08-21", 51, true)
	found := false
	for _, d := range l.st.GapDates {
		if d == "2026-08-20" {
			found = true
		}
	}
	if !found {
		t.Errorf("an incomplete date the book moved past must become a gap, got %v", l.st.GapDates)
	}
}

// --------------------------------------------------------------------------- statistics

// Below the sample floor the Sharpe is NULL and says why. A Sharpe from nineteen daily observations
// is not a small number, it is not a number — and printed without comment it reads as a finding.
func TestSharpeIsNullBelowTwentySnapshots(t *testing.T) {
	l := newTestLedger(t, t.TempDir())
	keys := []string{cfgA.Key()}
	price := func(i int) float64 { return 100.0 + float64(i%5) } // real dispersion, no trend

	l.Mark(keys, cfgA.Key(), "2026-01-01", price(0), true)
	if _, err := l.Open(cfgA, "long", "2026-01-01", "t1", price(0), 6, 1, ledgerNow); err != nil {
		t.Fatalf("open: %v", err)
	}
	day := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	for i := 1; i < minSnapshotsForSharpe-1; i++ { // 18 more, 19 snapshots in total
		l.Mark(keys, cfgA.Key(), day.AddDate(0, 0, i-1).Format("2006-01-02"), price(i), true)
	}
	if n := len(l.st.Snapshots); n != minSnapshotsForSharpe-1 {
		t.Fatalf("built %d snapshots, wanted %d", n, minSnapshotsForSharpe-1)
	}
	m := l.Metrics()
	if m.DailySharpe != nil {
		t.Errorf("a Sharpe was served on %d snapshots (floor is %d): %v",
			m.NSnapshots, minSnapshotsForSharpe, *m.DailySharpe)
	}
	if !strings.Contains(m.SharpeNote, "UNMEASURED") {
		t.Errorf("the absent Sharpe must say it is UNMEASURED, got %q", m.SharpeNote)
	}
	// Drawdown and the return series are still real numbers — only the Sharpe needs the sample.
	if m.MaxDrawdown == nil || m.MeanDaily == nil {
		t.Errorf("drawdown and mean daily return do not need the Sharpe floor: %+v", m)
	}

	// One more snapshot crosses the floor.
	l.Mark(keys, cfgA.Key(), day.AddDate(0, 0, minSnapshotsForSharpe).Format("2006-01-02"),
		price(minSnapshotsForSharpe), true)
	m = l.Metrics()
	if m.NSnapshots != minSnapshotsForSharpe {
		t.Fatalf("snapshots = %d", m.NSnapshots)
	}
	if m.DailySharpe == nil {
		t.Fatalf("at the floor the Sharpe must be served, got %q", m.SharpeNote)
	}
	if !strings.Contains(m.SharpeNote, "252") {
		t.Errorf("the served Sharpe must state its annualization, got %q", m.SharpeNote)
	}
	if m.Annualization != dailyAnnualization {
		t.Errorf("annualization = %v, want %v (the same constant the evaluator uses for 1D)",
			m.Annualization, dailyAnnualization)
	}
}

// --------------------------------------------------------------------------- durability

// The fill line is written BEFORE the state moves. A crash in that window leaves a state file behind
// the fills — and the book must replay to exactly what it would have been, not to what the stale
// state says.
func TestACrashBetweenTheFillAndTheStateReplaysToTheSameBook(t *testing.T) {
	dir := t.TempDir()
	l := newTestLedger(t, dir)
	keys := []string{cfgA.Key()}

	l.Mark(keys, cfgA.Key(), "2026-08-18", 100, true)
	if _, err := l.Open(cfgA, "long", "2026-08-18", "t1", 100, 6, 1, ledgerNow); err != nil {
		t.Fatalf("open: %v", err)
	}
	l.Mark(keys, cfgA.Key(), "2026-08-19", 110, true)

	// The state as it stood BEFORE the last fill — what a crash would have left on disk.
	stale, err := os.ReadFile(filepath.Join(dir, "ledger.json"))
	if err != nil {
		t.Fatalf("reading state: %v", err)
	}

	if _, err := l.Close(cfgA, "2026-08-19", 110, ledgerNow, ""); err != nil {
		t.Fatalf("close: %v", err)
	}
	wantCash, wantRealized, wantSeq := l.st.Cash, l.st.Realized, l.st.LastSeq
	wantSnapshots := len(l.st.Snapshots)

	if err := os.WriteFile(filepath.Join(dir, "ledger.json"), stale, 0o644); err != nil {
		t.Fatalf("restoring the stale state: %v", err)
	}

	reopened, err := openLedger(dir, defaultStartingCash)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if !reopened.rebuilt {
		t.Fatal("a state file behind the fills must be REBUILT, not trusted")
	}
	if !strings.Contains(reopened.rebuiltReason, "crash") {
		t.Errorf("the rebuild must say why it happened, got %q", reopened.rebuiltReason)
	}
	if math.Abs(reopened.st.Cash-wantCash) > 1e-9 || math.Abs(reopened.st.Realized-wantRealized) > 1e-9 {
		t.Errorf("replayed cash/realized = %.6f/%.6f, want %.6f/%.6f",
			reopened.st.Cash, reopened.st.Realized, wantCash, wantRealized)
	}
	if reopened.st.LastSeq != wantSeq {
		t.Errorf("replayed lastFillSeq = %d, want %d", reopened.st.LastSeq, wantSeq)
	}
	if len(reopened.st.Lots) != 0 {
		t.Errorf("the replayed book still holds %d lots after a close", len(reopened.st.Lots))
	}
	if len(reopened.st.Snapshots) != wantSnapshots {
		t.Errorf("replayed %d snapshots, want %d — the snapshot series is append-only too",
			len(reopened.st.Snapshots), wantSnapshots)
	}
}

// The harder case: no state file at all. The book is reconstructable from the two append-only files
// alone, which is what makes them the record and ledger.json a cache.
func TestTheBookRebuildsFromTheAppendOnlyFilesAlone(t *testing.T) {
	dir := t.TempDir()
	l := newTestLedger(t, dir)
	keys := []string{cfgA.Key()}
	l.Mark(keys, cfgA.Key(), "2026-08-18", 100, true)
	if _, err := l.Open(cfgA, "long", "2026-08-18", "t1", 100, 6, 1, ledgerNow); err != nil {
		t.Fatalf("open: %v", err)
	}
	l.Mark(keys, cfgA.Key(), "2026-08-19", 110, true)
	wantEquity, wantSnaps := l.Equity(), len(l.st.Snapshots)

	if err := os.Remove(filepath.Join(dir, "ledger.json")); err != nil {
		t.Fatalf("removing the state file: %v", err)
	}
	reopened, err := openLedger(dir, defaultStartingCash)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if !reopened.rebuilt {
		t.Error("a missing state file with fills on disk must be rebuilt")
	}
	if math.Abs(reopened.Equity()-wantEquity) > 1e-9 {
		t.Errorf("rebuilt equity %.6f, want %.6f", reopened.Equity(), wantEquity)
	}
	if len(reopened.st.Snapshots) != wantSnaps {
		t.Errorf("rebuilt %d snapshots, want %d", len(reopened.st.Snapshots), wantSnaps)
	}
	lot := reopened.LotFor(cfgA.Key())
	if lot == nil || lot.Side != "long" || lot.TradeID != "t1" {
		t.Errorf("the open lot did not survive the rebuild: %+v", lot)
	}
	// Marks are recovered from the snapshot series, so the rebuilt book values its lot the same way.
	if reopened.st.LastMark[cfgA.Key()] != 110 {
		t.Errorf("the last mark was not recovered from the snapshots: %v", reopened.st.LastMark)
	}
}

// A reset ARCHIVES the append-only files with a timestamp. `rm` cannot be undone, and the record of
// what a validation book did before it was reset is exactly what somebody wants afterwards.
func TestResetArchivesTheFillsRatherThanDeletingThem(t *testing.T) {
	dir := t.TempDir()
	l := newTestLedger(t, dir)
	keys := []string{cfgA.Key()}
	l.Mark(keys, cfgA.Key(), "2026-08-18", 100, true)
	if _, err := l.Open(cfgA, "long", "2026-08-18", "t1", 100, 6, 1, ledgerNow); err != nil {
		t.Fatalf("open: %v", err)
	}

	if err := l.Reset(ledgerNow); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "fills.jsonl")); !os.IsNotExist(err) {
		t.Errorf("fills.jsonl must be moved aside by a reset, err = %v", err)
	}
	archived, _ := filepath.Glob(filepath.Join(dir, "fills-*.jsonl"))
	if len(archived) != 1 {
		t.Fatalf("expected exactly one archived fills file, got %v", archived)
	}
	if b, err := os.ReadFile(archived[0]); err != nil || !strings.Contains(string(b), `"t1"`) {
		t.Errorf("the archive must still hold the fills: %v / %s", err, b)
	}
	snaps, _ := filepath.Glob(filepath.Join(dir, "snapshots-*.jsonl"))
	if len(snaps) != 1 {
		t.Errorf("the snapshot series must be archived too, got %v", snaps)
	}
	if l.Equity() != defaultStartingCash || len(l.st.Lots) != 0 {
		t.Errorf("after a reset the book is back at its opening balance, got equity %.2f and %d lots",
			l.Equity(), len(l.st.Lots))
	}
	// And a reopened ledger must NOT replay the archived fills back in.
	reopened, err := openLedger(dir, defaultStartingCash)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if reopened.Equity() != defaultStartingCash || len(reopened.st.Lots) != 0 {
		t.Errorf("the archived fills were replayed into a reset book: equity %.2f", reopened.Equity())
	}
}

func TestResetRollsBackAnEarlierArchiveWhenALaterArchiveFails(t *testing.T) {
	dir := t.TempDir()
	l := newTestLedger(t, dir)
	keys := []string{cfgA.Key()}
	if err := l.Mark(keys, cfgA.Key(), "2026-08-18", 100, true); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Open(cfgA, "long", "2026-08-18", "t1", 100, 6, 1, ledgerNow); err != nil {
		t.Fatal(err)
	}
	stamp := ledgerNow.UTC().Format("20060102T150405.000000000Z")
	collision := filepath.Join(dir, "snapshots-"+stamp+".jsonl")
	if err := os.WriteFile(collision, []byte("existing archive"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := l.Reset(ledgerNow); err == nil {
		t.Fatal("reset must refuse to overwrite a prior archive")
	}
	if _, err := os.Stat(filepath.Join(dir, "fills.jsonl")); err != nil {
		t.Fatalf("the first archive move must have been rolled back: %v", err)
	}
	if lot := l.LotFor(cfgA.Key()); lot == nil || lot.TradeID != "t1" {
		t.Fatalf("a failed reset must preserve the in-memory book, got %+v", lot)
	}
}

func TestSnapshotWriteFailureIsReturnedAndRetried(t *testing.T) {
	dir := t.TempDir()
	l := newTestLedger(t, dir)
	path := filepath.Join(dir, "snapshots.jsonl")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	keys := []string{cfgA.Key()}
	if err := l.Mark(keys, cfgA.Key(), "2026-08-18", 100, true); err == nil {
		t.Fatal("a snapshot append failure must reach the caller")
	}
	if len(l.st.Snapshots) != 0 || l.st.Pending["2026-08-18"] == nil {
		t.Fatalf("the failed date must remain retryable, state = %+v", l.st)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := l.Mark(keys, cfgA.Key(), "2026-08-18", 100, true); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(l.st.Snapshots) != 1 || l.st.Pending["2026-08-18"] != nil {
		t.Fatalf("the retry must settle exactly once, state = %+v", l.st)
	}
}

// --------------------------------------------------------------------------- HTTP

func TestLedgerPayloadShape(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	e, store := harness(t, f, t.TempDir())
	e.evalConfig(context.Background(), testCfg, now) // marks, then opens a long

	api := newAPI(e.cfg, store, e.clients, e)
	rec := httptest.NewRecorder()
	api.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/paper/ledger", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}

	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, k := range []string{
		"paper", "simulation", "startingCash", "cash", "equity", "realizedPnl", "unrealized",
		"exposure", "allocation", "positions", "snapshots", "equityCurve", "metrics", "gapDates",
		"fills", "lastFillSeq", "contract", "note", "book", "sizing", "nConfigs", "asOf",
	} {
		if _, ok := body[k]; !ok {
			t.Errorf("the ledger payload is missing %q", k)
		}
	}
	if body["simulation"] != true {
		t.Error("the payload must declare itself a simulation")
	}
	if note, _ := body["note"].(string); !strings.Contains(note, "No broker") {
		t.Errorf("the payload must say there is no broker, got %q", note)
	}
	positions, _ := body["positions"].([]any)
	if len(positions) != 1 {
		t.Fatalf("expected the one open lot in the payload, got %v", positions)
	}
	pos, _ := positions[0].(map[string]any)
	for _, k := range []string{"config", "side", "qty", "entryPrice", "entryFee", "entryNotional",
		"costBps", "nAtEntry", "mark", "marketValue", "unrealized"} {
		if _, ok := pos[k]; !ok {
			t.Errorf("the open position is missing %q", k)
		}
	}
	fills, _ := body["fills"].([]any)
	if len(fills) != 1 {
		t.Fatalf("expected one fill in the tail, got %v", fills)
	}
	fill, _ := fills[0].(map[string]any)
	for _, k := range []string{"seq", "at", "bar", "config", "side", "qty", "price", "notional",
		"fee", "kind", "tradeId"} {
		if _, ok := fill[k]; !ok {
			t.Errorf("the fill record is missing %q", k)
		}
	}
	metrics, _ := body["metrics"].(map[string]any)
	if metrics["dailySharpeAnnualized"] != nil {
		t.Errorf("a one-snapshot book must serve a null Sharpe, got %v", metrics["dailySharpeAnnualized"])
	}
	if sn, _ := metrics["sharpeNote"].(string); !strings.Contains(sn, "UNMEASURED") {
		t.Errorf("the absent Sharpe must be labelled UNMEASURED, got %q", sn)
	}
}

// The status payload carries the one-line equity summary and says how positions are sized.
func TestStatusCarriesTheEquitySummary(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	e, store := harness(t, f, t.TempDir())
	api := newAPI(e.cfg, store, e.clients, e)
	rec := httptest.NewRecorder()
	api.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/paper/status", nil))

	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	ledger, _ := body["ledger"].(map[string]any)
	if ledger == nil {
		t.Fatal("the status payload must carry the book's equity summary")
	}
	if ledger["startingCash"] != defaultStartingCash {
		t.Errorf("startingCash = %v", ledger["startingCash"])
	}
	for _, k := range []string{"cash", "equity", "realizedPnl", "unrealized", "openLots", "nSnapshots"} {
		if _, ok := ledger[k]; !ok {
			t.Errorf("the equity summary is missing %q", k)
		}
	}
	if s, _ := body["sizing"].(string); !strings.Contains(s, "equity/N") {
		t.Errorf("status must say how positions are sized, got %q", s)
	}
}

// With no book the service remains observable, but it never takes a position it cannot score.
func TestWithoutALedgerTheEngineBlocksPositionChanges(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	e, store := harnessNoLedger(t, f, t.TempDir())
	e.evalConfig(context.Background(), testCfg, now)

	if st := store.StateFor(testCfg); st.Side != "" || st.LastDecision == nil || st.LastDecision.Gate != "sync" {
		t.Fatalf("the engine must fail closed without a book, state = %+v", st)
	}
	if !strings.Contains(e.sizing(), "BLOCKED") || strings.Contains(e.sizing(), "DEPRECATED FALLBACK") {
		t.Errorf("sizing() must name the refusal and no fallback, got %q", e.sizing())
	}

	api := newAPI(e.cfg, store, e.clients, e)
	rec := httptest.NewRecorder()
	api.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/paper/ledger", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("with no book /paper/ledger must refuse rather than serve zeroes, got %d", rec.Code)
	}
}

// CORS answers NOTHING by default. The wildcard it replaced was forbidden by browsers alongside
// credentials, so it advertised access the authenticated POSTs could never actually be given.
func TestCORSIsOffByDefaultAndExactWhenConfigured(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	e, store := harness(t, f, t.TempDir())

	req := httptest.NewRequest(http.MethodGet, "/paper/status", nil)
	req.Header.Set("Origin", "http://localhost:5173")

	off := newAPI(e.cfg, store, e.clients, e) // e.cfg has no CORSOrigins
	rec := httptest.NewRecorder()
	off.routes().ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("with no CORS_ORIGINS the service must answer no CORS header at all, got %q", got)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("the request itself is unaffected, got %d", rec.Code)
	}

	cfg := e.cfg
	cfg.CORSOrigins = []string{"http://localhost:5173"}
	on := newAPI(cfg, store, e.clients, e)
	rec = httptest.NewRecorder()
	on.routes().ServeHTTP(rec, req)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
		t.Errorf("an allow-listed origin must be echoed exactly, got %q", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("credentials must be allowed so the authenticated POSTs are callable, got %q", got)
	}
	if got := rec.Header().Get("Vary"); got != "Origin" {
		t.Errorf("a per-origin response must Vary on Origin, got %q", got)
	}

	// An origin that is not on the list gets nothing, however plausible it looks.
	other := httptest.NewRequest(http.MethodGet, "/paper/status", nil)
	other.Header.Set("Origin", "http://localhost:4173")
	rec = httptest.NewRecorder()
	on.routes().ServeHTTP(rec, other)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("an unlisted origin must get no header, got %q", got)
	}
}

// The book identity reports the two records SEPARATELY. A dead journal recorder and a running
// ledger are different facts, and a payload that conflated them let an empty table read as
// "no signals fired".
func TestBookIdentityReportsTheJournalAndTheLedgerSeparately(t *testing.T) {
	now := time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)
	f := newFakes(t, now)
	e, store := harness(t, f, t.TempDir())

	api := newAPI(e.cfg, store, e.clients, e) // no AUTH_SECRET
	id := api.bookIdentity()
	records, _ := id["records"].(map[string]any)
	if records == nil {
		t.Fatal("the book identity must report each record's liveness")
	}
	if records["journal"] != false || records["ledger"] != true {
		t.Errorf("records = %v, want journal:false ledger:true", records)
	}
	note, _ := id["recordingNote"].(string)
	if !strings.Contains(note, "JOURNAL RECORDING IS DEAD") || !strings.Contains(note, "LEDGER is running") {
		t.Errorf("the note must state both halves, got %q", note)
	}
}
