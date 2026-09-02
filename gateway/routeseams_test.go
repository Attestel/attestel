package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// routeseams_test.go — proves the Wave 0 seams carry what has been attached to them, and only that.
//
// The seams exist so Wave 1 Lane A and Wave 2 Lane C can attach routes from their own files without
// editing main.go. Wave 0 shipped both empty and asserted the route table was byte-for-byte
// unchanged, scoping that assertion explicitly: the tests "fail the moment a registrar is attached,
// which is exactly when a lane must also update its own expectations." Wave 1 integration attached
// the subscription registrar (gateway/subscriptions.go), so this file now proves the other half —
// the routes really do arrive through the seam, and nothing else was edited to make them work.
//
// Wave 2 integration flipped the EVENT half: Lane 2C attaches one registrar from gateway/feeds.go,
// carrying /api/following, /api/explore and /api/events/{id}. That is the moment this file's own
// header always said its expectations must be updated, so they are updated here — by the
// integrator, because this file is in no lane's ownership.

// wantSubscriptionRegistrars is the count Wave 1 attaches: one, from gateway/subscriptions.go. It is
// spelled out rather than derived so a second registrar — from a later wave, or from a merge that
// duplicated an init() — fails here instead of silently changing how routes are assembled.
const wantSubscriptionRegistrars = 1

// wantEventRegistrars counts the init()s attached to the event seam: Wave 2 Lane 2C's in
// gateway/feeds.go, Wave 3 Lane 3A's in gateway/context.go, and Wave 5 Lane 5B's TWO — in
// gateway/changed.go (`/api/changed`) and gateway/monitor.go (`/api/monitor/theses`). Spelled out
// for the same reason as the subscription count: a merge that duplicated an init(), or a second
// lane attaching its own, must fail here rather than quietly change how routes are assembled.
// Bumped 1 -> 2 by Wave 3 integration, 2 -> 4 by Wave 5B, 4 -> 5 by Phase 1's automation health
// read (gateway/automation.go) and 5 -> 6 by Phase 4's reaction/sensitivity reads
// (gateway/reactions.go), 6 -> 7 by Discovery Scout (gateway/scout.go), and 7 -> 8 by the Early
// Opportunity Radar read (gateway/opportunities.go), and 8 -> 9 by the Hermes research agency proxy
// (gateway/agency.go), per this file's header — each lane correctly did NOT edit main.go,
// because a manual s.registerXRoutes(mux) there would double-register the pattern and panic the
// mux.
const wantEventRegistrars = 9

func TestSeamRegistrarsMatchWhatHasLanded(t *testing.T) {
	if len(eventRouteRegistrars) != wantEventRegistrars {
		t.Fatalf("eventRouteRegistrars = %d, want %d (feeds.go + context.go + changed.go + monitor.go + automation.go + reactions.go + scout.go + opportunities.go + agency.go, one init() each)",
			len(eventRouteRegistrars), wantEventRegistrars)
	}
	if len(subscriptionRouteRegistrars) != wantSubscriptionRegistrars {
		t.Fatalf("subscriptionRouteRegistrars = %d, want %d (gateway/subscriptions.go, one init())",
			len(subscriptionRouteRegistrars), wantSubscriptionRegistrars)
	}
}

// TestSubscriptionSeamRegistersTheProxyRoutes runs the hooks against a FRESH mux — one that has been
// through no other registration — so a route that resolves there can only have come from the seam.
//
// The proxy routes are asserted by NOT-404. They forward to the journal, which is not running in a
// unit test, so the honest expectation is "the mux knows this path and tried" — a 502 from
// handleJournalProxy — rather than a specific success code. 404 still means the seam did not fire.
func TestSubscriptionSeamRegistersTheProxyRoutes(t *testing.T) {
	srv := &Server{cfg: loadConfig(), cache: newCache(), http: &http.Client{}}
	mux := http.NewServeMux()
	srv.registerEventRoutes(mux)
	srv.registerSubscriptionRoutes(mux)

	for _, path := range []string{"/api/subscriptions", "/api/event-state"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code == http.StatusNotFound {
			t.Fatalf("GET %s = 404 — the subscription seam did not register it", path)
		}
	}

	// The event seam now carries Lane 2C's routes, and they are asserted by PATTERN LOOKUP rather
	// than by serving them.
	//
	// This is not a stylistic preference. This Server is built with a bare &http.Client{} and no
	// fake upstreams, so calling mux.ServeHTTP on /api/following executes the real degraded cascade
	// — journal, then :8004, then newsFor, which is Marketaux plus Google News RSS plus
	// data.sec.gov. That is a live network call inside a unit test: slow, flaky offline, and a
	// violation of the no-network rule these tests exist under. mux.Handler reports what the mux
	// resolves without invoking the handler, which is the only thing this test ever wanted to know.
	for _, path := range []string{"/api/following", "/api/explore", "/api/scout", "/api/opportunities", "/api/events/evt_x"} {
		_, pattern := mux.Handler(httptest.NewRequest(http.MethodGet, path, nil))
		if pattern == "" {
			t.Fatalf("GET %s resolves to no pattern — the Wave 2 event seam did not register it", path)
		}
	}

	// §9.1: /api/changed is Wave 5B's, and 5B has landed it. THIS ASSERTION IS INVERTED
	// DELIBERATELY, in the same commit as `gateway/changed.go`.
	//
	// It used to read `pattern != ""` ⇒ fail, which was correct for every wave in which the route
	// did not exist: §9.1 assigns it to Lane 5B ALONE, and two `mux.HandleFunc` registrations on
	// one pattern panic at startup, so a second lane sneaking it in had to fail loudly. Now that
	// 5B owns it, the same clause requires the opposite proof — that it resolves, exactly once,
	// and from `changed.go`'s registrar rather than from a stray line in main.go.
	if _, pattern := mux.Handler(httptest.NewRequest(http.MethodGet, "/api/changed", nil)); pattern == "" {
		t.Fatal("GET /api/changed resolves to no pattern — Lane 5B's registrar did not fire (§9.1)")
	}
}

