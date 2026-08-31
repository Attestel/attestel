package main

// context.go — the gateway-side CONTEXT ENVELOPE (Wave 3, Lane 3A).
//
// `GET /api/context/{ticker}` assembles the user-scoped and composite data that `services/llm` must
// never fetch for itself, and hands it back in one object that Lane 3B forwards into the analyst
// request body. Three of the eight §6 tools — `get_user_subscriptions`, `get_ticker_context` and
// `get_market_context` — are answered from this envelope and open no socket in the llm service.
//
// WHY THE ENVELOPE EXISTS AT ALL. `journal` is session-cookie authenticated and `services/llm` holds
// no cookie; the gateway is the sole aggregator (§9.3). The alternative — llm calling back into the
// gateway — is a re-entrant hop that moves the auth boundary and buys nothing. So the gateway does
// what it already does for every other composite, and the llm does what it already does: reads two
// keyless, `as_of`-aware services directly (§9.31).
//
// THE POINT-IN-TIME RULE (§1.3). When `asOf` is strictly in the past, NO provider call may originate
// here, including on a cache miss. The envelope is then assembled from `services/analysis` and
// `services/events` store reads only, both of which enforce the cutoff in SQL themselves. The one
// piece of the envelope that is a live provider read — the EPS beat/miss series, which comes from a
// keyed provider through `earningsFor` and is not point-in-time at all — is therefore OMITTED on a
// historical request and labelled, rather than silently served as if it had been visible then.
// `coverage: "insufficient"` from either service propagates into the envelope untouched: coverage is
// a result, not an error, and there is no fallback fetch.
//
// HONESTY OVER COMPLETENESS in `marketContext`. PROJECT_PLAN §2 records that this repo has no
// SPY/QQQ/VIX/breadth aggregate, and adding a provider is out of scope for this lane. So the
// benchmark leg is built from the proxy §9.26 already names (`SPY`) through the analysis service,
// realised volatility is arithmetic on that same reading and is LABELLED as realised rather than
// implied, and the dimensions this deployment genuinely cannot compute are `null` with a `degraded`
// token. A fabricated breadth number would be worse than an absent one.
//
// This file registers its own route through the Wave 0 seam (`routeseams.go`), so neither `main.go`
// nor `routeseams.go` is edited — `subscriptions.go` and `feeds.go` do exactly the same. Adding a
// `srv.registerContextRoutes(mux)` line to `main.go` would register the route TWICE and panic the
// mux; §9.28 is the general form of that warning.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// init attaches this file's route through the Wave 0 seam. Do NOT also add a line to main.go.
func init() {
	registerEventRoute(func(s *Server, mux *http.ServeMux) {
		s.registerContextRoutes(mux)
	})
}

// registerContextRoutes mounts the analyst context envelope.
func (s *Server) registerContextRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/context/{ticker}", s.handleContext)
}

// ───────────────────────────────────────────────────────────────────────────── constants

// contextCachePrefix is this lane's ttlCache namespace. Distinct from every existing prefix
// (quote: confluence: predict: dashboard: news: earnings: sentiment: brief: thesischecks: digest:
// committee: calendar: nextearn:), because a shared prefix across two shapes is a silent type
// confusion waiting for the first collision.
const contextCachePrefix = "context:"

// contextTTL is CONTEXT_TTL, in the style of COMMITTEE_TTL. It lives here rather than in config.go
// so this lane's footprint in shared files stays at zero lines — `subscriptions.go` gives the same
// reasoning for `subscriptionMigrations`.
var contextTTL = envDur("CONTEXT_TTL", 5*time.Minute)

// benchmarkProxy is §9.26's named benchmark. Not a vendor and not a choice made here: the contract
// fixes it, and the analysis service already serves it keyless.
const benchmarkProxy = "SPY"

const (
	contextEventLimit = 25 // one page of events; the analyst asks for detail per event if it wants it
	contextMacroLimit = 25
	contextEventDays  = 30 // how far back `recentEvents` looks from the cutoff
	defaultHorizon    = "5d"
)

// contextHorizons is the §4.4 / Conventions short-form vocabulary. One spelling per position.
var contextHorizons = map[string]bool{"1d": true, "5d": true, "20d": true, "60d": true}

// ─────────────────────────────────────────────────────────────────────────────── handler

