package main

// feeds_test.go — Wave 2 Lane 2C.
//
// NO TEST HERE TOUCHES THE NETWORK. `newsFor` calls Marketaux, Google News RSS and SEC EDGAR, so
// "don't configure a key" is not enough — `deniedTransport` fails every host that is not one of
// this file's own `httptest` servers, and records every host that was attempted so a test can
// assert a NEGATIVE (nothing reached the llm) as a positive measurement rather than as the absence
// of a log line (§9.44).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// ───────────────────────────────────────────────────────────────────────────────── scaffolding

// deniedTransport allows exactly the test's own fakes and fails everything else.
type deniedTransport struct {
	allow map[string]bool
	base  http.RoundTripper

	mu       sync.Mutex
	attempts []string
}

func (d *deniedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	d.mu.Lock()
	d.attempts = append(d.attempts, r.URL.Host)
	d.mu.Unlock()
	if d.allow[r.URL.Host] {
		return d.base.RoundTrip(r)
	}
	return nil, fmt.Errorf("network denied in test: %s", r.URL.String())
}

func (d *deniedTransport) attempted() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.attempts...)
}

// feedsFixture is one wired-up gateway plus the fakes it is allowed to reach.
type feedsFixture struct {
	srv       *Server
	transport *deniedTransport
	mux       *http.ServeMux
}

// newFeedsFixture builds a Server pointed at the given fake upstreams. `events` or `journal` may be
// "" to mean "not deployed"; a CLOSED httptest server's URL means "deployed and unreachable".
func newFeedsFixture(t *testing.T, events, journal string) *feedsFixture {
	t.Helper()

	cfg := loadConfig()
	cfg.EventsURL = events
	cfg.JournalURL = journal
	cfg.AuthURL = "http://auth.invalid:8099"
	cfg.LLMURL = "http://llm.invalid:8002"
	cfg.Secret = "test-secret"
	cfg.CookieName = "nvda_session"
	cfg.Tickers = parseTickers("NVDA,GOOGL,TSLA")
	// Keys are cleared explicitly rather than assumed absent: a developer with a real key in their
	// shell must not turn a unit test into a live provider call.
	cfg.AlphaVantageKey, cfg.MarketauxKey = "", ""
	cfg.StockTwitsEnabled = false

	allow := map[string]bool{}
	for _, raw := range []string{events, journal} {
		if raw == "" {
			continue
		}
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			allow[u.Host] = true
		}
	}
	tr := &deniedTransport{allow: allow, base: http.DefaultTransport}

	srv := &Server{cfg: cfg, cache: newCache(), http: &http.Client{Transport: tr, Timeout: 5 * time.Second}}
	mux := http.NewServeMux()
	srv.registerEventRoutes(mux)
	return &feedsFixture{srv: srv, transport: tr, mux: mux}
}

// get issues a request through the real mux, so every test exercises the routes as they are
// actually registered through the Wave 0 seam.
func (f *feedsFixture) get(t *testing.T, path, uid string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	return f.getCtx(t, context.Background(), path, uid)
}

func (f *feedsFixture) getCtx(t *testing.T, ctx context.Context, path, uid string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
	if uid != "" {
		req.AddCookie(&http.Cookie{Name: f.srv.cfg.CookieName, Value: testSessionToken(f.srv.cfg.Secret, uid)})
	}
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("GET %s: body is not JSON (%v): %s", path, err, rec.Body.String())
	}
	return rec, body
}

// testSessionToken mints the same token `auth` issues, using the shared verifier already in
// auth.go. Signing it here rather than stubbing `userIDFrom` means the tests exercise the real
// cookie path.
func testSessionToken(secret, uid string) string {
	raw, _ := json.Marshal(sessionPayload{
		UID: uid, IAT: time.Now().Unix(), Exp: time.Now().Add(time.Hour).Unix(),
	})
	body := b64.EncodeToString(raw)
	return body + "." + signPayload(secret, body)
}

// journalFake serves §2.1's `GET /subscriptions` for the given tickers.
func journalFake(t *testing.T, byUser map[string][]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/subscriptions" {
			http.NotFound(w, r)
			return
		}
		uid := ""
		if c, err := r.Cookie("nvda_session"); err == nil {
			uid, _ = parseToken("test-secret", c.Value)
		}
		if uid == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "auth required"})
			return
		}
		subs := []any{}
		for i, tk := range byUser[uid] {
			subs = append(subs, map[string]any{
				"id": fmt.Sprintf("sub_%d", i), "ticker": tk, "priority": i,
				"notificationsEnabled": true, "notificationLevel": "material", "source": "manual",
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{"subscriptions": subs})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// eventsFake serves §3.11's `GET /events`, `GET /events/{id}` and `GET /macro` from a fixture.
type eventsFake struct {
	events []map[string]any
	macro  []map[string]any
	status int // non-zero ⇒ every /events call returns this status
	delay  time.Duration
	server *httptest.Server

	mu      sync.Mutex
	queries []url.Values
}

func newEventsFake(t *testing.T, events []map[string]any) *eventsFake {
	t.Helper()
	f := &eventsFake{events: events}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *eventsFake) URL() string { return f.server.URL }

func (f *eventsFake) lastQuery() url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.queries) == 0 {
		return url.Values{}
	}
	return f.queries[len(f.queries)-1]
}

