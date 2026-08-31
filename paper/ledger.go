package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// ledger.go — the FAKE-MONEY BOOK (docs/PAPER_EXECUTION_CONTRACT.md §5).
//
// SIMULATED BOOKKEEPING, NOT AN ACCOUNT. There is no broker, no order, no money movement and no
// balance anybody can withdraw. This file exists so the engine can keep score the SAME WAY the
// offline evaluator does — a date-aligned, equal-weight 1/N portfolio of the enabled configs — so
// that a live number and a backtest number can finally DISAGREE MEANINGFULLY instead of merely
// being printed next to each other in different units.
//
// WHAT IT REPLACED. Every simulated trade used to be a fixed $10,000 notional journal entry with no
// cash behind it, live statistics were per-trade and un-annualized, and `comparison.go` could only
// set two mismatched units side by side under a disclaimer. Neither column was wrong; they were
// answers to different questions.
//
// THE THREE RULES THAT MAKE IT COMPARABLE (all three are §5, and all three are testable):
//
//  1. ONE BOOK, 1/N. A new position's notional is `equity_at_entry / N`, N = the enabled config
//     count at that moment. No intra-hold rebalancing: the backtest charges turnover only on
//     position CHANGES, and so does this.
//  2. FEES ON EVERY FILL, at the cost the config's model was VALIDATED under (`report.costBps`,
//     already recorded on the trade at entry) — entries, exits, and BOTH legs of a flip.
//  3. MARKS ARE BAR CLOSES, never quotes and never guesses. A synthetic or missing mark produces
//     NO snapshot for that date — a recorded GAP. An invented mark would make the equity curve a
//     drawing rather than a measurement.
//
// DURABILITY. `fills.jsonl` and `snapshots.jsonl` are append-only and fsync'd; `ledger.json` is the
// fast-path state, written tmp+rename like `store.go`. The fill line is written BEFORE state is
// mutated, so a crash in the window leaves a fill the state has not applied — which `openLedger`
// detects (state's `lastFillSeq` is behind the file's) and repairs by replaying. The book is
// reconstructable from the two append-only files alone.

// defaultStartingCash is the book's opening balance when PAPER_STARTING_CASH is unset. It is
// simulated money and the number is arbitrary; what matters is that RETURNS are scale-free, so a
// different starting balance changes no statistic this service reports.
const defaultStartingCash = 100000.0

// minSnapshotsForSharpe is the floor below which the daily Sharpe is served as `null` rather than
// as a number. Nineteen daily observations do not have a standard deviation worth annualizing by
// √252, and a small-sample Sharpe printed without comment is the single most misleading number a
// paper book can produce.
const minSnapshotsForSharpe = 20

// dailyAnnualization is √252's input: the snapshot series has ONE observation per DATE, whatever
// timeframe the underlying configs trade, so the per-date annualization is the daily one. This is
// the same constant `backtest.ANNUALIZATION["1D"]` uses, which is what makes the live daily Sharpe
// and the evaluator's portfolio Sharpe the same kind of number (§5.4).
const dailyAnnualization = 252.0

// fill kinds. A flip is TWO fills with TWO fees, and they are named separately so a reader of
// fills.jsonl can see that both legs were billed without reconstructing the sequence.
const (
	fillOpen      = "open"
	fillClose     = "close"
	fillFlipClose = "flip-close"
	fillFlipOpen  = "flip-open"
)

// Lot is one config's open position in the book. Qty is always POSITIVE; Side carries the sign.
type Lot struct {
	ConfigKey     string  `json:"configKey"`
	Ticker        string  `json:"ticker"`
	Side          string  `json:"side"` // "long" | "short"
	Qty           float64 `json:"qty"`
	EntryPrice    float64 `json:"entryPrice"`
	EntryNotional float64 `json:"entryNotional"`
	EntryFee      float64 `json:"entryFee"`
	EntryBar      string  `json:"entryBar"`
	CostBps       float64 `json:"costBps"`
	TradeID       string  `json:"tradeId"`
	OpenedAt      string  `json:"openedAt"`
	// NAtEntry is the config count the 1/N slice was taken against. Recorded because N CHANGES: a
	// position opened when three configs were enabled holds a third of the book even after a fourth
	// is added, and a reader comparing notionals needs to know why they differ.
	NAtEntry int `json:"nAtEntry"`
}

// signedQty is the position in share terms: positive long, negative short.
func (l Lot) signedQty() float64 {
	if l.Side == "short" {
		return -l.Qty
	}
	return l.Qty
}

// Fill is one leg of one reconciliation. Append-only, one JSON object per line in `fills.jsonl`.
// Everything needed to REPLAY the book is on the record — a fills file that needs the state file to
// be interpreted is not a recovery mechanism.
type Fill struct {
	Seq      int64   `json:"seq"`
	At       string  `json:"at"`  // RFC3339, when the fill was booked
	Bar      string  `json:"bar"` // the bar the decision was made on (contract §3.2)
	Config   string  `json:"config"`
	Ticker   string  `json:"ticker"`
	Side     string  `json:"side"`     // "buy" | "sell" — the direction of THIS leg
	Position string  `json:"position"` // the position it opened, or the one it closed
	Qty      float64 `json:"qty"`
	Price    float64 `json:"price"`
	Notional float64 `json:"notional"` // qty * price — what the fee is charged on
	Fee      float64 `json:"fee"`
	CostBps  float64 `json:"costBps"`
	Kind     string  `json:"kind"`
	TradeID  string  `json:"tradeId"`  // the D-20 journal trade, "" when the journal refused it
	Realized float64 `json:"realized"` // P&L this leg realized, net of BOTH fees (0 on an entry)
	NAtEntry int     `json:"nAtEntry"`
	// Note explains a fill that was not an ordinary reconciliation — today, only the forced close
	// of a lot whose journal trade vanished. A fill nobody can explain later is a fill nobody can
	// audit, and this book exists to be audited.
	Note string `json:"note,omitempty"`
}

