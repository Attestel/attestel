package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// engine.go — the paper-trading validation loop, as a PER-BAR ALLOCATION RULE.
//
// The rule, in full, is docs/PAPER_EXECUTION_CONTRACT.md; this file implements §3 of it. Once per
// bar, per config:
//
//  1. Ask analysis for the newest bar. If it is not strictly newer than the bar this config last
//     acted on, OR it names a session that has not finished yet, the tick ends — EVAL_INTERVAL is a
//     POLLING rate, not a decision rate, and a FORMING bar is not a bar (contract §2).
//  2. Ask /predict for the validated signal and derive the TARGET position: Buy -> long,
//     Sell -> short, Hold -> flat.
//  3. Run the gates (gates.go). A refusal is recorded against the bar and nothing moves.
//  4. Reconcile the target against what is held: open, close, flip (close+open), or hold.
//
// WHAT THIS REPLACED, AND WHY. The engine used to open a position and hold it for exactly `horizon`
// bars, and its own comment claimed that "mirrors the backtest's fixed-horizon exit exactly — this
// is what makes the live/backtest comparison fair". The backtest has no fixed-horizon exit: it
// re-decides every bar and pays the next bar's close-to-close return (backtest.run_backtest). The
// hold, the claim, and the fairness the claim asserted were all false, and the wall-clock arithmetic
// underneath it counted Saturdays as bars. Comments that lie are part of the bug, so both went.
//
// It executes nothing and moves no money — every trade is written to the journal with mode="paper".

type Engine struct {
	cfg     Config
	store   *Store
	clients *Clients

	// The fake-money book (contract §5). It may be NIL so the service can remain observable, but a
	// missing book blocks every position change: a trade that cannot be scored is not taken.
	ledger    *Ledger
	ledgerErr error

	// One operation owns the engine at a time. A two-phase tick, config replacement and reset must
	// never interleave: N and the enabled set are inputs to sizing, and reset is one experiment
	// boundary rather than an HTTP call racing a fill.
	opMu sync.Mutex

	// This tick's three-store reconciliation, by config key (reconcile.go). Guarded because
	// /paper/status reads it from an HTTP goroutine while the tick writes it.
	syncMu sync.Mutex
	sync   map[string]syncState
}

func newEngine(cfg Config, store *Store, clients *Clients, ledger *Ledger, ledgerErr error) *Engine {
	return &Engine{cfg: cfg, store: store, clients: clients, ledger: ledger, ledgerErr: ledgerErr}
}

// sizing describes, in one line, how the engine sizes a new position — and, when the ledger is
// unavailable, WHY it is using the deprecated constant instead. It is served under /paper/status.
func (e *Engine) sizing() string {
	if e.ledger != nil {
		return "equity/N — equal-weight 1/N of the simulated book, mirroring the evaluator's portfolio (contract §5.2)"
	}
	reason := "the ledger is unavailable"
	if e.ledgerErr != nil {
		reason = "the ledger could not be initialized: " + e.ledgerErr.Error()
	}
	return fmt.Sprintf("BLOCKED: %s. Position changes are refused until the equal-weight simulated "+
		"book is available; fixed POSITION_SIZE fallback is disabled", reason)
}

func (e *Engine) Run(ctx context.Context) {
	log.Printf("paper engine: poll=%s configs=%d sizing=%s (per-bar allocation rule; "+
		"gates: no-synthetic-data, fresh-data, backtest-passed, evaluator-verdict; completed bars only)",
		e.cfg.EvalInterval, len(e.store.Configs()), e.sizing())
	if e.ledger == nil {
		log.Printf("PAPER LEDGER UNAVAILABLE — %v; position changes are BLOCKED. No fixed-size "+
			"fallback is permitted because it would not be comparable with the evaluator.", e.ledgerErr)
	}
	// RECONCILE AT STARTUP, before the first decision. A process that crashed mid-reconciliation
	// comes back with three stores that may disagree, and the first thing it must do is find out —
	// not open a position on top of a book that is already behind (reconcile.go).
	startCtx, startCancel := context.WithTimeout(ctx, 30*time.Second)
	e.opMu.Lock()
	e.retryPendingBookings(time.Now().UTC())
	for key, st := range e.reconcileStores(startCtx) {
		if !st.Consistent {
			log.Printf("PAPER DESYNC at startup for %s: %s", key, st.Detail)
		}
	}
	e.opMu.Unlock()
	startCancel()

	t := time.NewTicker(e.cfg.EvalInterval)
	defer t.Stop()
	e.evalAll(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.evalAll(ctx)
		}
	}
}

func (e *Engine) evalAll(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	e.tick(ctx, time.Now().UTC())
}