func (f *eventsFake) serve(w http.ResponseWriter, r *http.Request) {
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	switch {
	case r.URL.Path == "/events":
		f.mu.Lock()
		f.queries = append(f.queries, r.URL.Query())
		f.mu.Unlock()
		if f.status != 0 {
			writeJSON(w, f.status, map[string]any{"detail": "upstream is unwell"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"events": f.events, "asOf": "2026-08-20T00:00:00Z",
			"nextCursor": nil, "coverage": "ok", "degraded": []string{},
		})
	case r.URL.Path == "/macro":
		writeJSON(w, http.StatusOK, map[string]any{
			"macro": f.macro, "asOf": "2026-08-20T00:00:00Z", "degraded": []string{},
		})
	case strings.HasPrefix(r.URL.Path, "/events/"):
		id := strings.TrimPrefix(r.URL.Path, "/events/")
		for _, ev := range f.events {
			if ev["id"] == id {
				writeJSON(w, http.StatusOK, map[string]any{
					"event": ev, "documents": ev["sources"], "tickers": ev["tickers"],
					"asOf": "2026-08-20T00:00:00Z", "degraded": []string{},
				})
				return
			}
		}
		writeJSON(w, http.StatusNotFound, map[string]any{
			"detail": "no such event at this cutoff: " + id})
	default:
		http.NotFound(w, r)
	}
}

// fixtureEvent builds a §3.12 event. `enriched` controls whether the per-ticker enrichment fields
// are present, so the same helper covers both halves of the shipped default.
func fixtureEvent(id, eventType string, published string, importance, novelty float64,
	tier string, docs int, tickers []map[string]any) map[string]any {
	return map[string]any{
		"id": id, "eventType": eventType, "title": "Title " + id, "summary": "Summary " + id,
		"occurredAt": published, "publishedAt": published, "firstSeenAt": published,
		"sourceTier": tier, "officialConfirmed": tier == "official",
		"importance": importance, "novelty": novelty, "documentCount": docs,
		"enrichmentState": "raw", "tickers": tickers,
		"sources": []map[string]any{{
			"documentId": "doc_" + id, "provider": "sec-edgar", "sourceTier": tier,
			"url": "https://example.invalid/" + id, "publishedAt": published,
		}},
		"audit": nil,
	}
}

// rawTicker is a ticker link as it comes back with enrichment OFF (the shipped default): relevance
// and isPrimary only. §9.15 keeps those two write-once and code-owned, so they are never gated.
func rawTicker(sym string, relevance float64, primary bool) map[string]any {
	return map[string]any{"ticker": sym, "relevance": relevance, "isPrimary": primary}
}

// enrichedTicker adds the fields `services/events` emits only inside `if enriched:`.
func enrichedTicker(sym string, relevance float64, primary bool, dims []string, transmission string) map[string]any {
	m := rawTicker(sym, relevance, primary)
	m["potentialDirection"] = "positive"
	m["potentialStrength"] = 0.6
	m["expectedHorizon"] = "medium_term"
	if len(dims) > 0 {
		m["affectedDimensions"] = dims
	}
	if transmission != "" {
		m["transmission"] = transmission
	}
	return m
}

func items(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("response has no items array: %v", body)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		m, _ := r.(map[string]any)
		out = append(out, m)
	}
	return out
}

func degradedOf(t *testing.T, body map[string]any) []string {
	t.Helper()
	raw, _ := body["degraded"].([]any)
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		out = append(out, fmt.Sprint(r))
	}
	return out
}

func hasString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// poisonNewsCache pre-seeds `news:{ticker}` with an EMPTY result so `newsFor` returns nothing
// without a single network call. This is how step 5 of the cascade is reached deterministically:
// for a seeded ticker `newsFor` has its own seed branch, so "no network" alone never empties it.
func poisonNewsCache(s *Server, tickers ...string) {
	for _, tk := range tickers {
		data, _ := json.Marshal([]map[string]any{})
		wrapped, _ := json.Marshal(cachedResult{Source: "none", Data: data})
		s.cache.set("news:"+strings.ToUpper(tk), wrapped, time.Hour)
	}
}

// ───────────────────────────────────────────────────────────── the routes arrive via the seam

// TestFeedRoutesRegisterThroughTheWave0Seam: this lane edits neither main.go nor routeseams.go, so
// the ONLY way its routes can resolve is the registrar attached by feeds.go's init(). A fresh mux
// that has been through no other registration proves it.
func TestFeedRoutesRegisterThroughTheWave0Seam(t *testing.T) {
	f := newFeedsFixture(t, "", "")
	for _, path := range []string{"/api/following", "/api/explore", "/api/events/evt_1"} {
		rec := httptest.NewRecorder()
		f.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code == http.StatusNotFound {
			t.Fatalf("GET %s = 404 — the event seam did not register it", path)
		}
	}
}

// TestChangedIsNotRegisteredByThisLane — §9.1, restated now that Lane 5B has landed the route.
//
// This test used to assert a 404 on `/api/changed`, which was the right assertion while the route
// did not exist. `gateway/changed.go` now attaches it through the SAME event seam this fixture
// runs, so the path resolves and a 404 assertion would fail for the correct reason. What §9.1
// actually forbids is unchanged and is what is asserted here instead: `feeds.go` must not be the
// file that registers it. Two `mux.HandleFunc` calls on one pattern panic at startup, and the way
// that happens is a second file quietly adding one.
//
// Source-level, because that is the only level at which "which FILE registered it" is observable
// once both files share a seam.
func TestChangedIsNotRegisteredByThisLane(t *testing.T) {
	source, err := os.ReadFile("feeds.go")
	if err != nil {
		t.Fatalf("reading feeds.go: %v", err)
	}
	if strings.Contains(string(source), `"GET /api/changed"`) {
		t.Fatal("feeds.go registers GET /api/changed — §9.1 assigns that route to Lane 5B alone " +
			"(gateway/changed.go), and a second registration panics the mux at startup")
	}

	// And it does resolve, through 5B's registrar on the shared seam.
	f := newFeedsFixture(t, "", "")
	if _, pattern := f.mux.Handler(httptest.NewRequest(http.MethodGet, "/api/changed", nil)); pattern == "" {
		t.Fatal("GET /api/changed resolves to no pattern — Lane 5B's registrar did not fire")
	}
}

// ─────────────────────────────────────────────── cascade steps 1 and 2: whose feed is this