func (s *Server) handleContext(w http.ResponseWriter, r *http.Request) {
	ticker := strings.ToUpper(strings.TrimSpace(r.PathValue("ticker")))
	if ticker == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "ticker is required"})
		return
	}

	q := r.URL.Query()
	asOf := strings.TrimSpace(q.Get("asOf"))
	if asOf != "" {
		if _, err := time.Parse(time.RFC3339, asOf); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "asOf must be an RFC3339 UTC timestamp: " + asOf})
			return
		}
	}
	horizon := strings.TrimSpace(q.Get("horizon"))
	if horizon == "" {
		horizon = defaultHorizon
	}
	if !contextHorizons[horizon] {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "horizon must be one of 1d | 5d | 20d | 60d: " + horizon})
		return
	}

	id := s.identityFrom(r)

	// The cache key carries the RAW asOf, so a live request (empty) and a historical request can
	// never collide: they are different keys by construction, not by convention. `uid` is in the
	// key because `subscriptions` is user-scoped, and serving one user's follow graph to another
	// out of a shared cache would be a data leak, not a stale read.
	key := contextCachePrefix + strings.Join([]string{ticker, asOf, horizon, id.userID}, "|")
	if b, ok := s.cache.get(key); ok {
		writeRawJSON(w, b)
		return
	}

	env := s.buildContextEnvelope(r.Context(), ticker, asOf, horizon, id)
	body, err := json.Marshal(env)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError,
			map[string]any{"error": "could not encode context envelope"})
		return
	}
	s.cache.set(key, body, contextTTL)
	writeRawJSON(w, body)
}

func writeRawJSON(w http.ResponseWriter, b []byte) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

// ────────────────────────────────────────────────────────────────────────────── the build

// contextPart is one leg of the fan-out: its payload plus whatever it had to admit about itself.
type contextPart struct {
	value    any
	degraded []string
}

// buildContextEnvelope assembles the envelope. Four upstream legs run concurrently, exactly as
// `handleDashboard` does; each leg fails soft into a labelled absence, so a dead dependency costs a
// section and never the response.
func (s *Server) buildContextEnvelope(ctx context.Context, ticker, asOf, horizon string, id identity) map[string]any {
	historical := isHistoricalAsOf(asOf)

	var (
		wg                                  sync.WaitGroup
		subs, technical, events, macro, mkt contextPart
		earnings, fundamentals              contextPart
	)

	wg.Add(7)
	go func() { defer wg.Done(); subs = s.contextSubscriptions(ctx, id) }()
	go func() { defer wg.Done(); technical = s.contextTechnical(ctx, ticker, asOf) }()
	go func() { defer wg.Done(); events = s.contextEvents(ctx, ticker, asOf) }()
	go func() { defer wg.Done(); macro = s.contextMacro(ctx, asOf) }()
	go func() { defer wg.Done(); mkt = s.contextMarket(ctx, asOf) }()
	go func() { defer wg.Done(); earnings = s.contextEarnings(ctx, ticker, historical) }()
	go func() { defer wg.Done(); fundamentals = s.contextFundamentals(ticker, historical) }()
	wg.Wait()

	degraded := mergeDegraded(subs, technical, events, macro, mkt, earnings, fundamentals)

	resolved := asOf
	if resolved == "" {
		// A live request has no requested cutoff, so the envelope echoes the one the analysis
		// service actually resolved when it can, and the wall clock when it cannot. §1.4: the echo
		// is the resolved value, never the requested one.
		resolved = firstResolvedAsOf(technical.value, events.value, macro.value)
	}

	return map[string]any{
		"ticker":  ticker,
		"asOf":    resolved,
		"horizon": horizon,
		// `subscriptions` is [] for a guest, never absent and never a 401 — the analyst must still
		// run for a signed-out visitor on the seed ticker.
		"subscriptions": subs.value,
		"tickerContext": map[string]any{
			"ticker":       ticker,
			"horizon":      horizon,
			"technical":    technical.value,
			"recentEvents": events.value,
			"earnings":     earnings.value,
			"fundamentals": fundamentals.value,
			"macro":        macro.value,
		},
		"marketContext": mkt.value,
		"degraded":      degraded,
	}
}

// isHistoricalAsOf reports whether the cutoff is strictly in the past. An unparseable value never
// reaches here (the handler 400s first); an empty one is "now".
func isHistoricalAsOf(asOf string) bool {
	if asOf == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, asOf)
	if err != nil {
		return false
	}
	return t.Before(time.Now().UTC())
}

// mergeDegraded flattens the legs' tokens, de-duplicated and never nil — `degraded` is an array in
// the contract, and a JSON `null` there would make every consumer write a nil check.
func mergeDegraded(parts ...contextPart) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, p := range parts {
		for _, tok := range p.degraded {
			if !seen[tok] {
				seen[tok] = true
				out = append(out, tok)
			}
		}
	}
	return out
}

// firstResolvedAsOf reads the `asOf` echo out of whichever leg answered first-listed.
func firstResolvedAsOf(vals ...any) string {
	for _, v := range vals {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if s, ok := m["asOf"].(string); ok && s != "" {
			return s
		}
	}
	return time.Now().UTC().Format(time.RFC3339)
}

// ───────────────────────────────────────────────────────────────────────────────── legs