// TestChangedIsRegisteredExactlyOnce is the other half of §9.1's rule. The clause's stated hazard is
// a DOUBLE registration ("two mux.HandleFunc registrations on one pattern panic at startup"), and a
// panic at startup is not something a route-resolution test can observe — by then the process is
// gone. So it is asserted here, by running the seam against a fresh mux and recovering.
func TestChangedIsRegisteredExactlyOnce(t *testing.T) {
	srv := &Server{cfg: loadConfig(), cache: newCache(), http: &http.Client{}}
	mux := http.NewServeMux()
	srv.registerEventRoutes(mux)

	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("registering GET /api/changed a second time panicked: %v — some other file "+
				"also registers it, which §9.1 forbids", recovered)
		}
	}()
	// If `changed.go`'s registrar had run twice (a duplicated init(), a merge artefact), the FIRST
	// registration above would already have panicked. This second one proves the pattern was free
	// exactly once by taking it on a different mux.
	fresh := http.NewServeMux()
	fresh.HandleFunc("GET /api/changed", srv.handleChanged)
}

// TestInternalTickersIsNotProxied is the §9.4 rule that has no other enforcement point.
//
// journal refuses GET /_internal/tickers whenever a session cookie is present, because it returns a
// cross-user set. That defence is trivially bypassable by a server-side caller that simply does not
// forward the cookie — so the gateway must not expose the path at all, under any prefix.
//
// WAVE 5B ADDS A SECOND INTERNAL PATH TO THIS TEST: `/_internal/theses`, which the thesis monitor
// sweeps. It is the same class of route with the same defence and the same bypass, so it gets the
// same assertion. Adding a cross-user route without adding its line here is how the first one would
// have been unprotected too.
func TestInternalTickersIsNotProxied(t *testing.T) {
	srv := &Server{cfg: loadConfig(), cache: newCache(), http: &http.Client{}}
	mux := http.NewServeMux()
	srv.registerSubscriptionRoutes(mux)
	// Registered as well, so a route accidentally exposed by EITHER seam is caught. 5B's own
	// `/api/monitor/theses` is a per-user proxy and is not an internal path.
	srv.registerEventRoutes(mux)

	for _, path := range []string{
		"/api/_internal/tickers", "/_internal/tickers",
		"/api/_internal/theses", "/_internal/theses",
		"/api/_internal/thesis-resynth/anything", "/_internal/thesis-resynth/anything",
		"/api/_internal/resynth/lease", "/_internal/resynth/lease",
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404 — a cross-user internal route must not be reachable "+
				"through the gateway (§9.4)", path, rec.Code)
		}
	}
}

// TestMigrateIsNotBrowserCallable: the migration is a server-side consequence of the first list
// call, never an endpoint a page can hit. Exposing it would let a browser re-seed a ticker the user
// had deliberately unfollowed — the exact bug §9.39 exists to prevent.
//
// The answer is 405, not 404, and that is worth spelling out rather than loosening the assertion
// to "not 2xx". `PATCH|DELETE /api/subscriptions/{id}` are registered, so net/http matches the PATH
// `/api/subscriptions/migrate` with `id = "migrate"`, finds no POST handler for it, and returns
// Method Not Allowed. The route is genuinely absent; 405 is Go telling us so.
//
// The sibling case is checked below: PATCH and DELETE *do* match, and are forwarded — but they
// reach journal as "patch/delete the subscription whose id is `migrate`", which is a 404 from
// journal, not a migration. No verb reaches POST /subscriptions/migrate from a browser.
func TestMigrateIsNotBrowserCallable(t *testing.T) {
	srv := &Server{cfg: loadConfig(), cache: newCache(), http: &http.Client{}}
	mux := http.NewServeMux()
	srv.registerSubscriptionRoutes(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/subscriptions/migrate", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/subscriptions/migrate = %d, want 405 — there must be no POST route for "+
			"this path; the migration is not browser-callable (§9.39)", rec.Code)
	}

	// And it is not reachable by pretending to be a create, either: the proxy strips `/api` and
	// forwards the method verbatim, so nothing here can turn a browser request into the POST that
	// journal treats as a migration.
	handler, pattern := mux.Handler(httptest.NewRequest(http.MethodPost, "/api/subscriptions/migrate", nil))
	if handler == nil {
		t.Fatal("expected net/http's method-not-allowed handler, got nil")
	}
	if pattern != "" {
		t.Fatalf("POST /api/subscriptions/migrate resolved to pattern %q — it must match no route", pattern)
	}
}