func TestFollowingUsesTheUsersSubscriptions(t *testing.T) {
	journal := journalFake(t, map[string][]string{"u1": {"AMD", "TSM"}})
	ev := newEventsFake(t, nil)
	f := newFeedsFixture(t, ev.URL(), journal.URL)

	_, body := f.get(t, "/api/following", "u1")
	if d := degradedOf(t, body); len(d) != 0 {
		t.Fatalf("degraded = %v, want empty for a signed-in user with subscriptions", d)
	}
	if got := ev.lastQuery().Get("tickers"); got != "AMD,TSM" {
		t.Fatalf("tickers pushed to :8004 = %q, want %q", got, "AMD,TSM")
	}
}

func TestFollowingGuestFallsBackToTheUniverse(t *testing.T) {
	journal := journalFake(t, nil)
	ev := newEventsFake(t, nil)
	f := newFeedsFixture(t, ev.URL(), journal.URL)

	_, body := f.get(t, "/api/following", "")
	if d := degradedOf(t, body); !hasString(d, degradedSubscriptionsGuest) {
		t.Fatalf("degraded = %v, want %q", d, degradedSubscriptionsGuest)
	}
	if got := ev.lastQuery().Get("tickers"); got != "NVDA,GOOGL,TSLA" {
		t.Fatalf("tickers = %q, want the configured universe", got)
	}
}

func TestFollowingJournalUnreachableFallsBackToTheUniverse(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close() // deployed, and refusing connections

	ev := newEventsFake(t, nil)
	f := newFeedsFixture(t, ev.URL(), deadURL)
	f.transport.allow[mustHost(t, deadURL)] = true // the denial must come from the socket, not the test

	_, body := f.get(t, "/api/following", "u1")
	if d := degradedOf(t, body); !hasString(d, degradedSubscriptionsUnreachable) {
		t.Fatalf("degraded = %v, want %q", d, degradedSubscriptionsUnreachable)
	}
}

// TestFollowingEmptySubscriptionsIsNotGuest: a signed-in user who follows nothing and a guest are
// different states and the payload says which. Collapsing them would make the one case that should
// offer onboarding indistinguishable from the one that should offer sign-in.
func TestFollowingEmptySubscriptionsIsNotGuest(t *testing.T) {
	journal := journalFake(t, map[string][]string{"u1": {}})
	ev := newEventsFake(t, nil)
	f := newFeedsFixture(t, ev.URL(), journal.URL)

	_, body := f.get(t, "/api/following", "u1")
	d := degradedOf(t, body)
	if !hasString(d, degradedSubscriptionsEmpty) {
		t.Fatalf("degraded = %v, want %q", d, degradedSubscriptionsEmpty)
	}
	if hasString(d, degradedSubscriptionsGuest) {
		t.Fatalf("degraded = %v — an empty follow graph must not be reported as a guest", d)
	}
}

// ───────────────────────────────────────────── cascade steps 3, 4 and 5: where the items come from

func TestFollowingServesCanonicalEvents(t *testing.T) {
	journal := journalFake(t, map[string][]string{"u1": {"NVDA"}})
	ev := newEventsFake(t, []map[string]any{
		fixtureEvent("evt_a", "product_launch", "2026-08-19T12:00:00Z", 0.8, 0.9, "official", 4,
			[]map[string]any{rawTicker("NVDA", 1.0, true)}),
	})
	f := newFeedsFixture(t, ev.URL(), journal.URL)

	_, body := f.get(t, "/api/following", "u1")
	got := items(t, body)
	if len(got) != 1 {
		t.Fatalf("items = %d, want 1", len(got))
	}
	if got[0]["origin"] != feedOriginEvents || got[0]["dataState"] != dataStateLive {
		t.Fatalf("origin/dataState = %v/%v, want %q/%q",
			got[0]["origin"], got[0]["dataState"], feedOriginEvents, dataStateLive)
	}
	// The time boundary: ISO in, unix int64 out, original string kept alongside.
	if got[0]["publishedAt"].(float64) != 1787140800 {
		t.Fatalf("publishedAt = %v, want unix 1787140800", got[0]["publishedAt"])
	}
	if got[0]["publishedAtISO"] != "2026-08-19T12:00:00Z" {
		t.Fatalf("publishedAtISO = %v — the source string must survive", got[0]["publishedAtISO"])
	}
}

// TestFollowingEvents500FallsBackToNews — cascade step 4.
func TestFollowingEvents500FallsBackToNews(t *testing.T) {
	journal := journalFake(t, map[string][]string{"u1": {"NVDA"}})
	ev := newEventsFake(t, nil)
	ev.status = http.StatusInternalServerError
	f := newFeedsFixture(t, ev.URL(), journal.URL)

	_, body := f.get(t, "/api/following", "u1")
	if d := degradedOf(t, body); !hasString(d, degradedEventsUnreachable) {
		t.Fatalf("degraded = %v, want %q", d, degradedEventsUnreachable)
	}
	got := items(t, body)
	if len(got) == 0 {
		t.Fatal("the degraded feed is empty — NVDA has embedded seed news and must answer")
	}
	for _, it := range got {
		if it["origin"] != feedOriginNews && it["origin"] != feedOriginSeed {
			t.Fatalf("origin = %v, want %q or %q", it["origin"], feedOriginNews, feedOriginSeed)
		}
		// Fields the events service computes are ABSENT, not zero. A headline was never scored,
		// and `0.0` would read as "scored, and scored zero".
		if _, present := it["importance"]; present {
			t.Fatalf("a degraded item carries importance %v — it must be absent, not invented", it["importance"])
		}
		if _, present := it["novelty"]; present {
			t.Fatal("a degraded item carries novelty — it must be absent")
		}
		if _, present := it["officialConfirmed"]; present {
			t.Fatal("a degraded item claims an officialConfirmed status nothing computed")
		}
		for _, raw := range it["tickers"].([]any) {
			if _, present := raw.(map[string]any)["relevance"]; present {
				t.Fatal("a degraded item claims a relevance — that field is services/events' (§9.15)")
			}
		}
	}
}