// tick is ONE evaluation pass over every config, in TWO PHASES — and the split is what makes the
// day's recorded equity independent of the order `CONFIGS` happens to be written in.
//
// THE BUG THIS REPLACED. `evalAll` used to walk the configs once, marking and then reconciling each
// before moving to the next. The ledger settles a date's snapshot on the LAST mark that completes it
// (`ledger.settleLocked`), so the snapshot was taken after every earlier config had already traded
// that bar and before every later one had. Reorder `CONFIGS` and the same market data produces a
// different equity curve. For a system whose primary output is that curve, that is not a rounding
// difference — it is a measurement that depends on a configuration string.
//
//	PHASE 1 — for every config: fetch its bar, apply the cursor and completeness rules, and submit
//	          its MARK. Nothing trades. The date's snapshot therefore settles at the end of phase 1,
//	          and is always the PRE-TRADE mark of that bar's close (contract §5.3).
//	PHASE 2 — for every config: the decision, the gates and the reconciliation, exactly as before.
//
// The bar is fetched ONCE per config per tick and carried between the phases; nothing else about the
// per-config rules changed. Cursor semantics (a bar is consumed only on a decision), fast-forward,
// gap behaviour for a synthetic or missing mark, and one-decision-per-bar are all preserved because
// they all live in the same code, called in the same order, for one config at a time.
func (e *Engine) tick(ctx context.Context, now time.Time) {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	// PHASE 0 — settle what is owed and find out whether the stores agree, BEFORE anything decides
	// (reconcile.go). A desynced config still marks; it just may not move a position.
	e.retryPendingBookings(now)
	e.reconcileStores(ctx)

	cfgs := e.store.Configs()
	fetched := make([]barFetch, 0, len(cfgs))
	for _, cfg := range cfgs {
		fetched = append(fetched, e.markPhase(ctx, cfg, now))
	}
	// Every mark for this tick is in. Whatever date completed has now been snapshotted at pre-trade
	// closes, so the fills below cannot land inside it.
	for _, f := range fetched {
		if f.decide {
			e.decidePhase(ctx, f, now)
		}
	}
}

// barFetch is one config's phase-1 result, carried into phase 2 so the bar is read once per tick.
type barFetch struct {
	cfg PaperCfg
	st  *ConfigState
	bar *latestBar
	// decide is true when this config reached a NEW, COMPLETED bar and its mark was submitted —
	// i.e. when phase 2 has something to decide. False means "no new bar yet" or "no bar at all",
	// both of which phase 1 has already recorded whatever needs recording for.
	decide bool
}

// evalConfig runs ONE config's whole tick — both phases — for the single-config paths (the tests,
// and any future on-demand evaluation). `tick` does not call it: batching the marks is the point.
func (e *Engine) evalConfig(ctx context.Context, cfg PaperCfg, now time.Time) {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	e.retryPendingBookings(now)
	// Reconcile THIS config specifically rather than the enabled list: evalConfig is also how a
	// config that is not (or not yet) in `CONFIGS` gets evaluated, and an unreconciled config is
	// treated as blocked, so reconciling only the enabled ones would freeze it for the wrong reason.
	journalOpen, journalChecked := e.openTradeIDs(ctx)
	e.setSync(cfg.Key(), e.syncStateFor(cfg, *e.store.StateFor(cfg), journalOpen, journalChecked))
	f := e.markPhase(ctx, cfg, now)
	if f.decide {
		e.decidePhase(ctx, f, now)
	}
}

// markPhase is phase 1: read the bar, apply the cursor + completeness rules, mark the book. It
// NEVER trades and never touches a position.
func (e *Engine) markPhase(ctx context.Context, cfg PaperCfg, now time.Time) barFetch {
	st := e.store.StateFor(cfg)

	history, err := e.clients.bars(ctx, cfg.Ticker, cfg.Timeframe, shadowHistoryLimit)
	if err != nil {
		// No bar means no decision to make — not a decision to make nothing happen. The cursor does
		// not move, so the engine retries on the next tick.
		e.record(st, nil, Decision{
			From: sideOf(st.Side), Target: "unknown", Action: "none", Gate: "data",
			Reason: fmt.Sprintf("latest bar unavailable: %v", err),
		}, now, false)
		return barFetch{cfg: cfg, st: st}
	}
	bar := &history[len(history)-1]

	// Shadow outcomes use completed real bars but do not drive the official engine. Retain the
	// bounded history before applying the latest-bar cursor so a restart can fill an H1/H3/H5/H10
	// gap without pretending several missed bars were one.
	shadowBars := make([]ShadowBar, 0, len(history))
	for i := range history {
		candidate := &history[i]
		if !barComplete(candidate, cfg.Timeframe, now) {
			continue
		}
		shadowBars = append(shadowBars, ShadowBar{
			Ticker: cfg.Ticker, Timeframe: cfg.Timeframe,
			Bar: candidate.Time.Label, BarUnix: candidate.Time.Unix, Close: candidate.Close,
			Source: candidate.Source, Synthetic: candidate.Synthetic,
		})
	}
	if err := e.store.SaveShadowBars(shadowBars, now); err != nil {
		log.Printf("SHADOW EVIDENCE NOT DURABLE for %s: %v", cfg.Key(), err)
	}

	// One decision per bar, and only on a bar that has FINISHED. Fast-forward
	// (PAPER_BAR_SECONDS > 0) is demo/testing only: it counts every tick as a new bar and bypasses
	// the completeness check.
	if !e.cfg.fastForward() {
		if bar.Time.Unix <= st.LastBarUnix {
			return barFetch{cfg: cfg, st: st, bar: bar}
		}
		// A forming bar is not a bar to act on (contract §2). `/candles?limit=1` serves the newest
		// bar the provider has, and during a session that bar's close is not its close. Acting on it
		// decides at a price that has not happened yet and then never re-decides, because the cursor
		// has moved past it. This is "no new bar YET": nothing is recorded, the cursor does not move,
		// and the next tick asks again.
		if !barComplete(bar, cfg.Timeframe, now) {
			return barFetch{cfg: cfg, st: st, bar: bar}
		}
	}

	// MARK THE BOOK (contract §5.3). This runs on every completed bar, before the gates and
	// independently of whether anything trades: the ledger marks positions to bar closes, and a book
	// that only marked itself on days it traded would have an equity curve with holes in it exactly
	// where the market moved. A synthetic or missing close records a GAP, never a substituted price.
	//
	// It is the LAST thing phase 1 does, and phase 2 is the first thing that can move a position, so
	// the snapshot a mark completes is always taken at PRE-TRADE closes — whatever order the configs
	// are in.
	e.markBar(cfg, bar)
	return barFetch{cfg: cfg, st: st, bar: bar, decide: true}
}

