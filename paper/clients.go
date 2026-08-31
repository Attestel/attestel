package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// clients.go — thin HTTP clients for the upstream services. The paper engine READS the validated
// signal + a fresh quote, and RECORDS simulated trades in the journal. It executes nothing and
// touches no broker.

// ---- prediction /predict ----

type predictResp struct {
	Ticker          string `json:"ticker"`
	Timeframe       string `json:"timeframe"`
	Horizon         int    `json:"horizon"`
	ModelVersion    string `json:"modelVersion"`
	StrategyVersion string `json:"strategyVersion"`
	Signal          *struct {
		Direction  string  `json:"direction"`
		ProbUp     float64 `json:"probUp"`
		Confidence float64 `json:"confidence"`
	} `json:"signal"`
	Backtest           map[string]any `json:"backtest"`
	TrainedOnSynthetic bool           `json:"trainedOnSynthetic"`
	DataThrough        string         `json:"dataThrough"` // last bar the model was trained through
	Reason             string         `json:"reason"`

	// Provenance of the frame /predict actually scored. NIL means the frame was never fetched
	// (an early return), which the synthetic gate treats as unknown — not as clean.
	CurrentData *struct {
		Source    string `json:"source"`
		Synthetic bool   `json:"synthetic"`
	} `json:"currentData"`

	// The offline evaluator's persisted verdict for this config (services/prediction/app/verdicts.py),
	// or nil when none exists. `Current` reports whether the strategy it was made about is still the
	// one the prediction service runs. This is gate 4 — see gates.go; there is deliberately no
	// `passed()` helper here any more, because `report.passed` alone was never enough to trade on.
	Evaluation *struct {
		Verdict         string   `json:"verdict"`
		EvaluatedAt     string   `json:"evaluatedAt"`
		StrategyVersion string   `json:"strategyVersion"`
		Current         bool     `json:"current"`
		EvidenceCurrent bool     `json:"evidenceCurrent"`
		EvidenceIssues  []string `json:"evidenceIssues"`
		Method          string   `json:"method"`
		Report          string   `json:"report"`

		// The evaluator's DATE-ALIGNED 1/N PORTFOLIO Sharpe — the number the live book's daily
		// Sharpe is directly comparable with (contract §5.4), because both are annualized from one
		// observation per date.
		//
		// A POINTER, and today always nil: `verdicts.evaluation_block` serves the verdict and the
		// report's FILENAME, not its statistics, and this service has no access to the evaluator's
		// output directory. So the portfolio comparison falls back to the model backtest's
		// annualized per-bar Sharpe WITH the unit caveat kept — which is the honest second-best, not
		// a silent substitution. When the prediction service starts serving this, the comparison
		// upgrades itself and says which source it used.
		PortfolioSharpe *float64 `json:"portfolioSharpe"`
	} `json:"evaluation"`
}

// costBps is the cost assumption the model's backtest charged on every position change. Recorded on
// the trade, never used to adjust live P&L (contract §3.3 — live accounting is priority-2 work).
func (r *predictResp) costBps() any {
	if r == nil || r.Backtest == nil {
		return nil
	}
	return r.Backtest["costBps"]
}

// costBpsFloat is the same number as a float, for the LEDGER: the fee every simulated fill is
// charged at is the cost the model's own backtest was validated under, never a fee this service
// invented. A record that does not state one yields 0 — a book charging a cost nobody validated
// would make its own numbers incomparable with the backtest's, which is the whole point of §5.
func (r *predictResp) costBpsFloat() float64 {
	if r == nil || r.Backtest == nil {
		return 0
	}
	v, _ := r.Backtest["costBps"].(float64)
	return v
}

// allowShort reports whether shorting was actually backtested for this record. /predict already
// refuses to emit "Sell" otherwise (model.derive_direction); this is the engine checking rather than
// trusting, because a short opened on a long-only backtest is un-validated by construction.
func (r *predictResp) allowShort() bool {
	if r == nil || r.Backtest == nil {
		return false
	}
	v, _ := r.Backtest["allowShort"].(bool)
	return v
}

// ---- analysis /quote ----

// quoteResp is the execution price and, crucially, WHERE AND WHEN it came from.
//
// `AsOf` and `Source` used to be dropped on the floor: the engine parsed a price and filled at it,
// so a quote of unknown provenance and unknown age was indistinguishable from a fresh real one. A
// fill dated BEFORE the bar it reconciles is not a fill — it is a price from before the decision
// existed — and the engine now refuses it (contract §3, §4.1).
type quoteResp struct {
	Symbol string   `json:"symbol"`
	Price  *float64 `json:"price"`
	Source string   `json:"source"`
	AsOf   string   `json:"asOf"`
}

