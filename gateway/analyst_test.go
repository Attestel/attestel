package main

// analyst_test.go — Wave 3 Lane 3B.
//
// The negatives matter more than the positives here. An analyst run costs up to eight sequential
// local-model generations, each holding the cross-process model lease, so the tests that earn their
// place are the ones proving the route CANNOT be reached by anything other than a deliberate user
// POST (invariant #4) and that a historical run can never be served a live answer (§1).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// analystUpstream stands in for services/llm. It records every hit so a test can assert that a
// request reached — or did NOT reach — the model service.
//
// It is MUTEX-GUARDED because a run is now a background goroutine (`analystjobs.go`): the test
// goroutine reads the counter while the run goroutine writes it, and an unguarded int here would
// make the whole suite flaky under `-race` for reasons that have nothing to do with the code under
// test. `gate`, when non-nil, blocks the upstream until the test closes it — that is how a test
// observes the state of a run that has NOT finished.
type analystUpstream struct {
	mu   sync.Mutex
	hits int
	body map[string]any
	gate chan struct{}
	srv  *httptest.Server
}

func newAnalystUpstream(t *testing.T, reply map[string]any) *analystUpstream {
	t.Helper()
	u := &analystUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.hits++
		gate := u.gate
		_ = json.NewDecoder(r.Body).Decode(&u.body)
		u.mu.Unlock()
		if gate != nil {
			<-gate
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(reply)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *analystUpstream) hitCount() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits
}

func (u *analystUpstream) lastBody() map[string]any {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.body
}

// hold makes the upstream block until the returned func is called, so a test can inspect a run
// mid-flight — the state that used to be invisible because it lived inside a request.
func (u *analystUpstream) hold() func() {
	u.mu.Lock()
	u.gate = make(chan struct{})
	gate := u.gate
	u.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { close(gate) }) }
}

// analystServer builds a Server whose llm URL points at `llmURL` and whose context provider is
// replaced by a fixture, so no test in this file touches the analysis service, the journal,
// Marketaux, Google News RSS or data.sec.gov. These are unit tests and they make no network call
// beyond the httptest servers they start themselves.
func analystServer(t *testing.T, llmURL string) *Server {
	t.Helper()
	cfg := loadConfig()
	cfg.LLMURL = llmURL
	srv := &Server{cfg: cfg, cache: newCache(), http: &http.Client{Timeout: 5 * time.Second}}

	saved := analystContextProvider
	analystContextProvider = func(s *Server, ctx context.Context, ticker, horizon, asOf string, id identity) map[string]any {
		return map[string]any{"ticker": ticker, "horizon": horizon, "asOf": asOf, "fixture": true}
	}
	t.Cleanup(func() { analystContextProvider = saved })
	return srv
}