// decidePhase is phase 2: the signal, the gates, and the reconciliation of one config's position.
func (e *Engine) decidePhase(ctx context.Context, f barFetch, now time.Time) {
	cfg, st, bar := f.cfg, f.st, f.bar

	// Adopt an orphan before deciding anything, so the reconciliation below sees the position the
	// journal actually holds (see adoptOrphan).
	e.adoptOrphan(ctx, cfg, st)
	if err := e.store.PersistenceError(); err != nil {
		e.record(st, bar, Decision{
			From: sideOf(st.Side), Target: "unknown", Action: "none", Gate: "storage",
			Reason: "paper state is not durable; position changes are blocked: " + err.Error(),
		}, now, false)
		return
	}
	pred, err := e.clients.predict(ctx, cfg)
	if err != nil {
		e.record(st, bar, Decision{
			From: sideOf(st.Side), Target: "unknown", Action: "none", Gate: "signal",
			Reason: fmt.Sprintf("prediction unavailable: %v", err),
		}, now, false)
		return
	}

	target, ok := targetFor(pred)
	if !ok {
		// A null signal is an ABSENCE, not a flat target: the prediction service is declining to
		// speak, which is not the same as telling us to be flat. Nothing moves, and — unlike a
		// transport failure — the bar IS consumed, because re-asking within it gets the same answer.
		reason := pred.Reason
		if reason == "" {
			reason = "no signal served"
		}
		e.record(st, bar, withLineage(Decision{
			From: sideOf(st.Side), Target: "unknown", Action: "none", Gate: "signal",
			Reason: fmt.Sprintf("no validated signal from /predict: %s", reason),
			Gates:  e.gateInputs(cfg, pred, bar, nil, now).gates(),
		}, pred), now, true)
		return
	}

	// EXPERIMENTAL SHADOW CAPTURE. This happens before every official gate and before any journal or
	// ledger reconciliation. The first observation for config+bar wins forever; a retry cannot
	// replace an inconvenient quote. The quote is reused below if the official book needs it.
	q, quoteErr := e.clients.quote(ctx, cfg.Ticker)
	if observation, ok := newShadowObservation(cfg, bar, pred, q, quoteErr, now); ok {
		if err := e.store.SaveShadowObservation(observation); err != nil {
			log.Printf("SHADOW OBSERVATION NOT DURABLE for %s on %s: %v", cfg.Key(), bar.Time.Label, err)
		}
	}

	if sync := e.syncFor(cfg.Key()); !sync.JournalChecked || !sync.LedgerChecked {
		e.record(st, bar, withLineage(Decision{
			From: sideOf(st.Side), Target: target, Action: "none", Gate: "sync",
			Reason: "position records could not all be verified: " + sync.Detail,
		}, pred), now, false)
		return
	}

	// THE BOOK CATCHES UP TO A POSITION IT NEVER BOOKED. Two paths produce one: a legacy position
	// migrated from the pre-ledger state shape, and an orphan just adopted above. Both leave the
	// engine holding a lot the book has no record of — permanently and silently, before
	// reconcile.go, and permanently and LOUDLY after it, since a desynced config may not trade.
	// Neither is a reason to freeze the config forever: the entry is known (price, side, bar, trade
	// id) and the cost the model was validated under is on the response we just fetched, so the
	// honest repair is to book it and say so on the fill.
	e.bookMissingLot(ctx, cfg, st, pred, now)
	if err := e.store.PersistenceError(); err != nil {
		e.record(st, bar, Decision{
			From: sideOf(st.Side), Target: "unknown", Action: "none", Gate: "storage",
			Reason: "ledger catch-up state is not durable; position changes are blocked: " + err.Error(),
		}, now, false)
		return
	}

	current := sideOf(st.Side)
	if target == current {
		e.record(st, bar, withLineage(Decision{
			From: current, Target: target, Action: "hold",
			Reason: fmt.Sprintf("target %s is already held — no position change, no cost", target),
		}, pred), now, true)
		return
	}

	// A DESYNCED CONFIG DOES NOT MOVE (reconcile.go). Everything below this line changes the
	// position, and changing a position whose three records already disagree makes the disagreement
	// permanent: the next fill is sized off a book that is missing one, or closes a lot the engine
	// does not think it holds. It is surfaced exactly like a gate refusal, with the mismatch named.
	//
	// The bar is NOT consumed. A pending booking is retried at the start of every tick, so the
	// stores can agree again within the same bar and the decision still be acted on. A desync that
	// does not resolve simply re-records the same refusal, does nothing, and stays visible.
	if sync := e.syncFor(cfg.Key()); sync.blocked() {
		e.record(st, bar, withLineage(Decision{
			From: current, Target: target, Action: "none", Gate: "sync",
			Reason: "the engine, the ledger and the journal disagree about this config: " + sync.Detail,
		}, pred), now, false)
		log.Printf("PAPER DESYNC %s target=%s: %s", cfg.Key(), target, sync.Detail)
		return
	}

	// Everything below changes the position, so it needs an execution price.
	if quoteErr != nil || q == nil || q.Price == nil || *q.Price <= 0 {
		e.record(st, bar, withLineage(Decision{
			From: current, Target: target, Action: "none", Gate: "quote",
			Reason: fmt.Sprintf("no usable quote to reconcile at (%v)", quoteErr),
		}, pred), now, false)
		return
	}

	// Closing to flat is exempt from the four gates but NOT from execution-price integrity: a close
	// at a synthetic, source-less or stale price records a P&L that never happened. Defer instead —
	// position kept, bar not consumed, retried next tick, visible in /paper/status.
	if target == "flat" {
		if issue := executionQuoteIssue(q, bar, cfg.Timeframe); issue != "" {
			e.record(st, bar, withLineage(Decision{
				From: current, Target: target, Action: "none", Gate: "quote",
				Reason: "close deferred: " + issue,
			}, pred), now, false)
			return
		}
	}

	// Gates apply to OPENING or FLIPPING INTO a position. Closing to flat on an explicit signal is
	// always allowed — a gate must never trap an open position (contract §4).
	if target != "flat" {
		okToTrade, gate, reason, results := e.gateInputs(cfg, pred, bar, q, now).tradeable()
		if !okToTrade {
			e.record(st, bar, withLineage(Decision{
				From: current, Target: target, Action: "none", Gate: gate, Reason: reason, Gates: results,
			}, pred), now, !retryable(results))
			log.Printf("PAPER REFUSED %s target=%s: [%s] %s", cfg.Key(), target, gate, reason)
			return
		}
		// Defence in depth: /predict only emits Sell on a record that backtested shorts, but a short
		// opened against a long-only backtest would be un-validated by construction.
		if target == "short" && !pred.allowShort() {
			e.record(st, bar, withLineage(Decision{
				From: current, Target: target, Action: "none", Gate: "backtest-passed",
				Reason: "a short was signalled but this record never backtested shorting (allowShort=false)",
				Gates:  results,
			}, pred), now, true)
			return
		}
		// The gates cleared the strategy; this clears the FILL. Gate 1 has already rejected a
		// synthetic or source-less quote, so what is left here is the `asOf` check: a price dated
		// before the bar it reconciles is not a fill (contract §3).
		if issue := executionQuoteIssue(q, bar, cfg.Timeframe); issue != "" {
			e.record(st, bar, withLineage(Decision{
				From: current, Target: target, Action: "none", Gate: "quote",
				Reason: issue, Gates: results,
			}, pred), now, false)
			return
		}
		e.reconcile(ctx, cfg, st, bar, pred, q, target, current, now, results)
		return
	}
	e.reconcile(ctx, cfg, st, bar, pred, q, target, current, now, nil)
}