func TestFollowingDoesNotCacheTransientEventsOutage(t *testing.T) {
	journal := journalFake(t, map[string][]string{"u1": {"NVDA"}})
	ev := newEventsFake(t, []map[string]any{})
	ev.status = http.StatusServiceUnavailable
	f := newFeedsFixture(t, ev.URL(), journal.URL)

	first, body := f.get(t, "/api/following", "u1")
	if first.Header().Get("X-Cache") != "BYPASS" {
		t.Fatalf("first X-Cache = %q, want BYPASS", first.Header().Get("X-Cache"))
	}
	if d := degradedOf(t, body); !hasString(d, degradedEventsUnreachable) {
		t.Fatalf("first degraded = %v, want %q", d, degradedEventsUnreachable)
	}

	ev.status = 0
	second, body := f.get(t, "/api/following", "u1")
	if second.Header().Get("X-Cache") != "MISS" {
		t.Fatalf("recovery X-Cache = %q, want MISS", second.Header().Get("X-Cache"))
	}
	if d := degradedOf(t, body); hasString(d, degradedEventsUnreachable) {
		t.Fatalf("recovery still reports cached outage: %v", d)
	}
}

// TestFollowingEventsTimeoutFallsBackToNews: a hung upstream and a refused connection are the same
// answer — no events — and must produce the same degraded marker.
func TestFollowingEventsTimeoutFallsBackToNews(t *testing.T) {
	journal := journalFake(t, map[string][]string{"u1": {"NVDA"}})
	ev := newEventsFake(t, nil)
	ev.delay = 400 * time.Millisecond
	f := newFeedsFixture(t, ev.URL(), journal.URL)

	// The handler derives its context from the request's, so an earlier deadline wins over
	// feedsTimeout and the test does not have to wait 20 seconds to prove the point.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	_, body := f.getCtx(t, ctx, "/api/following", "u1")

	if d := degradedOf(t, body); !hasString(d, degradedEventsUnreachable) {
		t.Fatalf("degraded = %v, want %q on a timed-out :8004", d, degradedEventsUnreachable)
	}
}

// TestFollowingUnconfiguredEventsService: EVENTS_URL empty is "not deployed", which is a different
// fact from "deployed and failing" and gets its own marker.
func TestFollowingUnconfiguredEventsService(t *testing.T) {
	journal := journalFake(t, map[string][]string{"u1": {"NVDA"}})
	f := newFeedsFixture(t, "", journal.URL)

	_, body := f.get(t, "/api/following", "u1")
	if d := degradedOf(t, body); !hasString(d, degradedEventsUnconfigured) {
		t.Fatalf("degraded = %v, want %q", d, degradedEventsUnconfigured)
	}
}

// TestFollowingOfflineFloorIsTheEmbeddedSeed — §9.33, and the single most important test in this
// file. `newsFor` is Marketaux + Google News RSS + SEC EDGAR: ALL NETWORK. With the network denied
// AND its cache holding an empty result, it produces nothing, and the cascade must land on the
// EMBEDDED SEED rather than on an empty page. Invariant #1's offline half is exactly this.
func TestFollowingOfflineFloorIsTheEmbeddedSeed(t *testing.T) {
	journal := journalFake(t, map[string][]string{"u1": {"NVDA"}})
	dead := newEventsFake(t, nil)
	deadURL := dead.URL()
	dead.server.Close()

	f := newFeedsFixture(t, deadURL, journal.URL)
	f.transport.allow[mustHost(t, deadURL)] = true
	poisonNewsCache(f.srv, "NVDA")

	_, body := f.get(t, "/api/following", "u1")
	d := degradedOf(t, body)
	if !hasString(d, degradedEventsUnreachable) || !hasString(d, degradedNewsUnavailable) {
		t.Fatalf("degraded = %v, want both %q and %q", d, degradedEventsUnreachable, degradedNewsUnavailable)
	}
	got := items(t, body)
	if len(got) == 0 {
		t.Fatal("the offline floor returned nothing — the embedded seed is compiled into the " +
			"binary and cannot fail; invariant #1's offline half must answer")
	}
	for _, it := range got {
		if it["dataState"] != dataStateSeed {
			t.Fatalf("dataState = %v, want %q — seed data must be labelled as such", it["dataState"], dataStateSeed)
		}
		if it["origin"] != feedOriginSeed {
			t.Fatalf("origin = %v, want %q", it["origin"], feedOriginSeed)
		}
	}
}

// TestFollowingOfflineSeedLabelSurvivesNewsFor: the other offline shape. With the network denied
// but the news cache cold, `newsFor` reaches its OWN seed branch for the seeded ticker. Those items
// must still be labelled seed — the label is about the data, not about which function produced it.
func TestFollowingOfflineSeedLabelSurvivesNewsFor(t *testing.T) {
	journal := journalFake(t, map[string][]string{"u1": {"NVDA"}})
	dead := newEventsFake(t, nil)
	deadURL := dead.URL()
	dead.server.Close()

	f := newFeedsFixture(t, deadURL, journal.URL)
	f.transport.allow[mustHost(t, deadURL)] = true

	_, body := f.get(t, "/api/following", "u1")
	got := items(t, body)
	if len(got) == 0 {
		t.Fatal("no items with the network denied — NVDA has embedded seed news")
	}
	for _, it := range got {
		if it["dataState"] != dataStateSeed {
			t.Fatalf("dataState = %v, want %q", it["dataState"], dataStateSeed)
		}
	}
}

// TestFollowingEmptyStoreIsAnHonestEmpty: :8004 up, corpus empty. `items: []`, counts zeroed, no
// degraded marker — nothing is wrong, there is simply nothing to show.
func TestFollowingEmptyStoreIsAnHonestEmpty(t *testing.T) {
	journal := journalFake(t, map[string][]string{"u1": {"NVDA"}})
	ev := newEventsFake(t, []map[string]any{})
	f := newFeedsFixture(t, ev.URL(), journal.URL)

	_, body := f.get(t, "/api/following", "u1")
	if got := items(t, body); len(got) != 0 {
		t.Fatalf("items = %d, want 0 for an empty store", len(got))
	}
	if d := degradedOf(t, body); len(d) != 0 {
		t.Fatalf("degraded = %v — an empty corpus is not a degraded one", d)
	}
	counts, _ := body["counts"].(map[string]any)
	if counts["total"].(float64) != 0 || counts["companiesAffected"].(float64) != 0 {
		t.Fatalf("counts = %v, want zeros", counts)
	}
}

