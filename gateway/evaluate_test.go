package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// evaluate_test.go — the access-control matrix for the edge-evaluation runner (gateway/evaluate.go).
//
// The property under test is fail-closed: with EVAL_ADMIN_UIDS unset — which is how every
// deployment ships — nobody can start a minutes-long, CPU-heavy job, and a guest cannot even read
// what this deployment has evaluated.

type evalUpstream struct {
	srv     *httptest.Server
	hits    int
	lastURI string
	lastMth string
	lastBdy string
	lastAct string
	status  int
	body    string
}

func newEvalUpstream(t *testing.T) *evalUpstream {
	t.Helper()
	u := &evalUpstream{status: http.StatusAccepted, body: `{"started":true}`}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, 4096)
		n, _ := r.Body.Read(buf)
		u.hits++
		u.lastURI, u.lastMth, u.lastBdy = r.URL.RequestURI(), r.Method, string(buf[:n])
		u.lastAct = r.Header.Get("X-Operator-Uid")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(u.status)
		_, _ = w.Write([]byte(u.body))
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func newEvalGateway(t *testing.T, up *evalUpstream, admins []string) (*Server, http.Handler) {
	t.Helper()
	cfg := loadConfig()
	cfg.PredictionURL = up.srv.URL
	cfg.Secret = "test-secret"
	cfg.CookieName = "nvda_session"
	cfg.EvalAdminUIDs = admins
	srv := &Server{cfg: cfg, cache: newCache(), http: &http.Client{Timeout: 5 * time.Second}}
	mux := http.NewServeMux()
	srv.registerEvaluateRoutes(mux)
	return srv, mux
}

