package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// agency_test.go — the gateway half of the research-agency lane: it forwards four owner routes and
// it forwards NOTHING ELSE.
//
// The interesting assertions here are all negative. The gateway is a thin proxy, so what makes it
// correct is the set of things it refuses to do: reach the worker API, forward a request without a
// session, or pass a caller's query string through to an internal service.

// agencyJournalFake stands in for the journal and records what the gateway asked it for.
type agencyJournalFake struct {
	seenPath   string
	seenMethod string
	seenCookie string
	seenBody   string
}

func agencyServer(t *testing.T) (*Server, *agencyJournalFake) {
	t.Helper()
	fake := &agencyJournalFake{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.seenMethod = r.Method
		fake.seenPath = r.URL.RequestURI()
		fake.seenCookie = r.Header.Get("Cookie")
		body, _ := io.ReadAll(r.Body)
		fake.seenBody = string(body)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": r.URL.RequestURI()})
	}))
	t.Cleanup(upstream.Close)

	cfg := loadConfig()
	cfg.JournalURL = upstream.URL
	return &Server{
		cfg: cfg, cache: newCache(), http: &http.Client{Timeout: 2 * time.Second},
	}, fake
}

func agencyRequest(t *testing.T, srv *Server, method, path, body, uid string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if uid != "" {
		req.AddCookie(&http.Cookie{
			Name: srv.cfg.CookieName, Value: testSessionToken(srv.cfg.Secret, uid),
		})
	}
	w := httptest.NewRecorder()
	mux := http.NewServeMux()
	srv.registerEventRoutes(mux)
	mux.ServeHTTP(w, req)
	return w
}

func TestAgencyRoutesRequireASignedInUser(t *testing.T) {
	srv, fake := agencyServer(t)
	cases := []struct{ method, path, body string }{
		{http.MethodPost, "/api/agency/runs", `{"ticker":"NVDA","question":"why"}`},
		{http.MethodGet, "/api/agency/runs", ""},
		{http.MethodGet, "/api/agency/runs/agr_1", ""},
		{http.MethodPost, "/api/agency/runs/agr_1/cancel", ""},
	}
	for _, tc := range cases {
		w := agencyRequest(t, srv, tc.method, tc.path, tc.body, "")
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s as a guest = %d, want 401", tc.method, tc.path, w.Code)
		}
		if fake.seenPath != "" {
			t.Fatalf("a guest request reached the journal at %q", fake.seenPath)
		}
	}
}

func TestAgencyRoutesForwardTheSessionCookieToTheJournal(t *testing.T) {
	// The journal resolves the caller and enforces the owner allowlist itself. It can only do that
	// if the cookie arrives, and the gateway deliberately does NOT duplicate the allowlist — two
	// copies of an allowlist drift, and the one next to the data is the one that matters.
	srv, fake := agencyServer(t)
	w := agencyRequest(t, srv, http.MethodPost, "/api/agency/runs",
		`{"ticker":"NVDA","question":"why"}`, "owner-uid")
	if w.Code != http.StatusOK {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	if fake.seenPath != "/agency/runs" || fake.seenMethod != http.MethodPost {
		t.Fatalf("the gateway called %s %s, want POST /agency/runs", fake.seenMethod, fake.seenPath)
	}
	if !strings.Contains(fake.seenCookie, srv.cfg.CookieName) {
		t.Fatalf("the session cookie did not reach the journal: %q", fake.seenCookie)
	}
	if !strings.Contains(fake.seenBody, "NVDA") {
		t.Fatalf("the request body did not reach the journal: %q", fake.seenBody)
	}
}

func TestTheGatewayNeverProxiesTheWorkerAPI(t *testing.T) {
	// The claim, heartbeat, complete and fail routes mutate lease state and carry the worker
	// credential. Reaching them through a browser route would make a page load able to claim a
	// research job — the shape gateway/automation.go refuses for the same reason.
	srv, fake := agencyServer(t)
	for _, path := range []string{
		"/api/_internal/agency/claim",
		"/api/agency/claim",
		"/api/agency/runs/agr_1/complete",
		"/api/agency/runs/agr_1/heartbeat",
		"/api/agency/runs/agr_1/fail",
		"/_internal/agency/claim",
	} {
		w := agencyRequest(t, srv, http.MethodPost, path, "{}", "owner-uid")
		if w.Code != http.StatusNotFound {
			t.Fatalf("POST %s = %d, want 404 — the worker API must not be reachable from a browser",
				path, w.Code)
		}
		if fake.seenPath != "" {
			t.Fatalf("%s reached the journal at %q", path, fake.seenPath)
		}
	}
}

func TestOnlyTheLimitParameterIsForwarded(t *testing.T) {
	// Forwarding RawQuery wholesale would let a caller append arbitrary parameters to an internal
	// request. An allowlist of one is the right size for a surface with one parameter.
	srv, fake := agencyServer(t)
	agencyRequest(t, srv, http.MethodGet,
		"/api/agency/runs?limit=5&userId=someone-else&admin=1", "", "owner-uid")
	if fake.seenPath != "/agency/runs?limit=5" {
		t.Fatalf("the gateway forwarded %q; only limit may cross", fake.seenPath)
	}

	// A non-numeric limit becomes an absent one rather than being passed through.
	fake.seenPath = ""
	agencyRequest(t, srv, http.MethodGet, "/api/agency/runs?limit=../../etc", "", "owner-uid")
	if strings.Contains(fake.seenPath, "..") {
		t.Fatalf("a hostile limit was forwarded: %q", fake.seenPath)
	}
}

func TestTheStatusReadReachesTheJournalAndNothingElse(t *testing.T) {
	srv, fake := agencyServer(t)
	w := agencyRequest(t, srv, http.MethodGet, "/api/agency/runs/agr_abc", "", "owner-uid")
	if w.Code != http.StatusOK {
		t.Fatalf("read = %d", w.Code)
	}
	if fake.seenPath != "/agency/runs/agr_abc" {
		t.Fatalf("the gateway called %q", fake.seenPath)
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("the proxied body is not JSON: %s", w.Body.String())
	}
}

func TestAJournalOutageIsReportedRatherThanFabricated(t *testing.T) {
	// A degraded upstream must produce an honest error, never an invented "no runs" answer that a
	// reader would take as fact.
	srv, _ := agencyServer(t)
	srv.cfg.JournalURL = "http://127.0.0.1:1" // nothing listens here
	w := agencyRequest(t, srv, http.MethodGet, "/api/agency/runs", "", "owner-uid")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("an unreachable journal = %d, want 502", w.Code)
	}
	if strings.Contains(w.Body.String(), `"runs":[]`) {
		t.Fatalf("an outage was rendered as an empty result: %s", w.Body.String())
	}
}