// ────────────────────────────────────────────────────────────────────── /api/following semantics

// TestOneEventAppearsOnce — contract §3.8, and the whole difference between this feed and the five
// duplicate headlines `newsFor` produces today. The fake returns the event twice (as a naive join
// would) AND it matches three followed tickers; it must appear once, carrying all of them.
func TestOneEventAppearsOnce(t *testing.T) {
	journal := journalFake(t, map[string][]string{"u1": {"NVDA", "AMD", "TSM"}})
	shared := fixtureEvent("evt_shared", "regulatory_action", "2026-08-19T09:00:00Z", 0.9, 0.8,
		"official", 6, []map[string]any{
			rawTicker("NVDA", 0.94, true), rawTicker("AMD", 0.62, false), rawTicker("TSM", 0.4, false),
		})
	ev := newEventsFake(t, []map[string]any{shared, shared})
	f := newFeedsFixture(t, ev.URL(), journal.URL)

	_, body := f.get(t, "/api/following", "u1")
	got := items(t, body)
	if len(got) != 1 {
		t.Fatalf("items = %d, want 1 — one event appears once however many followed tickers it matches", len(got))
	}
	tks, _ := got[0]["tickers"].([]any)
	if len(tks) != 3 {
		t.Fatalf("tickers on the item = %d, want 3 — the item carries ALL its matching companies", len(tks))
	}
	for _, raw := range tks {
		m := raw.(map[string]any)
		if m["followed"] != true {
			t.Fatalf("ticker %v is not marked followed", m["ticker"])
		}
	}
	counts, _ := body["counts"].(map[string]any)
	if counts["companiesAffected"].(float64) != 3 {
		t.Fatalf("companiesAffected = %v, want 3", counts["companiesAffected"])
	}
	if counts["byImportance"].(map[string]any)["high"].(float64) != 1 {
		t.Fatalf("byImportance = %v, want one high", counts["byImportance"])
	}
}

// TestMacroEventsAreInTheSameFeed — doc §15.2/§15.8: a macro release mapped to a followed company
// is a card in Following, not a separate feed, and it carries its actual/expected/surprise.
func TestMacroEventsAreInTheSameFeed(t *testing.T) {
	journal := journalFake(t, map[string][]string{"u1": {"NVDA"}})
	macroEvent := fixtureEvent("evt_cpi", "macro_release", "2026-08-18T12:30:00Z", 0.85, 0.5,
		"official", 3, []map[string]any{rawTicker("NVDA", 0.5, false)})
	ev := newEventsFake(t, []map[string]any{macroEvent})
	ev.macro = []map[string]any{{
		"id": "mac_1", "eventId": "evt_cpi", "series": "CPI", "releaseAt": "2026-08-18T12:30:00Z",
		"actual": 3.1, "expected": 2.9, "previous": 3.0, "surprise": 0.2, "unit": "pct",
		"expectationSource": "fmp", "expectationCapturedAt": "2026-08-17T00:00:00Z",
	}}
	f := newFeedsFixture(t, ev.URL(), journal.URL)

	_, body := f.get(t, "/api/following", "u1")
	got := items(t, body)
	if len(got) != 1 || got[0]["eventType"] != "macro_release" {
		t.Fatalf("items = %v, want the macro release in the same feed", got)
	}
	m, ok := got[0]["macro"].(map[string]any)
	if !ok {
		t.Fatal("the macro card carries no §3.9 detail")
	}
	if m["series"] != "CPI" || m["surprise"].(float64) != 0.2 {
		t.Fatalf("macro detail = %v, want CPI with surprise 0.2", m)
	}
	if m["releaseAt"].(float64) != 1787056200 {
		t.Fatalf("releaseAt = %v, want unix 1787056200 — the boundary converts every …At field", m["releaseAt"])
	}
}

// TestFollowingFiltersAreIntersectedAndPushedDown: `tickers` is intersected with the follow graph
// (a ticker you do not follow is IGNORED, not a 400), and the surviving filters are handed to
// :8004, which implements them in SQL.
func TestFollowingFiltersAreIntersectedAndPushedDown(t *testing.T) {
	journal := journalFake(t, map[string][]string{"u1": {"NVDA", "AMD"}})
	ev := newEventsFake(t, nil)
	f := newFeedsFixture(t, ev.URL(), journal.URL)

	rec, _ := f.get(t,
		"/api/following?tickers=NVDA,MSFT&types=earnings_result,not_a_type&minImportance=0.5&since=1787000000", "u1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — an unfollowed ticker and an unknown type are ignored, not errors", rec.Code)
	}
	q := ev.lastQuery()
	if q.Get("tickers") != "NVDA" {
		t.Fatalf("tickers = %q, want %q — MSFT is not followed and is dropped", q.Get("tickers"), "NVDA")
	}
	if q.Get("types") != "earnings_result" {
		t.Fatalf("types = %q, want %q — an unknown §3.4 member is ignored", q.Get("types"), "earnings_result")
	}
	if q.Get("minImportance") != "0.5" {
		t.Fatalf("minImportance = %q, want 0.5", q.Get("minImportance"))
	}
	if want := time.Unix(1787000000, 0).UTC().Format(time.RFC3339); q.Get("since") != want {
		t.Fatalf("since = %q, want %q — the RFC3339 form of the caller's unix bound", q.Get("since"), want)
	}
}

