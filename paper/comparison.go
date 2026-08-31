package main

import (
	"fmt"
	"math"
	"strconv"
)

// itoa/ftoa keep the note strings readable without dragging fmt.Sprintf through every branch.
func itoa(n int) string { return strconv.Itoa(n) }

func ftoa(v float64) string { return strconv.FormatFloat(v, 'f', 3, 64) }

// comparison.go — the whole point of paper trading: put live out-of-sample paper results NEXT TO
// the model's backtest, honestly. A validation is only meaningful with enough closed trades (~30);
// below that we say so. Divergence (live materially worse than backtest on a sufficient sample) is
// flagged as possible edge decay.
//
// UNITS — WHAT CHANGED, AND WHAT DID NOT (docs/PAPER_EXECUTION_CONTRACT.md §5.4).
//
// This file used to be able to do one thing only: set two mismatched columns side by side under a
// disclaimer. Live P&L was the journal's raw entry-to-exit move on a fixed $10,000 notional, gross
// of fees, and live Sharpe was per-TRADE and un-annualized against a per-BAR annualized backtest
// Sharpe. Nothing was wrong; they were answers to different questions, and no honest comparison
// could be made from them.
//
// The LEDGER (contract §5) closed that gap for the statistical half. It keeps ONE book, allocates
// equal-weight 1/N exactly as `evaluate.portfolio_returns` does, charges the SAME cost_bps the model
// was validated under on every fill, and marks to bar closes once per date — so its daily return
// series is the same KIND of series the evaluator judges. Three pairs are now genuinely
// like-for-like:
//
//	live daily portfolio Sharpe   <->  evaluator portfolio Sharpe (both annualized, per DATE)
//	live mean daily net return    <->  backtest expectancy        (both net of costs, per BAR)
//	live max drawdown on equity   <->  backtest max drawdown      (both on an equity curve)
//
// What did NOT change, and is still stated rather than smoothed over: the PER-TRADE counting stats.
// Hit rate, closed-episode counts and per-episode expectancy come from the journal's raw
// entry-to-exit move, are GROSS of fees, and count position episodes where the backtest's hit rate
// counts bars. Those disclaimers survive because they are still true. Only the ones this change
// made false were removed.

const minMeaningful = 30

// comparisonUnits is served alongside the comparisons so a consumer never has to infer which
// number is in which unit.
var comparisonUnits = map[string]string{
	// --- COUNTING stats: per DECISION. Still not like-for-like, and still labelled. --------------
	"live.expectancy":             "mean % move per closed position episode, GROSS of fees/slippage (a counting stat, not the ledger's)",
	"live.sharpePerTradeIfEnough": "per-trade, UN-annualized (mean/stdev of episode returns) — a counting stat",
	"live.hitRate":                "share of closed episodes with pnlPct > 0",
	"backtest.hitRate":            "share of BARS whose held position matched the next bar's sign",
	"note": "the PER-TRADE columns are counting stats in different denominators (episodes vs bars) and " +
		"live is gross of fees — compare them as counts, not as returns. The PORTFOLIO block is the " +
		"like-for-like comparison.",

	// --- PORTFOLIO stats: per DATE, net of costs, on an equity curve. These ARE comparable. -------
	"portfolio.live.dailySharpeAnnualized": "annualized (x sqrt(252)) Sharpe of the simulated book's DAILY portfolio returns — null below 20 snapshot days",
	"portfolio.live.meanDailyReturn":       "mean NET daily portfolio return of the 1/N book, after fees on every fill",
	"portfolio.live.maxDrawdown":           "largest peak-to-trough decline of the book's daily equity curve",
	"portfolio.reference.sharpe":           "the evaluator's date-aligned 1/N portfolio Sharpe when served, else the model backtest's per-BAR annualized Sharpe (the `source` field says which)",
	"backtest.expectancy":                  "mean NET return per BAR, after cost_bps on every position change — comparable with portfolio.live.meanDailyReturn for a 1D config",
	"backtest.sharpe":                      "per-bar Sharpe, ANNUALIZED (x sqrt(252) for 1D)",
}

