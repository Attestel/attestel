package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// eval.go — the scheduled evaluator. Every EVAL_INTERVAL it evaluates each active rule against
// FRESH deterministic signals (analysis regime/indicators, confluence, quote, gateway news) and,
// on an edge (transition into the condition), records an event and notifies. It uses lastState to
// edge-trigger so a persistent condition fires ONCE, not every tick; cooldownSec adds a second
// guard against flapping.
//
// The LLM is deliberately NOT involved: alerts are pure rules over pre-computed numbers.

type Evaluator struct {
	cfg      Config
	store    *Store
	http     *http.Client
	notifier *Notifier
}

func newEvaluator(cfg Config, store *Store, notifier *Notifier) *Evaluator {
	return &Evaluator{
		cfg:      cfg,
		store:    store,
		http:     &http.Client{Timeout: 15 * time.Second},
		notifier: notifier,
	}
}

// Run drives the evaluation loop until ctx is cancelled. It evaluates once immediately (so a
// freshly-created rule that's already satisfied fires on the first tick), then on every interval.
func (e *Evaluator) Run(ctx context.Context) {
	log.Printf("evaluator: interval=%s tickers=%v", e.cfg.EvalInterval, e.cfg.Tickers)
	ticker := time.NewTicker(e.cfg.EvalInterval)
	defer ticker.Stop()
	e.evalAll(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.evalAll(ctx)
		}
	}
}

// evalAll evaluates every active rule, batching upstream fetches per ticker so N rules on the same
// ticker cost one analysis/confluence/quote/news round-trip set, not N.
func (e *Evaluator) evalAll(parent context.Context) {
	rules := e.store.Rules()
	byTicker := map[string][]Rule{}
	for _, r := range rules {
		if r.Active {
			byTicker[r.Ticker] = append(byTicker[r.Ticker], r)
		}
	}
	if len(byTicker) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()

	updates := map[string]evalUpdate{}
	var fired []Event
	now := time.Now().Unix()

	for ticker, group := range byTicker {
		fc := &fetchCache{e: e, ctx: ctx, ticker: ticker, aByTF: map[string]*analysisResp{}}
		for _, rule := range group {
			res := e.evaluate(rule, fc)
			if res.skip {
				continue // couldn't evaluate (missing data / upstream down) — leave rule untouched
			}
			changed := !equalState(rule.LastState, res.state)
			lastTriggered := rule.LastTriggered
			if res.fired {
				if rule.LastTriggered == 0 || now-rule.LastTriggered >= int64(rule.CooldownSec) {
					fired = append(fired, e.buildEvent(rule, res, fc, now))
					lastTriggered = now
					changed = true
				}
				// else: within cooldown — suppress the event, but still advance lastState below.
			}
			if changed {
				updates[rule.ID] = evalUpdate{lastTriggered: lastTriggered, lastState: res.state}
			}
		}
	}

	if err := e.store.ApplyEvalUpdatesE(updates); err != nil {
		log.Printf("persist rule evaluation state failed: %v", err)
		return
	}
	for _, ev := range fired {
		if err := e.store.AppendEvent(ev); err != nil {
			log.Printf("append event failed: %v", err)
			continue
		}
		log.Printf("ALERT %s/%s %s: %s", ev.Ticker, ev.Timeframe, ev.Type, ev.Message)
		e.notifier.Notify(ev)
	}
}