// TestFollowingCursorIsStable: the cursor encodes the boundary ITEM, so paging twice yields
// disjoint pages that reassemble into the full ordering — no skips, no repeats.
func TestFollowingCursorIsStable(t *testing.T) {
	journal := journalFake(t, map[string][]string{"u1": {"NVDA"}})
	var fixtures []map[string]any
	for i := 0; i < 6; i++ {
		// All six share a published second on purpose: that is when a non-total order skips rows.
		fixtures = append(fixtures, fixtureEvent(fmt.Sprintf("evt_%d", i), "product_launch",
			"2026-08-19T10:00:00Z", 0.5, 0.5, "professional", 1,
			[]map[string]any{rawTicker("NVDA", 1.0, true)}))
	}
	ev := newEventsFake(t, fixtures)
	f := newFeedsFixture(t, ev.URL(), journal.URL)

	_, body := f.get(t, "/api/following?limit=3", "u1")
	first := items(t, body)
	cursor, _ := body["nextCursor"].(string)
	if len(first) != 3 || cursor == "" {
		t.Fatalf("first page = %d items, cursor = %q; want 3 and a cursor", len(first), cursor)
	}
	_, body2 := f.get(t, "/api/following?limit=3&cursor="+url.QueryEscape(cursor), "u1")
	second := items(t, body2)
	if len(second) != 3 {
		t.Fatalf("second page = %d items, want 3", len(second))
	}
	seen := map[string]bool{}
	for _, it := range append(first, second...) {
		id := fmt.Sprint(it["id"])
		if seen[id] {
			t.Fatalf("%s appears on both pages — the cursor repeated an item", id)
		}
		seen[id] = true
	}
	if len(seen) != 6 {
		t.Fatalf("two pages covered %d of 6 events — the cursor skipped some", len(seen))
	}
}

// TestFollowingCacheIsPerUser — invariant #5, and the one that leaks a follow graph if it is wrong.
// `ttlCache` is ONE process-wide map. Two users issue the SAME URL; the second must not be served
// the first's warm entry.
func TestFollowingCacheIsPerUser(t *testing.T) {
	journal := journalFake(t, map[string][]string{"alice": {"NVDA"}, "bob": {"TSLA"}})
	ev := newEventsFake(t, []map[string]any{
		fixtureEvent("evt_nvda", "product_launch", "2026-08-19T10:00:00Z", 0.6, 0.6, "official", 2,
			[]map[string]any{rawTicker("NVDA", 1.0, true)}),
	})
	f := newFeedsFixture(t, ev.URL(), journal.URL)

	recA, bodyA := f.get(t, "/api/following", "alice")
	if recA.Header().Get("X-Cache") != "MISS" {
		t.Fatalf("alice's first call = %q, want MISS", recA.Header().Get("X-Cache"))
	}
	recA2, _ := f.get(t, "/api/following", "alice")
	if recA2.Header().Get("X-Cache") != "HIT" {
		t.Fatal("alice's second call did not hit the cache — the key is not stable")
	}

	recB, bodyB := f.get(t, "/api/following", "bob")
	if recB.Header().Get("X-Cache") == "HIT" {
		t.Fatal("bob was served alice's cached feed — the cache key omits the user identity")
	}
	if fmt.Sprint(bodyA["tickers"]) == fmt.Sprint(bodyB["tickers"]) {
		t.Fatalf("alice and bob resolved to the same ticker set (%v) — the follow graph leaked",
			bodyA["tickers"])
	}
	if fmt.Sprint(bodyB["tickers"]) != "[TSLA]" {
		t.Fatalf("bob's tickers = %v, want [TSLA]", bodyB["tickers"])
	}
}

// TestFollowingMakesNoModelCall — invariant #4. A feed is a page load, and a model call caused by a
// page load is forbidden. Asserted from the RECORDED HOSTS rather than from a log line: §9.44, log
// absence is never evidence.
func TestFollowingMakesNoModelCall(t *testing.T) {
	journal := journalFake(t, map[string][]string{"u1": {"NVDA"}})
	ev := newEventsFake(t, []map[string]any{
		fixtureEvent("evt_a", "product_launch", "2026-08-19T10:00:00Z", 0.6, 0.6, "official", 2,
			[]map[string]any{rawTicker("NVDA", 1.0, true)}),
	})
	f := newFeedsFixture(t, ev.URL(), journal.URL)
	f.get(t, "/api/following", "u1")

	llmHost := mustHost(t, f.srv.cfg.LLMURL)
	for _, host := range f.transport.attempted() {
		if host == llmHost {
			t.Fatalf("the feed reached %s — invariant #4 forbids a model call on a page load", host)
		}
	}
}

// ──────────────────────────────────────────────────────────────── GET /api/events/{id} (§9.2)