// reconcile moves the position to `target` at the observed quote: close, open, or flip (close then
// open, in that order, at the same reconciliation and the same quote).
func (e *Engine) reconcile(ctx context.Context, cfg PaperCfg, st *ConfigState, bar *latestBar,
	pred *predictResp, q *quoteResp, target, current string, now time.Time, results []gateResult) {

	price := *q.Price
	action := "open"
	switch {
	case target == "flat":
		action = "close"
	case current != "flat":
		action = "flip"
	}

	// Close first when there is something to close (a flip is one close + one open).
	if current != "flat" {
		gone, err := e.closePosition(ctx, cfg, st, bar, price, now, action == "flip")
		if err != nil {
			reason := fmt.Sprintf("could not close the open paper trade: %v — the position is KEPT "+
				"and the close is retried next tick", err)
			if gone {
				reason = fmt.Sprintf("the paper trade is gone from the journal (%v) — dropped the "+
					"stale bookkeeping; the next bar decides from flat", err)
			}
			e.record(st, bar, withLineage(Decision{
				From: current, Target: target, Action: "none", Gate: "journal",
				Reason: reason, Gates: results,
			}, pred), now, false)
			return
		}
	}

	if target == "flat" {
		e.record(st, bar, withLineage(Decision{
			From: current, Target: target, Action: action, Gates: results,
			Reason: fmt.Sprintf("signal went flat — closed the %s position at %.2f", current, price),
		}, pred), now, true)
		return
	}

	if err := e.openPosition(ctx, cfg, st, bar, pred, q, target, now, action == "flip"); err != nil {
		log.Printf("paper open failed for %s: %v", cfg.Key(), err)
		e.record(st, bar, withLineage(Decision{
			From: current, Target: target, Action: map[bool]string{true: "close", false: "none"}[action == "flip"],
			Gate: "journal", Reason: fmt.Sprintf("could not record the new paper trade: %v", err), Gates: results,
		}, pred), now, false)
		return
	}
	e.record(st, bar, withLineage(Decision{
		From: current, Target: target, Action: action, Gates: results,
		Reason: fmt.Sprintf("target %s (from %s) — reconciled at %.2f on bar %s",
			target, current, price, bar.Time.Label),
	}, pred), now, true)
}