// buildEvent assembles the event for one firing, carrying the rule's thesis context onto it (§4.2).
//
// Everything here is derived from stored fields and the data actually consulted this pass. There is
// no model call and no inference: `bearing` comes from bearingFor, which returns nil unless the user
// themselves tied the rule to a named invalidation condition or catalyst, and `researchAction` is a
// research verb by construction.
func (e *Evaluator) buildEvent(rule Rule, res evalResult, fc *fetchCache, now int64) Event {
	ev := Event{
		ID: newID(), RuleID: rule.ID, UserID: rule.UserID, Ticker: rule.Ticker,
		Timeframe: rule.Timeframe, Type: rule.Type, Message: res.message, TS: now,

		ThesisID:     rule.ThesisID,
		ThesisItemID: rule.ThesisItemID,
		Intent:       rule.Intent,
		Bearing:      bearingFor(rule),
		DedupeKey:    dedupeKeyFor(rule.ID, rule.CooldownSec, now),
		ResearchLink: researchLinkFor(rule.Ticker, rule.ThesisID),
	}
	// A scheduled review consults no market data at all, so it inherits no price provenance; the
	// trigger is a date the user themselves set.
	if rule.Type == TypeThesisReview {
		ev.DataState = DataStateLive
	} else {
		ev.DataState = fc.dataState()
	}
	// Only a thesis-linked event carries a research action — an untargeted rule has no thesis to
	// send the user back to, and inventing one would be a false claim about their research.
	if rule.ThesisID != "" {
		ev.ResearchAction = researchActionFor(rule.Intent)
		// D-10: the auto-link CASE is recorded here as a suggestion flag. This lane never writes an
		// evidence link from the evaluator — `alerts` never writes to `journal` (§4.3), and a
		// suggestion the user has not accepted is not a link.
		ev.EvidenceSuggested = rule.MayAutoLinkEvidence()
	}
	return ev
}

// evalResult is one rule's evaluation outcome. skip=true means "don't touch the rule this pass".
type evalResult struct {
	fired   bool
	message string
	state   any
	skip    bool
}

func skip() evalResult { return evalResult{skip: true} }

func (e *Evaluator) evaluate(r Rule, fc *fetchCache) evalResult {
	switch r.Type {
	case TypePriceCross:
		return e.evalPriceCross(r, fc)
	case TypePctMove:
		return e.evalPctMove(r, fc)
	case TypeRSIThreshold:
		return e.evalRSIThreshold(r, fc)
	case TypeMACDCross:
		return e.evalMACDCross(r, fc)
	case TypeTrendFlip:
		return e.evalTrendFlip(r, fc)
	case TypeConfluenceConflict:
		return e.evalConfluenceConflict(r, fc)
	case TypeVWAPCross:
		return e.evalVWAPCross(r, fc)
	case TypeNew8K:
		return e.evalNew8K(r, fc)
	case TypeCalendarEvent:
		return e.evalCalendarEvent(r, fc)
	case TypeThesisReview:
		return e.evalThesisReview(r)
	}
	return skip()
}

// evalThesisReview fires when a scheduled research review comes due. It consults NO market data and
// NO model — it is a calendar computation over the rule's own fields, which is exactly why it is the
// honest home for a condition no evaluator can test (§4.1).
//
// Two shapes, per the contract: a one-shot date (reviewAt) or a recurring cadence (params.everyDays)
// measured from the last firing, or from creation if it has never fired.
func (e *Evaluator) evalThesisReview(r Rule) evalResult {
	now := time.Now().Unix()

	if r.ReviewAt > 0 {
		due := now >= r.ReviewAt
		fired, state := enterBool(r.LastState, due)
		return evalResult{
			fired:   fired,
			message: fmt.Sprintf("Review due: this %s research review was scheduled for %s", r.Ticker, isoDay(r.ReviewAt)),
			state:   state,
		}
	}

	days, err := paramFloat(r.Params, "everyDays")
	if err != nil || days <= 0 {
		return skip()
	}
	since := r.LastTriggered
	if since == 0 {
		since = r.CreatedAt
	}
	if since == 0 {
		return skip() // no boundary to measure from — never guess one
	}
	elapsed := now - since
	period := int64(days * 86400)
	if elapsed < period {
		// Not due. Report state honestly so a later tick sees the transition rather than a fresh edge.
		return evalResult{fired: false, state: false}
	}
	return evalResult{
		fired:   true,
		message: fmt.Sprintf("Review due: %s research review every %d days (last %s)", r.Ticker, int(days), isoDay(since)),
		state:   true,
	}
}

func isoDay(ts int64) string { return time.Unix(ts, 0).UTC().Format("2006-01-02") }

// ---- per-type evaluators ----

