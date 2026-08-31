package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// context_test.go — the analyst context envelope (`context.go`, contract §6 / §1).
//
// What these tests pin, in order of importance:
//
//   · LEAKAGE. A historical request reaches NO provider. This is enforced two ways at once: a
//     transport that fails the test on any host outside the fakes, and an assertion that every
//     analysis/events request carried the cutoff. Both are positive measurements — nothing here
//     greps a log (§9.44).
//   · CACHE ISOLATION. A live and a historical request for the same ticker and user never share a
//     key, so a live answer can never be served to a point-in-time question.
//   · A guest gets 200 and `subscriptions: []`, never a 401. The analyst must still run signed out.
//   · Every upstream dead is still 200 with a seed-backed, labelled envelope and no panic.
//   · `degraded` is always an array. Never null, whatever happened.

const ctxUID = "u_ctx_1"

// ─────────────────────────────────────────────────────────────────────── fake upstreams

type contextStubs struct {
	analysis, events, journal *httptest.Server

	mu           sync.Mutex
	analysisReqs []url.Values
	eventsReqs   []url.Values
	journalHits  int

	subscriptions []any
	eventsBody    map[string]any
	failJournal   bool
}

func newContextStubs(t *testing.T) *contextStubs {
	t.Helper()
	st := &contextStubs{
		subscriptions: []any{map[string]any{"id": "sub_9f2c1a4b7e0d3856", "ticker": "NVDA"}},
		eventsBody: map[string]any{
			"events":   []any{map[string]any{"id": "evt_1a2b3c4d5e6f7081", "title": "A thing"}},
			"asOf":     "2026-08-15T14:32:00Z",
			"coverage": "ok",
			"degraded": []any{},
		},
	}

	st.analysis = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		st.analysisReqs = append(st.analysisReqs, r.URL.Query())
		st.mu.Unlock()
		ticker := strings.TrimPrefix(r.URL.Path, "/technical-context/")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ticker": ticker, "timeframe": "1D",
			"asOf":              valueOr(r.URL.Query().Get("as_of"), "2026-08-20T09:31:49Z"),
			"source":            "store",
			"sourceIsSynthetic": false,
			"price":             map[string]any{"lastClose": 700.0},
			"indicators":        map[string]any{"atr": 7.0, "rsi": 57.85},
			"structure": map[string]any{
				"trend":             map[string]any{"direction": "up"},
				"recentPriceAction": []any{1, 2, 3},
				"raw":               []any{1, 2, 3},
			},
			"coverage": "ok", "coverageFrom": "2024-08-20T00:00:00Z",
			"coverageTo": "2026-08-19T00:00:00Z",
		})
	}))

	st.events = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		st.eventsReqs = append(st.eventsReqs, r.URL.Query())
		st.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/macro") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"macro": []any{}, "asOf": r.URL.Query().Get("as_of"), "degraded": []any{}})
			return
		}
		_ = json.NewEncoder(w).Encode(st.eventsBody)
	}))

	st.journal = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st.mu.Lock()
		st.journalHits++
		fail := st.failJournal
		st.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "down"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"subscriptions": st.subscriptions})
	}))

	t.Cleanup(func() {
		st.analysis.Close()
		st.events.Close()
		st.journal.Close()
	})
	return st
}

func valueOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// hostGuard is the leakage tripwire. Any request to a host that is not one of the fakes fails the
// test AND errors the call, so an accidental provider read cannot pass quietly.
type hostGuard struct {
	t       *testing.T
	allowed map[string]bool
}