// openPosition records a new SIMULATED position in the journal and adopts it into engine state.
func (e *Engine) openPosition(ctx context.Context, cfg PaperCfg, st *ConfigState, bar *latestBar,
	pred *predictResp, q *quoteResp, target string, now time.Time, flip bool) error {

	price := *q.Price
	// SIZING (contract §5.2): equity/N out of the simulated book, so the live book allocates the way
	// the evaluator's 1/N portfolio does. A missing or unusable book is a refusal, never a fallback.
	plan, planErr := e.planSize(cfg, price)
	if planErr != nil {
		return fmt.Errorf("cannot size an evaluator-comparable fill: %w", planErr)
	}
	kind := fillOpen
	if flip {
		kind = fillFlipOpen
	}
	date := now.UTC().Format("2006-01-02")
	trade := map[string]any{
		"ticker": cfg.Ticker,
		"side":   target,
		"mode":   "paper",
		"origin": "signal",
		"entry":  map[string]any{"date": date, "price": price, "size": plan.qty, "timeframe": cfg.Timeframe},
		"thesis": fmt.Sprintf(
			"auto paper (per-bar allocation rule): target %s from signal %s (probUp %.2f, conf %.2f), "+
				"decided on bar %s. Re-evaluated every bar — no fixed holding period.",
			target, pred.Signal.Direction, pred.Signal.ProbUp, pred.Signal.Confidence, bar.Time.Label),
		"tags": []string{"paper", "signal"},
		"attachedSignal": map[string]any{
			"direction": pred.Signal.Direction, "probUp": pred.Signal.ProbUp,
			"confidence":   pred.Signal.Confidence,
			"modelVersion": pred.ModelVersion, "strategyVersion": pred.StrategyVersion,
			// `horizon` is the model's LABEL horizon, not a holding period (contract §1.3).
			"horizon":  cfg.Horizon,
			"backtest": pred.Backtest, "capturedAt": now.UTC().Format(time.RFC3339),
			// Both halves of the live mapping (contract §3.2): the bar the decision was made on, and
			// the quote it was executed at. `costBps` is the cost the backtest was validated under —
			// recorded, NOT applied to live P&L (§3.3).
			"decidedOnBar": bar.Time.Label,
			"quoteSource":  q.Source,
			"quoteAsOf":    q.AsOf,
			"costBps":      pred.costBps(),
			"strategy":     "per-bar allocation (docs/PAPER_EXECUTION_CONTRACT.md v1.1.0)",
			// How the notional was decided, on the trade itself: a size nobody can explain later is
			// a number nobody can reproduce (contract §5.2).
			"sizing":   plan.basis,
			"notional": plan.notional,
			"nAtEntry": plan.n,
			"fillKind": kind,
		},
	}
	created, err := e.clients.journalCreate(ctx, trade)
	if err != nil {
		return err
	}
	st.Side = target
	st.TradeID = created.ID
	st.EntryPrice = price
	st.EntryTime = now.Unix()
	st.EntryBar = bar.Time.Label
	st.EntryQty = plan.qty
	st.EntryNotional = plan.notional
	st.EntryCostBps = pred.costBpsFloat()
	st.EntryN = plan.n
	st.EntryFillKind = kind
	st.EntryModelVersion = pred.ModelVersion
	st.EntryStrategyVersion = pred.StrategyVersion
	st.EntrySynthetic = false // unreachable by construction: gate 1 rejects synthetic inputs
	// PERSIST NOW, not at the end of the tick. `record()` used to be the only writer, so a crash
	// between the journal accepting the trade and the tick finishing left a trade the engine had no
	// memory of — and the next bar opened a SECOND one. The window is one HTTP round trip wide and
	// it is closed here; `adoptOrphan` is the belt for a crash inside this very line.
	if err := e.store.SaveState(st); err != nil {
		return fmt.Errorf("journal accepted trade %s but engine state was not durable: %w", created.ID, err)
	}

	// Persist the exact intended fill BEFORE offering it to the ledger. A crash after the ledger
	// accepts but before this obligation is cleared is safe: (trade id, kind) is idempotent.
	pending := PendingBooking{
		ConfigKey: cfg.Key(), Ticker: cfg.Ticker, Timeframe: cfg.Timeframe, Horizon: cfg.Horizon,
		Kind: kind, Side: target, TradeID: created.ID, Price: price, Bar: bar.Time.Label,
		N: plan.n, CostBps: pred.costBpsFloat(), Qty: plan.qty, Notional: plan.notional,
	}
	if err := e.store.AddPendingBooking(pending); err != nil {
		return fmt.Errorf("journal accepted trade %s but its ledger intent was not durable: %w", created.ID, err)
	}
	f, lerr := e.ledger.openExact(cfg, target, bar.Time.Label, created.ID, price,
		pred.costBpsFloat(), plan.n, plan.qty, plan.notional, now, kind, "")
	switch {
	case lerr == nil:
		log.Printf("PAPER FILL %s %s qty=%.4f @ %.2f notional=%.2f fee=%.2f (%s, N=%d)",
			cfg.Key(), f.Side, f.Qty, f.Price, f.Notional, f.Fee, f.Kind, f.NAtEntry)
		if err := e.store.ResolvePendingBooking(pending.ident()); err != nil {
			log.Printf("PAPER STATE: fill %s is booked but its pending marker could not be cleared: %v",
				pending.ident(), err)
		}
	case errors.Is(lerr, errDuplicateFill):
		log.Printf("PAPER LEDGER: the open for %s trade %s was already booked", cfg.Key(), created.ID)
		if err := e.store.ResolvePendingBooking(pending.ident()); err != nil {
			log.Printf("PAPER STATE: duplicate fill %s could not be cleared: %v", pending.ident(), err)
		}
	default:
		if err := e.owePending(pending, now, lerr); err != nil {
			return err
		}
		log.Printf("PAPER LEDGER: could not book the open for %s: %v — the position IS open; the "+
			"fill is queued for exact retry and GET /paper/status reports this config as desynced",
			cfg.Key(), lerr)
	}
	log.Printf("PAPER OPEN %s %s @ %.2f (bar %s)", cfg.Ticker, target, price, bar.Time.Label)
	return nil
}