func (e *Evaluator) evalPriceCross(r Rule, fc *fetchCache) evalResult {
	level, _ := paramFloat(r.Params, "level")
	dir, _ := r.Params["direction"].(string)
	price := fc.price()
	if price == nil {
		return skip()
	}
	pos := "below"
	if *price >= level {
		pos = "above"
	}
	fired, state := enterRegion(r.LastState, pos, dir)
	msg := fmt.Sprintf("%s price crossed %s %.2f (now %.2f)", r.Ticker, dir, level, *price)
	return evalResult{fired: fired, message: msg, state: state}
}

func (e *Evaluator) evalPctMove(r Rule, fc *fetchCache) evalResult {
	pct, _ := paramFloat(r.Params, "pct")
	chg := fc.dayChangePct()
	if chg == nil {
		return skip()
	}
	satisfied := math.Abs(*chg) >= pct
	fired, state := enterBool(r.LastState, satisfied)
	msg := fmt.Sprintf("%s moved %+.2f%% on the day (threshold %.2f%%)", r.Ticker, *chg, pct)
	return evalResult{fired: fired, message: msg, state: state}
}

func (e *Evaluator) evalRSIThreshold(r Rule, fc *fetchCache) evalResult {
	level, _ := paramFloat(r.Params, "level")
	dir, _ := r.Params["direction"].(string)
	a, err := fc.analysis(r.Timeframe)
	if err != nil || a.Indicators.Latest.RSI == nil {
		return skip()
	}
	rsi := *a.Indicators.Latest.RSI
	pos := "below"
	if rsi >= level {
		pos = "above"
	}
	fired, state := enterRegion(r.LastState, pos, dir)
	msg := fmt.Sprintf("%s %s RSI crossed %s %g (now %.2f)", r.Ticker, r.Timeframe, dir, level, rsi)
	return evalResult{fired: fired, message: msg, state: state}
}

func (e *Evaluator) evalMACDCross(r Rule, fc *fetchCache) evalResult {
	dir, _ := r.Params["direction"].(string)
	a, err := fc.analysis(r.Timeframe)
	if err != nil || a.Indicators.Latest.MacdHist == nil {
		return skip()
	}
	hist := *a.Indicators.Latest.MacdHist
	sign := "bearish"
	if hist > 0 {
		sign = "bullish"
	}
	fired, state := transitionTo(r.LastState, sign, dir)
	msg := fmt.Sprintf("%s %s MACD histogram flipped %s (hist %.4f)", r.Ticker, r.Timeframe, dir, hist)
	return evalResult{fired: fired, message: msg, state: state}
}

func (e *Evaluator) evalTrendFlip(r Rule, fc *fetchCache) evalResult {
	a, err := fc.analysis(r.Timeframe)
	if err != nil || a.Regime.Trend == "" {
		return skip()
	}
	trend := a.Regime.Trend
	prev := asString(r.LastState)
	fired := prev != "" && prev != trend
	msg := fmt.Sprintf("%s %s trend changed: %s → %s", r.Ticker, r.Timeframe, prev, trend)
	return evalResult{fired: fired, message: msg, state: trend}
}

func (e *Evaluator) evalConfluenceConflict(r Rule, fc *fetchCache) evalResult {
	mode, _ := r.Params["mode"].(string)
	c, err := fc.confluence()
	if err != nil {
		return skip()
	}
	conflicts := c.Confluence.Conflicts
	has := len(conflicts) > 0
	prev, prevKnown := r.LastState.(bool)
	var fired bool
	var msg string
	if mode == "appears" {
		fired = prevKnown && has && !prev
		detail := ""
		if len(conflicts) > 0 {
			detail = ": " + conflicts[0]
		}
		msg = fmt.Sprintf("%s multi-timeframe conflict appeared%s", r.Ticker, detail)
	} else { // resolves
		fired = prevKnown && !has && prev
		msg = fmt.Sprintf("%s multi-timeframe conflicts resolved", r.Ticker)
	}
	return evalResult{fired: fired, message: msg, state: has}
}