type liveStats struct {
	NClosed      int      `json:"nClosed"`
	HitRate      *float64 `json:"hitRate"`
	AvgReturnPct *float64 `json:"avgReturnPct"`
	Expectancy   *float64 `json:"expectancy"` // per closed EPISODE, %, gross of costs
	// Per-TRADE and UN-ANNUALIZED — deliberately not called "sharpe", because the backtest's field
	// of that name is a different statistic in different units.
	SharpePerTrade *float64 `json:"sharpePerTradeIfEnough"`
}

type backtestStats struct {
	HitRate    *float64 `json:"hitRate"`
	Sharpe     *float64 `json:"sharpe"`     // per-bar, ANNUALIZED
	Expectancy *float64 `json:"expectancy"` // mean NET return per BAR
	NumTrades  int      `json:"numTrades"`
	Passed     bool     `json:"passed"`
}

// livePortfolioStats is the SIMULATED BOOK's statistics — one book, shared by every config, taken
// from the daily snapshot series (contract §5.4). It is repeated on each comparison row because the
// reference it is being compared against is per-config; the numbers themselves are the book's.
type livePortfolioStats struct {
	NSnapshots  int      `json:"nSnapshots"`
	NReturns    int      `json:"nDailyReturns"`
	DailySharpe *float64 `json:"dailySharpeAnnualized"` // null below minSnapshotsForSharpe
	MeanDaily   *float64 `json:"meanDailyReturn"`
	MaxDrawdown *float64 `json:"maxDrawdown"`
	TotalReturn *float64 `json:"totalReturn"`
	Equity      float64  `json:"equity"`
	SharpeNote  string   `json:"sharpeNote"`
}

// portfolioReference is what the live book's daily Sharpe is being held up against, and WHERE that
// number came from. The source is served rather than assumed: an evaluator portfolio Sharpe and a
// model-backtest per-bar Sharpe are not the same claim, and a reader must be able to tell which one
// they are looking at without reading this file.
type portfolioReference struct {
	Source     string   `json:"source"` // "evaluator-portfolio" | "model-backtest" | "none"
	Sharpe     *float64 `json:"sharpe"`
	Expectancy *float64 `json:"expectancy"`
	Annualized bool     `json:"annualized"`
	Caveat     string   `json:"caveat"`
}

// portfolioComparison is the like-for-like half of the payload.
type portfolioComparison struct {
	Live       *livePortfolioStats `json:"live"`
	Reference  *portfolioReference `json:"reference"`
	Comparable bool                `json:"comparable"`
	Note       string              `json:"note"`
}

type comparison struct {
	Config             string         `json:"config"`
	Ticker             string         `json:"ticker"`
	Timeframe          string         `json:"timeframe"`
	Horizon            int            `json:"horizon"`
	ModelVersion       string         `json:"modelVersion"`
	StrategyVersion    string         `json:"strategyVersion"`
	Live               liveStats      `json:"live"`
	Backtest           *backtestStats `json:"backtest"`
	TrainedOnSynthetic bool           `json:"trainedOnSynthetic"`
	NOpen              int            `json:"nOpen"`
	Divergence         bool           `json:"divergence"`
	Meaningful         bool           `json:"meaningful"`
	Note               string         `json:"note"`
	// The like-for-like comparison (contract §5.4): the simulated book's daily portfolio statistics
	// against the evaluator's portfolio Sharpe when it is served, and against the model backtest's
	// annualized Sharpe — caveat intact — when it is not.
	Portfolio *portfolioComparison `json:"portfolio"`
	// What the engine last did for this config, and — when it did nothing — which gate refused.
	LastDecision *Decision `json:"lastDecision"`
}

// livePortfolioFrom converts the ledger's metrics into the comparison's live column. Nil when there
// is no book: an absent book must read as absent, never as a book with zeroes in it.
func livePortfolioFrom(l *Ledger) *livePortfolioStats {
	if l == nil {
		return nil
	}
	m := l.Metrics()
	return &livePortfolioStats{
		NSnapshots: m.NSnapshots, NReturns: m.NReturns, DailySharpe: m.DailySharpe,
		MeanDaily: m.MeanDaily, MaxDrawdown: m.MaxDrawdown, TotalReturn: m.TotalReturn,
		Equity: round2(l.Equity()), SharpeNote: m.SharpeNote,
	}
}