// bookMissingLot books an entry the engine holds and the book does not, at the entry the engine
// recorded and the cost the model was validated under.
//
// It is deliberately narrow. It fires ONLY when the (trade id, `open`) fill has never been booked —
// so a lot that was opened, booked and then CLOSED can never be re-opened by it. That case (engine
// still holding, book already square) is a real disagreement nobody can repair automatically, and
// it stays a desync.
//
// The fill carries `note` saying where it came from, because a fill that appears in fills.jsonl at a
// price from days ago needs to explain itself to whoever reads the book later.
func (e *Engine) bookMissingLot(ctx context.Context, cfg PaperCfg, st *ConfigState, _ *predictResp,
	now time.Time) {

	if e.ledger == nil || st == nil || st.Side == "" || st.EntryPrice <= 0 {
		return
	}
	key := cfg.Key()
	kind := st.EntryFillKind
	if kind == "" {
		kind = fillOpen
	}
	if kind != fillOpen && kind != fillFlipOpen {
		log.Printf("PAPER LEDGER CATCH-UP REFUSED %s trade %s: unknown original fill kind %q",
			key, st.TradeID, kind)
		return
	}
	if e.ledger.HasLot(key) || e.ledger.HasBooked(st.TradeID, kind) {
		return
	}
	if st.EntryQty <= 0 || st.EntryNotional <= 0 || st.EntryN < 1 {
		log.Printf("PAPER LEDGER CATCH-UP REFUSED %s trade %s: the original qty/notional/N were not captured; "+
			"reset or repair this legacy position instead of reconstructing it from current equity", key, st.TradeID)
		return
	}
	note := "book catch-up: the engine held this position and the book had no lot for it (a position " +
		"or an orphan adopted after a crash — contract §3.6). Replayed from the exact sizing evidence " +
		"captured on the original journal trade; current equity and current N are not consulted."
	f, err := e.ledger.openExact(cfg, st.Side, st.EntryBar, st.TradeID, st.EntryPrice,
		st.EntryCostBps, st.EntryN, st.EntryQty, st.EntryNotional, now, kind, note)
	if err != nil {
		if persistErr := e.owePending(PendingBooking{
			ConfigKey: key, Ticker: cfg.Ticker, Timeframe: cfg.Timeframe, Horizon: cfg.Horizon,
			Kind: kind, Side: st.Side, TradeID: st.TradeID, Price: st.EntryPrice,
			Bar: st.EntryBar, N: st.EntryN, CostBps: st.EntryCostBps, Qty: st.EntryQty,
			Notional: st.EntryNotional, Note: note,
		}, now, err); persistErr != nil {
			log.Printf("PAPER STATE: could not persist catch-up obligation for %s: %v", key, persistErr)
		}
		return
	}
	log.Printf("PAPER LEDGER CATCH-UP %s: booked the entry for trade %s (%s @ %.2f) the book had no "+
		"lot for; qty=%.4f fee=%.2f", key, st.TradeID, st.Side, st.EntryPrice, f.Qty, f.Fee)
	// The stores just changed. Re-compare THIS config so the bar's decision is judged on what they
	// hold now rather than on the mismatch the repair has already removed.
	if err := e.store.ResolvePendingBooking(st.TradeID + "|" + kind); err != nil {
		log.Printf("PAPER STATE: catch-up fill for %s booked but pending marker could not be cleared: %v", key, err)
	}
	e.compareOne(ctx, cfg)
}

// adoptOrphan takes ownership of an open paper trade the journal holds but this engine has no
// memory of — the residue of a crash between `journalCreate` returning and state being persisted.
//
// Without it the engine opens a second position for the same config and the first is never closed:
// a live paper trade nothing will ever reconcile. Adoption runs BEFORE the decision, so the ordinary
// reconciliation then treats the adopted side as the current position — holding, closing or flipping
// it exactly as if the engine had opened it itself.
//
// It is conservative in both directions. It only ever adopts when this config is flat, only a trade
// no other config's state already owns, and only one it can attribute: matching `attachedSignal.
// horizon` when the trade carries one, and — when it does not (a legacy trade) — only when exactly
// one configured (ticker, timeframe) could have produced it. An unattributable orphan is left alone
// and reported, never guessed at.
func (e *Engine) adoptOrphan(ctx context.Context, cfg PaperCfg, st *ConfigState) bool {
	if !st.Flat() {
		return false
	}
	trades, err := e.clients.journalPaperTrades(ctx, "open")
	if err != nil {
		// Cannot tell. Proceeding may duplicate in the (already narrow) crash window; refusing would
		// stop the engine dead every time this one endpoint blips. Say so and proceed.
		log.Printf("paper: could not list open paper trades for %s (%v) — skipping orphan check",
			cfg.Key(), err)
		return false
	}
	owned := map[string]bool{}
	for _, p := range e.store.OpenPositions() {
		if p.TradeID != "" {
			owned[p.TradeID] = true
		}
	}
	ambiguous := e.configsMatching(cfg.Ticker, cfg.Timeframe) > 1
	for _, t := range trades {
		if t.ID == "" || owned[t.ID] || t.Ticker != cfg.Ticker || t.Entry.Timeframe != cfg.Timeframe {
			continue
		}
		if t.Side != "long" && t.Side != "short" {
			continue
		}
		switch {
		case t.AttachedSignal != nil && t.AttachedSignal.Horizon != nil:
			if *t.AttachedSignal.Horizon != cfg.Horizon {
				continue
			}
		case ambiguous:
			// A legacy trade with no horizon, and more than one config could own it. Guessing would
			// attribute one config's position to another.
			log.Printf("paper: orphan trade %s (%s %s) carries no horizon and %s is ambiguous — left alone",
				t.ID, t.Ticker, t.Entry.Timeframe, cfg.Ticker+":"+cfg.Timeframe)
			continue
		}
		st.Side = t.Side
		st.TradeID = t.ID
		st.EntryPrice = t.Entry.Price
		st.EntryBar = ""
		if t.AttachedSignal != nil {
			st.EntryBar = t.AttachedSignal.DecidedOnBar
			st.EntryModelVersion = t.AttachedSignal.ModelVersion
			st.EntryStrategyVersion = t.AttachedSignal.StrategyVersion
			if t.AttachedSignal.CostBps != nil {
				st.EntryCostBps = *t.AttachedSignal.CostBps
			}
			if t.AttachedSignal.Notional != nil {
				st.EntryNotional = *t.AttachedSignal.Notional
			}
			if t.AttachedSignal.NAtEntry != nil {
				st.EntryN = *t.AttachedSignal.NAtEntry
			}
			st.EntryFillKind = t.AttachedSignal.FillKind
		}
		st.EntryQty = t.Entry.Size
		if err := e.store.SaveState(st); err != nil {
			log.Printf("PAPER ADOPT REFUSED %s trade %s: state was not durable: %v", cfg.Key(), t.ID, err)
			clearEntry(st)
			return false
		}
		log.Printf("PAPER ADOPT %s: took ownership of orphan journal trade %s (%s @ %.2f) — the "+
			"engine had no record of it, so it was opened and then lost before state was persisted",
			cfg.Key(), t.ID, t.Side, t.Entry.Price)
		// The BOOK has not seen this fill either — the crash that orphaned the trade happened before
		// the ledger booked it. `bookMissingLot`, below in decidePhase, is what catches the book up;
		// it runs after /predict, but uses only the original journal sizing evidence.
		return true
	}
	return false
}