func (e *Evaluator) evalVWAPCross(r Rule, fc *fetchCache) evalResult {
	dir, _ := r.Params["direction"].(string)
	a, err := fc.analysis(r.Timeframe)
	if err != nil || a.Regime.VwapState == nil {
		return skip() // VWAP is intraday-only; null on 1D
	}
	pos := "below"
	if strings.HasPrefix(*a.Regime.VwapState, "above") {
		pos = "above"
	}
	fired, state := transitionTo(r.LastState, pos, dir)
	msg := fmt.Sprintf("%s %s price crossed %s VWAP", r.Ticker, r.Timeframe, dir)
	return evalResult{fired: fired, message: msg, state: state}
}

func (e *Evaluator) evalNew8K(r Rule, fc *fetchCache) evalResult {
	n, err := fc.news()
	if err != nil {
		return skip()
	}
	var newest *newsItem
	for i := range n.News {
		if n.News[i].Category == "filing" {
			newest = &n.News[i]
			break // gateway returns news newest-first
		}
	}
	if newest == nil {
		return skip() // no filings visible (e.g. offline / no SEC) — nothing to compare
	}
	id := newest.URL
	if id == "" {
		id = newest.Headline + "|" + newest.PublishedAt
	}
	prev := asString(r.LastState)
	fired := prev != "" && prev != id
	msg := fmt.Sprintf("New SEC 8-K filed for %s: %s", r.Ticker, newest.Headline)
	return evalResult{fired: fired, message: msg, state: id}
}

// evalCalendarEvent fires when a scheduled catalyst is within `lead` days — the ticker's next
// earnings (kind=earnings) or the nearest high-importance macro release (kind=macro). Edge-triggered
// (enterBool) + cooldown, so it fires ONCE as the window opens, not every tick. Descriptive dates via
// the gateway (/api/next-earnings, /api/calendar); no order, no verdict.
func (e *Evaluator) evalCalendarEvent(r Rule, fc *fetchCache) evalResult {
	kind, _ := r.Params["kind"].(string)
	lead, _ := paramFloat(r.Params, "lead")

	var date, label string
	if kind == "earnings" {
		ne, err := fc.nextEarnings()
		if err != nil || ne.Date == "" {
			return skip()
		}
		date, label = ne.Date, fmt.Sprintf("%s earnings", r.Ticker)
	} else { // macro
		cal, err := fc.calendar()
		if err != nil {
			return skip()
		}
		todayStr := time.Now().UTC().Format("2006-01-02")
		for _, ev := range cal.Events { // events sorted ascending — take the nearest future high-impact
			if ev.Importance == "high" && ev.Date >= todayStr {
				date, label = ev.Date, ev.Name
				break
			}
		}
		if date == "" {
			return skip()
		}
	}

	d, err := time.Parse("2006-01-02", date)
	if err != nil {
		return skip()
	}
	today, _ := time.Parse("2006-01-02", time.Now().UTC().Format("2006-01-02"))
	days := int(d.Sub(today).Hours() / 24)
	within := days >= 0 && float64(days) <= lead
	fired, state := enterBool(r.LastState, within)

	msg := fmt.Sprintf("%s in %d days (%s)", label, days, date)
	switch days {
	case 0:
		msg = fmt.Sprintf("%s today (%s)", label, date)
	case 1:
		msg = fmt.Sprintf("%s in 1 day (%s)", label, date)
	}
	return evalResult{fired: fired, message: msg, state: state}
}

// ---- edge-trigger helpers ----

func asString(v any) string { s, _ := v.(string); return s }

// enterRegion: "condition" semantics. Fires when we're on the target side and weren't already.
// A nil/unknown prior counts as "not on target", so a condition that's already true when the rule
// is created fires once on the first evaluation (needed for the zero-key demo where synthetic
// prices are static). Returns the new state to store.
func enterRegion(prev any, current, target string) (bool, any) {
	return current == target && asString(prev) != target, current
}

// enterBool: condition semantics over a boolean (e.g. pct_move threshold breached).
func enterBool(prev any, satisfied bool) (bool, any) {
	prevBool, _ := prev.(bool)
	return satisfied && !prevBool, satisfied
}