// ---- analysis /candles (the newest bar) ----

// latestBar is the newest bar the analysis service has for a (ticker, timeframe): the engine's ONLY
// definition of "a new bar has happened" (contract §2). Source/Synthetic ride along because the same
// request answers gate 1's question about the bar's provenance.
type latestBar struct {
	Time      barTime
	Source    string
	Synthetic bool
	// Close is the bar's own close, which is what the LEDGER marks positions at
	// (docs/PAPER_EXECUTION_CONTRACT.md §5.3). It is deliberately not the quote: a quote is an
	// execution price observed at some other moment, and a book marked at one is not reproducible
	// from the bar series anybody else can fetch. 0 means the provider served no close, which the
	// ledger treats as "no mark" — a gap, never a substituted price.
	Close float64
}

// ---- journal trade (the subset we read back) ----

// attachedSignal is the decision provenance the engine writes onto every paper trade (contract
// §3.2) and now reads BACK. `Horizon` is the reason: (ticker, timeframe) is not a config key once
// two horizons of one ticker are configured, so grouping closed trades by the pair attributes one
// config's results to another. A legacy trade written before this block has a nil Horizon and is
// attributed only when it is unambiguous.
type attachedSignal struct {
	Direction       string   `json:"direction"`
	Horizon         *int     `json:"horizon"`
	ModelVersion    string   `json:"modelVersion"`
	StrategyVersion string   `json:"strategyVersion"`
	DecidedOnBar    string   `json:"decidedOnBar"`
	QuoteSource     string   `json:"quoteSource"`
	QuoteAsOf       string   `json:"quoteAsOf"`
	CostBps         *float64 `json:"costBps"`
	Notional        *float64 `json:"notional"`
	NAtEntry        *int     `json:"nAtEntry"`
	FillKind        string   `json:"fillKind"`
}

type journalTrade struct {
	ID     string `json:"id"`
	Ticker string `json:"ticker"`
	Side   string `json:"side"`
	Status string `json:"status"`
	Mode   string `json:"mode"`
	Entry  struct {
		Date      string  `json:"date"`
		Price     float64 `json:"price"`
		Size      float64 `json:"size"`
		Timeframe string  `json:"timeframe"`
	} `json:"entry"`
	AttachedSignal *attachedSignal `json:"attachedSignal"`
	PnlAbs         *float64        `json:"pnlAbs"`
	PnlPct         *float64        `json:"pnlPct"`
	MarkPrice      *float64        `json:"markPrice"`
	MarkSynthetic  bool            `json:"markIsSynthetic"`
	HoldingDays    *int            `json:"holdingDays"`
}

// httpStatusError carries the STATUS a journal call came back with, not just the fact that it
// failed. The distinction decides whether an open paper position is dropped or kept: a 404/410 means
// the trade is genuinely gone (deleted or reset externally) and our bookkeeping is the thing that is
// wrong, while a timeout or a 5xx means the journal is temporarily unreachable and the position is
// still very much open. Dropping it on a blip orphans a live paper trade — the engine forgets a
// position the journal still holds, and nothing ever closes it.
//
// Stdlib only: a typed error and errors.As, no dependency.
type httpStatusError struct {
	Method string
	URL    string
	Status int
	Body   string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("%s %s -> %d: %s", e.Method, e.URL, e.Status, e.Body)
}

// gone reports whether the resource is definitively absent (as opposed to unreachable).
func (e *httpStatusError) gone() bool {
	return e.Status == http.StatusNotFound || e.Status == http.StatusGone
}

type Clients struct {
	cfg  Config
	http *http.Client
}

func newClients(cfg Config) *Clients {
	return &Clients{cfg: cfg, http: &http.Client{Timeout: 20 * time.Second}}
}

// authorize attaches the engine's system-user session — but ONLY on calls to the journal. The
// prediction and analysis services never see it: a credential should reach exactly the service that
// needs it, and both of those are read-only endpoints that do not want one.
func (c *Clients) authorize(req *http.Request, u string) {
	if !strings.HasPrefix(u, c.cfg.JournalURL) {
		return
	}
	if cookie := c.systemCookie(); cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
}

func (c *Clients) getJSON(ctx context.Context, u string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	c.authorize(req, u)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return &httpStatusError{Method: http.MethodGet, URL: u, Status: resp.StatusCode, Body: string(body)}
	}
	return json.Unmarshal(body, target)
}