// contextSubscriptions reads the caller's follow graph from journal with their own cookie.
//
// A guest is a normal state here, not an error: `[]` plus a token. Journal being unreachable is a
// different state and gets a different token, because "you follow nothing" and "we could not ask"
// are different facts and the analyst is entitled to tell them apart.
func (s *Server) contextSubscriptions(ctx context.Context, id identity) contextPart {
	empty := []any{}
	if id.userID == "" {
		return contextPart{value: empty, degraded: []string{"journal:guest"}}
	}
	body, err := s.journalGet(ctx, s.cfg.JournalURL+"/subscriptions", id.cookie)
	if err != nil {
		return contextPart{value: empty, degraded: []string{"journal:unreachable"}}
	}
	list, ok := body["subscriptions"].([]any)
	if !ok || list == nil {
		return contextPart{value: empty, degraded: []string{"journal:empty_payload"}}
	}
	return contextPart{value: list}
}

// contextTechnical reads the point-in-time technical picture (§4.4). `/technical-context` is used
// rather than `/analysis/{ticker}` deliberately: §9.38 keeps the four pre-existing endpoints
// byte-identical, which means they answer a past cutoff they cannot serve with a 409 and no
// `coverage` field. The new endpoint carries coverage IN THE BODY, which is what an envelope needs.
func (s *Server) contextTechnical(ctx context.Context, ticker, asOf string) contextPart {
	q := url.Values{"timeframe": {"1D"}}
	if asOf != "" {
		q.Set("as_of", asOf)
	}
	body, err := s.getJSON(ctx, fmt.Sprintf("%s/technical-context/%s?%s",
		s.cfg.AnalysisURL, url.PathEscape(ticker), q.Encode()))
	if err != nil {
		return contextPart{value: nil, degraded: []string{"analysis:unreachable"}}
	}
	return contextPart{value: compactTechnical(body)}
}

// compactTechnical keeps the readable summary and drops the two bulk arrays (`recentPriceAction`
// and `raw`), which are a per-bar dump the analyst has `get_candles` for. Every provenance and
// coverage field survives: `source`, `sourceIsSynthetic`, `coverage`, `coverageFrom`, `coverageTo`.
func compactTechnical(body map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"ticker", "timeframe", "asOf", "price", "indicators",
		"source", "sourceIsSynthetic", "coverage", "coverageFrom", "coverageTo"} {
		if v, ok := body[k]; ok {
			out[k] = v
		}
	}
	if st, ok := body["structure"].(map[string]any); ok {
		sub := map[string]any{}
		for _, k := range []string{"trend", "movingAverages", "momentum", "volume", "structure"} {
			if v, ok := st[k]; ok {
				sub[k] = v
			}
		}
		out["structure"] = sub
	}
	return out
}

// contextEvents reads the recent event window from `services/events`, which enforces the cutoff in
// SQL on both `published_at` and `first_seen_at` (§1.1, §1.2).
//
// The offline floor is the embedded seed, per §9.33 and the cascade `enrich.go` already uses for
// news — labelled, so nobody mistakes a catalyst list for the live corpus.
func (s *Server) contextEvents(ctx context.Context, ticker, asOf string) contextPart {
	since := eventWindowStart(asOf)
	q := url.Values{
		"tickers": {ticker},
		"since":   {since},
		"limit":   {fmt.Sprint(contextEventLimit)},
	}
	if asOf != "" {
		q.Set("as_of", asOf)
	}
	raw, err := s.eventsGet(ctx, "events?"+q.Encode())
	if err == nil {
		var body map[string]any
		if json.Unmarshal(raw, &body) == nil {
			return contextPart{value: body}
		}
		return contextPart{value: nil, degraded: []string{"events:bad_payload"}}
	}
	if isSeedTicker(ticker) {
		return contextPart{
			value:    map[string]any{"events": []any{}, "seedCatalysts": seedNews(), "source": "seed"},
			degraded: []string{"events:seed"},
		}
	}
	return contextPart{value: nil, degraded: []string{"events:unreachable"}}
}

// eventWindowStart is the cutoff minus the event window, as an ISO 8601 UTC string ending Z
// (Conventions). Derived from the CUTOFF, not from the wall clock — deriving it from `now` would
// make two identical historical requests differ, which is the reproducibility hole §9.38.4 closed
// for `sessionRemainingPct`.
func eventWindowStart(asOf string) string {
	base := time.Now().UTC()
	if asOf != "" {
		if t, err := time.Parse(time.RFC3339, asOf); err == nil {
			base = t.UTC()
		}
	}
	return base.AddDate(0, 0, -contextEventDays).Format("2006-01-02T15:04:05Z")
}

