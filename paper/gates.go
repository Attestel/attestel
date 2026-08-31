package main

import (
	"fmt"
	"strings"
	"time"
)

// gates.go — the hard preconditions on CHANGING a position (docs/PAPER_EXECUTION_CONTRACT.md §4).
//
// Every one of these must hold before the engine may OPEN or FLIP INTO a position. Closing an
// existing position to flat on an explicit signal is never gated: a gate must not be able to trap a
// position that is already open.
//
// The bar this replaces: the engine traded on `report.passed` alone, and treated synthetic data as
// something to LABEL rather than something to refuse. Both are wrong in the same way — they let a
// number that cannot mean anything reach a book that is supposed to be validating whether numbers
// mean anything. The gate remains necessary even after production records have been retrained.
//
// FAIL-CLOSED EVERYWHERE. A missing field, an unparseable date, an absent verdict and an unreachable
// service all REFUSE. There is no bypass flag and there must never be one: the flag would be used.

// gateResult is one gate's verdict, carried into the status payload so a refusal is legible without
// reading logs.
type gateResult struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
	// Retryable marks a refusal that could resolve WITHIN the same bar (a synthetic quote, a
	// provider blip). Those do not consume the bar — the engine tries again on the next tick.
	// Everything else is settled for this bar: re-asking would return the same answer.
	Retryable bool `json:"retryable,omitempty"`
}

// gateInputs bundles everything the gates judge. `quote` may be nil when the gates are evaluated for
// DISPLAY only (no action was due, so no quote was fetched); the synthetic gate then says so rather
// than pretending it checked. The engine never trades on a nil-quote evaluation.
type gateInputs struct {
	cfg                 PaperCfg
	pred                *predictResp
	bar                 *latestBar
	quote               *quoteResp
	now                 time.Time
	maxBarAgeSessions   int
	maxModelAgeSessions int
}

// gates evaluates all four gates, in a fixed order: data integrity first (a synthetic or stale input
// makes every later question meaningless), then model quality, then the pooled evaluator verdict.
func (g gateInputs) gates() []gateResult {
	return []gateResult{
		g.noSyntheticData(),
		g.freshData(),
		g.backtestPassed(),
		g.evaluatorVerdict(),
	}
}

// tradeable reports whether a position may be opened or flipped into, and — when it may not — the
// FIRST failing gate's reason, which is what the status payload shows and what the engine logs.
//
// It replaces the old `predictResp.passed()`, which asked one question (`report.passed`) and treated
// the answer as sufficient. It is necessary; it is gate 3 of 4.
func (g gateInputs) tradeable() (bool, string, string, []gateResult) {
	results := g.gates()
	for _, r := range results {
		if !r.OK {
			return false, r.Name, r.Detail, results
		}
	}
	return true, "", "", results
}

// retryableRefusal reports whether a refusal should leave the bar un-consumed.
func retryable(results []gateResult) bool {
	for _, r := range results {
		if !r.OK {
			return r.Retryable
		}
	}
	return false
}

// --- gate 1: no synthetic anywhere -------------------------------------------------------------

func (g gateInputs) noSyntheticData() gateResult {
	const name = "no-synthetic-data"
	if g.pred == nil {
		return gateResult{Name: name, Detail: "no prediction response to judge"}
	}
	if g.pred.TrainedOnSynthetic {
		return gateResult{Name: name, Detail: "the model record was trained on synthetic data " +
			"(trainedOnSynthetic=true) — a backtest of a model fitted on invented prices is not evidence"}
	}
	// The frame /predict actually scored. null means "not fetched", which is unknown, not clean.
	if g.pred.CurrentData == nil {
		return gateResult{Name: name, Detail: "the current feature frame's provenance is unknown " +
			"(/predict served currentData=null)"}
	}
	if g.pred.CurrentData.Synthetic {
		return gateResult{Name: name, Detail: fmt.Sprintf(
			"the current feature frame is synthetic (source %q)", g.pred.CurrentData.Source)}
	}
	if g.bar == nil {
		return gateResult{Name: name, Detail: "no bar was read, so its provenance is unknown"}
	}
	if g.bar.Synthetic {
		return gateResult{Name: name, Detail: fmt.Sprintf(
			"the latest bar is synthetic (analysis source %q)", g.bar.Source), Retryable: true}
	}
	// An EMPTY source is not a clean one. Only the literal string "synthetic" used to refuse, so a
	// provider that named itself with nothing at all sailed through a gate whose whole claim is that
	// it fails closed. Unknown provenance is unknown, and unknown refuses.
	if strings.TrimSpace(g.bar.Source) == "" {
		return gateResult{Name: name, Retryable: true, Detail: "the latest bar carries no source — " +
			"its provenance is unknown, and unknown is not clean"}
	}
	if g.quote == nil {
		// Display-only evaluation. Say what was NOT checked rather than imply it passed.
		return gateResult{Name: name, OK: true,
			Detail: "model, feature frame and bar are all real (the quote was not checked — no action was due)"}
	}
	if g.quote.Source == "synthetic" {
		return gateResult{Name: name, Retryable: true, Detail: fmt.Sprintf(
			"the execution quote is synthetic (source %q) — a fill at an invented price is not a fill",
			g.quote.Source)}
	}
	if strings.TrimSpace(g.quote.Source) == "" {
		return gateResult{Name: name, Retryable: true, Detail: "the execution quote carries no " +
			"source — a fill at a price of unknown provenance is not a fill"}
	}
	return gateResult{Name: name, OK: true, Detail: "model, feature frame, bar and quote are all real"}
}