// TestEventReadProxiesVerbatim: A VERBATIM PROXY MAY ADD, NEVER ALTER.
//
// The read-through returns every upstream key unmodified, unrenamed, unreordered and unremoved —
// a detail page that disagreed with the store about a field name would be a bug that only ever
// appeared on one screen. The gateway MAY add a top-level sibling it owns, and adds exactly one:
// §9.18's `disclaimer`, which is gateway/LLM vocabulary that `services/events` holds none of and
// should not be made to hold. Byte-equality was the old encoding of this rule; the assertion below
// is the sharper one — the upstream object is a strict subset, byte-identical member by member, and
// the ONLY added key is the one the gateway owns.
func TestEventReadProxiesVerbatim(t *testing.T) {
	ev := newEventsFake(t, []map[string]any{
		fixtureEvent("evt_a", "product_launch", "2026-08-19T10:00:00Z", 0.6, 0.6, "official", 2,
			[]map[string]any{rawTicker("NVDA", 1.0, true)}),
	})
	f := newFeedsFixture(t, ev.URL(), "")

	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/events/evt_a", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	direct, err := http.Get(ev.URL() + "/events/evt_a")
	if err != nil {
		t.Fatalf("direct call to the fake failed: %v", err)
	}
	defer direct.Body.Close()
	var want, got any
	_ = json.NewDecoder(direct.Body).Decode(&want)
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("proxied body is not JSON: %v", err)
	}
	wantMap, ok := want.(map[string]any)
	if !ok {
		t.Fatalf("the upstream fixture is not a JSON object")
	}
	gotMap, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("the proxied body is not a JSON object")
	}

	// NEVER ALTER: every upstream key is present and its value is byte-identical after a round trip.
	for key, wantVal := range wantMap {
		gotVal, present := gotMap[key]
		if !present {
			t.Fatalf("the read-through REMOVED the upstream key %q", key)
		}
		wantJSON, _ := json.Marshal(wantVal)
		gotJSON, _ := json.Marshal(gotVal)
		if string(wantJSON) != string(gotJSON) {
			t.Fatalf("the read-through ALTERED upstream key %q.\n got: %s\nwant: %s", key, gotJSON, wantJSON)
		}
	}

	// MAY ADD, and only what the gateway owns. Any other new key is a reshaping this rule forbids.
	// The list is explicit rather than open-ended: "the gateway may add things" is not a rule.
	owned := map[string]bool{"disclaimer": true, "importanceBand": true}
	for key := range gotMap {
		if _, fromUpstream := wantMap[key]; fromUpstream {
			continue
		}
		if !owned[key] {
			t.Fatalf("the read-through ADDED %q, which the gateway does not own", key)
		}
	}

	// The added disclosure is §9.18's constant, byte-for-byte — not a paraphrase composed here.
	if gotMap["disclaimer"] != analystDisclaimer {
		t.Fatalf("disclaimer = %q, want the §9.18 constant %q", gotMap["disclaimer"], analystDisclaimer)
	}

	// The added band agrees with the one function that defines it, computed from the importance the
	// STORE sent. Without this key the detail panel rendered "Routine" beside a feed card reading
	// "High impact" for the same event — the client reads the served band and the store computes
	// none, so the gateway supplies it.
	storeEvent, _ := wantMap["event"].(map[string]any)
	imp, hasImp := storeEvent["importance"].(float64)
	if hasImp {
		if got := gotMap["importanceBand"]; got != importanceBand(imp) {
			t.Fatalf("importanceBand = %v, want %q for importance %.2f", got, importanceBand(imp), imp)
		}
	} else if _, present := gotMap["importanceBand"]; present {
		t.Fatalf("a band was served for an event the store did not score")
	}

	// UNREORDERED: the byte order of the upstream members is preserved, and the gateway-owned key is
	// spliced in as a sibling rather than the body being re-encoded (which would sort every key).
	upstreamBytes, _ := json.Marshal(want)
	inner := string(upstreamBytes[1:]) // drop the leading '{' — the rest must appear verbatim
	if !strings.Contains(rec.Body.String(), strings.TrimSuffix(inner, "}")) {
		t.Fatalf("the upstream member ORDER was not preserved.\n got: %s\nwant to contain: %s",
			rec.Body.String(), inner)
	}

	for _, key := range []string{"event", "documents", "tickers", "asOf"} {
		if _, ok := gotMap[key]; !ok {
			t.Fatalf("§9.2 requires %q in the read-through response", key)
		}
	}
}

// TestEventReadMissingIDIs404: the upstream's status is the upstream's answer. Turning it into a
// 200-with-null would make "no such event" indistinguishable from "the event exists and is empty".
func TestEventReadMissingIDIs404(t *testing.T) {
	ev := newEventsFake(t, nil)
	f := newFeedsFixture(t, ev.URL(), "")

	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/events/evt_missing", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if !strings.Contains(fmt.Sprint(body["detail"]), "evt_missing") {
		t.Fatalf("detail = %v, want the upstream's own message", body["detail"])
	}
}

// TestEventReadDegradedIsNotA500: :8004 unreachable is a degraded READ — the gateway is fine, its
// upstream is not — so it is a typed marker the client can branch on, not a 500 it can only log.
func TestEventReadDegradedIsNotA500(t *testing.T) {
	dead := newEventsFake(t, nil)
	deadURL := dead.URL()
	dead.server.Close()
	f := newFeedsFixture(t, deadURL, "")
	f.transport.allow[mustHost(t, deadURL)] = true

	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/events/evt_a", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 with a degraded marker", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	raw, _ := body["degraded"].([]any)
	if len(raw) == 0 || fmt.Sprint(raw[0]) != degradedEventsUnreachable {
		t.Fatalf("degraded = %v, want [%s]", body["degraded"], degradedEventsUnreachable)
	}
	if body["event"] != nil {
		t.Fatalf("event = %v, want null on a degraded read", body["event"])
	}
}

// TestEventReadForwardsAsOf — §9.2's `as_of?`. It is the only as-of the gateway has ever had, and
// it is a pass-through: the store does the point-in-time filtering in SQL (§1.1).
func TestEventReadForwardsAsOf(t *testing.T) {
	var seen string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.Query().Get("as_of")
		writeJSON(w, http.StatusOK, map[string]any{
			"event": map[string]any{"id": "evt_a"}, "documents": []any{}, "tickers": []any{},
			"asOf": seen, "degraded": []string{},
		})
	}))
	defer upstream.Close()
	f := newFeedsFixture(t, upstream.URL, "")
	f.transport.allow[mustHost(t, upstream.URL)] = true

	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		"/api/events/evt_a?as_of=2026-08-01T00:00:00Z", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if seen != "2026-08-01T00:00:00Z" {
		t.Fatalf("as_of reached the store as %q, want the caller's value", seen)
	}
}

// ─────────────────────────────────────────────────────────────────────────────── unit checks

// TestFeedUnixIsTheOneBoundary: every `…At` field in a gateway payload passes through this
// function exactly once. Getting it wrong in five places is how a feed ends up an hour off in one
// section, so the conversion is tested directly rather than only through a handler.
func TestFeedUnixIsTheOneBoundary(t *testing.T) {
	cases := map[string]int64{
		"2026-08-19T12:00:00Z":        1787140800,
		"2026-08-19T12:00:00.123456Z": 1787140800,
		"2026-08-19T12:00:00":         1787140800,
		"2026-08-19":                  1787097600,
		"":                            0,
		"not a timestamp":             0,
	}
	for in, want := range cases {
		if got := feedUnix(in); got != want {
			t.Errorf("feedUnix(%q) = %d, want %d", in, got, want)
		}
	}
}