// transitionTo: "change" semantics. Only fires on an OBSERVED transition into target — a rule
// created while already on the target side does NOT fire (it re-baselines first). Used for cross
// events (MACD/VWAP) where "flipped" must mean an actual flip.
func transitionTo(prev any, current, target string) (bool, any) {
	prevStr := asString(prev)
	return prevStr != "" && current == target && prevStr != target, current
}

// equalState compares two edge-memory values (nil/bool/string only — all comparable).
func equalState(a, b any) bool { return a == b }

// ---- per-pass fetch cache (memoizes upstream calls within a single evalAll for one ticker) ----

type fetchCache struct {
	e      *Evaluator
	ctx    context.Context
	ticker string

	aByTF    map[string]*analysisResp
	aByTFErr map[string]error

	conf     *confluenceResp
	confErr  error
	confDone bool

	quoteResp *quoteResp
	quoteErr  error
	quoteDone bool

	newsResp *newsResp
	newsErr  error
	newsDone bool

	calData *calResp
	calErr  error
	calDone bool

	neData *nextEarnResp
	neErr  error
	neDone bool
}

func (c *fetchCache) analysis(tf string) (*analysisResp, error) {
	if c.aByTFErr == nil {
		c.aByTFErr = map[string]error{}
	}
	if v, ok := c.aByTF[tf]; ok {
		return v, c.aByTFErr[tf]
	}
	var out analysisResp
	u := fmt.Sprintf("%s/analysis/%s?timeframe=%s", c.e.cfg.AnalysisURL, url.PathEscape(c.ticker), url.QueryEscape(tf))
	err := c.e.getJSON(c.ctx, u, &out)
	if err != nil {
		c.aByTF[tf] = nil
		c.aByTFErr[tf] = err
		return nil, err
	}
	c.aByTF[tf] = &out
	return &out, nil
}

func (c *fetchCache) confluence() (*confluenceResp, error) {
	if !c.confDone {
		c.confDone = true
		var out confluenceResp
		u := fmt.Sprintf("%s/analysis/%s/confluence", c.e.cfg.AnalysisURL, url.PathEscape(c.ticker))
		if err := c.e.getJSON(c.ctx, u, &out); err != nil {
			c.confErr = err
		} else {
			c.conf = &out
		}
	}
	return c.conf, c.confErr
}

func (c *fetchCache) quote() (*quoteResp, error) {
	if !c.quoteDone {
		c.quoteDone = true
		var out quoteResp
		u := fmt.Sprintf("%s/quote/%s", c.e.cfg.AnalysisURL, url.PathEscape(c.ticker))
		if err := c.e.getJSON(c.ctx, u, &out); err != nil {
			c.quoteErr = err
		} else {
			c.quoteResp = &out
		}
	}
	return c.quoteResp, c.quoteErr
}

func (c *fetchCache) news() (*newsResp, error) {
	if !c.newsDone {
		c.newsDone = true
		var out newsResp
		u := fmt.Sprintf("%s/api/news/%s", c.e.cfg.GatewayURL, url.PathEscape(c.ticker))
		if err := c.e.getJSON(c.ctx, u, &out); err != nil {
			c.newsErr = err
		} else {
			c.newsResp = &out
		}
	}
	return c.newsResp, c.newsErr
}

func (c *fetchCache) calendar() (*calResp, error) {
	if !c.calDone {
		c.calDone = true
		var out calResp
		if err := c.e.getJSON(c.ctx, c.e.cfg.GatewayURL+"/api/calendar", &out); err != nil {
			c.calErr = err
		} else {
			c.calData = &out
		}
	}
	return c.calData, c.calErr
}

func (c *fetchCache) nextEarnings() (*nextEarnResp, error) {
	if !c.neDone {
		c.neDone = true
		var out nextEarnResp
		u := fmt.Sprintf("%s/api/next-earnings/%s", c.e.cfg.GatewayURL, url.PathEscape(c.ticker))
		if err := c.e.getJSON(c.ctx, u, &out); err != nil {
			c.neErr = err
		} else {
			c.neData = &out
		}
	}
	return c.neData, c.neErr
}