func (c *Clients) sendJSON(ctx context.Context, method, u string, payload any, target any) error {
	var body io.Reader
	if payload != nil {
		buf, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	c.authorize(req, u)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return &httpStatusError{Method: method, URL: u, Status: resp.StatusCode, Body: string(rb)}
	}
	if target != nil {
		return json.Unmarshal(rb, target)
	}
	return nil
}

func (c *Clients) predict(ctx context.Context, cfg PaperCfg) (*predictResp, error) {
	u := fmt.Sprintf("%s/predict/%s?timeframe=%s&horizon=%d",
		c.cfg.PredictionURL, url.PathEscape(cfg.Ticker), url.QueryEscape(cfg.Timeframe), cfg.Horizon)
	var out predictResp
	if err := c.getJSON(ctx, u, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// bars asks analysis for the newest bounded slice of a (ticker, timeframe). The official engine
// still acts only on the last bar; the short history lets the experimental shadow ledger settle
// H1/H3/H5/H10 outcomes after a restart without mistaking missed calendar time for one bar.
func (c *Clients) bars(ctx context.Context, ticker, timeframe string, limit int) ([]latestBar, error) {
	if limit < 1 {
		limit = 1
	}
	u := fmt.Sprintf("%s/candles/%s?timeframe=%s&limit=%d",
		c.cfg.AnalysisURL, url.PathEscape(ticker), url.QueryEscape(timeframe), limit)
	var out struct {
		Source    string `json:"source"`
		Synthetic bool   `json:"sourceIsSynthetic"`
		Bars      []struct {
			Time  json.RawMessage `json:"time"`
			Close *float64        `json:"close"`
		} `json:"bars"`
	}
	if err := c.getJSON(ctx, u, &out); err != nil {
		return nil, err
	}
	if len(out.Bars) == 0 {
		return nil, fmt.Errorf("no bars for %s %s", ticker, timeframe)
	}
	bars := make([]latestBar, 0, len(out.Bars))
	for _, raw := range out.Bars {
		bt, err := parseBarTime(raw.Time)
		if err != nil {
			return nil, err
		}
		bar := latestBar{Time: bt, Source: out.Source, Synthetic: out.Synthetic}
		if raw.Close != nil && *raw.Close > 0 {
			bar.Close = *raw.Close
		}
		bars = append(bars, bar)
	}
	return bars, nil
}

func (c *Clients) latestBar(ctx context.Context, ticker, timeframe string) (*latestBar, error) {
	bars, err := c.bars(ctx, ticker, timeframe, 1)
	if err != nil {
		return nil, err
	}
	return &bars[len(bars)-1], nil
}

func (c *Clients) quote(ctx context.Context, ticker string) (*quoteResp, error) {
	u := fmt.Sprintf("%s/quote/%s", c.cfg.AnalysisURL, url.PathEscape(ticker))
	var out quoteResp
	if err := c.getJSON(ctx, u, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// journalCreate POSTs a paper trade and returns the created trade (with its id).
func (c *Clients) journalCreate(ctx context.Context, trade map[string]any) (*journalTrade, error) {
	var wrap struct {
		Trade journalTrade `json:"trade"`
	}
	if err := c.sendJSON(ctx, http.MethodPost, c.cfg.JournalURL+"/trades", trade, &wrap); err != nil {
		return nil, err
	}
	return &wrap.Trade, nil
}

func (c *Clients) journalCloseExit(ctx context.Context, id, date string, price float64) error {
	patch := map[string]any{"exit": map[string]any{"date": date, "price": price}}
	return c.sendJSON(ctx, http.MethodPatch, c.cfg.JournalURL+"/trades/"+id, patch, nil)
}

func (c *Clients) journalDelete(ctx context.Context, id string) error {
	return c.sendJSON(ctx, http.MethodDelete, c.cfg.JournalURL+"/trades/"+id, nil, nil)
}

// journalPaperTrades lists paper trades (status = open|closed|all).
func (c *Clients) journalPaperTrades(ctx context.Context, status string) ([]journalTrade, error) {
	u := fmt.Sprintf("%s/trades?mode=paper&status=%s", c.cfg.JournalURL, url.QueryEscape(status))
	var wrap struct {
		Trades []journalTrade `json:"trades"`
	}
	if err := c.getJSON(ctx, u, &wrap); err != nil {
		return nil, err
	}
	return wrap.Trades, nil
}