// referenceFor picks the number the live book is compared against, preferring the evaluator's own
// portfolio Sharpe and falling back to the model's backtest — never silently.
func referenceFor(pred *predictResp, bt *backtestStats) *portfolioReference {
	if pred != nil && pred.Evaluation != nil && pred.Evaluation.PortfolioSharpe != nil {
		return &portfolioReference{
			Source: "evaluator-portfolio", Sharpe: pred.Evaluation.PortfolioSharpe, Annualized: true,
			Caveat: "The evaluator's DATE-ALIGNED 1/N portfolio Sharpe. Same statistic, same kind of " +
				"series as the live book's — this is a like-for-like comparison. It is still a POOLED " +
				"universe number, so it says what the strategy did across ~30 names, not what it did here.",
		}
	}
	if bt == nil || bt.Sharpe == nil {
		return &portfolioReference{
			Source: "none",
			Caveat: "No reference Sharpe is available: no evaluator portfolio statistic is served and " +
				"this config has no backtest report.",
		}
	}
	return &portfolioReference{
		Source: "model-backtest", Sharpe: bt.Sharpe, Expectancy: bt.Expectancy, Annualized: true,
		Caveat: "FALLBACK, and the unit caveat stands: this is the model's own per-BAR Sharpe for one " +
			"ticker, annualized. The live number is a per-DATE portfolio Sharpe across every enabled " +
			"config. Both are annualized and both are net of costs, so they are the same ORDER of " +
			"quantity — but a single-ticker per-bar series and a 1/N portfolio series are not the same " +
			"series, and the evaluator's portfolio Sharpe (not served here yet) is the number this " +
			"should be measured against.",
	}
}

// buildPortfolio assembles the like-for-like block, and says plainly when it cannot yet be read.
func buildPortfolio(live *livePortfolioStats, ref *portfolioReference) *portfolioComparison {
	pc := &portfolioComparison{Live: live, Reference: ref}
	switch {
	case live == nil:
		pc.Note = "The fake-money ledger is not running, so there is no live portfolio series to compare. " +
			"Only the per-trade counting stats above are available, and they are not like-for-like."
	case live.DailySharpe == nil:
		pc.Note = "The book has " + itoa(live.NSnapshots) + " daily snapshots. " + live.SharpeNote +
			" Until it has enough, there is no live Sharpe to compare — and a small-sample one would be " +
			"worse than none."
	case ref == nil || ref.Sharpe == nil:
		pc.Note = "The live book has a daily Sharpe but there is nothing validated to compare it against."
	default:
		pc.Comparable = ref.Source == "evaluator-portfolio"
		pc.Note = "Live daily portfolio Sharpe " + ftoa(*live.DailySharpe) + " vs " + ref.Source +
			" Sharpe " + ftoa(*ref.Sharpe) + ". " + ref.Caveat
	}
	return pc
}

// computeLive summarizes closed paper trades (per-trade % returns) for one config.
func computeLive(closed []journalTrade) liveStats {
	ls := liveStats{NClosed: len(closed)}
	if len(closed) == 0 {
		return ls
	}
	var rets []float64
	wins := 0
	for _, t := range closed {
		if t.PnlPct == nil {
			continue
		}
		rets = append(rets, *t.PnlPct)
		if *t.PnlPct > 0 {
			wins++
		}
	}
	if len(rets) == 0 {
		return ls
	}
	n := float64(len(rets))
	hit := float64(wins) / n
	mean := 0.0
	for _, r := range rets {
		mean += r
	}
	mean /= n
	ls.HitRate = &hit
	ls.AvgReturnPct = &mean
	ls.Expectancy = &mean
	if len(rets) >= minMeaningful {
		var ss float64
		for _, r := range rets {
			ss += (r - mean) * (r - mean)
		}
		std := math.Sqrt(ss / n)
		if std > 1e-12 {
			sh := mean / std // per-trade, un-annualized — see comparisonUnits
			ls.SharpePerTrade = &sh
		}
	}
	return ls
}

func fnum(m map[string]any, k string) *float64 {
	if m == nil {
		return nil
	}
	if v, ok := m[k].(float64); ok {
		return &v
	}
	return nil
}

func inum(m map[string]any, k string) int {
	if m == nil {
		return 0
	}
	if v, ok := m[k].(float64); ok {
		return int(v)
	}
	return 0
}

func bflag(m map[string]any, k string) bool {
	if m == nil {
		return false
	}
	v, _ := m[k].(bool)
	return v
}