// configsMatching counts how many configured configs share a (ticker, timeframe).
func (e *Engine) configsMatching(ticker, timeframe string) int {
	n := 0
	for _, c := range e.store.Configs() {
		if c.Ticker == ticker && c.Timeframe == timeframe {
			n++
		}
	}
	return n
}

// closePosition closes the open paper trade at the observed quote via a journal PATCH.
//
// It returns (gone, err). GONE and UNREACHABLE are not the same failure and must not have the same
// consequence:
//
//   - 404/410 — the trade really is not there (deleted, or an external reset). Our bookkeeping is
//     the thing that is wrong: drop it, and let the next bar decide from flat.
//   - anything else — a timeout, a connection refused, a 500. The trade is still open; the journal
//     is merely unreachable. KEEP the position and retry next tick. Dropping it here orphans a live
//     paper trade on a blip: the engine forgets a position the journal still holds, and nothing ever
//     closes it.
func (e *Engine) closePosition(ctx context.Context, cfg PaperCfg, st *ConfigState, bar *latestBar,
	price float64, now time.Time, flip bool) (bool, error) {

	date := now.UTC().Format("2006-01-02")
	err := e.clients.journalCloseExit(ctx, st.TradeID, date, price)
	if err != nil {
		var se *httpStatusError
		if errors.As(err, &se) && se.gone() {
			log.Printf("paper close for %s: trade %s is gone from the journal (%d) — dropping stale state",
				st.ConfigKey, st.TradeID, se.Status)
			// The BOOK must follow the engine here. If the engine drops a position the ledger still
			// holds, the lot is marked forever against a position nobody will ever close, and every
			// equity number after it is wrong. The exit is booked at the observed quote and the fill
			// carries the reason, so the forced close is visible rather than inferred.
			e.bookClose(cfg, st, bar, price, now, false,
				"forced: the journal trade was gone (deleted or externally reset), so the engine dropped "+
					"the position and the book followed it at the observed quote")
			clearEntry(st)
			if saveErr := e.store.SaveState(st); saveErr != nil {
				return true, fmt.Errorf("%w; stale engine position could not be durably cleared: %v", err, saveErr)
			}
			return true, err
		}
		log.Printf("paper close for %s failed (%v) — the journal is unreachable, KEEPING the position",
			st.ConfigKey, err)
		return false, err
	}
	e.bookClose(cfg, st, bar, price, now, flip, "")
	log.Printf("PAPER CLOSE %s %s exit %.2f (entry %.2f)", st.Ticker, st.Side, price, st.EntryPrice)
	clearEntry(st)
	if err := e.store.SaveState(st); err != nil {
		return false, fmt.Errorf("journal and ledger closed the position but engine state was not durable: %w", err)
	}
	return false, nil
}

func clearEntry(st *ConfigState) {
	st.Side, st.TradeID, st.EntryPrice, st.EntryBar = "", "", 0, ""
	st.EntryTime, st.EntryQty, st.EntryNotional, st.EntryCostBps, st.EntryN = 0, 0, 0, 0, 0
	st.EntryFillKind = ""
	st.EntryModelVersion, st.EntryStrategyVersion = "", ""
}

// bookClose books the exit leg in the ledger, charging the cost the LOT was opened under. Silent
// when there is no ledger or no lot: the ledger is the engine's book, not a precondition for it.
func (e *Engine) bookClose(cfg PaperCfg, st *ConfigState, bar *latestBar, price float64,
	now time.Time, flip bool, note string) {

	if e.ledger == nil || !e.ledger.HasLot(cfg.Key()) {
		return
	}
	kind := fillClose
	if flip {
		kind = fillFlipClose
	}
	barLabel := ""
	if bar != nil {
		barLabel = bar.Time.Label
	}
	lot := e.ledger.LotFor(cfg.Key())
	f, err := e.ledger.close(cfg, barLabel, price, now, kind, note)
	if err != nil {
		if errors.Is(err, errDuplicateFill) {
			log.Printf("PAPER LEDGER: the close for %s was already booked", cfg.Key())
			return
		}
		// Same rule as the open: the exit HAPPENED (the journal took it), so the book owes the fill.
		// Queue it durably rather than losing it, and freeze the config until it is booked.
		owed := PendingBooking{
			ConfigKey: cfg.Key(), Ticker: cfg.Ticker, Timeframe: cfg.Timeframe, Horizon: cfg.Horizon,
			Kind: kind, TradeID: st.TradeID, Price: price, Bar: barLabel, Note: note,
		}
		if lot != nil {
			owed.TradeID, owed.Side, owed.CostBps, owed.N = lot.TradeID, lot.Side, lot.CostBps, lot.NAtEntry
		}
		if persistErr := e.owePending(owed, now, err); persistErr != nil {
			log.Printf("PAPER STATE: could not persist close obligation for %s: %v", cfg.Key(), persistErr)
		}
		log.Printf("PAPER LEDGER: could not book the close for %s: %v — the exit IS recorded; the "+
			"fill is queued for retry and GET /paper/status reports this config as desynced",
			cfg.Key(), err)
		return
	}
	log.Printf("PAPER FILL %s %s qty=%.4f @ %.2f notional=%.2f fee=%.2f realized=%.2f (%s)",
		cfg.Key(), f.Side, f.Qty, f.Price, f.Notional, f.Fee, f.Realized, f.Kind)
}