// TestDataStateForNewsSource: `newsFor` returns a composite label; the badge the user sees must
// follow the data, not the function that produced it.
func TestDataStateForNewsSource(t *testing.T) {
	cases := map[string]string{
		"marketaux+rss+sec8k": dataStateLive,
		"seed+rss":            dataStateLive,
		"rss":                 dataStateLive,
		"seed":                dataStateSeed,
		"none":                dataStateSeed,
		"":                    dataStateSeed,
	}
	for in, want := range cases {
		if got := dataStateForNewsSource(in); got != want {
			t.Errorf("dataStateForNewsSource(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestEventsErrStatusRecoversTheUpstreamCode covers the string-parsing seam that exists only
// because `gateway/events.go` is Wave 0's file and this lane may not edit it.
func TestEventsErrStatusRecoversTheUpstreamCode(t *testing.T) {
	err := fmt.Errorf("events http://x/events/evt_1 -> 404: {\"detail\":\"no such event\"}")
	code, detail, ok := eventsErrStatus(err)
	if !ok || code != 404 || detail != "no such event" {
		t.Fatalf("got (%d, %q, %v), want (404, %q, true)", code, detail, ok, "no such event")
	}
	if _, _, ok := eventsErrStatus(fmt.Errorf("events unreachable: dial tcp: refused")); ok {
		t.Fatal("a transport error has no upstream status and must not synthesise one")
	}
	if _, _, ok := eventsErrStatus(nil); ok {
		t.Fatal("nil error must not yield a status")
	}
}

func mustHost(t *testing.T, raw string) string {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("bad url %q: %v", raw, err)
	}
	return u.Host
}

// ────────────────────────────────── Wave 4 integration: §9.18 on the feed that renders directions

// TestFollowingCarriesTheAnalystDisclaimer: §9.18 requires ANALYST_DISCLAIMER adjacent to the output
// it qualifies, and it is a SERVER-supplied constant — a browser that composed the sentence itself
// would satisfy the letter and destroy the point, which is that one place owns the words.
//
// The Following feed renders per-ticker `potentialDirection` and `transmission`. Before this it
// carried neither the directions' disclosure nor any way for the client to get one without keeping a
// fourth unasserted copy of the string. Lanes 4A and 4B asked for this independently.
func TestFollowingCarriesTheAnalystDisclaimer(t *testing.T) {
	journal := journalFake(t, map[string][]string{"u1": {"NVDA"}})
	ev := newEventsFake(t, []map[string]any{
		fixtureEvent("evt_a", "earnings_result", "2026-08-19T10:00:00Z", 0.83, 0.9, "official", 5,
			[]map[string]any{rawTicker("NVDA", 1.0, true)}),
	})
	f := newFeedsFixture(t, ev.URL(), journal.URL)

	_, body := f.get(t, "/api/following", "u1")
	got, _ := body["disclaimer"].(string)
	if got != analystDisclaimer {
		t.Fatalf("disclaimer = %q, want the §9.18 constant %q", got, analystDisclaimer)
	}

	// It is served on the degraded path too. A disclosure that disappears when the upstream does is
	// worse than no disclosure: the directions may still be there.
	evDown := newEventsFake(t, nil)
	evDown.server.Close()
	f2 := newFeedsFixture(t, evDown.URL(), journal.URL)
	_, degradedBody := f2.get(t, "/api/following", "u1")
	if d, _ := degradedBody["disclaimer"].(string); d != analystDisclaimer {
		t.Fatalf("degraded disclaimer = %q, want it present anyway", d)
	}
}

// TestFollowingServesTheImportanceBand: one definition of the band, on the server, where the
// thresholds live. `importanceBand` is omitted exactly when `importance` is — an unscored item has
// no band, and "low" would read as "scored, and scored lowest".
func TestFollowingServesTheImportanceBand(t *testing.T) {
	journal := journalFake(t, map[string][]string{"u1": {"NVDA"}})
	ev := newEventsFake(t, []map[string]any{
		fixtureEvent("evt_hi", "earnings_result", "2026-08-19T10:00:00Z", 0.83, 0.9, "official", 5,
			[]map[string]any{rawTicker("NVDA", 1.0, true)}),
		fixtureEvent("evt_mid", "product_launch", "2026-08-19T11:00:00Z", 0.55, 0.5, "official", 3,
			[]map[string]any{rawTicker("NVDA", 1.0, true)}),
		fixtureEvent("evt_lo", "partnership", "2026-08-19T12:00:00Z", 0.20, 0.3, "professional", 1,
			[]map[string]any{rawTicker("NVDA", 1.0, true)}),
	})
	f := newFeedsFixture(t, ev.URL(), journal.URL)

	_, body := f.get(t, "/api/following", "u1")
	raw, _ := json.Marshal(body["items"])
	var items []feedItem
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatalf("decode items: %v", err)
	}
	if len(items) == 0 {
		t.Fatal("no items — the assertion would be vacuous")
	}

	want := map[string]string{"evt_hi": "high", "evt_mid": "medium", "evt_lo": "low"}
	for _, it := range items {
		w, ok := want[it.ID]
		if !ok {
			continue
		}
		if it.ImportanceBand != w {
			t.Fatalf("%s importanceBand = %q, want %q", it.ID, it.ImportanceBand, w)
		}
		if it.Importance == nil {
			t.Fatalf("%s: a band was served without the importance it bands", it.ID)
		}
		if it.ImportanceBand != importanceBand(*it.Importance) {
			t.Fatalf("%s: served band %q disagrees with importanceBand()", it.ID, it.ImportanceBand)
		}
	}
}