// dataState reports whether the numbers behind this pass were real, seed, or synthetic (§4.2).
//
// It asks the price feed rather than guessing, and it defaults to "synthetic" ONLY when a source
// actually says so — an unknown/unreachable source reports "live" would be a lie in the other
// direction, so an unresolvable source reports "seed": the honest "this did not come from a live
// market feed" label. Invariant #1's visible half depends on this never being optimistic.
func (c *fetchCache) dataState() string {
	if q, err := c.quote(); err == nil && q != nil && q.Source != "" {
		return dataStateFromSource(q.Source)
	}
	if a, err := c.analysis("1D"); err == nil && a != nil {
		if a.SourceIsSynthetic {
			return DataStateSynthetic
		}
		if a.Source != "" {
			return dataStateFromSource(a.Source)
		}
	}
	return DataStateSeed
}

// dataStateFromSource maps a provider label from the price cascade onto the three-state enum.
func dataStateFromSource(src string) string {
	switch strings.ToLower(strings.TrimSpace(src)) {
	case "synthetic":
		return DataStateSynthetic
	case "", "seed":
		return DataStateSeed
	default:
		return DataStateLive // tiingo | yfinance | alpaca:iex | twelvedata …
	}
}

// price returns the freshest spot price (quote endpoint), falling back to the 1D analysis close.
func (c *fetchCache) price() *float64 {
	if q, err := c.quote(); err == nil && q.Price != nil {
		return q.Price
	}
	if a, err := c.analysis("1D"); err == nil {
		p := a.Price.LastClose
		return &p
	}
	return nil
}

// dayChangePct returns the day's % change (quote first, then 1D analysis).
func (c *fetchCache) dayChangePct() *float64 {
	if q, err := c.quote(); err == nil && q.ChangePct != nil {
		return q.ChangePct
	}
	if a, err := c.analysis("1D"); err == nil {
		p := a.Price.ChangePct
		return &p
	}
	return nil
}

// ---- upstream response shapes (only the fields alerts read) ----

type analysisResp struct {
	// Source / SourceIsSynthetic are the price cascade's own provenance labels (services/analysis
	// data.py). They are what makes a synthetic-triggered event labellable as such.
	Source            string `json:"source"`
	SourceIsSynthetic bool   `json:"sourceIsSynthetic"`

	Price struct {
		LastClose float64 `json:"lastClose"`
		ChangePct float64 `json:"changePct"`
	} `json:"price"`
	Indicators struct {
		Latest struct {
			RSI      *float64 `json:"rsi"`
			MacdHist *float64 `json:"macdHist"`
			ADX      *float64 `json:"adx"`
			StochK   *float64 `json:"stochK"`
		} `json:"latest"`
	} `json:"indicators"`
	Regime struct {
		Trend     string  `json:"trend"`
		MacdState string  `json:"macdState"`
		VwapState *string `json:"vwapState"`
	} `json:"regime"`
}

type confluenceResp struct {
	Confluence struct {
		Conflicts []string `json:"conflicts"`
	} `json:"confluence"`
}

type quoteResp struct {
	Price     *float64 `json:"price"`
	ChangePct *float64 `json:"changePct"`
	Source    string   `json:"source"`
}

type newsItem struct {
	Headline    string `json:"headline"`
	URL         string `json:"url"`
	PublishedAt string `json:"publishedAt"`
	Category    string `json:"category"`
}

type newsResp struct {
	News []newsItem `json:"news"`
}

type calResp struct {
	Events []struct {
		Date       string `json:"date"`
		Name       string `json:"name"`
		Importance string `json:"importance"`
		Source     string `json:"source"`
	} `json:"events"`
}

type nextEarnResp struct {
	Date   string `json:"date"`
	Source string `json:"source"`
}

// getJSON does a GET and decodes JSON into target. Non-2xx is an error.
func (e *Evaluator) getJSON(ctx context.Context, u string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := e.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("GET %s -> %d", u, resp.StatusCode)
	}
	return json.Unmarshal(body, target)
}