// buildComparison assembles one config's live-vs-backtest picture and the honest note.
func buildComparison(cfg PaperCfg, closed []journalTrade, pred *predictResp, nOpen int,
	last *Decision, live *livePortfolioStats) comparison {
	c := comparison{
		Config: cfg.Key(), Ticker: cfg.Ticker, Timeframe: cfg.Timeframe, Horizon: cfg.Horizon,
		Live: computeLive(closed), NOpen: nOpen, LastDecision: last,
	}
	var bt *backtestStats
	if pred != nil {
		c.ModelVersion = pred.ModelVersion
		c.StrategyVersion = pred.StrategyVersion
		c.TrainedOnSynthetic = pred.TrainedOnSynthetic
		if pred.Backtest != nil {
			bt = &backtestStats{
				HitRate: fnum(pred.Backtest, "hitRate"), Sharpe: fnum(pred.Backtest, "sharpe"),
				Expectancy: fnum(pred.Backtest, "expectancy"), NumTrades: inum(pred.Backtest, "numTrades"),
				Passed: bflag(pred.Backtest, "passed"),
			}
		}
	}
	c.Backtest = bt
	c.Meaningful = c.Live.NClosed >= minMeaningful
	c.Portfolio = buildPortfolio(live, referenceFor(pred, bt))

	// Divergence: only on a sufficient sample, when live materially underperforms the backtest.
	// Both tests are unit-safe. `hitWorse` compares two shares (different denominators, same scale);
	// `edgeGone` compares only the SIGNS of the two expectancies — "live is losing where the
	// backtest made money" — and never their magnitudes, which are in different units.
	if c.Meaningful && bt != nil {
		hitWorse := c.Live.HitRate != nil && bt.HitRate != nil && *c.Live.HitRate < *bt.HitRate-0.10
		edgeGone := c.Live.AvgReturnPct != nil && *c.Live.AvgReturnPct <= 0 && bt.Expectancy != nil && *bt.Expectancy > 0
		c.Divergence = hitWorse || edgeGone
	}
	// The PORTFOLIO divergence test is a SIGN test too, and deliberately so. Now that both Sharpes
	// are annualized per-date statistics they could be differenced — but "how much worse is worse
	// enough" is a threshold nobody has validated, and inventing one here would be a judgement
	// dressed as a measurement. "The live book is losing risk-adjusted where the validated strategy
	// made money" needs no threshold.
	if c.Portfolio != nil && c.Portfolio.Live != nil && c.Portfolio.Reference != nil {
		lv, rf := c.Portfolio.Live.DailySharpe, c.Portfolio.Reference.Sharpe
		if lv != nil && rf != nil && *lv <= 0 && *rf > 0 {
			c.Divergence = true
		}
	}

	// The note must never imply the two columns are like-for-like — they are not (see
	// comparisonUnits). The refusal reason, when there is one, is the most useful thing to say.
	refused := ""
	if last != nil && last.Action == "none" && last.Gate != "" {
		refused = fmt.Sprintf("Nothing is being traded for this config: [%s] %s", last.Gate, last.Reason)
	}
	switch {
	case bt == nil:
		c.Note = "No backtest available (prediction service down or model not trained) — cannot validate yet."
	case refused != "":
		c.Note = refused
	case !bt.Passed:
		c.Note = "No passing backtest for this config — no position is opened, so there is nothing to validate."
	case !c.Meaningful:
		c.Note = fmt.Sprintf("Not yet meaningful — need ~%d closed paper trades to judge (have %d). "+
			"The `portfolio` block is the like-for-like comparison once the book has enough daily "+
			"snapshots; the per-trade columns above are counting stats and live is gross of fees.",
			minMeaningful, c.Live.NClosed)
	case c.Divergence:
		c.Note = "Live paper underperforms the backtest on a sufficient sample — possible edge decay. " +
			"Treat the signal with caution. The per-trade columns are compared on hit rate and the SIGN " +
			"of expectancy only; the risk-adjusted comparison is in the `portfolio` block, where both " +
			"sides are annualized per-date and net of costs."
	default:
		c.Note = "Live paper hit rate roughly tracks the backtest's so far. The like-for-like " +
			"risk-adjusted comparison is in the `portfolio` block; the per-trade columns above remain " +
			"counting stats in different denominators."
	}
	return c
}