func analystPost(t *testing.T, srv *Server, ticker, body string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	srv.registerAnalystRoutes(mux)
	req := httptest.NewRequest(http.MethodPost, "/api/analyst/"+ticker, strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// analystRunGet reads `GET /api/analyst/runs/{id}` through the mux, exactly as the browser does.
func analystRunGet(t *testing.T, srv *Server, runID string) *httptest.ResponseRecorder {
	t.Helper()
	mux := http.NewServeMux()
	srv.registerAnalystRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/analyst/runs/"+runID, nil))
	return rec
}

// analystAwait polls a started run to a terminal state and returns the served body. This is what
// the client does, and it is now the only way to see a run's OUTPUT — which is the point of the
// change: the POST that starts a run no longer carries its result.
func analystAwait(t *testing.T, srv *Server, post *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var started map[string]any
	if err := json.Unmarshal(post.Body.Bytes(), &started); err != nil {
		t.Fatalf("decode POST body: %v (%s)", err, post.Body.String())
	}
	runID, _ := started["runId"].(string)
	if runID == "" {
		t.Fatalf("POST returned no runId: %s", post.Body.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rec := analystRunGet(t, srv, runID)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET run %s = %d: %s", runID, rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode run body: %v", err)
		}
		if st, _ := body["status"].(string); st != analystRunning {
			return body
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("run %s never left the running state", runID)
	return nil
}

var analystOKReply = map[string]any{
	"ticker": "NVDA",
	"theses": map[string]any{"20d": map[string]any{"direction": "neutral"}},
	"forecast": map[string]any{"horizons": map[string]any{
		"20d": map[string]any{"direction": "neutral", "score": 0.0, "abstain": true}}},
	"disclaimer": "from the pipeline",
}

// ─────────────────────────────────────────────────────────────────────── the route contract

func TestAnalystRouteIsRegisteredAndProxiesToTheLLMService(t *testing.T) {
	up := newAnalystUpstream(t, analystOKReply)
	srv := analystServer(t, up.srv.URL)

	rec := analystPost(t, srv, "nvda", `{"horizon":"20d"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST /api/analyst/nvda = %d, want 202 (the run is a job, not a request): %s",
			rec.Code, rec.Body.String())
	}
	res := analystAwait(t, srv, rec)
	if st, _ := res["status"].(string); st != analystDone {
		t.Fatalf("finished run status = %q, want done: %v", st, res)
	}
	if up.hitCount() != 1 {
		t.Fatalf("llm upstream hit %d times, want 1", up.hitCount())
	}
	body := up.lastBody()
	if got, _ := body["ticker"].(string); got != "NVDA" {
		t.Errorf("upstream ticker = %q, want NVDA (the path value must be upper-cased)", got)
	}
	if got, _ := body["horizon"].(string); got != "20d" {
		t.Errorf("upstream horizon = %q, want 20d", got)
	}
	if _, ok := body["context"]; !ok {
		t.Error("the gateway must assemble and send a context envelope")
	}

	if avail, _ := res["available"].(bool); !avail {
		t.Error(`a successful run must be labelled "available": true`)
	}
	// §9.18: the pipeline's own disclosure survives; the gateway only fills a MISSING one.
	if d, _ := res["disclaimer"].(string); d != "from the pipeline" {
		t.Errorf("disclaimer = %q — the gateway must not overwrite the pipeline's own", d)
	}
}

// TestAnalystStampsTheDisclaimerWhenThePipelineOmitsIt — §9.18 requires the string on EVERY
// response, and "the upstream forgot" is not an exemption.
func TestAnalystStampsTheDisclaimerWhenThePipelineOmitsIt(t *testing.T) {
	up := newAnalystUpstream(t, map[string]any{"ticker": "NVDA"})
	srv := analystServer(t, up.srv.URL)

	res := analystAwait(t, srv, analystPost(t, srv, "NVDA", `{}`))
	if d, _ := res["disclaimer"].(string); d != analystDisclaimer {
		t.Errorf("disclaimer = %q, want the §9.18 constant", d)
	}
}

// TestAnalystRejectsAnUnknownHorizon — §9.11's vocabulary is closed, and the gateway must not
// forward a token the pipeline would have to guess at.
func TestAnalystRejectsAnUnknownHorizon(t *testing.T) {
	up := newAnalystUpstream(t, analystOKReply)
	srv := analystServer(t, up.srv.URL)

	rec := analystPost(t, srv, "NVDA", `{"horizon":"20_trading_days"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("horizon=20_trading_days = %d, want 400 — §9.11's REQUEST token is `20d`; "+
			"`20_trading_days` is the OUTPUT spelling and must never be accepted here", rec.Code)
	}
	if up.hitCount() != 0 {
		t.Error("a rejected horizon must not reach the model service")
	}
}

// A malformed cutoff must be rejected before context assembly or an expensive model call. More
// importantly, it must never degrade into a live request that is merely labelled historical.
func TestAnalystRejectsMalformedAsOfBeforeTheModel(t *testing.T) {
	up := newAnalystUpstream(t, analystOKReply)
	srv := analystServer(t, up.srv.URL)

	rec := analystPost(t, srv, "NVDA", `{"asOf":"last Tuesday"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed asOf = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if up.hitCount() != 0 {
		t.Fatalf("malformed asOf reached the model service %d times", up.hitCount())
	}
}

// This exercises the production context provider rather than the unit-test fixture. It protects
// the integration seam that previously called committeeContextFor (a live-data assembler) and
// then merely relabelled its response with the requested historical cutoff.
func TestProductionAnalystContextProviderUsesPointInTimeEnvelope(t *testing.T) {
	st := newContextStubs(t)
	srv, _ := newContextGateway(t, st)
	const cutoff = "2026-08-15T14:32:00Z"

	env := productionAnalystContextProvider(
		srv, context.Background(), "NVDA", "20d", cutoff, identity{},
	)
	if env["asOf"] != cutoff {
		t.Fatalf("asOf = %v, want %s", env["asOf"], cutoff)
	}
	if _, ok := env["tickerContext"].(map[string]any); !ok {
		t.Fatalf("production analyst context did not use the context envelope: %T", env["tickerContext"])
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.analysisReqs) == 0 || len(st.eventsReqs) == 0 {
		t.Fatalf("expected cutoff-aware analysis and event reads, got %d and %d", len(st.analysisReqs), len(st.eventsReqs))
	}
	for _, q := range append(append([]url.Values{}, st.analysisReqs...), st.eventsReqs...) {
		if got := q.Get("as_of"); got != cutoff {
			t.Errorf("point-in-time upstream as_of = %q, want %q", got, cutoff)
		}
	}
}

// ─────────────────────────────────────────────────────────── invariant #4: no work on a GET

// TestAnalystDoesNoWorkOnAGet is the invariant-#4 test that matters.
//
// A GET is what a browser prefetch, a link-prefetch hint, a speculative navigation and an
// over-eager `useEffect` all send. If any of them could start a run, a page load would cause up to
// eight model generations — the exact thing invariant #4 forbids — and nobody would have written a
// line of code to cause it. Two assertions: the mux refuses the method, and the handler called
// DIRECTLY (bypassing the mux, as a future misregistration under a bare path would) also refuses
// it, before touching an upstream.
func TestAnalystDoesNoWorkOnAGet(t *testing.T) {
	up := newAnalystUpstream(t, analystOKReply)
	srv := analystServer(t, up.srv.URL)

	mux := http.NewServeMux()
	srv.registerAnalystRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/analyst/NVDA", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET through the mux = %d, want 405", rec.Code)
	}

	direct := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/analyst/NVDA", nil)
	req.SetPathValue("ticker", "NVDA")
	srv.handleAnalyst(direct, req)
	if direct.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET straight to the handler = %d, want 405", direct.Code)
	}
	if direct.Header().Get("Allow") != http.MethodPost {
		t.Errorf("Allow = %q, want POST", direct.Header().Get("Allow"))
	}
	if up.hitCount() != 0 {
		t.Fatalf("a GET reached the model service %d times — invariant #4 violation", up.hitCount())
	}
}

// ─────────────────────────────────────────────────────────────────── offline degradation

// TestAnalystDegradesWhenTheLLMServiceIsUnreachable — `committee.go`'s posture: 200 with a labelled
// degraded envelope, never a 500 that breaks the page. Reaching this path means the SERVICE is
// gone; the model being down is `analyst.py`'s deterministic stub, which returns 200 with content.
func TestAnalystDegradesWhenTheLLMServiceIsUnreachable(t *testing.T) {
	// A port nothing is listening on. No network egress: 127.0.0.1 refuses immediately.
	srv := analystServer(t, "http://127.0.0.1:1")

	rec := analystPost(t, srv, "NVDA", `{}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("unreachable llm = %d, want 202 — the failure is reported on the run, not here", rec.Code)
	}
	res := analystAwait(t, srv, rec)
	// THE FINDING THIS TEST NOW COVERS: a run that could not finish says so. It does not disappear
	// with the request that started it, and the client is not left to infer an outage from a card
	// that quietly reverted to "Not yet assessed".
	if st, _ := res["status"].(string); st != analystFailed {
		t.Errorf("failed run status = %q, want failed", st)
	}
	if msg, _ := res["error"].(string); msg == "" {
		t.Error("a failed run must carry a reason a UI can show")
	}
	if avail, _ := res["available"].(bool); avail {
		t.Error(`an unreachable llm service must report "available": false`)
	}
	if deg, _ := res["degraded"].(bool); !deg {
		t.Error(`the degraded envelope must be labelled "degraded": true`)
	}
	if d, _ := res["disclaimer"].(string); d != analystDisclaimer {
		t.Error("§9.18: the disclosure travels on the degraded path too")
	}
	// Nothing was cached, so the next request retries rather than serving the failure for 30 min.
	if _, ok := srv.cache.get(analystCacheKey("NVDA", "", "", "guest")); ok {
		t.Error("a degraded response must not be cached")
	}
}

// ───────────────────────────────────────────────────── cache: live and historical never mix

// TestAnalystCacheKeysIsolateLiveFromHistorical is §1 at the cache layer.
//
// A live answer served for a past cutoff is the present leaking into the past — the single failure
// mode the whole point-in-time contract exists to prevent, and a cache is exactly where it happens
// silently. The keys must differ on `asOf`, on `horizon` and on `uid`.
func TestAnalystCacheKeysIsolateLiveFromHistorical(t *testing.T) {
	live := analystCacheKey("NVDA", "20d", "", "u1")
	hist := analystCacheKey("NVDA", "20d", "2026-08-15T14:32:00Z", "u1")
	if live == hist {
		t.Fatalf("live and historical share the cache key %q — §1 violation", live)
	}
	if !strings.Contains(live, "|live|") {
		t.Errorf("the live key must be self-describing, got %q", live)
	}
	for _, pair := range [][2]string{
		{analystCacheKey("NVDA", "1d", "", "u1"), analystCacheKey("NVDA", "5d", "", "u1")},
		{analystCacheKey("NVDA", "1d", "", "u1"), analystCacheKey("NVDA", "1d", "", "u2")},
		{analystCacheKey("NVDA", "1d", "", "u1"), analystCacheKey("AMD", "1d", "", "u1")},
	} {
		if pair[0] == pair[1] {
			t.Errorf("keys collide: %q", pair[0])
		}
	}
	if !strings.HasPrefix(live, "analyst:") {
		t.Errorf("the key must use the new `analyst:` prefix, got %q", live)
	}
}

// TestAnalystServesTheSecondIdenticalRequestFromCache — and, more importantly, proves the second
// request does NOT reach the model service. Every cache hit here is eight model generations that
// did not happen.
func TestAnalystServesTheSecondIdenticalRequestFromCache(t *testing.T) {
	up := newAnalystUpstream(t, analystOKReply)
	srv := analystServer(t, up.srv.URL)

	first := analystPost(t, srv, "NVDA", `{"horizon":"5d"}`)
	if first.Header().Get("X-Cache") != "MISS" {
		t.Fatalf("first X-Cache = %q, want MISS", first.Header().Get("X-Cache"))
	}
	analystAwait(t, srv, first) // the entry is written when the RUN finishes, not when the POST returns

	second := analystPost(t, srv, "NVDA", `{"horizon":"5d"}`)
	if second.Header().Get("X-Cache") != "HIT" {
		t.Fatalf("second X-Cache = %q, want HIT", second.Header().Get("X-Cache"))
	}
	if second.Code != http.StatusOK {
		t.Fatalf("a cache hit = %d, want 200 with the finished envelope", second.Code)
	}
	var hit map[string]any
	_ = json.Unmarshal(second.Body.Bytes(), &hit)
	if st, _ := hit["status"].(string); st != analystDone {
		t.Errorf("cache hit status = %q, want done — the client tells the two POST outcomes apart "+
			"on this field alone", st)
	}
	if up.hitCount() != 1 {
		t.Fatalf("llm upstream hit %d times, want 1 — the cache did not hold", up.hitCount())
	}

	// A DIFFERENT cutoff is a different run, and must reach the pipeline.
	hist := analystPost(t, srv, "NVDA", `{"horizon":"5d","asOf":"2026-08-15T14:32:00Z"}`)
	if hist.Code != http.StatusAccepted {
		t.Fatalf("historical run = %d", hist.Code)
	}
	analystAwait(t, srv, hist)
	if up.hitCount() != 2 {
		t.Fatalf("llm upstream hit %d times, want 2 — a historical cutoff must not be served the "+
			"live cache entry", up.hitCount())
	}
}

// ───────────────────────────────────────────────────────────────────────── the guest path

// TestAnalystWorksForAGuest — invariant #1: the whole stack runs with no configuration at all, and
// that includes no session cookie. A guest gets a run under its own cache namespace.
func TestAnalystWorksForAGuest(t *testing.T) {
	up := newAnalystUpstream(t, analystOKReply)
	srv := analystServer(t, up.srv.URL)

	rec := analystPost(t, srv, "NVDA", `{}`) // no cookie is set by analystPost
	if rec.Code != http.StatusAccepted {
		t.Fatalf("guest POST = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	analystAwait(t, srv, rec)
	if _, ok := srv.cache.get(analystCacheKey("NVDA", "", "", "guest")); !ok {
		t.Error("a guest run must be cached under the `guest` namespace, not under an empty uid")
	}
}

// TestAnalystTTLIsNamedAndOverridable — the `COMMITTEE_TTL` precedent, so an operator can tune one
// expensive route without a rebuild.
func TestAnalystTTLIsNamedAndOverridable(t *testing.T) {
	cfg := loadConfig()
	if got := cfg.AnalystTTL(); got != 30*time.Minute {
		t.Errorf("default AnalystTTL = %v, want 30m (the COMMITTEE_TTL precedent)", got)
	}
	t.Setenv("ANALYST_TTL", "90s")
	if got := cfg.AnalystTTL(); got != 90*time.Second {
		t.Errorf("ANALYST_TTL=90s gave %v", got)
	}
	_ = os.Unsetenv("ANALYST_TTL")
}

// TestAnalystDisclaimerIsTheContractString pins §9.18's text on the Go side. `explore.go` declares
// it (Lane 2C shipped first, before `analyst.py` existed); this asserts the two lanes agree, and
// `services/llm/tests/test_analyst.py` asserts the Python constant matches this same literal.
func TestAnalystDisclaimerIsTheContractString(t *testing.T) {
	const want = "Analytical context, not investment advice and not a recommendation. " +
		"Read-throughs are hypotheses about how an event might reach a company, never a verdict on it."
	if analystDisclaimer != want {
		t.Fatalf("analystDisclaimer drifted from §9.18:\n got: %q\nwant: %q", analystDisclaimer, want)
	}
}

// ──────────────────────────────────────────────────── the async contract (the 504 this fixes)

// TestAnalystPostAnswersWhileTheRunIsStillGoing is the regression test for the production failure.
//
// The pipeline is up to eight sequential local-model generations. Held inside the request, it
// outlived `deploy/nginx.conf.template`'s `proxy_read_timeout 120s`, so the browser got a 504 and
// the card reverted to "Not yet assessed" while the run carried on unobserved. The POST must
// therefore return with the model still working — that is the property, and it is asserted here
// against an upstream that is deliberately still blocked when the assertion runs.
func TestAnalystPostAnswersWhileTheRunIsStillGoing(t *testing.T) {
	up := newAnalystUpstream(t, analystOKReply)
	release := up.hold()
	defer release()
	srv := analystServer(t, up.srv.URL)

	rec := analystPost(t, srv, "NVDA", `{"horizon":"5d"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("POST = %d, want 202 while the run is still in flight: %s", rec.Code, rec.Body.String())
	}
	var started map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &started)
	runID, _ := started["runId"].(string)
	if runID == "" {
		t.Fatal("the POST must name the run it started")
	}
	if st, _ := started["status"].(string); st != analystRunning {
		t.Errorf("POST status = %q, want running", st)
	}

	// The run is observable while it runs — the state the synchronous version had no way to express.
	var status map[string]any
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_ = json.Unmarshal(analystRunGet(t, srv, runID).Body.Bytes(), &status)
		if st, _ := status["status"].(string); st == analystRunning {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if st, _ := status["status"].(string); st != analystRunning {
		t.Fatalf("in-flight run status = %q, want running", st)
	}

	release()
	if res := analystAwait(t, srv, rec); res["status"] != analystDone {
		t.Fatalf("run did not finish after the upstream was released: %v", res["status"])
	}
}

// TestAnalystSecondPostAttachesToTheRunningRun — invariant #4 at the concurrency layer. Two runs
// for the same key would queue against each other for the cross-process model lease and starve the
// interactive path, so the second POST must join the first rather than start a rival.
func TestAnalystSecondPostAttachesToTheRunningRun(t *testing.T) {
	up := newAnalystUpstream(t, analystOKReply)
	release := up.hold()
	defer release()
	srv := analystServer(t, up.srv.URL)

	first := analystPost(t, srv, "NVDA", `{"horizon":"5d"}`)
	var a map[string]any
	_ = json.Unmarshal(first.Body.Bytes(), &a)

	// Wait until the run has actually reached the upstream, so "one hit" cannot pass by accident.
	deadline := time.Now().Add(2 * time.Second)
	for up.hitCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	second := analystPost(t, srv, "NVDA", `{"horizon":"5d"}`)
	var b map[string]any
	_ = json.Unmarshal(second.Body.Bytes(), &b)
	if a["runId"] != b["runId"] {
		t.Fatalf("second POST started a rival run: %v vs %v", a["runId"], b["runId"])
	}
	if attached, _ := b["attached"].(bool); !attached {
		t.Error("a POST that joined an in-flight run must say so")
	}
	if up.hitCount() != 1 {
		t.Fatalf("llm upstream hit %d times, want 1 — the second POST started another run", up.hitCount())
	}

	release()
	analystAwait(t, srv, first)
}

// TestAnalystStatusReadStartsNothing — the poll route is the one thing in this lane a browser may
// call repeatedly, and it earns that only by being incapable of causing a generation.
func TestAnalystStatusReadStartsNothing(t *testing.T) {
	up := newAnalystUpstream(t, analystOKReply)
	srv := analystServer(t, up.srv.URL)

	rec := analystRunGet(t, srv, "analyst_deadbeef")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET on an unknown run = %d, want 404", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if code, _ := body["code"].(string); code != "unknown_run" {
		t.Errorf("unknown run code = %q, want unknown_run — the client explains it, it does not re-POST", code)
	}

	// Poll it a few more times for good measure: still no model service contact, ever.
	for i := 0; i < 5; i++ {
		analystRunGet(t, srv, "analyst_deadbeef")
	}
	if up.hitCount() != 0 {
		t.Fatalf("the status read reached the model service %d times — invariant #4 violation", up.hitCount())
	}
}

// TestAnalystRunRecordsAreReaped — an in-memory registry that only grows is a leak. Finished
// records outlive the client's poll loop and nothing more; the envelope itself lives in the cache.
func TestAnalystRunRecordsAreReaped(t *testing.T) {
	js := newAnalystJobs()
	job, owned := js.begin("analyst:NVDA|5d|live|guest", "NVDA", "5d", "")
	if !owned {
		t.Fatal("the first caller must own the run")
	}
	js.finish(job.ID, map[string]any{"ticker": "NVDA"}, "")

	js.mu.Lock()
	js.byID[job.ID].EndedAt = time.Now().Add(-analystRunRetention - time.Minute)
	js.mu.Unlock()

	if _, ok := js.get(job.ID); ok {
		t.Fatal("a finished run past its retention must be reaped")
	}
}