// --- gate 2: fresh data ------------------------------------------------------------------------

func (g gateInputs) freshData() gateResult {
	const name = "fresh-data"
	if g.bar == nil {
		return gateResult{Name: name, Detail: "no latest bar was read"}
	}
	barDate, ok := parseDate(g.bar.Time.Label)
	if !ok {
		barDate = time.Unix(g.bar.Time.Unix, 0).UTC()
	}
	if age := sessionsBehind(barDate, g.now); age > g.maxBarAgeSessions {
		return gateResult{Name: name, Detail: fmt.Sprintf(
			"the latest bar %s is ~%d sessions behind today (max %d) — the market has moved on without us",
			g.bar.Time.Label, age, g.maxBarAgeSessions)}
	}
	if g.pred == nil || g.pred.DataThrough == "" {
		return gateResult{Name: name, Detail: "the model record has no dataThrough, so its age is unknown"}
	}
	through, ok := parseDate(g.pred.DataThrough)
	if !ok {
		return gateResult{Name: name, Detail: fmt.Sprintf(
			"the model record's dataThrough %q is not a date this engine can read", g.pred.DataThrough)}
	}
	if age := sessionsBehind(through, barDate); age > g.maxModelAgeSessions {
		return gateResult{Name: name, Detail: fmt.Sprintf(
			"the model was trained through %s, ~%d sessions behind the latest bar %s (max %d) — retrain before trading it",
			g.pred.DataThrough, age, g.bar.Time.Label, g.maxModelAgeSessions)}
	}
	return gateResult{Name: name, OK: true, Detail: fmt.Sprintf(
		"latest bar %s, model trained through %s", g.bar.Time.Label, g.pred.DataThrough)}
}

// --- gate 3: the model's own walk-forward backtest ----------------------------------------------

func (g gateInputs) backtestPassed() gateResult {
	const name = "backtest-passed"
	if g.pred == nil || g.pred.Backtest == nil {
		return gateResult{Name: name, Detail: fmt.Sprintf(
			"no backtest report served for %s (the model is not trained)", g.cfg.Key())}
	}
	if passed, _ := g.pred.Backtest["passed"].(bool); !passed {
		return gateResult{Name: name, Detail: fmt.Sprintf(
			"the walk-forward backtest for %s has not passed (report.passed=false)", g.cfg.Key())}
	}
	return gateResult{Name: name, OK: true, Detail: "the model's walk-forward backtest passes " +
		"(necessary, not sufficient — see gate evaluator-verdict)"}
}

// --- gate 4: the offline evaluator's pooled verdict ---------------------------------------------