func callEval(t *testing.T, srv *Server, h http.Handler, method, path, uid, body string) (int, map[string]any, http.Header) {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	if uid != "" {
		r.Header.Set("Cookie", gatewaySession(srv, uid))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	var out map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out, rec.Result().Header
}

func TestEvaluateRunRequiresSession(t *testing.T) {
	up := newEvalUpstream(t)
	srv, h := newEvalGateway(t, up, []string{"admin-1"})

	code, body, _ := callEval(t, srv, h, http.MethodPost, "/api/evaluate/run", "", "")
	if code != http.StatusUnauthorized {
		t.Fatalf("guest run: got %d, want 401", code)
	}
	if body["error"] != "sign in required" {
		t.Errorf("guest run: error = %v", body["error"])
	}
	if up.hits != 0 {
		t.Errorf("a guest reached the prediction service (%d hits) — auth must refuse first", up.hits)
	}
}

func TestEvaluateStatusRequiresSession(t *testing.T) {
	up := newEvalUpstream(t)
	srv, h := newEvalGateway(t, up, nil)

	code, _, _ := callEval(t, srv, h, http.MethodGet, "/api/evaluate/status", "", "")
	if code != http.StatusUnauthorized {
		t.Fatalf("guest status: got %d, want 401", code)
	}
	if up.hits != 0 {
		t.Errorf("a guest reached the prediction service (%d hits)", up.hits)
	}
}

// The shipped default. EVAL_ADMIN_UIDS is empty, so a perfectly valid session still may not start a
// run: "nobody is configured" must mean nobody, not everybody.
func TestEvaluateRunEmptyAllowListForbidsEveryone(t *testing.T) {
	up := newEvalUpstream(t)
	srv, h := newEvalGateway(t, up, nil)

	code, body, _ := callEval(t, srv, h, http.MethodPost, "/api/evaluate/run", "u1", "")
	if code != http.StatusForbidden {
		t.Fatalf("empty allow-list: got %d, want 403", code)
	}
	msg, _ := body["error"].(string)
	if !strings.Contains(msg, "EVAL_ADMIN_UIDS") {
		t.Errorf("403 must name the allow-list so the operator can fix it; got %q", msg)
	}
	if up.hits != 0 {
		t.Errorf("a non-admin reached the prediction service (%d hits)", up.hits)
	}
}

func TestEvaluateRunNonMemberForbidden(t *testing.T) {
	up := newEvalUpstream(t)
	srv, h := newEvalGateway(t, up, []string{"admin-1", "admin-2"})

	code, _, _ := callEval(t, srv, h, http.MethodPost, "/api/evaluate/run", "u-not-admin", "")
	if code != http.StatusForbidden {
		t.Fatalf("non-member: got %d, want 403", code)
	}
	if up.hits != 0 {
		t.Errorf("a non-member reached the prediction service (%d hits)", up.hits)
	}
}

func TestEvaluateRunMemberIsProxied(t *testing.T) {
	up := newEvalUpstream(t)
	up.status, up.body = http.StatusAccepted, `{"started":true,"logFile":"run-x.log"}`
	srv, h := newEvalGateway(t, up, []string{"admin-1"})

	code, body, hdr := callEval(t, srv, h, http.MethodPost, "/api/evaluate/run", "admin-1", "")
	if code != http.StatusAccepted {
		t.Fatalf("member run: got %d, want the upstream's 202", code)
	}
	if body["logFile"] != "run-x.log" {
		t.Errorf("upstream body not passed through: %v", body)
	}
	if up.hits != 1 || up.lastMth != http.MethodPost || up.lastURI != "/evaluate/run" {
		t.Errorf("upstream call = %d x %s %s", up.hits, up.lastMth, up.lastURI)
	}
	if hdr.Get("Cache-Control") != "no-store" {
		t.Errorf("run must not be cacheable; Cache-Control = %q", hdr.Get("Cache-Control"))
	}
}

// The upstream owns the no-parameters rule (contract §4.3). The gateway must FORWARD the query
// string rather than strip it, so the caller is told their parameters were refused instead of
// silently getting a default-parameter run they did not ask for.
func TestEvaluateRunForwardsParametersSoUpstreamCanRefuseThem(t *testing.T) {
	up := newEvalUpstream(t)
	up.status, up.body = http.StatusBadRequest, `{"detail":"POST /evaluate/run takes no parameters."}`
	srv, h := newEvalGateway(t, up, []string{"admin-1"})

	code, body, _ := callEval(t, srv, h, http.MethodPost, "/api/evaluate/run?upper=0.9", "admin-1", "")
	if code != http.StatusBadRequest {
		t.Fatalf("got %d, want the upstream's 400", code)
	}
	if up.lastURI != "/evaluate/run?upper=0.9" {
		t.Errorf("query string was not forwarded: %q", up.lastURI)
	}
	if !strings.Contains(body["detail"].(string), "no parameters") {
		t.Errorf("upstream refusal not passed through: %v", body)
	}
}

// A run already in flight is a 409 from the upstream carrying that run's status. The gateway must
// not flatten it into a generic gateway error — "one is already going" is only actionable next to
// what that one is doing.
func TestEvaluateRunPassesConflictThrough(t *testing.T) {
	up := newEvalUpstream(t)
	up.status, up.body = http.StatusConflict, `{"started":false,"status":{"state":"running"}}`
	srv, h := newEvalGateway(t, up, []string{"admin-1"})

	code, body, _ := callEval(t, srv, h, http.MethodPost, "/api/evaluate/run", "admin-1", "")
	if code != http.StatusConflict {
		t.Fatalf("got %d, want 409", code)
	}
	st, _ := body["status"].(map[string]any)
	if st == nil || st["state"] != "running" {
		t.Errorf("running job's status not passed through: %v", body)
	}
}

// status needs only a session — a signed-in non-admin may read it. Reading what has been evaluated
// costs nothing; starting a run costs the box.
func TestEvaluateStatusSessionOnly(t *testing.T) {
	up := newEvalUpstream(t)
	up.status, up.body = http.StatusOK, `{"state":"idle","verdicts":[]}`
	srv, h := newEvalGateway(t, up, []string{"admin-1"})

	code, body, hdr := callEval(t, srv, h, http.MethodGet, "/api/evaluate/status", "u-not-admin", "")
	if code != http.StatusOK {
		t.Fatalf("signed-in status: got %d, want 200", code)
	}
	if body["state"] != "idle" {
		t.Errorf("upstream body not passed through: %v", body)
	}
	if up.lastMth != http.MethodGet || up.lastURI != "/evaluate/status" {
		t.Errorf("upstream call = %s %s", up.lastMth, up.lastURI)
	}
	if hdr.Get("Cache-Control") != "no-store" {
		t.Errorf("status must not be cached; Cache-Control = %q", hdr.Get("Cache-Control"))
	}
}

func TestEvaluateUnreachableUpstreamIsAReadable502(t *testing.T) {
	up := newEvalUpstream(t)
	srv, h := newEvalGateway(t, up, []string{"admin-1"})
	up.srv.Close() // the prediction service is gone

	code, body, _ := callEval(t, srv, h, http.MethodGet, "/api/evaluate/status", "admin-1", "")
	if code != http.StatusBadGateway {
		t.Fatalf("got %d, want 502", code)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "prediction unreachable") {
		t.Errorf("502 should say what was unreachable; got %q", msg)
	}
}

func TestEvaluateEventsUsesSameFailClosedAdminBoundary(t *testing.T) {
	up := newEvalUpstream(t)
	srv, h := newEvalGateway(t, up, []string{"admin-1"})

	code, _, _ := callEval(t, srv, h, http.MethodPost, "/api/evaluate-events/run", "u1", "")
	if code != http.StatusForbidden || up.hits != 0 {
		t.Fatalf("non-admin PEAD run: code=%d upstream hits=%d; want 403/0", code, up.hits)
	}
	code, body, hdr := callEval(
		t, srv, h, http.MethodPost, "/api/evaluate-events/run", "admin-1", "",
	)
	if code != http.StatusAccepted || body["started"] != true {
		t.Fatalf("admin PEAD run: code=%d body=%v", code, body)
	}
	if up.lastURI != "/evaluate-events/run" || up.lastMth != http.MethodPost {
		t.Errorf("upstream PEAD call = %s %s", up.lastMth, up.lastURI)
	}
	if hdr.Get("Cache-Control") != "no-store" {
		t.Errorf("PEAD run must not be cached; Cache-Control = %q", hdr.Get("Cache-Control"))
	}
}

func TestEvaluateEventsStatusIsSessionOnly(t *testing.T) {
	up := newEvalUpstream(t)
	up.status, up.body = http.StatusOK, `{"state":"idle","kind":"event","verdicts":[]}`
	srv, h := newEvalGateway(t, up, []string{"admin-1"})

	code, _, _ := callEval(t, srv, h, http.MethodGet, "/api/evaluate-events/status", "", "")
	if code != http.StatusUnauthorized || up.hits != 0 {
		t.Fatalf("guest PEAD status: code=%d hits=%d; want 401/0", code, up.hits)
	}
	code, body, _ := callEval(
		t, srv, h, http.MethodGet, "/api/evaluate-events/status", "u-not-admin", "",
	)
	if code != http.StatusOK || body["kind"] != "event" {
		t.Fatalf("signed-in PEAD status: code=%d body=%v", code, body)
	}
	if up.lastURI != "/evaluate-events/status" {
		t.Errorf("upstream PEAD status URI = %q", up.lastURI)
	}
}

func TestEstimateSnapshotsUsesSameFailClosedAdminBoundary(t *testing.T) {
	up := newEvalUpstream(t)
	srv, h := newEvalGateway(t, up, []string{"admin-1"})

	code, _, _ := callEval(t, srv, h, http.MethodPost, "/api/estimate-snapshots/run", "u1", "")
	if code != http.StatusForbidden || up.hits != 0 {
		t.Fatalf("non-admin estimate run: code=%d hits=%d; want 403/0", code, up.hits)
	}
	code, body, hdr := callEval(
		t, srv, h, http.MethodPost, "/api/estimate-snapshots/run", "admin-1", "",
	)
	if code != http.StatusAccepted || body["started"] != true {
		t.Fatalf("admin estimate run: code=%d body=%v", code, body)
	}
	if up.lastURI != "/estimate-snapshots/run" || up.lastMth != http.MethodPost {
		t.Errorf("upstream estimate call = %s %s", up.lastMth, up.lastURI)
	}
	if hdr.Get("Cache-Control") != "no-store" {
		t.Errorf("estimate run must not be cached; Cache-Control = %q", hdr.Get("Cache-Control"))
	}
}

func TestEstimateSnapshotsStatusIsSessionOnlyAndParametersAreForwarded(t *testing.T) {
	up := newEvalUpstream(t)
	srv, h := newEvalGateway(t, up, []string{"admin-1"})

	code, _, _ := callEval(t, srv, h, http.MethodGet, "/api/estimate-snapshots/status", "", "")
	if code != http.StatusUnauthorized || up.hits != 0 {
		t.Fatalf("guest estimate status: code=%d hits=%d; want 401/0", code, up.hits)
	}

	up.status, up.body = http.StatusBadRequest, `{"detail":"takes no parameters"}`
	code, _, _ = callEval(
		t, srv, h, http.MethodPost, "/api/estimate-snapshots/run?maxCalls=999", "admin-1", "",
	)
	if code != http.StatusBadRequest || up.lastURI != "/estimate-snapshots/run?maxCalls=999" {
		t.Fatalf("parameter forwarding: code=%d URI=%q", code, up.lastURI)
	}

	up.status, up.body = http.StatusOK, `{"state":"done","kind":"estimate"}`
	code, body, _ := callEval(
		t, srv, h, http.MethodGet, "/api/estimate-snapshots/status", "u1", "",
	)
	if code != http.StatusOK || body["kind"] != "estimate" {
		t.Fatalf("signed-in estimate status: code=%d body=%v", code, body)
	}
}