// Snapshot is one date's mark-to-market of the whole book. Written only when EVERY enabled config
// has a REAL mark for that date (§5.3).
type Snapshot struct {
	Date       string             `json:"date"`
	Equity     float64            `json:"equity"`
	Cash       float64            `json:"cash"`
	Realized   float64            `json:"realized"`
	Unrealized float64            `json:"unrealized"` // net of entry fees already paid — see equityLocked
	Exposure   float64            `json:"exposure"`   // |market value| / equity
	Marks      map[string]float64 `json:"marks"`
	// Gap records a date the book could NOT mark (a synthetic or missing bar). It carries no
	// equity: a gap is the absence of a measurement, never a zero.
	Gap       bool   `json:"gap,omitempty"`
	GapReason string `json:"gapReason,omitempty"`
}

// pendingMark is one config's mark for a date that is not yet complete across the book.
type pendingMark struct {
	Price float64 `json:"price"`
	Real  bool    `json:"real"`
}

type ledgerState struct {
	Generation   int64   `json:"generation,omitempty"`
	StartingCash float64 `json:"startingCash"`
	Cash         float64 `json:"cash"`
	Realized     float64 `json:"realizedPnl"`
	// OfficialStartedAt is day 0 of the CURRENT evidence-bearing experiment. It is empty for setup
	// books and is written only after the journal, ledger and engine state have all reset
	// successfully. A process restart therefore cannot turn "we clicked reset sometime" into a
	// guessed date.
	OfficialStartedAt string          `json:"officialStartedAt,omitempty"`
	OfficialConfigs   []string        `json:"officialConfigs,omitempty"`
	Lots              map[string]*Lot `json:"lots"`
	Snapshots         []Snapshot      `json:"snapshots"`
	GapDates          []string        `json:"gapDates"`
	// LastMark is the most recent REAL bar close per config — the price open lots are valued at
	// between snapshots, including when the engine asks for `equity/N`. Bar closes only: a quote is
	// an execution price, not a mark, and mixing the two makes the equity curve un-reproducible.
	LastMark map[string]float64 `json:"lastMark"`
	// Pending marks, by date then config, for dates not yet complete across the book.
	Pending map[string]map[string]pendingMark `json:"pendingMarks"`
	// Booked is the set of (trade id, kind) pairs this book has already filled — the IDEMPOTENCY
	// KEY. It exists so a RETRIED booking (store.PendingBooking) cannot double-book after a crash
	// between the ledger accepting a fill and the engine clearing the pending record. It is
	// reconstructed by applyFill, so a replayed book carries exactly the same set as a live one.
	Booked    map[string]bool `json:"bookedFills"`
	LastSeq   int64           `json:"lastFillSeq"`
	UpdatedAt string          `json:"updatedAt"`
}

// errDuplicateFill is what the book answers when it is asked to record a (trade id, kind) it
// already holds. It is not a failure: it is the retry finding out that the work is already done,
// and the caller resolves the pending record on it exactly as it does on success.
var errDuplicateFill = errors.New("ledger: this (trade id, kind) fill is already booked")

// fillIdent is the idempotency key. A fill with no trade id (the journal refused it) has no
// identity to deduplicate on and is never entered into the set.
func fillIdent(tradeID, kind string) string {
	if strings.TrimSpace(tradeID) == "" {
		return ""
	}
	return tradeID + "|" + kind
}

// Ledger is the book. Mutex-guarded, like Store.
type Ledger struct {
	dir        string
	mu         sync.Mutex
	st         ledgerState
	db         *paperDatabase
	generation int64
	// rebuilt records that this process had to replay the append-only files at startup, and why.
	// Surfaced in the payload: silently repairing a torn write and silently having nothing to
	// repair look identical from outside, and they are not the same event.
	rebuilt       bool
	rebuiltReason string
}

func (l *Ledger) Generation() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.generation
}

func (l *Ledger) statePath() string     { return filepath.Join(l.dir, "ledger.json") }
func (l *Ledger) fillsPath() string     { return filepath.Join(l.dir, "fills.jsonl") }
func (l *Ledger) snapshotsPath() string { return filepath.Join(l.dir, "snapshots.jsonl") }

func freshLedgerState(startingCash float64) ledgerState {
	return ledgerState{
		StartingCash: startingCash,
		Cash:         startingCash,
		Lots:         map[string]*Lot{},
		Snapshots:    []Snapshot{},
		GapDates:     []string{},
		LastMark:     map[string]float64{},
		Pending:      map[string]map[string]pendingMark{},
		Booked:       map[string]bool{},
	}
}