func (g gateInputs) evaluatorVerdict() gateResult {
	const name = "evaluator-verdict"
	if g.pred == nil || g.pred.Evaluation == nil {
		return gateResult{Name: name, Detail: fmt.Sprintf(
			"no persisted evaluator verdict covers %s — run `python -m app.evaluate` against real data "+
				"(an operator action; a verdict must never be written by hand)", g.cfg.Key())}
	}
	ev := g.pred.Evaluation
	if ev.Verdict != verdictEdge {
		return gateResult{Name: name, Detail: fmt.Sprintf(
			"the evaluator's pooled verdict for %s is %q, not %q", g.cfg.Key(), ev.Verdict, verdictEdge)}
	}
	if !ev.EvidenceCurrent {
		detail := "the verdict does not carry sample evidence that meets the hard evaluator floors"
		if len(ev.EvidenceIssues) > 0 {
			detail += ": " + strings.Join(ev.EvidenceIssues, "; ")
		}
		return gateResult{Name: name, Detail: detail + " — re-evaluate; sufficiency overrides cannot weaken live policy"}
	}
	if !ev.Current {
		return gateResult{Name: name, Detail: fmt.Sprintf(
			"the %s verdict was made under strategy version %q / method %q, which is no longer the one this service runs "+
				"— re-evaluate before trading it", verdictEdge, ev.StrategyVersion, ev.Method)}
	}
	return gateResult{Name: name, OK: true, Detail: fmt.Sprintf(
		"pooled verdict %s (%s), current strategy version", ev.Verdict, ev.EvaluatedAt)}
}

// verdictEdge is the ONLY verdict that permits a position change. INCONCLUSIVE and a missing verdict
// both mean "we have not shown there is an edge", which is not a licence to trade.
const verdictEdge = "EDGE"

// --- execution-price integrity (contract §3, §4.1) ----------------------------------------------
//
// NOT a gate, and deliberately separate from the four. The four gates decide whether the STRATEGY
// may take a position; this decides whether a particular QUOTE may stand as the fill that
// reconciles a particular bar. The difference matters at the close: closing to flat is exempt from
// the gates — a gate must never trap an open position — but it is NOT exempt from this. A close at
// an invented or stale price records a P&L that never happened, and a fabricated exit is worse for
// the book than a deferred one.
//
// A deferral can persist indefinitely offline. That is correct: the position stays open, the
// deferral is visible in /paper/status, and no number is invented in the meantime.
//
// TIMING PRECISION IS PER TIMEFRAME. The rule used to be date-level for every timeframe, which is
// right for 1D and wrong for everything else: on a 15m frame a quote stamped 09:35 is "the same
// calendar day" as a bar that closed at 15:45, so a fill six hours BEFORE the decision passed the
// check. For an intraday timeframe the quote's instant must therefore be at or after the decided
// bar's END (`start + barDuration`) — the moment the close it reconciles actually existed. 1D keeps
// the date-level rule, because a daily bar has no wall-clock end this service can compute (§2.1
// judges the session over by date, and so does this).
func executionQuoteIssue(q *quoteResp, bar *latestBar, timeframe string) string {
	if q == nil || q.Price == nil || *q.Price <= 0 {
		return "no usable quote price"
	}
	src := strings.TrimSpace(q.Source)
	switch {
	case src == "":
		return "the quote carries no source — a fill at a price of unknown provenance is not a fill"
	case src == "synthetic":
		return "the quote is synthetic — a fill at an invented price is not a fill"
	}
	asOf := strings.TrimSpace(q.AsOf)
	if asOf == "" {
		return "the quote carries no asOf — a fill of unknown age cannot be checked against the bar it reconciles"
	}
	at, ok := parseDate(asOf)
	if !ok {
		return fmt.Sprintf("the quote's asOf %q is not a timestamp this engine can read", asOf)
	}
	if d := barDuration(timeframe); d > 0 {
		// Intraday: the bar ENDS at start+d, and a quote from before that instant is a price from
		// inside (or before) the session being reconciled, not the price that reconciles it.
		if bar == nil || bar.Time.Unix <= 0 {
			return "the bar carries no instant, so an intraday quote's age cannot be checked against it"
		}
		end := time.Unix(bar.Time.Unix, 0).UTC().Add(d)
		if at.Before(end) {
			return fmt.Sprintf(
				"the quote is stamped %s, BEFORE the %s bar %s ends at %s — an intraday fill from "+
					"inside the bar it reconciles is not a fill",
				asOf, timeframe, bar.Time.Label, end.Format(time.RFC3339))
		}
		return ""
	}
	if barDate, ok := barDateOf(bar); ok && day(at).Before(day(barDate)) {
		return fmt.Sprintf(
			"the quote is dated %s, BEFORE the bar %s it would reconcile — a fill older than the "+
				"decision it executes is not a fill", asOf, bar.Time.Label)
	}
	return ""
}