// contextMacro reads recent macro releases from `services/events` (§3.9).
func (s *Server) contextMacro(ctx context.Context, asOf string) contextPart {
	q := url.Values{"limit": {fmt.Sprint(contextMacroLimit)}}
	if asOf != "" {
		q.Set("as_of", asOf)
	}
	raw, err := s.eventsGet(ctx, "macro?"+q.Encode())
	if err != nil {
		return contextPart{value: nil, degraded: []string{"macro:unreachable"}}
	}
	var body map[string]any
	if json.Unmarshal(raw, &body) != nil {
		return contextPart{value: nil, degraded: []string{"macro:bad_payload"}}
	}
	return contextPart{value: body}
}

// contextEarnings is the one live-provider leg, and therefore the one that disappears on a
// historical request.
//
// `earningsFor` reads a keyed provider (falling back to the embedded seed) and is cached under
// `earnings:` with no cutoff in the key — it is a LIVE series with no point-in-time semantics at
// all. Serving it against a past cutoff would state that a beat reported after that date was
// visible before it, which is exactly the leakage §1.3 forbids. So a historical request gets an
// honest `null` and a token instead. Do not "fix" this by adding a cutoff to `earningsFor`: that
// series has no as-of dimension to filter on.
func (s *Server) contextEarnings(ctx context.Context, ticker string, historical bool) contextPart {
	if historical {
		return contextPart{value: nil, degraded: []string{"earnings:not_point_in_time"}}
	}
	items, source := s.earningsFor(ctx, ticker)
	if len(items) == 0 {
		return contextPart{value: nil, degraded: []string{"earnings:unavailable"}}
	}
	return contextPart{value: map[string]any{"source": source, "points": items}}
}

// contextFundamentals serves the verified local company dossier only for the ticker it describes.
// It is intentionally absent on historical requests: the seed is a current reference snapshot,
// not a versioned filing store, so attaching it to an old cutoff would create look-ahead leakage.
func (s *Server) contextFundamentals(ticker string, historical bool) contextPart {
	if historical {
		return contextPart{value: nil, degraded: []string{"fundamentals:not_point_in_time"}}
	}
	if !isSeedTicker(ticker) {
		return contextPart{value: nil, degraded: []string{"fundamentals:unavailable"}}
	}
	return contextPart{value: map[string]any{
		"source":          "verified_seed",
		"asOfNote":        seedData["asOfNote"],
		"fundamentals":    seedData["fundamentals"],
		"accountingTraps": seedData["accountingTraps"],
	}}
}

// contextMarket builds the market backdrop, and states plainly what it does not have.
//
// PROJECT_PLAN §2: there is no index/rates/volatility/breadth aggregate in this repo. This lane does
// not add a provider to invent one. What it CAN do honestly is read §9.26's named benchmark proxy
// through the analysis service — keyless, point-in-time, already in the warm set — and derive
// realised volatility arithmetically from that same reading. Everything else is `null` with a token.
func (s *Server) contextMarket(ctx context.Context, asOf string) contextPart {
	out := map[string]any{
		"benchmark":  nil,
		"rates":      nil,
		"volatility": nil,
		"breadth":    nil,
		"basis": "Benchmark is the " + benchmarkProxy + " proxy read from the analysis service. " +
			"Dimensions this deployment cannot compute are null, not estimated.",
	}
	degraded := []string{"market:rates_unavailable", "market:breadth_unavailable"}

	bench := s.contextTechnical(ctx, benchmarkProxy, asOf)
	if bench.value == nil {
		degraded = append(degraded, "market:benchmark_unavailable", "market:volatility_unavailable")
		return contextPart{value: out, degraded: degraded}
	}
	body, _ := bench.value.(map[string]any)
	out["benchmark"] = body

	if pct, ok := realisedVolatilityPct(body); ok {
		out["volatility"] = map[string]any{
			"measure":     "realizedAtrPct",
			"proxyTicker": benchmarkProxy,
			"value":       pct,
			"note":        "Realised range as a percentage of last close. Not an implied-volatility index.",
		}
	} else {
		degraded = append(degraded, "market:volatility_unavailable")
	}
	return contextPart{value: out, degraded: degraded}
}

// realisedVolatilityPct is ATR as a percentage of last close — arithmetic on two numbers the
// analysis service computed, rounded to two places. It is NOT an implied-volatility index and the
// payload says so; that label is the difference between a proxy and a fabrication.
func realisedVolatilityPct(body map[string]any) (float64, bool) {
	ind, _ := body["indicators"].(map[string]any)
	price, _ := body["price"].(map[string]any)
	if ind == nil || price == nil {
		return 0, false
	}
	atr, okA := ind["atr"].(float64)
	last, okL := price["lastClose"].(float64)
	if !okA || !okL || last == 0 {
		return 0, false
	}
	pct := atr / last * 100
	return float64(int(pct*100+0.5)) / 100, true
}