// sizePlan is one position's notional decision, and the sentence that explains it.
type sizePlan struct {
	notional float64
	qty      float64
	n        int
	basis    string
}

// planSize decides the notional for a new position: `equity/N` from the book. It never falls back
// to a different allocation rule.
//
// It only PLANS — the fill is booked in openPosition, after the journal has accepted the trade, so
// the fill can carry the trade id it belongs to. The plan and the fill compute the same notional
// from the same equity because nothing between them can change it: one config is reconciled at a
// time and the ledger's own lock covers the read.
func (e *Engine) planSize(cfg PaperCfg, price float64) (sizePlan, error) {
	n := len(e.store.Configs())
	if n < 1 {
		n = 1
	}
	if e.ledger == nil {
		return sizePlan{}, fmt.Errorf("no ledger: %v", e.ledgerErr)
	}
	equity := e.ledger.Equity()
	if equity <= 0 {
		return sizePlan{}, fmt.Errorf("the book's equity is %.2f — nothing to allocate", equity)
	}
	notional := equity / float64(n)
	return sizePlan{
		notional: notional, qty: notional / price, n: n,
		basis: fmt.Sprintf("equity/N: $%.2f of a $%.2f book across %d enabled configs (contract §5.2)",
			notional, equity, n),
	}, nil
}

// markBar records one config's close for the ledger's daily mark (contract §5.3). A bar with no
// close, a synthetic bar, or a bar of unknown provenance is NOT a mark: it produces a gap for that
// date, which the payload reports, rather than a substituted price nobody could reproduce.
func (e *Engine) markBar(cfg PaperCfg, bar *latestBar) {
	if e.ledger == nil || bar == nil {
		return
	}
	date, ok := barDateOf(bar)
	if !ok {
		return
	}
	real := !bar.Synthetic && strings.TrimSpace(bar.Source) != "" && bar.Close > 0
	keys := make([]string, 0, len(e.store.Configs()))
	for _, c := range e.store.Configs() {
		keys = append(keys, c.Key())
	}
	if err := e.ledger.Mark(keys, cfg.Key(), date.UTC().Format("2006-01-02"), bar.Close, real); err != nil {
		log.Printf("PAPER LEDGER MARK NOT DURABLE for %s on %s: %v", cfg.Key(), date.Format("2006-01-02"), err)
	}
}

// targetFor maps the validated signal to a target position. Returns ok=false when there is no
// signal at all — an absence, which is not a flat target (contract §3).
func targetFor(pred *predictResp) (string, bool) {
	if pred == nil || pred.Signal == nil {
		return "", false
	}
	switch pred.Signal.Direction {
	case "Buy":
		return "long", true
	case "Sell":
		return "short", true
	case "Hold":
		return "flat", true
	default:
		return "", false
	}
}

func (e *Engine) gateInputs(cfg PaperCfg, pred *predictResp, bar *latestBar, q *quoteResp, now time.Time) gateInputs {
	return gateInputs{
		cfg: cfg, pred: pred, bar: bar, quote: q, now: now,
		maxBarAgeSessions:   e.cfg.MaxBarAgeSessions,
		maxModelAgeSessions: e.cfg.MaxModelAgeSessions,
	}
}

func withLineage(d Decision, pred *predictResp) Decision {
	if pred != nil {
		d.ModelVersion = pred.ModelVersion
		d.StrategyVersion = pred.StrategyVersion
	}
	return d
}

// record persists the decision and, when the bar was actually decided, advances the bar cursor.
//
// `advance` is the idempotency hinge (contract §3.1). It is TRUE when a decision was reached —
// including a refusal, which is a decision — and FALSE when an upstream was simply unavailable, so
// the engine retries within the same bar rather than skipping it.
func (e *Engine) record(st *ConfigState, bar *latestBar, d Decision, now time.Time, advance bool) {
	d.At = now.UTC().Format(time.RFC3339)
	if bar != nil {
		d.Bar, d.BarUnix = bar.Time.Label, bar.Time.Unix
		if advance {
			st.LastBarActedOn, st.LastBarUnix = bar.Time.Label, bar.Time.Unix
		}
	}
	st.LastDecision = &d
	var err error
	if advance && bar != nil {
		generation := int64(0)
		if e.ledger != nil {
			generation = e.ledger.generation
		}
		err = e.store.SaveDecision(st, DecisionEvent{
			Generation: generation,
			Config:     st.ConfigKey,
			Ticker:     st.Ticker,
			Timeframe:  st.Timeframe,
			Horizon:    st.Horizon,
			Decision:   d,
		})
	} else {
		err = e.store.SaveState(st)
	}
	if err != nil {
		log.Printf("PAPER STATE NOT DURABLE for %s: %v", st.ConfigKey, err)
	}
}