// openLedger loads (or rebuilds) the book under `dir`.
//
// The state file is a CACHE of the append-only files, not the record. It is trusted only when it is
// at least as new as the last fill on disk; otherwise the book is replayed from `fills.jsonl` +
// `snapshots.jsonl`, which is also the path taken when the state file is missing or unparseable.
func openLedger(dir string, startingCash float64) (*Ledger, error) {
	if startingCash <= 0 {
		startingCash = defaultStartingCash
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return initializeLedger(&Ledger{dir: dir, st: freshLedgerState(startingCash)}, startingCash)
}

func openLedgerWithDatabase(dir string, startingCash float64, db *paperDatabase) (*Ledger, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	generation, err := db.generation(ctx)
	if err != nil {
		return nil, err
	}
	return initializeLedger(&Ledger{
		dir: dir, db: db, generation: generation, st: freshLedgerState(startingCash),
	}, startingCash)
}

func initializeLedger(l *Ledger, startingCash float64) (*Ledger, error) {
	if startingCash <= 0 {
		startingCash = defaultStartingCash
		l.st = freshLedgerState(startingCash)
	}

	lastFill, err := l.lastFillSeq()
	if err != nil {
		return nil, err
	}

	reason := ""
	b, stateFound, err := l.readPersistedState()
	if err != nil {
		return nil, err
	}
	if !stateFound {
		if lastFill > 0 {
			reason = "no ledger.json, but fills.jsonl holds " + fmt.Sprint(lastFill) + " fills"
		}
	} else {
		var st ledgerState
		if json.Unmarshal(b, &st) != nil {
			reason = "ledger.json is unparseable"
		} else {
			normalizeLedgerState(&st, startingCash)
			if st.LastSeq < lastFill {
				reason = fmt.Sprintf("ledger.json stops at fill %d but fills.jsonl reaches %d — a crash "+
					"between the fill being written and the state being persisted", st.LastSeq, lastFill)
			} else {
				l.st = st
				if l.db == nil {
					l.generation = st.Generation
				}
			}
		}
	}

	if reason != "" || (lastFill > 0 && len(l.st.Lots) == 0 && l.st.LastSeq == 0) {
		if err := l.replay(startingCash); err != nil {
			return nil, err
		}
		l.rebuilt = true
		l.rebuiltReason = reason
		if l.rebuiltReason == "" {
			l.rebuiltReason = "state was empty while fills existed"
		}
		if err := l.persistLocked(); err != nil {
			return nil, err
		}
	}
	// A book that has never traded still needs its opening balance on disk, so an operator can see
	// that the ledger initialized rather than guessing from an absent file.
	if !stateFound {
		if err := l.persistLocked(); err != nil {
			return nil, err
		}
	}
	return l, nil
}

func (l *Ledger) readPersistedState() ([]byte, bool, error) {
	if l.db != nil {
		payload, found, generation, err := l.db.loadDocument("ledger_state")
		if err != nil || !found {
			return payload, found, err
		}
		if generation != l.generation {
			return nil, false, fmt.Errorf("ledger state generation %d does not match active generation %d", generation, l.generation)
		}
		return payload, true, nil
	}
	payload, err := os.ReadFile(l.statePath())
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	return payload, err == nil, err
}

// normalizeLedgerState fills in maps a hand-edited or older state file may be missing, so every
// later access is nil-safe without a guard at each use.
func normalizeLedgerState(st *ledgerState, startingCash float64) {
	if st.Lots == nil {
		st.Lots = map[string]*Lot{}
	}
	if st.LastMark == nil {
		st.LastMark = map[string]float64{}
	}
	if st.Pending == nil {
		st.Pending = map[string]map[string]pendingMark{}
	}
	if st.Booked == nil {
		st.Booked = map[string]bool{}
	}
	if st.Snapshots == nil {
		st.Snapshots = []Snapshot{}
	}
	if st.GapDates == nil {
		st.GapDates = []string{}
	}
	if st.StartingCash <= 0 {
		st.StartingCash = startingCash
	}
}

// replay rebuilds the whole book from the append-only files. Deterministic: the same two files
// always produce the same state, which is what makes the crash window survivable.
func (l *Ledger) replay(startingCash float64) error {
	st := freshLedgerState(startingCash)
	st.Generation = l.generation

	fills, err := l.readFills(0)
	if err != nil {
		return err
	}
	for _, f := range fills {
		applyFill(&st, f)
	}

	snaps, err := l.readSnapshots()
	if err != nil {
		return err
	}
	for _, s := range snaps {
		if s.Gap {
			st.GapDates = append(st.GapDates, s.Date)
			continue
		}
		st.Snapshots = append(st.Snapshots, s)
		for k, v := range s.Marks {
			st.LastMark[k] = v
		}
	}
	l.st = st
	return nil
}

// applyFill is the ONE place a fill moves cash, lots and realized P&L. Both the live path and the
// replay path go through it, so a replayed book and a live book cannot drift.
func applyFill(st *ledgerState, f Fill) {
	switch f.Kind {
	case fillOpen, fillFlipOpen:
		st.Lots[f.Config] = &Lot{
			ConfigKey: f.Config, Ticker: f.Ticker, Side: f.Position, Qty: f.Qty,
			EntryPrice: f.Price, EntryNotional: f.Notional, EntryFee: f.Fee, EntryBar: f.Bar,
			CostBps: f.CostBps, TradeID: f.TradeID, OpenedAt: f.At, NAtEntry: f.NAtEntry,
		}
		if f.Position == "short" {
			st.Cash += f.Notional - f.Fee // proceeds of the sale, less the fee
		} else {
			st.Cash -= f.Notional + f.Fee
		}
	case fillClose, fillFlipClose:
		delete(st.Lots, f.Config)
		if f.Position == "short" {
			st.Cash -= f.Notional + f.Fee // buying the borrowed shares back
		} else {
			st.Cash += f.Notional - f.Fee
		}
		st.Realized += f.Realized
	}
	if id := fillIdent(f.TradeID, f.Kind); id != "" {
		if st.Booked == nil {
			st.Booked = map[string]bool{}
		}
		st.Booked[id] = true
	}
	if f.Seq > st.LastSeq {
		st.LastSeq = f.Seq
	}
}

// --- reads of the append-only files -------------------------------------------------------------

// readFills returns the fills on disk; `tail > 0` returns only the last `tail` of them.
//
// A line that will not parse is SKIPPED, not fatal: the only way one exists is a torn final write,
// and refusing to open the book because its last line is half-written would turn a recoverable
// crash into a permanent outage.
func (l *Ledger) readFills(tail int) ([]Fill, error) {
	if l.db != nil {
		out, err := l.db.fills(l.generation)
		if tail > 0 && len(out) > tail {
			out = out[len(out)-tail:]
		}
		return out, err
	}
	f, err := os.Open(l.fillsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []Fill
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var fill Fill
		if json.Unmarshal([]byte(line), &fill) != nil {
			continue
		}
		out = append(out, fill)
	}
	if tail > 0 && len(out) > tail {
		out = out[len(out)-tail:]
	}
	return out, nil
}

func (l *Ledger) readSnapshots() ([]Snapshot, error) {
	if l.db != nil {
		return l.db.snapshots(l.generation)
	}
	f, err := os.Open(l.snapshotsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []Snapshot
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var s Snapshot
		if json.Unmarshal([]byte(line), &s) != nil {
			continue
		}
		out = append(out, s)
	}
	return out, nil
}

func (l *Ledger) lastFillSeq() (int64, error) {
	fills, err := l.readFills(0)
	if err != nil {
		return 0, err
	}
	var max int64
	for _, f := range fills {
		if f.Seq > max {
			max = f.Seq
		}
	}
	return max, nil
}

// --- writes -------------------------------------------------------------------------------------

// appendJSONL appends one record and fsyncs it. The fsync is the point: "the fill was written
// before the state changed" is only true if the bytes actually reached the disk in that order.
func appendJSONL(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

func (l *Ledger) appendFillLocked(fill Fill) error {
	if l.db != nil {
		return l.db.appendFill(l.generation, fill)
	}
	return appendJSONL(l.fillsPath(), fill)
}

func (l *Ledger) appendSnapshotLocked(snapshot Snapshot) error {
	if l.db != nil {
		return l.db.appendSnapshot(l.generation, snapshot)
	}
	return appendJSONL(l.snapshotsPath(), snapshot)
}

func (l *Ledger) persistLocked() error {
	l.st.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	l.st.Generation = l.generation
	if l.db != nil {
		if err := l.db.saveDocument("ledger_state", l.generation, l.st); err != nil {
			return fmt.Errorf("ledger: persist PostgreSQL state: %w", err)
		}
		return nil
	}
	if err := writeJSONAtomic(l.statePath(), l.st); err != nil {
		return fmt.Errorf("ledger: persist state: %w", err)
	}
	return nil
}

// --- valuation ------------------------------------------------------------------------------------

// markPriceLocked is what an open lot is valued at: its config's last REAL bar close, falling back
// to its own entry price. The fallback is deliberately the entry price and not zero or the last
// quote: an unmarked lot contributes NO unrealized P&L, which is the honest answer to "we have not
// been able to mark this since it opened".
func (l *Ledger) markPriceLocked(key string, lot *Lot) float64 {
	if p, ok := l.st.LastMark[key]; ok && p > 0 {
		return p
	}
	return lot.EntryPrice
}

// equityLocked is cash plus the signed market value of every open lot.
//
// The book satisfies `equity == startingCash + realized + unrealized` exactly, where `unrealized`
// is NET OF THE ENTRY FEE already paid. That identity is asserted in the tests: an accounting
// engine whose three components do not add up to its own equity is not one.
func (l *Ledger) equityLocked() float64 {
	eq := l.st.Cash
	for key, lot := range l.st.Lots {
		eq += lot.signedQty() * l.markPriceLocked(key, lot)
	}
	return eq
}

func (l *Ledger) unrealizedLocked() float64 {
	var u float64
	for key, lot := range l.st.Lots {
		mark := l.markPriceLocked(key, lot)
		u += lot.signedQty()*(mark-lot.EntryPrice) - lot.EntryFee
	}
	return u
}

func (l *Ledger) exposureLocked(equity float64) float64 {
	if equity <= 0 {
		return 0
	}
	var gross float64
	for key, lot := range l.st.Lots {
		gross += math.Abs(lot.Qty * l.markPriceLocked(key, lot))
	}
	return gross / equity
}

// Equity is the book's current mark-to-market value.
func (l *Ledger) Equity() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.equityLocked()
}

// HasLot reports whether the book holds a position for a config. The engine uses it to keep its own
// state and the book in step: they are two records of one position and a divergence is a defect.
func (l *Ledger) HasLot(configKey string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.st.Lots[configKey] != nil
}

// HasBooked reports whether the book already holds a fill for a (trade id, kind) pair — the
// question a RETRIED booking asks before doing anything. A fill with no trade id has no identity to
// deduplicate on, so it is never "already booked".
func (l *Ledger) HasBooked(tradeID, kind string) bool {
	id := fillIdent(tradeID, kind)
	if id == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.st.Booked[id]
}

// LotFor returns a COPY of a config's open lot, or nil.
func (l *Ledger) LotFor(configKey string) *Lot {
	l.mu.Lock()
	defer l.mu.Unlock()
	if lot := l.st.Lots[configKey]; lot != nil {
		cp := *lot
		return &cp
	}
	return nil
}

// --- the two mutations ----------------------------------------------------------------------------

// Open books an entry leg: `equity_at_entry / N` of notional, at `price`, paying `costBps` on the
// traded notional (§5.2). `n` is the enabled config count AT THIS MOMENT — passed in rather than
// remembered, because it changes whenever /paper/config is rewritten.
//
// The fill line is written and fsync'd BEFORE the state moves, so the crash window leaves a fill the
// state has not applied — which `openLedger` detects and replays — rather than a state change no
// fill explains.
func (l *Ledger) Open(cfg PaperCfg, side, bar, tradeID string, price, costBps float64, n int,
	now time.Time) (Fill, error) {
	return l.open(cfg, side, bar, tradeID, price, costBps, n, now, fillOpen, "")
}

func (l *Ledger) open(cfg PaperCfg, side, bar, tradeID string, price, costBps float64, n int,
	now time.Time, kind, note string) (Fill, error) {

	l.mu.Lock()
	defer l.mu.Unlock()
	if n < 1 {
		n = 1
	}
	equity := l.equityLocked()
	notional := equity / float64(n)
	if notional <= 0 {
		return Fill{}, fmt.Errorf("ledger: equity is %.2f — there is nothing to allocate to %s", equity, cfg.Key())
	}
	return l.openExactLocked(cfg, side, bar, tradeID, price, costBps, n, notional/price,
		notional, now, kind, note)
}

// openExact replays an already-decided entry using the sizing values captured before its journal
// write. This is the recovery path; using current equity/N here would create a different fill.
func (l *Ledger) openExact(cfg PaperCfg, side, bar, tradeID string, price, costBps float64, n int,
	qty, notional float64, now time.Time, kind, note string) (Fill, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.openExactLocked(cfg, side, bar, tradeID, price, costBps, n, qty, notional, now, kind, note)
}

func (l *Ledger) openExactLocked(cfg PaperCfg, side, bar, tradeID string,
	price, costBps float64, n int, qty, notional float64, now time.Time, kind, note string) (Fill, error) {

	key := cfg.Key()
	// Idempotency first: a retried booking must find out that it is already booked BEFORE the
	// "already holds a lot" check, so the caller gets errDuplicateFill (resolve the pending record)
	// rather than a generic disagreement it would keep retrying forever.
	if id := fillIdent(tradeID, kind); id != "" && l.st.Booked[id] {
		return Fill{}, fmt.Errorf("%w: %s %s", errDuplicateFill, key, id)
	}
	if l.st.Lots[key] != nil {
		return Fill{}, fmt.Errorf("ledger: %s already holds a lot — the book and the engine disagree", key)
	}
	if price <= 0 {
		return Fill{}, fmt.Errorf("ledger: refusing to open %s at a non-positive price %.4f", key, price)
	}
	if side != "long" && side != "short" {
		return Fill{}, fmt.Errorf("ledger: unknown side %q for %s", side, key)
	}
	if n < 1 {
		return Fill{}, fmt.Errorf("ledger: refusing exact open for %s without nAtEntry", key)
	}
	if qty <= 0 || notional <= 0 {
		return Fill{}, fmt.Errorf("ledger: refusing exact open for %s without positive qty and notional", key)
	}
	if delta := math.Abs(qty*price - notional); delta > math.Max(0.01, notional*1e-9) {
		return Fill{}, fmt.Errorf("ledger: exact open for %s is internally inconsistent: qty*price %.8f != notional %.8f",
			key, qty*price, notional)
	}
	fee := notional * costBps / 10000.0

	l.st.LastSeq++
	f := Fill{
		Seq: l.st.LastSeq, At: now.UTC().Format(time.RFC3339), Bar: bar, Config: key,
		Ticker: cfg.Ticker, Side: map[bool]string{true: "sell", false: "buy"}[side == "short"],
		Position: side, Qty: qty, Price: price, Notional: notional, Fee: fee, CostBps: costBps,
		Kind: kind, TradeID: tradeID, NAtEntry: n, Note: note,
	}
	if err := l.appendFillLocked(f); err != nil {
		l.st.LastSeq-- // nothing was written, so nothing consumed the sequence number
		return Fill{}, fmt.Errorf("ledger: could not journal the fill: %w", err)
	}
	applyFill(&l.st, f)
	if err := l.persistLocked(); err != nil {
		return f, err
	}
	return f, nil
}

// Close books an exit leg at `price`, paying the SAME cost the lot was opened under — the cost the
// model was validated at, captured on the lot rather than re-read, so a later config change cannot
// retroactively re-price a fee that was already charged.
func (l *Ledger) Close(cfg PaperCfg, bar string, price float64, now time.Time, note string) (Fill, error) {
	return l.close(cfg, bar, price, now, fillClose, note)
}

func (l *Ledger) close(cfg PaperCfg, bar string, price float64, now time.Time, kind, note string) (Fill, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	key := cfg.Key()
	lot := l.st.Lots[key]
	if lot == nil {
		return Fill{}, fmt.Errorf("ledger: %s holds no lot to close", key)
	}
	if id := fillIdent(lot.TradeID, kind); id != "" && l.st.Booked[id] {
		return Fill{}, fmt.Errorf("%w: %s %s", errDuplicateFill, key, id)
	}
	if price <= 0 {
		return Fill{}, fmt.Errorf("ledger: refusing to close %s at a non-positive price %.4f", key, price)
	}
	proceeds := lot.Qty * price
	fee := proceeds * lot.CostBps / 10000.0
	realized := proceeds - lot.EntryNotional
	if lot.Side == "short" {
		realized = lot.EntryNotional - proceeds
	}
	realized -= lot.EntryFee + fee // both legs' fees land in realized P&L, never in a footnote

	l.st.LastSeq++
	f := Fill{
		Seq: l.st.LastSeq, At: now.UTC().Format(time.RFC3339), Bar: bar, Config: key,
		Ticker: cfg.Ticker, Side: map[bool]string{true: "buy", false: "sell"}[lot.Side == "short"],
		Position: lot.Side, Qty: lot.Qty, Price: price, Notional: proceeds, Fee: fee,
		CostBps: lot.CostBps, Kind: kind, TradeID: lot.TradeID, Realized: realized,
		NAtEntry: lot.NAtEntry, Note: note,
	}
	if err := l.appendFillLocked(f); err != nil {
		l.st.LastSeq--
		return Fill{}, fmt.Errorf("ledger: could not journal the fill: %w", err)
	}
	applyFill(&l.st, f)
	if err := l.persistLocked(); err != nil {
		return f, err
	}
	return f, nil
}

// Flip books BOTH legs of a flip: the close of the old side and the open of the new one, in that
// order, at the same price and the same bar — two fills and TWO fees, exactly the 2x turnover the
// backtest atom charges (§5.1).
func (l *Ledger) Flip(cfg PaperCfg, toSide, bar, tradeID string, price, costBps float64, n int,
	now time.Time) (Fill, Fill, error) {
	out, err := l.close(cfg, bar, price, now, fillFlipClose, "")
	if err != nil {
		return Fill{}, Fill{}, err
	}
	in, err := l.open(cfg, toSide, bar, tradeID, price, costBps, n, now, fillFlipOpen, "")
	if err != nil {
		// The close is already booked and fsync'd; it is not undone. A half-flipped book that says
		// "flat" is truthful — the position IS closed — and the next bar re-decides from flat.
		return out, Fill{}, err
	}
	return out, in, nil
}

// --- marking ---------------------------------------------------------------------------------------

// Mark records one config's close for one date, and writes the date's snapshot once EVERY enabled
// config has a REAL mark for it (§5.3).
//
// Three outcomes, and the third is the point:
//   - complete and all real  -> a snapshot, marked at those closes;
//   - any mark not real      -> a GAP for that date, immediately: a synthetic bar produces no mark,
//     and a book that substituted the previous close would be inventing a measurement;
//   - still incomplete       -> pending, until a LATER date arrives, at which point the stale date
//     becomes a gap. Without that rule an incomplete date pends forever and the
//     book silently stops snapshotting.
func (l *Ledger) Mark(enabled []string, configKey, date string, price float64, real bool) error {
	if date == "" || configKey == "" {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.st.Pending == nil {
		l.st.Pending = map[string]map[string]pendingMark{}
	}
	if l.st.Pending[date] == nil {
		l.st.Pending[date] = map[string]pendingMark{}
	}
	l.st.Pending[date][configKey] = pendingMark{Price: price, Real: real}
	if real && price > 0 {
		l.st.LastMark[configKey] = price
	}

	// Any pending date strictly older than this one can never complete now.
	for d := range l.st.Pending {
		if d < date {
			if err := l.gapLocked(d, "the book moved on to "+date+" before every config had marked "+d); err != nil {
				return err
			}
		}
	}
	if err := l.settleLocked(enabled, date); err != nil {
		return err
	}
	return l.persistLocked()
}

// settleLocked decides whether `date` is now a snapshot, a gap, or still pending.
func (l *Ledger) settleLocked(enabled []string, date string) error {
	marks := l.st.Pending[date]
	if marks == nil {
		return nil
	}
	for _, key := range enabled {
		m, ok := marks[key]
		if !ok {
			return nil // still waiting on this config
		}
		if !m.Real || m.Price <= 0 {
			return l.gapLocked(date, "config "+key+" had no real close for "+date+
				" (a synthetic or missing bar is not a mark)")
		}
	}
	if len(enabled) == 0 {
		return nil
	}
	snapMarks := map[string]float64{}
	for _, key := range enabled {
		snapMarks[key] = marks[key].Price
		l.st.LastMark[key] = marks[key].Price
	}
	// Value the book at exactly these closes, not at LastMark, so the snapshot is reproducible from
	// its own record.
	cash := l.st.Cash
	equity := cash
	var unreal, gross float64
	for key, lot := range l.st.Lots {
		mark, ok := snapMarks[key]
		if !ok || mark <= 0 {
			mark = l.markPriceLocked(key, lot)
		}
		equity += lot.signedQty() * mark
		unreal += lot.signedQty()*(mark-lot.EntryPrice) - lot.EntryFee
		gross += math.Abs(lot.Qty * mark)
	}
	exposure := 0.0
	if equity > 0 {
		exposure = gross / equity
	}
	s := Snapshot{
		Date: date, Equity: equity, Cash: cash, Realized: l.st.Realized,
		Unrealized: unreal, Exposure: exposure, Marks: snapMarks,
	}
	if l.hasSnapshot(date) {
		delete(l.st.Pending, date)
		return nil // idempotent: re-marking a settled date must not double-count it
	}
	if err := l.appendSnapshotLocked(s); err != nil {
		return fmt.Errorf("ledger: could not journal snapshot %s: %w", date, err)
	}
	delete(l.st.Pending, date)
	l.st.Snapshots = append(l.st.Snapshots, s)
	sort.SliceStable(l.st.Snapshots, func(i, j int) bool { return l.st.Snapshots[i].Date < l.st.Snapshots[j].Date })
	return nil
}

func (l *Ledger) hasSnapshot(date string) bool {
	for _, s := range l.st.Snapshots {
		if s.Date == date {
			return true
		}
	}
	return false
}

// gapLocked records a date the book could not mark. A gap is DATA — it is surfaced in the payload —
// because "no snapshot on 2026-08-21" and "the book was flat on 2026-08-21" are different facts.
func (l *Ledger) gapLocked(date, reason string) error {
	if slices.Contains(l.st.GapDates, date) {
		delete(l.st.Pending, date)
		return nil
	}
	if l.hasSnapshot(date) {
		delete(l.st.Pending, date)
		return nil
	}
	if err := l.appendSnapshotLocked(Snapshot{Date: date, Gap: true, GapReason: reason}); err != nil {
		return fmt.Errorf("ledger: could not journal gap %s: %w", date, err)
	}
	delete(l.st.Pending, date)
	l.st.GapDates = append(l.st.GapDates, date)
	sort.Strings(l.st.GapDates)
	return nil
}

// --- metrics ---------------------------------------------------------------------------------------

type ledgerMetrics struct {
	NSnapshots    int      `json:"nSnapshots"`
	NReturns      int      `json:"nDailyReturns"`
	DailySharpe   *float64 `json:"dailySharpeAnnualized"`
	MaxDrawdown   *float64 `json:"maxDrawdown"`
	TotalReturn   *float64 `json:"totalReturn"`
	MeanDaily     *float64 `json:"meanDailyReturn"`
	StdDaily      *float64 `json:"stdDailyReturn"`
	Annualization float64  `json:"annualization"`
	SharpeNote    string   `json:"sharpeNote"`
}

// dashboardSeriesPoint keeps dates attached to the book's measured values. Gap rows deliberately
// carry null numbers: a missing mark is not zero and is not yesterday's equity carried forward.
type dashboardSeriesPoint struct {
	Date        string   `json:"date"`
	Equity      *float64 `json:"equity"`
	Cash        *float64 `json:"cash"`
	Realized    *float64 `json:"realized"`
	Unrealized  *float64 `json:"unrealized"`
	Exposure    *float64 `json:"exposure"`
	DailyReturn *float64 `json:"dailyReturn"`
	Drawdown    *float64 `json:"drawdown"`
	Gap         bool     `json:"gap"`
	GapReason   string   `json:"gapReason,omitempty"`
}

func float64ptr(v float64) *float64 { return &v }

// DashboardSeries returns a bounded tail while computing returns and drawdown over the full
// generation first, so changing the display window cannot change a point's value.
func (l *Ledger) DashboardSeries(limit int) ([]dashboardSeriesPoint, error) {
	snapshots, err := l.readSnapshots()
	if err != nil {
		return nil, err
	}
	sort.SliceStable(snapshots, func(i, j int) bool { return snapshots[i].Date < snapshots[j].Date })
	out := make([]dashboardSeriesPoint, 0, len(snapshots))
	peak := 0.0
	previous := 0.0
	for _, snapshot := range snapshots {
		point := dashboardSeriesPoint{Date: snapshot.Date, Gap: snapshot.Gap, GapReason: snapshot.GapReason}
		if snapshot.Gap {
			out = append(out, point)
			continue
		}
		if snapshot.Equity > peak {
			peak = snapshot.Equity
		}
		drawdown := 0.0
		if peak > 0 {
			drawdown = (peak - snapshot.Equity) / peak
		}
		point.Equity = float64ptr(round2(snapshot.Equity))
		point.Cash = float64ptr(round2(snapshot.Cash))
		point.Realized = float64ptr(round2(snapshot.Realized))
		point.Unrealized = float64ptr(round2(snapshot.Unrealized))
		point.Exposure = float64ptr(round5(snapshot.Exposure))
		point.Drawdown = float64ptr(round5(drawdown))
		if previous != 0 {
			point.DailyReturn = float64ptr(round6(snapshot.Equity/previous - 1))
		}
		previous = snapshot.Equity
		out = append(out, point)
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

// metricsLocked computes the book's statistics from the SNAPSHOT SERIES — one observation per date,
// exactly like the evaluator's portfolio series (§5.4). That is what makes the two Sharpes the same
// kind of number.
func (l *Ledger) metricsLocked() ledgerMetrics {
	m := ledgerMetrics{
		NSnapshots: len(l.st.Snapshots), Annualization: dailyAnnualization,
	}
	eq := make([]float64, 0, len(l.st.Snapshots))
	for _, s := range l.st.Snapshots {
		eq = append(eq, s.Equity)
	}
	if len(eq) < 2 {
		m.SharpeNote = fmt.Sprintf("UNMEASURED: %d daily snapshots. The book has not yet been marked "+
			"on enough dates to have a return series at all.", len(eq))
		return m
	}
	rets := make([]float64, 0, len(eq)-1)
	for i := 1; i < len(eq); i++ {
		if eq[i-1] == 0 {
			continue
		}
		rets = append(rets, eq[i]/eq[i-1]-1.0)
	}
	m.NReturns = len(rets)

	if eq[0] != 0 {
		tr := eq[len(eq)-1]/eq[0] - 1.0
		m.TotalReturn = &tr
	}
	dd := maxDrawdown(eq)
	m.MaxDrawdown = &dd

	if len(rets) > 0 {
		mean, std := meanStd(rets)
		m.MeanDaily, m.StdDaily = &mean, &std
		// Below the floor the Sharpe is NULL, not a number. Nineteen observations do not have a
		// standard deviation worth multiplying by √252, and a small-sample Sharpe printed without
		// comment is read as a finding.
		if len(l.st.Snapshots) >= minSnapshotsForSharpe && std > 1e-12 {
			sh := mean / std * math.Sqrt(dailyAnnualization)
			m.DailySharpe = &sh
			m.SharpeNote = fmt.Sprintf("Annualized (x sqrt(%.0f)) from %d daily portfolio returns — the "+
				"same statistic, on the same kind of series, as the evaluator's portfolio Sharpe.",
				dailyAnnualization, len(rets))
			return m
		}
	}
	if len(l.st.Snapshots) < minSnapshotsForSharpe {
		m.SharpeNote = fmt.Sprintf("UNMEASURED: %d daily snapshots, %d required. A Sharpe from fewer "+
			"observations is not a small number, it is not a number.", len(l.st.Snapshots), minSnapshotsForSharpe)
	} else {
		m.SharpeNote = "UNMEASURED: the daily returns have no dispersion (the book has not moved), so " +
			"a Sharpe would divide by zero."
	}
	return m
}

// Metrics is the locked accessor.
func (l *Ledger) Metrics() ledgerMetrics {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.metricsLocked()
}

func meanStd(xs []float64) (float64, float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	mean := sum / float64(len(xs))
	var ss float64
	for _, x := range xs {
		ss += (x - mean) * (x - mean)
	}
	return mean, math.Sqrt(ss / float64(len(xs)))
}

// maxDrawdown is the largest peak-to-trough decline of an equity curve, as a POSITIVE fraction —
// the same definition as backtest.max_drawdown.
func maxDrawdown(equity []float64) float64 {
	if len(equity) == 0 {
		return 0
	}
	peak := equity[0]
	worst := 0.0
	for _, e := range equity {
		if e > peak {
			peak = e
		}
		if peak > 0 {
			if dd := 1.0 - e/peak; dd > worst {
				worst = dd
			}
		}
	}
	return worst
}

// downsampleEquity mirrors backtest._downsample so the live curve and the backtest curve are drawn
// at the same resolution by the same rule.
func downsampleEquity(equity []float64, maxPoints int) []float64 {
	n := len(equity)
	if n == 0 || maxPoints <= 0 {
		return []float64{}
	}
	if n <= maxPoints {
		out := make([]float64, n)
		for i, v := range equity {
			out[i] = round5(v)
		}
		return out
	}
	out := make([]float64, maxPoints)
	for i := range maxPoints {
		idx := int(float64(i) * float64(n-1) / float64(maxPoints-1))
		out[i] = round5(equity[idx])
	}
	return out
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
func round5(v float64) float64 { return math.Round(v*100000) / 100000 }
func round6(v float64) float64 { return math.Round(v*1e6) / 1e6 }

// --- reset -----------------------------------------------------------------------------------------

// Reset returns the book to its opening balance and ARCHIVES the append-only files rather than
// deleting them. A reset is an operator action on a validation book; the record of what the book did
// before it is exactly the thing somebody will want later, and `rm` cannot be undone.
func (l *Ledger) Reset(now time.Time) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.db != nil {
		fresh := freshLedgerState(l.st.StartingCash)
		next, err := l.db.resetLedger(l.generation, l.st, fresh, now)
		if err != nil {
			return err
		}
		l.generation = next
		fresh.Generation = next
		l.st = fresh
		l.rebuilt, l.rebuiltReason = false, ""
		return nil
	}
	stamp := now.UTC().Format("20060102T150405.000000000Z")
	type movedFile struct{ live, archived string }
	moved := make([]movedFile, 0, 2)
	rollback := func() error {
		var failures []error
		for i := len(moved) - 1; i >= 0; i-- {
			if err := os.Rename(moved[i].archived, moved[i].live); err != nil {
				failures = append(failures, fmt.Errorf("restore %s: %w", filepath.Base(moved[i].live), err))
			}
		}
		return errors.Join(failures...)
	}
	for _, p := range []string{l.fillsPath(), l.snapshotsPath()} {
		if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return errors.Join(fmt.Errorf("inspect %s before reset: %w", filepath.Base(p), err), rollback())
		}
		ext := filepath.Ext(p)
		archived := strings.TrimSuffix(p, ext) + "-" + stamp + ext
		if _, err := os.Stat(archived); err == nil {
			return errors.Join(fmt.Errorf("refusing to overwrite existing reset archive %s", filepath.Base(archived)), rollback())
		} else if !errors.Is(err, os.ErrNotExist) {
			return errors.Join(fmt.Errorf("inspect reset archive %s: %w", filepath.Base(archived), err), rollback())
		}
		if err := os.Rename(p, archived); err != nil {
			return errors.Join(fmt.Errorf("archive %s: %w", filepath.Base(p), err), rollback())
		}
		moved = append(moved, movedFile{live: p, archived: archived})
	}
	nextGeneration := l.generation + 1
	fresh := freshLedgerState(l.st.StartingCash)
	fresh.Generation = nextGeneration
	if err := writeJSONAtomic(l.statePath(), fresh); err != nil {
		if writeCommitted(err) {
			// Rename was the commit point. Keep memory aligned with the committed file and leave the
			// archives in place; the caller still gets the fsync error and will not declare day 0.
			l.st = fresh
			l.generation = nextGeneration
			l.rebuilt, l.rebuiltReason = false, ""
			return fmt.Errorf("persist fresh ledger during reset: %w", err)
		}
		return errors.Join(fmt.Errorf("persist fresh ledger during reset: %w", err), rollback())
	}
	l.st = fresh
	l.generation = nextGeneration
	l.rebuilt, l.rebuiltReason = false, ""
	return nil
}

// StartOfficial durably establishes day 0 after every reset step succeeded. Keeping this separate
// from Reset matters: Reset can succeed while the engine-state commit fails, and that partial
// operation must never leave a durable timestamp claiming the official experiment began.
func (l *Ledger) StartOfficial(now time.Time, configs []PaperCfg) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	oldAt := l.st.OfficialStartedAt
	oldConfigs := append([]string(nil), l.st.OfficialConfigs...)
	l.st.OfficialStartedAt = now.UTC().Format(time.RFC3339)
	l.st.OfficialConfigs = make([]string, 0, len(configs))
	for _, cfg := range configs {
		l.st.OfficialConfigs = append(l.st.OfficialConfigs, cfg.Key())
	}
	if err := l.persistLocked(); err != nil {
		if !writeCommitted(err) {
			l.st.OfficialStartedAt = oldAt
			l.st.OfficialConfigs = oldConfigs
		}
		return err
	}
	return nil
}

// InvalidateOfficial stops the evidence clock before an operator changes the configured portfolio.
// Changing N or the enabled streams mid-run creates a different experiment; preserving the old
// day-0 label across that change would mix two designs into one result.
func (l *Ledger) InvalidateOfficial() (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.st.OfficialStartedAt == "" {
		return false, nil
	}
	oldAt := l.st.OfficialStartedAt
	oldConfigs := append([]string(nil), l.st.OfficialConfigs...)
	l.st.OfficialStartedAt = ""
	l.st.OfficialConfigs = nil
	if err := l.persistLocked(); err != nil {
		if !writeCommitted(err) {
			l.st.OfficialStartedAt = oldAt
			l.st.OfficialConfigs = oldConfigs
		}
		return false, err
	}
	return true, nil
}

// --- payloads ---------------------------------------------------------------------------------------

// Summary is the one-line equity block GET /paper/status carries.
func (l *Ledger) Summary() map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	equity := l.equityLocked()
	m := l.metricsLocked()
	return map[string]any{
		"startingCash":      round2(l.st.StartingCash),
		"cash":              round2(l.st.Cash),
		"equity":            round2(equity),
		"realizedPnl":       round2(l.st.Realized),
		"unrealized":        round2(l.unrealizedLocked()),
		"openLots":          len(l.st.Lots),
		"nSnapshots":        m.NSnapshots,
		"nGapDates":         len(l.st.GapDates),
		"sizing":            "equity/N (equal-weight 1/N across the enabled configs)",
		"simulation":        true,
		"officialStartedAt": l.st.OfficialStartedAt,
		"officialConfigs":   append([]string(nil), l.st.OfficialConfigs...),
		"generation":        l.generation,
	}
}

// Payload is GET /paper/ledger.
func (l *Ledger) Payload(fillTail int) map[string]any {
	fills, ferr := l.readFills(fillTail)

	l.mu.Lock()
	defer l.mu.Unlock()

	equity := l.equityLocked()
	positions := make([]map[string]any, 0, len(l.st.Lots))
	keys := make([]string, 0, len(l.st.Lots))
	for k := range l.st.Lots {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		lot := l.st.Lots[k]
		mark := l.markPriceLocked(k, lot)
		marked := false
		if p, ok := l.st.LastMark[k]; ok && p > 0 {
			marked = true
			mark = p
		}
		positions = append(positions, map[string]any{
			"config": k, "ticker": lot.Ticker, "side": lot.Side,
			"qty": round6(lot.Qty), "entryPrice": lot.EntryPrice, "entryBar": lot.EntryBar,
			"entryNotional": round2(lot.EntryNotional), "entryFee": round2(lot.EntryFee),
			"costBps": lot.CostBps, "nAtEntry": lot.NAtEntry, "tradeId": lot.TradeID,
			"mark": mark, "marked": marked,
			"marketValue": round2(lot.signedQty() * mark),
			"unrealized":  round2(lot.signedQty()*(mark-lot.EntryPrice) - lot.EntryFee),
		})
	}

	eq := make([]float64, 0, len(l.st.Snapshots))
	for _, s := range l.st.Snapshots {
		eq = append(eq, s.Equity)
	}
	first, last := "", ""
	if n := len(l.st.Snapshots); n > 0 {
		first, last = l.st.Snapshots[0].Date, l.st.Snapshots[n-1].Date
	}

	tail := make([]Fill, 0, len(fills))
	tail = append(tail, fills...)

	out := map[string]any{
		"paper": true, "simulation": true,
		"officialStartedAt": l.st.OfficialStartedAt,
		"officialConfigs":   append([]string(nil), l.st.OfficialConfigs...),
		"generation":        l.generation,
		"startingCash":      round2(l.st.StartingCash),
		"cash":              round2(l.st.Cash),
		"equity":            round2(equity),
		"realizedPnl":       round2(l.st.Realized),
		"unrealized":        round2(l.unrealizedLocked()),
		"exposure":          round5(l.exposureLocked(equity)),
		"allocation":        "equal-weight 1/N across the enabled configs; a new position takes equity_at_entry / N",
		"positions":         positions,
		"snapshots": map[string]any{
			"n": len(l.st.Snapshots), "first": first, "last": last,
			"markedAt": "each new completed 1D bar's close; a synthetic or missing bar produces a gap, never a mark",
		},
		"equityCurve": downsampleEquity(eq, 120),
		"metrics":     l.metricsLocked(),
		// Dates the book could NOT mark. Surfaced rather than smoothed over: a gap is the absence of
		// a measurement, and an equity curve that hides its holes is a drawing.
		"gapDates":    append([]string{}, l.st.GapDates...),
		"fills":       tail,
		"lastFillSeq": l.st.LastSeq,
		"contract": "docs/PAPER_EXECUTION_CONTRACT.md §5 — one book, equal-weight 1/N, fees at the " +
			"cost the model was validated under, marked only at real bar closes",
		"note": "SIMULATED BOOKKEEPING. No broker, no order, no money movement, and no balance " +
			"anybody can withdraw. These numbers exist to be compared with the backtest's.",
	}
	if ferr != nil {
		out["fillsError"] = ferr.Error()
	}
	if l.rebuilt {
		out["rebuiltFromFills"] = map[string]any{
			"rebuilt": true, "reason": l.rebuiltReason,
			"note": "the fast-path ledger state was behind the append-only fills on startup and the " +
				"book was replayed from them — the fills are the record",
		}
	}
	return out
}