func (g *hostGuard) RoundTrip(r *http.Request) (*http.Response, error) {
	if !g.allowed[r.URL.Host] {
		g.t.Errorf("LEAKAGE: outbound request to %s (%s) on a point-in-time path", r.URL.Host, r.URL)
		return nil, fmt.Errorf("blocked host %s", r.URL.Host)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func newContextGateway(t *testing.T, st *contextStubs) (*Server, http.Handler) {
	t.Helper()
	cfg := loadConfig()
	cfg.AnalysisURL = st.analysis.URL
	cfg.EventsURL = st.events.URL
	cfg.JournalURL = st.journal.URL
	cfg.Secret = "test-secret"
	cfg.CookieName = "nvda_session"
	// No provider keys, and StockTwits off: this test bench is the empty-`.env` shape.
	cfg.AlphaVantageKey, cfg.MarketauxKey = "", ""
	cfg.StockTwitsEnabled = false

	allowed := map[string]bool{}
	for _, u := range []string{st.analysis.URL, st.events.URL, st.journal.URL} {
		if p, err := url.Parse(u); err == nil {
			allowed[p.Host] = true
		}
	}
	srv := &Server{
		cfg:   cfg,
		cache: newCache(),
		http:  &http.Client{Timeout: 5 * time.Second, Transport: &hostGuard{t: t, allowed: allowed}},
	}
	mux := http.NewServeMux()
	srv.registerContextRoutes(mux)
	return srv, mux
}

func getContext(t *testing.T, srv *Server, mux http.Handler, uid, path string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if uid != "" {
		req.Header.Set("Cookie", gatewaySession(srv, uid))
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func degradedTokens(t *testing.T, env map[string]any) []string {
	t.Helper()
	raw, ok := env["degraded"].([]any)
	if !ok {
		t.Fatalf("degraded must be an array, got %T (%v)", env["degraded"], env["degraded"])
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		s, _ := v.(string)
		out = append(out, s)
	}
	return out
}

func hasToken(tokens []string, want string) bool {
	for _, t := range tokens {
		if t == want {
			return true
		}
	}
	return false
}

// ─────────────────────────────────────────────────────────────────────── envelope shape

func TestContextEnvelopeShape(t *testing.T) {
	st := newContextStubs(t)
	srv, mux := newContextGateway(t, st)

	code, env := getContext(t, srv, mux, ctxUID, "/api/context/NVDA?asOf=2026-08-15T14:32:00Z&horizon=20d")
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d: %v", code, env)
	}
	for _, key := range []string{"ticker", "asOf", "horizon", "subscriptions", "tickerContext",
		"marketContext", "degraded"} {
		if _, ok := env[key]; !ok {
			t.Errorf("envelope is missing %q", key)
		}
	}
	if env["ticker"] != "NVDA" {
		t.Errorf("ticker = %v", env["ticker"])
	}
	// §1.4: the resolved cutoff is echoed.
	if env["asOf"] != "2026-08-15T14:32:00Z" {
		t.Errorf("asOf echo = %v", env["asOf"])
	}
	if env["horizon"] != "20d" {
		t.Errorf("horizon = %v", env["horizon"])
	}
	_ = degradedTokens(t, env) // fails if it is not an array

	tc, ok := env["tickerContext"].(map[string]any)
	if !ok {
		t.Fatalf("tickerContext = %T", env["tickerContext"])
	}
	for _, key := range []string{"technical", "recentEvents", "earnings", "fundamentals", "macro"} {
		if _, ok := tc[key]; !ok {
			t.Errorf("tickerContext is missing %q", key)
		}
	}
	// Compaction keeps provenance and coverage and drops only the bulk per-bar arrays.
	tech, _ := tc["technical"].(map[string]any)
	for _, key := range []string{"source", "sourceIsSynthetic", "coverage", "coverageFrom", "coverageTo"} {
		if _, ok := tech[key]; !ok {
			t.Errorf("technical dropped provenance/coverage field %q", key)
		}
	}
	structure, _ := tech["structure"].(map[string]any)
	if _, present := structure["raw"]; present {
		t.Error("technical should not carry the per-bar raw dump")
	}
}

func TestContextFundamentalsAreTickerScopedAndNotPointInTime(t *testing.T) {
	st := newContextStubs(t)
	srv, mux := newContextGateway(t, st)

	_, live := getContext(t, srv, mux, "", "/api/context/NVDA")
	liveTicker, _ := live["tickerContext"].(map[string]any)
	if liveTicker["fundamentals"] == nil {
		t.Fatal("live NVDA context should include its verified seed dossier")
	}

	_, historical := getContext(t, srv, mux, "",
		"/api/context/NVDA?asOf=2026-08-15T14:32:00Z")
	historicalTicker, _ := historical["tickerContext"].(map[string]any)
	if historicalTicker["fundamentals"] != nil {
		t.Fatal("a current seed dossier must not be attached to a historical cutoff")
	}
	if !hasToken(degradedTokens(t, historical), "fundamentals:not_point_in_time") {
		t.Fatalf("historical omission is not labelled: %v", historical["degraded"])
	}

	_, unsupported := getContext(t, srv, mux, "", "/api/context/TSLA")
	unsupportedTicker, _ := unsupported["tickerContext"].(map[string]any)
	if unsupportedTicker["fundamentals"] != nil {
		t.Fatal("TSLA received the NVDA seed dossier")
	}
}

func TestContextMarketContextIsHonestAboutWhatItLacks(t *testing.T) {
	st := newContextStubs(t)
	srv, mux := newContextGateway(t, st)

	_, env := getContext(t, srv, mux, ctxUID, "/api/context/NVDA")
	mc, ok := env["marketContext"].(map[string]any)
	if !ok {
		t.Fatalf("marketContext = %T", env["marketContext"])
	}
	if mc["rates"] != nil || mc["breadth"] != nil {
		t.Errorf("a dimension this repo cannot compute must be null, got rates=%v breadth=%v",
			mc["rates"], mc["breadth"])
	}
	tokens := degradedTokens(t, env)
	for _, want := range []string{"market:rates_unavailable", "market:breadth_unavailable"} {
		if !hasToken(tokens, want) {
			t.Errorf("missing %q in %v", want, tokens)
		}
	}
	bench, _ := mc["benchmark"].(map[string]any)
	if bench == nil || bench["ticker"] != benchmarkProxy {
		t.Errorf("benchmark should be the §9.26 proxy, got %v", mc["benchmark"])
	}
	// Realised volatility is arithmetic on the benchmark reading and says so; it is never presented
	// as an implied-volatility index.
	vol, _ := mc["volatility"].(map[string]any)
	if vol == nil || vol["measure"] != "realizedAtrPct" {
		t.Fatalf("volatility = %v", mc["volatility"])
	}
	if got := vol["value"].(float64); got != 1.0 { // 7.0 / 700.0 * 100
		t.Errorf("realised volatility = %v, want 1", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────── guest path

func TestContextGuestGetsAnEmptyFollowGraphAndNeverA401(t *testing.T) {
	st := newContextStubs(t)
	srv, mux := newContextGateway(t, st)

	code, env := getContext(t, srv, mux, "", "/api/context/NVDA")
	if code != http.StatusOK {
		t.Fatalf("a guest must still get 200, got %d", code)
	}
	subs, ok := env["subscriptions"].([]any)
	if !ok || len(subs) != 0 {
		t.Errorf("subscriptions = %v, want []", env["subscriptions"])
	}
	if !hasToken(degradedTokens(t, env), "journal:guest") {
		t.Errorf("guest path must be labelled: %v", env["degraded"])
	}
	if st.journalHits != 0 {
		t.Errorf("a guest has no cookie to forward; journal was called %d times", st.journalHits)
	}
}

func TestContextSignedInUserGetsTheirFollowGraph(t *testing.T) {
	st := newContextStubs(t)
	srv, mux := newContextGateway(t, st)

	_, env := getContext(t, srv, mux, ctxUID, "/api/context/NVDA")
	subs, _ := env["subscriptions"].([]any)
	if len(subs) != 1 {
		t.Fatalf("subscriptions = %v", env["subscriptions"])
	}
}

func TestContextDistinguishesGuestFromUnreachableJournal(t *testing.T) {
	st := newContextStubs(t)
	st.failJournal = true
	srv, mux := newContextGateway(t, st)

	code, env := getContext(t, srv, mux, ctxUID, "/api/context/NVDA")
	if code != http.StatusOK {
		t.Fatalf("want 200, got %d", code)
	}
	tokens := degradedTokens(t, env)
	if !hasToken(tokens, "journal:unreachable") {
		t.Errorf(`"you follow nothing" and "we could not ask" are different facts: %v`, tokens)
	}
}

// ────────────────────────────────────────────────────────────────────────── offline path

func TestContextOfflineFallsBackToSeedAndStays200(t *testing.T) {
	st := newContextStubs(t)
	srv, mux := newContextGateway(t, st)
	// Everything dead, mid-flight.
	st.analysis.Close()
	st.events.Close()
	st.journal.Close()

	code, env := getContext(t, srv, mux, ctxUID, "/api/context/NVDA")
	if code != http.StatusOK {
		t.Fatalf("an offline envelope is still an envelope: got %d", code)
	}
	tokens := degradedTokens(t, env)
	for _, want := range []string{"analysis:unreachable", "events:seed", "macro:unreachable",
		"journal:unreachable", "market:benchmark_unavailable"} {
		if !hasToken(tokens, want) {
			t.Errorf("missing %q in %v", want, tokens)
		}
	}
	tc, _ := env["tickerContext"].(map[string]any)
	rec, _ := tc["recentEvents"].(map[string]any)
	if rec == nil || rec["source"] != "seed" {
		t.Errorf("§9.33: the offline floor is the embedded seed, got %v", tc["recentEvents"])
	}
	if catalysts, _ := rec["seedCatalysts"].([]any); len(catalysts) == 0 {
		t.Error("seed fallback produced nothing")
	}
	if subs, _ := env["subscriptions"].([]any); subs == nil || len(subs) != 0 {
		t.Errorf("subscriptions = %v, want []", env["subscriptions"])
	}
}

func TestContextOfflineNonSeedTickerAdmitsItHasNothing(t *testing.T) {
	st := newContextStubs(t)
	srv, mux := newContextGateway(t, st)
	st.events.Close()

	_, env := getContext(t, srv, mux, ctxUID, "/api/context/AMD")
	if !hasToken(degradedTokens(t, env), "events:unreachable") {
		t.Errorf("a non-seed ticker has no seed floor and must say so: %v", env["degraded"])
	}
	tc, _ := env["tickerContext"].(map[string]any)
	if tc["recentEvents"] != nil {
		t.Errorf("recentEvents = %v, want null", tc["recentEvents"])
	}
}

// ────────────────────────────────────────────────────────────────────────────── leakage

// TestContextHistoricalRequestReachesNoProvider is the §1.3 / §1.5 leakage test.
//
// The transport fails the test on any host that is not one of the three fakes, so a provider read
// cannot slip through even if a future edit adds one. On top of that, every analysis and events
// request must carry the cutoff, and the one leg with no point-in-time semantics — the EPS series —
// must be absent and labelled rather than served as if it had been visible then.
func TestContextHistoricalRequestReachesNoProvider(t *testing.T) {
	st := newContextStubs(t)
	srv, mux := newContextGateway(t, st)
	const cutoff = "2024-03-01T00:00:00Z"

	code, env := getContext(t, srv, mux, ctxUID, "/api/context/NVDA?asOf="+cutoff)
	if code != http.StatusOK {
		t.Fatalf("want 200 from the store, got %d: %v", code, env)
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.analysisReqs) == 0 || len(st.eventsReqs) == 0 {
		t.Fatalf("expected store reads, got analysis=%d events=%d",
			len(st.analysisReqs), len(st.eventsReqs))
	}
	for _, q := range append(append([]url.Values{}, st.analysisReqs...), st.eventsReqs...) {
		if q.Get("as_of") != cutoff {
			t.Errorf("a store read went out without the cutoff: %v", q.Encode())
		}
	}

	tc, _ := env["tickerContext"].(map[string]any)
	if tc["earnings"] != nil {
		t.Errorf("the EPS series has no as-of dimension and must be omitted historically, got %v",
			tc["earnings"])
	}
	if !hasToken(degradedTokens(t, env), "earnings:not_point_in_time") {
		t.Errorf("the omission must be labelled: %v", env["degraded"])
	}
}

func TestContextEventWindowIsDerivedFromTheCutoffNotTheWallClock(t *testing.T) {
	st := newContextStubs(t)
	srv, mux := newContextGateway(t, st)
	const cutoff = "2024-03-01T00:00:00Z"

	getContext(t, srv, mux, ctxUID, "/api/context/NVDA?asOf="+cutoff)

	st.mu.Lock()
	defer st.mu.Unlock()
	var since string
	for _, q := range st.eventsReqs {
		if v := q.Get("since"); v != "" {
			since = v
		}
	}
	if since != "2024-01-31T00:00:00Z" {
		t.Errorf("since = %q, want the cutoff minus %d days", since, contextEventDays)
	}
}

func TestContextInsufficientCoveragePropagatesUnchanged(t *testing.T) {
	st := newContextStubs(t)
	st.eventsBody = map[string]any{
		"events": []any{}, "asOf": "2020-01-01T00:00:00Z", "coverage": "insufficient",
		"coverageFrom": nil, "coverageTo": nil, "degraded": []any{},
	}
	srv, mux := newContextGateway(t, st)

	_, env := getContext(t, srv, mux, ctxUID, "/api/context/NVDA?asOf=2020-01-01T00:00:00Z")
	tc, _ := env["tickerContext"].(map[string]any)
	rec, _ := tc["recentEvents"].(map[string]any)
	if rec == nil || rec["coverage"] != "insufficient" {
		t.Fatalf("coverage is a result, not an error; got %v", tc["recentEvents"])
	}
}

// ───────────────────────────────────────────────────────────────────── cache isolation

func TestContextLiveAndHistoricalNeverShareACacheKey(t *testing.T) {
	st := newContextStubs(t)
	srv, mux := newContextGateway(t, st)

	_, live := getContext(t, srv, mux, ctxUID, "/api/context/NVDA")
	_, hist := getContext(t, srv, mux, ctxUID, "/api/context/NVDA?asOf=2024-03-01T00:00:00Z")

	if live["asOf"] == hist["asOf"] {
		t.Fatalf("a historical request was served the live answer: %v", live["asOf"])
	}
	if hist["asOf"] != "2024-03-01T00:00:00Z" {
		t.Errorf("historical asOf = %v", hist["asOf"])
	}
	// And the reverse direction: the historical answer must not now be cached under the live key.
	_, live2 := getContext(t, srv, mux, ctxUID, "/api/context/NVDA")
	if live2["asOf"] != live["asOf"] {
		t.Errorf("the live key was overwritten by a historical result: %v", live2["asOf"])
	}
}

func TestContextCacheIsScopedPerUserHorizonAndTicker(t *testing.T) {
	st := newContextStubs(t)
	srv, mux := newContextGateway(t, st)

	getContext(t, srv, mux, ctxUID, "/api/context/NVDA?horizon=5d")
	before := st.journalHits
	getContext(t, srv, mux, ctxUID, "/api/context/NVDA?horizon=5d")
	if st.journalHits != before {
		t.Errorf("an identical request should have been served from cache")
	}
	for _, path := range []string{"/api/context/NVDA?horizon=20d", "/api/context/AMD?horizon=5d"} {
		hits := st.journalHits
		getContext(t, srv, mux, ctxUID, path)
		if st.journalHits == hits {
			t.Errorf("%s reused another request's cache entry", path)
		}
	}
	hits := st.journalHits
	getContext(t, srv, mux, "u_other", "/api/context/NVDA?horizon=5d")
	if st.journalHits == hits {
		t.Error("one user's follow graph was served to another out of the cache")
	}
}

// ─────────────────────────────────────────────────────────────────────── argument checks

func TestContextRejectsAMalformedCutoffAndAnUnknownHorizon(t *testing.T) {
	st := newContextStubs(t)
	srv, mux := newContextGateway(t, st)

	for _, path := range []string{
		"/api/context/NVDA?asOf=yesterday",
		"/api/context/NVDA?horizon=3d",
		"/api/context/NVDA?horizon=5_trading_days", // §Conventions: short form here, long form in §7
	} {
		code, _ := getContext(t, srv, mux, ctxUID, path)
		if code != http.StatusBadRequest {
			t.Errorf("%s -> %d, want 400", path, code)
		}
	}
}

func TestContextRouteRegistersItselfThroughTheWaveZeroSeam(t *testing.T) {
	st := newContextStubs(t)
	srv, _ := newContextGateway(t, st)
	mux := http.NewServeMux()
	// registerEventRoutes runs the registrars this file's init() appended. If a future edit ALSO
	// adds `srv.registerContextRoutes(mux)` to main.go the pattern is registered twice and this
	// panics — which is the point of the check.
	srv.registerEventRoutes(mux)
	req := httptest.NewRequest(http.MethodGet, "/api/context/NVDA", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code == http.StatusNotFound {
		t.Fatal("/api/context/{ticker} is not registered through the seam")
	}
}
