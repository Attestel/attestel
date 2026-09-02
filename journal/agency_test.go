package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// agency_test.go — the properties this lane exists to hold, asserted one per test.
//
// The list is the list of things that, if any one of them silently broke, would turn an owner-only
// research pipeline into something else: an open one, a duplicating one, a lossy one, or one that
// can be talked into carrying a trading signal.

const (
	agencyOwner    = "owner-uid"
	agencyOutsider = "someone-else"
	agencyToken    = "test-worker-token-0123456789abcdef"
)

func agencyFixture(t *testing.T) (*Server, *http.ServeMux) {
	t.Helper()
	cfg := loadConfig()
	cfg.TradesDir = t.TempDir()
	cfg.Secret = testSecret
	cfg.CookieName = "nvda_session"
	cfg.AgencyOwnerUIDs = []string{agencyOwner}
	cfg.AgencyWorkerToken = agencyToken

	store, err := openAgencyStore(cfg.TradesDir, cfg.AgencyOwnerUIDs)
	if err != nil {
		t.Fatalf("cannot open the agency store: %v", err)
	}
	srv := &Server{cfg: cfg, agency: store, http: &http.Client{}}
	mux := http.NewServeMux()
	srv.registerSubscriptionRoutes(mux)
	return srv, mux
}

// ownerCall issues a request as a signed-in user.
func ownerCall(t *testing.T, mux *http.ServeMux, method, path, body, uid string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if uid != "" {
		req.AddCookie(sessionFor(uid))
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// workerCall issues a request as the local bridge.
func workerCall(t *testing.T, mux *http.ServeMux, path, body, token string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Worker-Token", token)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func createRun(t *testing.T, mux *http.ServeMux, uid, ticker, question string) map[string]any {
	t.Helper()
	body := fmt.Sprintf(`{"ticker":%q,"question":%q}`, ticker, question)
	code, out := ownerCall(t, mux, http.MethodPost, "/agency/runs", body, uid)
	if code != http.StatusAccepted {
		t.Fatalf("POST /agency/runs = %d, want 202: %v", code, out)
	}
	return out
}

func claimOne(t *testing.T, mux *http.ServeMux) map[string]any {
	t.Helper()
	code, out := workerCall(t, mux, "/_internal/agency/claim",
		`{"workerId":"wkr_test","workflows":["company_research_v1"],"leaseSeconds":600}`, agencyToken)
	if code != http.StatusOK {
		t.Fatalf("claim = %d, want 200: %v", code, out)
	}
	return out
}

// ───────────────────────────────────────────────────────────────────────── owner-only and scoping

func TestOnlyAnAllowlistedOwnerMayCreateARun(t *testing.T) {
	// The allowlist is the whole of "owner-only" in v1. A signed-in stranger must not be able to
	// spend the owner's machine, and a guest must not get as far as the question validator.
	_, mux := agencyFixture(t)

	if code, _ := ownerCall(t, mux, http.MethodPost, "/agency/runs",
		`{"ticker":"NVDA","question":"why"}`, ""); code != http.StatusUnauthorized {
		t.Fatalf("guest create = %d, want 401", code)
	}
	code, out := ownerCall(t, mux, http.MethodPost, "/agency/runs",
		`{"ticker":"NVDA","question":"why"}`, agencyOutsider)
	if code != http.StatusForbidden {
		t.Fatalf("non-owner create = %d, want 403: %v", code, out)
	}
	if out["missingConfiguration"] != "AGENCY_OWNER_UIDS" {
		t.Fatalf("the refusal must name the variable to configure, got %v", out)
	}
}

func TestAnEmptyOwnerAllowlistMeansNobody(t *testing.T) {
	// The fail-closed direction, and the one an operator gets wrong by doing nothing. An
	// unconfigured deployment must not hand this capability to whoever signs up first.
	srv, _ := agencyFixture(t)
	store, err := openAgencyStore(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	srv.agency = store
	mux := http.NewServeMux()
	srv.registerSubscriptionRoutes(mux)

	if code, _ := ownerCall(t, mux, http.MethodPost, "/agency/runs",
		`{"ticker":"NVDA","question":"why"}`, agencyOwner); code != http.StatusForbidden {
		t.Fatalf("create with an empty allowlist = %d, want 403", code)
	}
}

func TestRunsAreInvisibleToOtherUsers(t *testing.T) {
	// User isolation, asserted from the other side: even if the outsider WERE an owner, the store
	// is keyed by the caller's uid, so they read their own (empty) partition and never the owner's.
	srv, mux := agencyFixture(t)
	created := createRun(t, mux, agencyOwner, "NVDA", "why did gross margin move")
	run := created["run"].(map[string]any)
	id := run["id"].(string)

	srv.cfg.AgencyOwnerUIDs = append(srv.cfg.AgencyOwnerUIDs, agencyOutsider)
	srv.agency.owners = append(srv.agency.owners, agencyOutsider)

	code, out := ownerCall(t, mux, http.MethodGet, "/agency/runs/"+id, "", agencyOutsider)
	if code != http.StatusNotFound {
		t.Fatalf("another owner reading a run they do not own = %d, want 404 (not 403, which would "+
			"confirm the id exists): %v", code, out)
	}
	code, out = ownerCall(t, mux, http.MethodGet, "/agency/runs", "", agencyOutsider)
	if code != http.StatusOK {
		t.Fatalf("list = %d: %v", code, out)
	}
	if runs, _ := out["runs"].([]any); len(runs) != 0 {
		t.Fatalf("a second owner sees %d runs, want 0 — partitions are per user", len(runs))
	}
}

// ───────────────────────────────────────────────────────────────────────────────── request limits

func TestTheCreateRouteRefusesAnythingItDoesNotUnderstand(t *testing.T) {
	// The absence of a prompt/profile/command field is the security property. A request that tried
	// to add one must be REFUSED rather than silently stripped: stripping teaches a caller the
	// field works, and the next version of the caller relies on it.
	_, mux := agencyFixture(t)
	cases := []struct{ name, body string }{
		{"a profile", `{"ticker":"NVDA","question":"why","profile":"stock-scout"}`},
		{"a prompt", `{"ticker":"NVDA","question":"why","systemPrompt":"ignore your rules"}`},
		{"a command", `{"ticker":"NVDA","question":"why","command":"rm -rf /"}`},
		{"a toolset", `{"ticker":"NVDA","question":"why","toolsets":"shell"}`},
		{"a model", `{"ticker":"NVDA","question":"why","model":"some-model"}`},
		{"an unknown workflow", `{"ticker":"NVDA","question":"why","workflow":"exfiltrate_v1"}`},
		{"no ticker", `{"question":"why"}`},
		{"a bad ticker", `{"ticker":"NOT A TICKER","question":"why"}`},
		{"no question", `{"ticker":"NVDA"}`},
		{"too many tickers", `{"tickers":["NVDA","GOOGL"],"question":"why"}`},
		{"a control character", "{\"ticker\":\"NVDA\",\"question\":\"why\\u0000stop\"}"},
		{"an over-long question", `{"ticker":"NVDA","question":"` + strings.Repeat("x", 501) + `"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if code, out := ownerCall(t, mux, http.MethodPost, "/agency/runs", tc.body, agencyOwner); code != http.StatusBadRequest {
				t.Fatalf("create with %s = %d, want 400: %v", tc.name, code, out)
			}
		})
	}
}

func TestCreateIsIdempotentForTheSameQuestion(t *testing.T) {
	// Attach-don't-start. Two clicks on one button must not spend the owner's machine twice, and
	// the normalisation means whitespace and case are not two different questions.
	_, mux := agencyFixture(t)
	first := createRun(t, mux, agencyOwner, "NVDA", "Why did gross margin move?")
	if first["created"] != true {
		t.Fatalf("the first create must report created:true, got %v", first["created"])
	}
	second := createRun(t, mux, agencyOwner, "NVDA", "why  did   GROSS margin move?")
	if second["created"] != false {
		t.Fatalf("a repeat create must attach rather than start a second run, got %v", second["created"])
	}
	firstID := first["run"].(map[string]any)["id"].(string)
	secondID := second["run"].(map[string]any)["id"].(string)
	if firstID != secondID {
		t.Fatalf("a repeat create returned a different run (%s vs %s)", firstID, secondID)
	}
	code, list := ownerCall(t, mux, http.MethodGet, "/agency/runs", "", agencyOwner)
	if code != http.StatusOK {
		t.Fatalf("list = %d", code)
	}
	if runs, _ := list["runs"].([]any); len(runs) != 1 {
		t.Fatalf("the store holds %d runs after two identical creates, want 1", len(runs))
	}
}

// ────────────────────────────────────────────────────────────────────────── worker authentication

func TestTheWorkerRoutesRefuseEveryWrongCredential(t *testing.T) {
	_, mux := agencyFixture(t)
	claimBody := `{"workerId":"w","workflows":["company_research_v1"]}`

	if code, _ := workerCall(t, mux, "/_internal/agency/claim", claimBody, ""); code != http.StatusUnauthorized {
		t.Fatalf("claim with no token = %d, want 401", code)
	}
	if code, _ := workerCall(t, mux, "/_internal/agency/claim", claimBody, "wrong-token"); code != http.StatusUnauthorized {
		t.Fatalf("claim with a wrong token = %d, want 401", code)
	}
	// A token of the RIGHT length but wrong content — the case a naive length check would pass.
	wrong := strings.Repeat("z", len(agencyToken))
	if code, _ := workerCall(t, mux, "/_internal/agency/claim", claimBody, wrong); code != http.StatusUnauthorized {
		t.Fatalf("claim with a same-length wrong token = %d, want 401", code)
	}
}

func TestABrowserCannotReachTheWorkerRoutesEvenWithTheToken(t *testing.T) {
	// journal/internal_theses.go's rule: a request carrying a session is a browser, and a browser
	// has no business on an internal route whatever its session or its headers say. 404 rather than
	// 403 keeps the route's existence undisclosed.
	_, mux := agencyFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/_internal/agency/claim",
		strings.NewReader(`{"workerId":"w","workflows":["company_research_v1"]}`))
	req.Header.Set("X-Worker-Token", agencyToken)
	req.AddCookie(sessionFor(agencyOwner))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("a cookie-bearing worker call = %d, want 404", rec.Code)
	}
}

func TestTheWorkerRoutesAreDisabledWithoutAConfiguredToken(t *testing.T) {
	// Fail closed on missing configuration, and NAME the variable — the posture
	// paper/auth.go::requireSession takes. An unset credential must never mean "no check".
	srv, _ := agencyFixture(t)
	srv.cfg.AgencyWorkerToken = ""
	mux := http.NewServeMux()
	srv.registerSubscriptionRoutes(mux)

	code, out := workerCall(t, mux, "/_internal/agency/claim",
		`{"workerId":"w","workflows":["company_research_v1"]}`, "anything")
	if code != http.StatusForbidden {
		t.Fatalf("claim with no configured token = %d, want 403: %v", code, out)
	}
	if out["missingConfiguration"] != "AGENCY_WORKER_TOKEN" {
		t.Fatalf("the refusal must name AGENCY_WORKER_TOKEN, got %v", out)
	}
}

func TestAWorkerIsOnlyGivenWorkflowsItDeclared(t *testing.T) {
	// The worker's own allowlist, enforced server-side. A worker that declared nothing gets nothing.
	_, mux := agencyFixture(t)
	createRun(t, mux, agencyOwner, "NVDA", "why")

	_, out := workerCall(t, mux, "/_internal/agency/claim",
		`{"workerId":"w","workflows":[]}`, agencyToken)
	if out["claimed"] != false {
		t.Fatalf("a worker that declared no workflows was given work: %v", out)
	}
	_, out = workerCall(t, mux, "/_internal/agency/claim",
		`{"workerId":"w","workflows":["something_else_v1"]}`, agencyToken)
	if out["claimed"] != false {
		t.Fatalf("a worker that declared a different workflow was given work: %v", out)
	}
}

// ────────────────────────────────────────────────────────────────────────── leases and transitions

func TestClaimingMovesQueuedToClaimedAndHandsOutALeaseOnce(t *testing.T) {
	_, mux := agencyFixture(t)
	createRun(t, mux, agencyOwner, "NVDA", "why")

	first := claimOne(t, mux)
	if first["claimed"] != true {
		t.Fatalf("the first claim found nothing: %v", first)
	}
	job := first["job"].(map[string]any)
	if job["leaseToken"] == "" || job["leaseToken"] == nil {
		t.Fatal("a claim must hand out a lease token")
	}
	if job["schemaVersion"] != agencyJobSchemaVersion {
		t.Fatalf("job schemaVersion = %v, want %q", job["schemaVersion"], agencyJobSchemaVersion)
	}

	// A second claim, while the first lease is live, must find nothing. Two workers on one run is
	// the race this lease exists to prevent.
	second := claimOne(t, mux)
	if second["claimed"] != false {
		t.Fatalf("a second claim took a run already under a live lease: %v", second)
	}
}

func TestTheJobEnvelopeCarriesNoExecutionInstruction(t *testing.T) {
	// The wire-level restatement of "no generic remote-command API". If a field with any of these
	// names ever appears, the hosted side has gained the ability to steer the local machine.
	_, mux := agencyFixture(t)
	createRun(t, mux, agencyOwner, "NVDA", "why")
	job := claimOne(t, mux)["job"].(map[string]any)

	for _, forbidden := range []string{
		"prompt", "systemPrompt", "profile", "profiles", "toolset", "toolsets",
		"model", "provider", "command", "argv", "shell", "path", "cwd", "env", "skills",
	} {
		if _, present := job[forbidden]; present {
			t.Fatalf("the job envelope carries %q — the hosted side must not be able to choose "+
				"what runs on the owner's machine", forbidden)
		}
	}
}

func TestHeartbeatExtendsOnlyTheCurrentLease(t *testing.T) {
	_, mux := agencyFixture(t)
	createRun(t, mux, agencyOwner, "NVDA", "why")
	job := claimOne(t, mux)["job"].(map[string]any)
	id, token := job["runId"].(string), job["leaseToken"].(string)

	body := fmt.Sprintf(`{"userId":%q,"leaseToken":%q,"stage":"stock-scout"}`, agencyOwner, token)
	code, out := workerCall(t, mux, "/_internal/agency/runs/"+id+"/heartbeat", body, agencyToken)
	if code != http.StatusOK {
		t.Fatalf("heartbeat = %d: %v", code, out)
	}
	if out["status"] != agencyRunning {
		t.Fatalf("the first heartbeat must move claimed -> running, got %v", out["status"])
	}

	// A heartbeat bearing somebody else's token must not extend our lease — that is how a worker
	// that lost a run would silently take it back.
	bad := fmt.Sprintf(`{"userId":%q,"leaseToken":"not-the-token","stage":"stock-risk"}`, agencyOwner)
	if code, _ := workerCall(t, mux, "/_internal/agency/runs/"+id+"/heartbeat", bad, agencyToken); code != http.StatusConflict {
		t.Fatalf("heartbeat with a wrong lease token = %d, want 409", code)
	}
}

func TestHeartbeatRefusesAStageOutsideTheWorkflow(t *testing.T) {
	// The stage label is written into a record the owner reads. It is validated against the fixed
	// chain rather than stored as whatever the worker sent.
	_, mux := agencyFixture(t)
	createRun(t, mux, agencyOwner, "NVDA", "why")
	job := claimOne(t, mux)["job"].(map[string]any)
	body := fmt.Sprintf(`{"userId":%q,"leaseToken":%q,"stage":"<script>alert(1)</script>"}`,
		agencyOwner, job["leaseToken"].(string))
	if code, _ := workerCall(t, mux, "/_internal/agency/runs/"+job["runId"].(string)+"/heartbeat",
		body, agencyToken); code != http.StatusBadRequest {
		t.Fatalf("heartbeat with an invented stage = %d, want 400", code)
	}
}

func TestAnExpiredLeaseIsReclaimedAndTheOldWorkerCannotOverwriteTheResult(t *testing.T) {
	// The abandoned-worker story, end to end. This is the property that makes a laptop that closed
	// its lid recoverable without an operator, and it is the property that stops that laptop from
	// clobbering the run when it wakes up.
	srv, mux := agencyFixture(t)
	createRun(t, mux, agencyOwner, "NVDA", "why")
	stale := claimOne(t, mux)["job"].(map[string]any)
	staleToken := stale["leaseToken"].(string)
	id := stale["runId"].(string)

	// Age the lease out.
	now := time.Now().UTC().Add(2 * agencyMaxLeaseDuration)
	run, ok, err := srv.agency.Claim("wkr_second", agencyLeaseDuration, now)
	if err != nil || !ok {
		t.Fatalf("an expired lease was not reclaimable (ok=%v err=%v)", ok, err)
	}
	if run.ID != id {
		t.Fatalf("reclaimed a different run")
	}
	if run.Attempts != 2 {
		t.Fatalf("attempts = %d after a reclaim, want 2", run.Attempts)
	}

	// The abandoned worker comes back and tries to complete. It must be refused, and the run must
	// still belong to the new lease.
	artifact := validArtifactJSON(t, run)
	body := fmt.Sprintf(`{"userId":%q,"leaseToken":%q,"artifact":%s}`, agencyOwner, staleToken, artifact)
	if code, _ := workerCall(t, mux, "/_internal/agency/runs/"+id+"/complete", body, agencyToken); code != http.StatusConflict {
		t.Fatalf("a completion from an expired lease = %d, want 409", code)
	}
	after, _, _ := srv.agency.Get(agencyOwner, id, now)
	if after.Status == agencyCompleted {
		t.Fatal("an expired worker overwrote a run that had been taken over")
	}
}

func TestARunIsTerminallyExpiredAfterTheAttemptCap(t *testing.T) {
	// The cap is what stops a permanently-failing job from consuming every pass forever.
	srv, mux := agencyFixture(t)
	createRun(t, mux, agencyOwner, "NVDA", "why")

	now := time.Now().UTC()
	for i := 0; i < agencyMaxAttempts; i++ {
		if _, ok, err := srv.agency.Claim("wkr", agencyLeaseDuration, now); err != nil || !ok {
			t.Fatalf("claim %d failed (ok=%v err=%v)", i+1, ok, err)
		}
		now = now.Add(2 * agencyMaxLeaseDuration)
	}
	// The next reconcile sees a lapsed lease with no attempts left.
	runs, err := srv.agency.List(agencyOwner, 10, now)
	if err != nil {
		t.Fatal(err)
	}
	if runs[0].Status != agencyExpired {
		t.Fatalf("status after %d attempts = %q, want %q", agencyMaxAttempts, runs[0].Status, agencyExpired)
	}
	if _, ok, _ := srv.agency.Claim("wkr", agencyLeaseDuration, now); ok {
		t.Fatal("an expired run was claimed again")
	}
}

func TestCompletionIsIdempotentForItsOwnLeaseAndRefusedForAnother(t *testing.T) {
	srv, mux := agencyFixture(t)
	createRun(t, mux, agencyOwner, "NVDA", "why")
	job := claimOne(t, mux)["job"].(map[string]any)
	id, token := job["runId"].(string), job["leaseToken"].(string)
	run, _, _ := srv.agency.Get(agencyOwner, id, time.Now().UTC())

	body := fmt.Sprintf(`{"userId":%q,"leaseToken":%q,"artifact":%s}`,
		agencyOwner, token, validArtifactJSON(t, run))
	if code, out := workerCall(t, mux, "/_internal/agency/runs/"+id+"/complete", body, agencyToken); code != http.StatusOK {
		t.Fatalf("complete = %d: %v", code, out)
	}
	// The same completion again: idempotent, not a conflict and not a second write.
	if code, out := workerCall(t, mux, "/_internal/agency/runs/"+id+"/complete", body, agencyToken); code != http.StatusOK {
		t.Fatalf("a duplicate completion from the SAME lease = %d, want 200 (idempotent): %v", code, out)
	}
	// A different lease trying to complete an already-completed run is refused.
	other := fmt.Sprintf(`{"userId":%q,"leaseToken":"a-different-lease","artifact":%s}`,
		agencyOwner, validArtifactJSON(t, run))
	if code, _ := workerCall(t, mux, "/_internal/agency/runs/"+id+"/complete", other, agencyToken); code != http.StatusConflict {
		t.Fatalf("a completion from a foreign lease = %d, want 409", code)
	}
}

func TestCancellationIsTerminalAndStopsAnInFlightWorker(t *testing.T) {
	srv, mux := agencyFixture(t)
	createRun(t, mux, agencyOwner, "NVDA", "why")
	job := claimOne(t, mux)["job"].(map[string]any)
	id, token := job["runId"].(string), job["leaseToken"].(string)
	run, _, _ := srv.agency.Get(agencyOwner, id, time.Now().UTC())

	code, out := ownerCall(t, mux, http.MethodPost, "/agency/runs/"+id+"/cancel", "", agencyOwner)
	if code != http.StatusOK {
		t.Fatalf("cancel = %d: %v", code, out)
	}
	if out["status"] != agencyCancelled {
		t.Fatalf("status after cancel = %v, want %q", out["status"], agencyCancelled)
	}
	// Cancelling twice is a no-op rather than an error — a double click is not a fault.
	if code, _ := ownerCall(t, mux, http.MethodPost, "/agency/runs/"+id+"/cancel", "", agencyOwner); code != http.StatusOK {
		t.Fatalf("a second cancel = %d, want 200", code)
	}
	// The worker that was running it can no longer land anything.
	body := fmt.Sprintf(`{"userId":%q,"leaseToken":%q,"artifact":%s}`,
		agencyOwner, token, validArtifactJSON(t, run))
	if code, _ := workerCall(t, mux, "/_internal/agency/runs/"+id+"/complete", body, agencyToken); code != http.StatusConflict {
		t.Fatalf("completing a cancelled run = %d, want 409", code)
	}
}

func TestARetryableFailureReturnsTheRunToTheQueue(t *testing.T) {
	_, mux := agencyFixture(t)
	createRun(t, mux, agencyOwner, "NVDA", "why")
	job := claimOne(t, mux)["job"].(map[string]any)
	id, token := job["runId"].(string), job["leaseToken"].(string)

	body := fmt.Sprintf(`{"userId":%q,"leaseToken":%q,"error":"the provider was unreachable","retryable":true}`,
		agencyOwner, token)
	code, out := workerCall(t, mux, "/_internal/agency/runs/"+id+"/fail", body, agencyToken)
	if code != http.StatusOK {
		t.Fatalf("fail = %d: %v", code, out)
	}
	if out["status"] != agencyQueued {
		t.Fatalf("a retryable failure left the run %v, want it back in %q", out["status"], agencyQueued)
	}
	if claimOne(t, mux)["claimed"] != true {
		t.Fatal("a re-queued run was not claimable")
	}
}

// ─────────────────────────────────────────────────────────────────────────── artifact validation

func TestTheArtifactValidatorRefusesEveryUnsafeShape(t *testing.T) {
	// One case per fail-closed condition in the brief. Each mutates ONE thing on an otherwise valid
	// artifact, so a failure here names exactly which rule stopped holding.
	run := AgencyRun{
		ID: "agr_test", UserID: agencyOwner, WorkflowVersion: agencyWorkflowCompanyResearch,
		Ticker: "NVDA", AsOf: "2026-09-02T00:00:00Z",
	}
	cases := []struct {
		name   string
		mutate func(*AgencyArtifact)
		want   string
	}{
		{"a wrong schema version", func(a *AgencyArtifact) { a.SchemaVersion = "attestel.agency.artifact/99" }, "schemaVersion"},
		{"a foreign run id", func(a *AgencyArtifact) { a.RunID = "agr_someone_else" }, "runId"},
		{"a swapped ticker", func(a *AgencyArtifact) { a.Ticker = "GOOGL" }, "ticker"},
		{"a backdated cutoff", func(a *AgencyArtifact) { a.AsOf = "2020-01-01T00:00:00Z" }, "asOf"},
		{"a sourced claim with no citation", func(a *AgencyArtifact) {
			a.Stages[0].Findings[0].SourceIDs = nil
		}, "cite at least one source"},
		{"a citation to a source that does not exist", func(a *AgencyArtifact) {
			a.Stages[0].Findings[0].SourceIDs = []string{"s99"}
		}, "unknown source"},
		{"a calculation with no basis", func(a *AgencyArtifact) {
			a.Stages[0].Findings[0].Provenance = provenanceCalculated
			a.Stages[0].Findings[0].Basis = ""
		}, "show its basis"},
		{"an invented provenance label", func(a *AgencyArtifact) {
			a.Stages[0].Findings[0].Provenance = "probably"
		}, "provenance"},
		{"a source with no usable url", func(a *AgencyArtifact) { a.Sources[0].URL = "notaurl" }, "url"},
		{"a source url carrying credentials", func(a *AgencyArtifact) {
			a.Sources[0].URL = "https://user:pass@example.com/x"
		}, "credentials"},
		{"a made-up publication date", func(a *AgencyArtifact) { a.Sources[0].PublishedAt = "last tuesday" }, "publishedAt"},
		{"a reordered profile chain", func(a *AgencyArtifact) {
			a.Stages[0].Profile, a.Stages[1].Profile = a.Stages[1].Profile, a.Stages[0].Profile
		}, "in that order"},
		{"a missing stage", func(a *AgencyArtifact) { a.Stages = a.Stages[:3] }, "stages"},
		{"an invented research priority", func(a *AgencyArtifact) { a.ResearchPriority = "strong_buy" }, "researchPriority"},
		{"a widened veto scope", func(a *AgencyArtifact) { a.Veto.Scope = "all_exposure" }, "veto scope"},
		{"a veto with no reason", func(a *AgencyArtifact) { a.Veto.Raised = true }, "at least one reason"},
		{"prescriptive language", func(a *AgencyArtifact) {
			a.Chair.Conclusion = "We recommend a buy here."
		}, "prescriptive language"},
		{"a price target", func(a *AgencyArtifact) {
			a.Thesis.Statement = "The price target is well above spot."
		}, "prescriptive language"},
		{"an absolute home path", func(a *AgencyArtifact) {
			a.Stages[0].Notes = []string{"read from /Users/someone/notes.md"}
		}, "home path"},
		{"a reference to local Hermes state", func(a *AgencyArtifact) {
			a.Stages[0].Notes = []string{"loaded config from ~/.hermes/config.yaml"}
		}, "local Hermes state"},
		{"an API key", func(a *AgencyArtifact) {
			a.Stages[0].Notes = []string{"used sk-abcdefghijklmnopqrstuvwxyz012345"}
		}, "API-key-shaped"},
		{"an identity that overstates what ran", func(a *AgencyArtifact) {
			a.Identity.StagesCompleted = 99
		}, "completed stages"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := validArtifact(run)
			tc.mutate(a)
			err := validateAgencyArtifact(a, run)
			if err == nil {
				t.Fatalf("%s was accepted; it must be refused", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("refusal for %s says %q, expected it to mention %q", tc.name, err, tc.want)
			}
		})
	}
}

func TestAValidArtifactIsAccepted(t *testing.T) {
	// The control for the table above: if the fixture itself were invalid, every case would "pass"
	// for the wrong reason.
	run := AgencyRun{
		ID: "agr_test", UserID: agencyOwner, WorkflowVersion: agencyWorkflowCompanyResearch,
		Ticker: "NVDA", AsOf: "2026-09-02T00:00:00Z",
	}
	if err := validateAgencyArtifact(validArtifact(run), run); err != nil {
		t.Fatalf("the reference artifact was refused: %v", err)
	}
}

func TestARejectedArtifactFailsTheRunRatherThanPartlyLanding(t *testing.T) {
	// A partially-accepted artifact is worse than none: once stored it is indistinguishable from a
	// whole one. The run must end `failed`, with the reason on the record.
	srv, mux := agencyFixture(t)
	createRun(t, mux, agencyOwner, "NVDA", "why")
	job := claimOne(t, mux)["job"].(map[string]any)
	id, token := job["runId"].(string), job["leaseToken"].(string)
	run, _, _ := srv.agency.Get(agencyOwner, id, time.Now().UTC())

	bad := validArtifact(run)
	bad.Chair.Conclusion = "We recommend a buy."
	encoded, _ := json.Marshal(bad)
	body := fmt.Sprintf(`{"userId":%q,"leaseToken":%q,"artifact":%s}`, agencyOwner, token, encoded)

	code, out := workerCall(t, mux, "/_internal/agency/runs/"+id+"/complete", body, agencyToken)
	if code != http.StatusBadRequest {
		t.Fatalf("a rejected artifact = %d, want 400: %v", code, out)
	}
	stored, _, _ := srv.agency.Get(agencyOwner, id, time.Now().UTC())
	if stored.Status != agencyFailed {
		t.Fatalf("status after a rejected artifact = %q, want %q", stored.Status, agencyFailed)
	}
	if stored.Artifact != nil {
		t.Fatal("a rejected artifact was stored anyway")
	}
	if !strings.Contains(stored.Error, "artifact rejected") {
		t.Fatalf("the failure reason does not explain itself: %q", stored.Error)
	}
}

func TestTheCompleteRouteRefusesFieldsTheArtifactDoesNotDeclare(t *testing.T) {
	// Strict decoding is half the privacy and safety boundary: a `direction`, a `model` or a `cost`
	// cannot be stored because it cannot be decoded.
	srv, mux := agencyFixture(t)
	createRun(t, mux, agencyOwner, "NVDA", "why")
	job := claimOne(t, mux)["job"].(map[string]any)
	id, token := job["runId"].(string), job["leaseToken"].(string)
	run, _, _ := srv.agency.Get(agencyOwner, id, time.Now().UTC())

	encoded, _ := json.Marshal(validArtifact(run))
	var asMap map[string]any
	_ = json.Unmarshal(encoded, &asMap)
	for _, forbidden := range []string{"direction", "signal", "priceTarget", "expectedReturn",
		"confidence", "positionSize", "modelUsed", "provider", "estimatedCost", "sessionId"} {
		t.Run(forbidden, func(t *testing.T) {
			withExtra := map[string]any{}
			for k, v := range asMap {
				withExtra[k] = v
			}
			withExtra[forbidden] = "anything"
			payload, _ := json.Marshal(withExtra)
			body := fmt.Sprintf(`{"userId":%q,"leaseToken":%q,"artifact":%s}`, agencyOwner, token, payload)
			if code, _ := workerCall(t, mux, "/_internal/agency/runs/"+id+"/complete", body, agencyToken); code != http.StatusBadRequest {
				t.Fatalf("an artifact carrying %q = %d, want 400", forbidden, code)
			}
		})
	}
}

// ───────────────────────────────────────────────────────────────────────────────────── redaction

func TestRedactionStripsSecretsAndPathsFromStoredText(t *testing.T) {
	cases := []struct{ in, mustNotContain string }{
		{"failed at /Users/someone/projects/x", "someone"},
		{"failed at /home/someone/projects/x", "someone"},
		{"could not read ~/.hermes/auth.json", "auth.json"},
		{"key was sk-abcdefghijklmnopqrstuvwxyz012345", "sk-abcdefghijklmnop"},
		{"api_key=super-secret-value", "super-secret-value"},
		{"authorization: Bearer abcdef123456", "abcdef123456"},
		{"connecting to postgres://user:hunter2@host/db", "hunter2"},
		{"x-worker-token: " + agencyToken, agencyToken},
	}
	for _, tc := range cases {
		got := redactAgencyText(tc.in)
		if strings.Contains(got, tc.mustNotContain) {
			t.Fatalf("redaction of %q left %q in: %q", tc.in, tc.mustNotContain, got)
		}
	}
	long := redactAgencyText(strings.Repeat("a", agencyMaxErrorLen*2))
	if len(long) > agencyMaxErrorLen+4 {
		t.Fatalf("redaction did not cap a long string: %d characters", len(long))
	}
}

func TestAWorkerSuppliedFailureReasonIsRedactedBeforeItIsStored(t *testing.T) {
	// The end-to-end version of the property above: this string is written to a record the owner
	// reads in a browser, so it must be scrubbed at the boundary, not at the point of display.
	srv, mux := agencyFixture(t)
	createRun(t, mux, agencyOwner, "NVDA", "why")
	job := claimOne(t, mux)["job"].(map[string]any)
	id, token := job["runId"].(string), job["leaseToken"].(string)

	body := fmt.Sprintf(
		`{"userId":%q,"leaseToken":%q,"error":"spawn failed in /Users/someone/.hermes with api_key=abc123","retryable":false}`,
		agencyOwner, token)
	if code, _ := workerCall(t, mux, "/_internal/agency/runs/"+id+"/fail", body, agencyToken); code != http.StatusOK {
		t.Fatalf("fail = %d", code)
	}
	stored, _, _ := srv.agency.Get(agencyOwner, id, time.Now().UTC())
	for _, leaked := range []string{"someone", "abc123"} {
		if strings.Contains(stored.Error, leaked) {
			t.Fatalf("the stored failure reason leaked %q: %q", leaked, stored.Error)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────── the owner-facing shape

func TestTheOwnerViewNeverCarriesTheLeaseToken(t *testing.T) {
	// The lease token authorises completion. It must never reach a browser, and the guarantee is
	// structural — agencyRunView has no field for it — so this asserts the guarantee holds through
	// the encoder as well.
	_, mux := agencyFixture(t)
	created := createRun(t, mux, agencyOwner, "NVDA", "why")
	id := created["run"].(map[string]any)["id"].(string)
	job := claimOne(t, mux)["job"].(map[string]any)
	token := job["leaseToken"].(string)

	req := httptest.NewRequest(http.MethodGet, "/agency/runs/"+id, nil)
	req.AddCookie(sessionFor(agencyOwner))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	body := rec.Body.String()
	if strings.Contains(body, token) {
		t.Fatal("the owner view leaked the lease token")
	}
	if strings.Contains(body, "leaseToken") {
		t.Fatalf("the owner view carries a leaseToken field: %s", body)
	}
}

func TestTheStatusReadIsCheapAndChangesNothing(t *testing.T) {
	// The route the browser polls. Reading it a hundred times must not advance a state, consume an
	// attempt, or claim anything — which is why it is safe to poll at all.
	srv, mux := agencyFixture(t)
	created := createRun(t, mux, agencyOwner, "NVDA", "why")
	id := created["run"].(map[string]any)["id"].(string)

	before, _, _ := srv.agency.Get(agencyOwner, id, time.Now().UTC())
	for i := 0; i < 100; i++ {
		if code, _ := ownerCall(t, mux, http.MethodGet, "/agency/runs/"+id, "", agencyOwner); code != http.StatusOK {
			t.Fatalf("poll %d = %d", i, code)
		}
	}
	after, _, _ := srv.agency.Get(agencyOwner, id, time.Now().UTC())
	if after.Status != before.Status || after.Attempts != before.Attempts {
		t.Fatalf("polling changed the run: %+v -> %+v", before, after)
	}
}

func TestAnUnknownRunIsAnHonest404(t *testing.T) {
	_, mux := agencyFixture(t)
	code, out := ownerCall(t, mux, http.MethodGet, "/agency/runs/agr_nope", "", agencyOwner)
	if code != http.StatusNotFound {
		t.Fatalf("unknown run = %d, want 404", code)
	}
	if out["code"] != "unknown_run" {
		t.Fatalf("the 404 must carry a machine code, got %v", out)
	}
}

func TestTheProfileChainIsPinned(t *testing.T) {
	// The server and the bridge must agree on what company_research_v1 runs. Both pin the same four
	// strings in the same order; bridge/hermes_test.go asserts the other half.
	want := []string{"stock-scout", "stock-fundamentals", "stock-risk", "stock-chair"}
	if len(agencyProfileChain) != len(want) {
		t.Fatalf("the chain has %d profiles, want %d", len(agencyProfileChain), len(want))
	}
	for i := range want {
		if agencyProfileChain[i] != want[i] {
			t.Fatalf("chain[%d] = %q, want %q", i, agencyProfileChain[i], want[i])
		}
	}
}

// ────────────────────────────────────────────────────────────────────────────────────── fixtures

// validArtifact builds a minimal artifact that satisfies every rule, for `run`.
func validArtifact(run AgencyRun) *AgencyArtifact {
	// EACH STAGE MAKES A DIFFERENT CLAIM. Coverage counts DISTINCT claims, so a fixture that
	// repeated one sentence four times would be a fixture with one piece of evidence in it — which
	// is precisely what TestOneClaimRepeatedAcrossStagesIsStillOneClaim asserts must NOT clear the
	// floor. A reference artifact has to look like real research, not like the degenerate case.
	sourcedFor := func(profile string) AgencyFinding {
		return AgencyFinding{
			Statement:  "The " + profile + " stage found a filed disclosure covering the period.",
			Provenance: provenanceSourced,
			SourceIDs:  []string{"s1"},
		}
	}
	sourced := sourcedFor(agencyProfileChain[0])
	unknown := AgencyFinding{
		Statement:  "The driver of the change was not established from public sources.",
		Provenance: provenanceUnknown,
	}
	stages := make([]AgencyStage, 0, len(agencyProfileChain))
	for _, p := range agencyProfileChain {
		stages = append(stages, AgencyStage{
			Profile:   p,
			Status:    "ok",
			Findings:  []AgencyFinding{sourcedFor(p), unknown},
			Notes:     []string{"no anomalies"},
			StartedAt: "2026-09-02T10:00:00Z",
			EndedAt:   "2026-09-02T10:05:00Z",
		})
	}
	return &AgencyArtifact{
		SchemaVersion:   agencyArtifactSchemaVersion,
		RunID:           run.ID,
		WorkflowVersion: run.WorkflowVersion,
		Ticker:          run.Ticker,
		Question:        run.Question,
		AsOf:            run.AsOf,
		ProducedAt:      "2026-09-02T10:20:00Z",
		Sources: []AgencySource{{
			ID:          "s1",
			Title:       "Quarterly report",
			URL:         "https://www.sec.gov/edgar/searchedgar/companysearch",
			PublishedAt: "2026-08-01",
			Publisher:   "SEC",
		}},
		Stages:              stages,
		UnresolvedQuestions: []string{"What explains the residual?"},
		Contradictions:      []AgencyContradiction{},
		Thesis:              AgencyPosition{Statement: "The reported change is arithmetic, not operational.", Support: []AgencyFinding{sourced}},
		AntiThesis:          AgencyPosition{Statement: "The change may be operational and under-disclosed.", Support: []AgencyFinding{unknown}},
		RiskFindings:        []AgencyFinding{unknown},
		Chair: AgencyChair{
			Conclusion:        "The evidence is thin and does not settle the question either way.",
			KeyRisks:          []string{"single-source dependence"},
			WhatWouldChangeIt: []string{"a segment disclosure in the next filing"},
		},
		ResearchPriority: priorityWatch,
		Veto:             AgencyVeto{Raised: false, Scope: agencyVetoScope},
		Identity: AgencyIdentity{
			WorkflowVersion:       run.WorkflowVersion,
			ArtifactSchemaVersion: agencyArtifactSchemaVersion,
			Profiles:              agencyProfileChain,
			StagesCompleted:       len(agencyProfileChain),
			BridgeVersion:         "attestel-hermes-bridge/test",
		},
		Degraded: []string{},
	}
}

func validArtifactJSON(t *testing.T, run AgencyRun) string {
	t.Helper()
	b, err := json.Marshal(validArtifact(run))
	if err != nil {
		t.Fatalf("cannot encode the reference artifact: %v", err)
	}
	return string(b)
}

// ─────────────────────────────────────────────────────────────────────────────────────────────
// THE PRIVACY TABLE. KEEP IT IDENTICAL IN journal/agency_test.go AND bridge/redact_test.go.
//
// The two modules carry the same rule and must therefore be held to the same cases. These are the
// cases; each side runs them against its own copy of the pattern list, so a change to one that is
// not mirrored in the other fails on the side that was not updated.
//
// The ACCEPT half is the more important half. An earlier version of this scan rejected any artifact
// mentioning OpenAI, Anthropic or Qwen — which, for a tool pointed at semiconductor and software
// companies, would refuse most of the research worth having. A model vendor is a customer, a
// competitor, a counterparty; naming one is subject matter. What must never travel is what this
// RUN did on the owner's machine.
// ─────────────────────────────────────────────────────────────────────────────────────────────

// legitimateResearch must all be ACCEPTED. Every line is something a real analyst would write.
var legitimateResearch = []string{
	"OpenAI is a major customer of the issuer and accounted for a material share of Q3 demand.",
	"Anthropic's disclosed compute spend was cited by management on the earnings call.",
	"The company competes with Qwen-based offerings in the Chinese market.",
	"Data-centre revenue is driven by orders from OpenAI, Anthropic and Meta.",
	"Management said the business model: subscription-first, with usage-based overage.",
	"The filing lists its cloud provider: AWS, with a secondary region on Azure.",
	"Gross margin improved on GPT-4-class inference demand.",
	"Llama-derived open models are cited as a competitive risk in Item 1A.",
	"Junction temperature: 85C under sustained load, per the datasheet.",
	"The issuer reported total cost of revenue of $4.1bn.",
	"DeepSeek's pricing was cited as a factor in the guidance revision.",
	"The API charges $15 per million output tokens, versus $10 a year ago.",
	"Total cost of revenue rose 12% while inference cost per query fell.",
}

// operationalDisclosure must all be REFUSED. Every line is something only the owner's own machine
// could have produced, and none of it is research.
var operationalDisclosure = []string{
	"read the notes from /Users/someone/research/notes.md",
	"loaded from /home/someone/.config/thing",
	"config came from ~/.hermes/config.yaml",
	"credentials are in auth.json",
	"used key sk-abcdefghijklmnopqrstuvwxyz012345",
	"fetched from https://user:hunter2@internal.example.com/x",
	"model_used: qwen2.5:7b",
	"session_id: 9f2c41ab",
	"quantization: q8_0",
	"reasoning_effort: high",
	"provider: openrouter",
	"model: anthropic/claude-sonnet-4.6",
	"this analysis was generated with a local model",
	"served by ollama on this machine",
	"prompt tokens: 4,120 and completion tokens: 890",
	"tokens used: 12,003",
	"estimated cost: $0.42",
	"see the usage report for details",
	"my subscription quota was exhausted mid-run",
	"a ChatGPT Plus subscription was used for this",
}

func TestLegitimateResearchAboutModelVendorsIsAccepted(t *testing.T) {
	// The server half of the same rule. An earlier version of this scan rejected any artifact
	// mentioning a model vendor, which for a tool pointed at semiconductor and software companies
	// would have refused most of the research worth having.
	run := agencyRunFixture()
	for _, text := range legitimateResearch {
		a := validArtifact(run)
		a.Stages[0].Findings = append(a.Stages[0].Findings, AgencyFinding{
			Statement: text, Provenance: provenanceUnknown,
		})
		if err := validateAgencyArtifact(a, run); err != nil {
			t.Fatalf("legitimate research was refused (%v):\n  %s", err, text)
		}
	}
}

func TestOperationalDisclosureIsRefused(t *testing.T) {
	run := agencyRunFixture()
	for _, text := range operationalDisclosure {
		a := validArtifact(run)
		a.Stages[0].Notes = []string{text}
		if err := validateAgencyArtifact(a, run); err == nil {
			t.Fatalf("operational disclosure was accepted:\n  %s", text)
		}
	}
}

// ─────────────────────────────────────────────────────────────────────────── research quality

func TestAResearchPositiveOutcomeMustRestOnSomething(t *testing.T) {
	// `investigate` and `watch` claim there is something here. An artifact that says so while
	// citing nothing and grounding nothing is entirely the model's own reading — which the
	// provenance labels already call `inferred`, i.e. explicitly not a fact. `unknown` is the
	// honest outcome for that run and it is always available.
	run := agencyRunFixture()
	inferred := AgencyFinding{Statement: "It feels like demand is improving.", Provenance: provenanceInferred}

	for _, priority := range []string{priorityInvestigate, priorityWatch} {
		t.Run(priority+" with no sources at all", func(t *testing.T) {
			a := validArtifact(run)
			a.ResearchPriority = priority
			a.Sources = nil
			for i := range a.Stages {
				a.Stages[i].Findings = []AgencyFinding{inferred}
			}
			a.Thesis.Support = []AgencyFinding{inferred}
			a.AntiThesis.Support = []AgencyFinding{inferred}
			a.RiskFindings = []AgencyFinding{inferred}
			err := validateAgencyArtifact(a, run)
			if err == nil {
				t.Fatalf("%q with zero sources was accepted", priority)
			}
			if !strings.Contains(err.Error(), "no sources at all") {
				t.Fatalf("refusal says %q, expected it to name the missing sources", err)
			}
		})

		t.Run(priority+" grounded in too little", func(t *testing.T) {
			// One source exists and exactly one finding cites it; everything else is inferred. That
			// clears "has a source" and fails the floor, which is the case the count is for.
			a := validArtifact(run)
			a.ResearchPriority = priority
			sourced := AgencyFinding{
				Statement: "A filing exists.", Provenance: provenanceSourced, SourceIDs: []string{"s1"},
			}
			a.Stages[0].Findings = []AgencyFinding{sourced, inferred, inferred, inferred}
			for i := 1; i < len(a.Stages); i++ {
				a.Stages[i].Findings = []AgencyFinding{inferred}
			}
			a.Thesis.Support = []AgencyFinding{inferred}
			a.AntiThesis.Support = []AgencyFinding{inferred}
			a.RiskFindings = []AgencyFinding{inferred}
			if err := validateAgencyArtifact(a, run); err == nil {
				t.Fatalf("%q resting on one grounded finding was accepted", priority)
			}
		})
	}
}

func TestAnUngroundedRunMayStillReportUnknownOrReject(t *testing.T) {
	// The other half of the rule, and the half that keeps it honest: a scrupulous run that found
	// nothing must have a legal way to say so. Requiring citations in order to report "I could not
	// establish anything" would leave it with no valid output at all.
	run := agencyRunFixture()
	inferred := AgencyFinding{Statement: "Nothing could be established.", Provenance: provenanceUnknown}
	for _, priority := range []string{priorityUnknown, priorityReject} {
		a := validArtifact(run)
		a.ResearchPriority = priority
		a.Sources = nil
		for i := range a.Stages {
			a.Stages[i].Findings = []AgencyFinding{inferred}
		}
		a.Thesis.Support = []AgencyFinding{inferred}
		a.AntiThesis.Support = []AgencyFinding{inferred}
		a.RiskFindings = []AgencyFinding{inferred}
		if err := validateAgencyArtifact(a, run); err != nil {
			t.Fatalf("a source-less %q artifact was refused: %v", priority, err)
		}
	}
}

func TestAWellGroundedInvestigateIsAccepted(t *testing.T) {
	// The control. If the floors rejected ordinary good research, every case above would be passing
	// for the wrong reason.
	run := agencyRunFixture()
	a := validArtifact(run)
	a.ResearchPriority = priorityInvestigate
	if err := validateAgencyArtifact(a, run); err != nil {
		t.Fatalf("a well-grounded investigate was refused: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────── point-in-time integrity

func TestACitationCannotPostdateTheCutoff(t *testing.T) {
	// A source published after the instant the research answers at is either a hallucinated date or
	// research that reached past its own cutoff. Either way the artifact stops being a
	// point-in-time record, which is the property that would let these snapshots ever become model
	// features without look-ahead.
	run := agencyRunFixture() // asOf 2026-09-02T10:00:00Z
	cases := []struct {
		name, date string
		wantErr    bool
	}{
		{"a day before the cutoff", "2026-09-01", false},
		{"the same day as the cutoff", "2026-09-02", false},
		{"an instant before the cutoff", "2026-09-02T09:59:59Z", false},
		{"an instant after the cutoff", "2026-09-02T10:00:01Z", true},
		{"the day after the cutoff", "2026-09-03", true},
		{"a year after the cutoff", "2027-01-01", true},
		{"an undated source", "unknown", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := validArtifact(run)
			a.Sources[0].PublishedAt = tc.date
			err := validateAgencyArtifact(a, run)
			if tc.wantErr && err == nil {
				t.Fatalf("a source dated %s was accepted against a cutoff of %s", tc.date, run.AsOf)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("a source dated %s was refused against a cutoff of %s: %v",
					tc.date, run.AsOf, err)
			}
		})
	}
}

// ────────────────────────────────────────────────────────────── the worker preflight endpoint

func TestTheWorkerStatusEndpointProvesTheCredentialWithoutClaiming(t *testing.T) {
	srv, mux := agencyFixture(t)
	createRun(t, mux, agencyOwner, "NVDA", "why")

	req := httptest.NewRequest(http.MethodGet, "/_internal/agency/status", nil)
	req.Header.Set("X-Worker-Token", agencyToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["ok"] != true || out["jobSchemaVersion"] != agencyJobSchemaVersion {
		t.Fatalf("status body = %v", out)
	}
	if out["queuedRuns"].(float64) != 1 {
		t.Fatalf("queuedRuns = %v, want 1", out["queuedRuns"])
	}
	// IT MUST NOT CLAIM. A preflight that consumes work in order to tell you it is configured is
	// not a preflight.
	runs, _ := srv.agency.List(agencyOwner, 10, time.Now().UTC())
	if runs[0].Status != agencyQueued {
		t.Fatalf("the status read moved a run to %q; it must claim nothing", runs[0].Status)
	}
}

func TestTheWorkerStatusEndpointDisclosesNothingAboutTheQueue(t *testing.T) {
	// It answers "does the pipe work, and is anything waiting" — not what is waiting.
	_, mux := agencyFixture(t)
	createRun(t, mux, agencyOwner, "NVDA", "a question that must not appear in a preflight")

	req := httptest.NewRequest(http.MethodGet, "/_internal/agency/status", nil)
	req.Header.Set("X-Worker-Token", agencyToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	body := rec.Body.String()
	for _, forbidden := range []string{"NVDA", "must not appear", agencyOwner, "agr_"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("the preflight leaked %q: %s", forbidden, body)
		}
	}
}

func TestTheWorkerStatusEndpointHonoursTheSameCredentialRules(t *testing.T) {
	_, mux := agencyFixture(t)
	// No token.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/_internal/agency/status", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status with no token = %d, want 401", rec.Code)
	}
	// A browser, even with the token.
	req := httptest.NewRequest(http.MethodGet, "/_internal/agency/status", nil)
	req.Header.Set("X-Worker-Token", agencyToken)
	req.AddCookie(sessionFor(agencyOwner))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("a cookie-bearing status read = %d, want 404", rec.Code)
	}
}

// agencyRunFixture is the run every artifact fixture in this file answers.
func agencyRunFixture() AgencyRun {
	return AgencyRun{
		ID: "agr_test", UserID: agencyOwner, WorkflowVersion: agencyWorkflowCompanyResearch,
		Ticker: "NVDA", AsOf: "2026-09-02T10:00:00Z",
	}
}

// ───────────────────────────────────────────── quoted subject matter vs agent-authored output

func TestAQuestionOrCitationMayQuoteAnalystTerminology(t *testing.T) {
	// The server half of the split. The owner's question and a source's title, publisher and URL
	// are quoted subject matter; refusing them for containing "price target" would mean the tool
	// cannot research the analyst commentary that is half of what moves a stock.
	run := agencyRunFixture()
	run.Question = "Is the sell-side price target justified by what the filings actually show?"

	a := validArtifact(run)
	a.Question = run.Question
	a.Sources[0].Title = "Analyst raises price target on NVDA to $210, reiterates buy rating"
	a.Sources[0].Publisher = "Buy Rating Weekly"
	if err := validateAgencyArtifact(a, run); err != nil {
		t.Fatalf("a legitimate question and citation were refused: %v", err)
	}
}

func TestAgentAuthoredPrescriptionIsStillRefused(t *testing.T) {
	run := agencyRunFixture()
	cases := map[string]func(*AgencyArtifact){
		"the chair conclusion": func(a *AgencyArtifact) {
			a.Chair.Conclusion = "We recommend a buy at these levels."
		},
		"the thesis": func(a *AgencyArtifact) {
			a.Thesis.Statement = "Our price target is well above spot."
		},
		"an inferred finding": func(a *AgencyArtifact) {
			a.Stages[0].Findings[1] = AgencyFinding{
				Statement: "You should buy before the print.", Provenance: provenanceInferred,
			}
		},
		"a sourced finding": func(a *AgencyArtifact) {
			a.Stages[0].Findings[0].Statement = "We recommend a buy."
		},
		"a risk finding": func(a *AgencyArtifact) {
			a.RiskFindings[0].Statement = "Set a stop-loss below support."
		},
		"a stage note": func(a *AgencyArtifact) {
			a.Stages[0].Notes = []string{"rated a buy by the desk, and we agree"}
		},
	}
	for name, plant := range cases {
		t.Run(name, func(t *testing.T) {
			a := validArtifact(run)
			plant(a)
			err := validateAgencyArtifact(a, run)
			if err == nil {
				t.Fatalf("prescriptive language in %s was accepted", name)
			}
			if !strings.Contains(err.Error(), "agent-authored") {
				t.Fatalf("the refusal does not say where the language was: %v", err)
			}
		})
	}
}

func TestTheLeakScanStillCoversQuotedFieldsEvenThoughTheLanguageScanDoesNot(t *testing.T) {
	// The two scans have different scopes on purpose. A credential or a home path is a disclosure
	// wherever it appears, including in a citation title the language scan skips.
	run := agencyRunFixture()
	a := validArtifact(run)
	a.Sources[0].Title = "notes from /Users/someone/research.md"
	if err := validateAgencyArtifact(a, run); err == nil {
		t.Fatal("a home path in a citation title was accepted")
	}
}

// ────────────────────────────────────────────────────── distinct claims, not repeated appearances

func TestOneClaimRepeatedAcrossStagesIsStillOneClaim(t *testing.T) {
	// The floor says "this rests on more than one thing". One sourced sentence echoed across four
	// stages is still one thing.
	run := agencyRunFixture()
	const claim = "The issuer filed a 10-Q covering the period."
	sourced := AgencyFinding{Statement: claim, Provenance: provenanceSourced, SourceIDs: []string{"s1"}}

	a := validArtifact(run)
	a.ResearchPriority = priorityInvestigate
	for i := range a.Stages {
		a.Stages[i].Findings = []AgencyFinding{sourced}
	}
	a.Thesis.Support = []AgencyFinding{sourced}
	a.AntiThesis.Support = []AgencyFinding{sourced}
	a.RiskFindings = []AgencyFinding{sourced}

	if grounded, total := agencyGroundedCoverage(a); grounded != 1 || total != 1 {
		t.Fatalf("one claim repeated seven times counted as %d grounded of %d", grounded, total)
	}
	err := validateAgencyArtifact(a, run)
	if err == nil {
		t.Fatal("a single claim repeated across every stage satisfied the two-claim floor")
	}
	if !strings.Contains(err.Error(), "DISTINCT") {
		t.Fatalf("the refusal does not explain that appearances are not evidence: %v", err)
	}

	// Two genuinely different sourced claims clear the floor.
	a.Stages[1].Findings = []AgencyFinding{{
		Statement:  "Segment revenue was disclosed separately.",
		Provenance: provenanceSourced, SourceIDs: []string{"s1"},
	}}
	if err := validateAgencyArtifact(a, run); err != nil {
		t.Fatalf("two distinct grounded claims were refused: %v", err)
	}
}

func TestARestatementDoesNotUngroundASourcedClaim(t *testing.T) {
	a := &AgencyArtifact{
		Stages: []AgencyStage{
			{Findings: []AgencyFinding{{Statement: "Margin fell.", Provenance: provenanceSourced}}},
			{Findings: []AgencyFinding{{Statement: "margin fell", Provenance: provenanceInferred}}},
		},
	}
	if grounded, total := agencyGroundedCoverage(a); grounded != 1 || total != 1 {
		t.Fatalf("a sourced claim restated as inferred counted %d grounded of %d, want 1 of 1",
			grounded, total)
	}
}

// ───────────────────────────────────────────────────── the preflight changes nothing at all

func TestQueuedCountPersistsNothing(t *testing.T) {
	// `GET /_internal/agency/status` exists so an operator can prove their configuration works. Its
	// whole value is that running it changes nothing — an earlier version reconciled and PERSISTED,
	// so a `-check` could quietly re-queue a lapsed run and rewrite the owner's stored document.
	srv, mux := agencyFixture(t)
	createRun(t, mux, agencyOwner, "NVDA", "why")
	claimOne(t, mux) // the run is now claimed, holding a lease

	// Well past the lease: reconciliation WOULD re-queue this run.
	later := time.Now().UTC().Add(2 * agencyMaxLeaseDuration)

	before, _ := os.ReadFile(srv.agency.path(agencyOwner))

	count, err := srv.agency.QueuedCount(later)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("QueuedCount = %d, want 1 — a lapsed lease is claimable again", count)
	}

	after, _ := os.ReadFile(srv.agency.path(agencyOwner))
	if !bytes.Equal(before, after) {
		t.Fatalf("QueuedCount rewrote the stored document:\nbefore: %s\nafter:  %s", before, after)
	}

	// And the in-memory row is untouched too: still `claimed`, still holding its lease token.
	runs, err := srv.agency.List(agencyOwner, 10, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if runs[0].Status != agencyClaimed || runs[0].LeaseToken == "" {
		t.Fatalf("QueuedCount mutated the run in memory: status=%q leaseHeld=%v",
			runs[0].Status, runs[0].LeaseToken != "")
	}
}

func TestQueuedCountDoesNotCountARunThatIsAboutToExpire(t *testing.T) {
	// A queued run older than the cutoff bound will be expired by the next write. Counting it would
	// tell an operator work is waiting when the next claim will discard it.
	srv, mux := agencyFixture(t)
	createRun(t, mux, agencyOwner, "NVDA", "why")

	if n, _ := srv.agency.QueuedCount(time.Now().UTC()); n != 1 {
		t.Fatalf("a fresh queued run counted %d, want 1", n)
	}
	stale := time.Now().UTC().Add(agencyMaxRunAge + time.Hour)
	if n, _ := srv.agency.QueuedCount(stale); n != 0 {
		t.Fatalf("a run past its cutoff counted %d, want 0", n)
	}
}

func TestAnUnreadableOwnerStoreIsReportedAsDegradedNotAsAnEmptyQueue(t *testing.T) {
	// `ok:true, queuedRuns:0` and "the queue cannot be read" look identical to a worker and mean
	// opposite things. A single-owner deployment whose document is corrupt used to answer the
	// former, so `-check` told the operator everything was fine while their runs were invisible.
	srv, mux := agencyFixture(t)

	// Corrupt the ONLY configured owner's document, exactly as a truncated write would.
	dir := filepath.Dir(srv.agency.path(agencyOwner))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(srv.agency.path(agencyOwner), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Drop the read cache so the store actually re-reads the corrupt file.
	srv.agency.cache = map[string]*agencyBucket{}

	if _, err := srv.agency.QueuedCount(time.Now().UTC()); err == nil {
		t.Fatal("QueuedCount reported a count over an unreadable owner store")
	}

	req := httptest.NewRequest(http.MethodGet, "/_internal/agency/status", nil)
	req.Header.Set("X-Worker-Token", agencyToken)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("an unreadable queue answered 200: %s", rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["ok"] != false {
		t.Fatalf("the degraded response does not say ok:false: %v", out)
	}
	if !strings.Contains(rec.Body.String(), "could not be read") {
		t.Fatalf("the degraded response does not explain itself: %s", rec.Body.String())
	}
	// And it still discloses nothing about what was in the queue.
	for _, forbidden := range []string{agencyOwner, "NVDA", "agr_"} {
		if strings.Contains(rec.Body.String(), forbidden) {
			t.Fatalf("the degraded response leaked %q", forbidden)
		}
	}
}

func TestALaneWithNoConfiguredOwnerIsNotAnEmptyQueue(t *testing.T) {
	// "Switched off" and "nothing waiting" are different answers too.
	store, err := openAgencyStore(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.QueuedCount(time.Now().UTC()); err == nil {
		t.Fatal("a lane with no configured owner reported a queue count")
	}
}

func TestAConfiguredTokenIsTrimmedSoAGeneratedNewlineCannotBreakAuthentication(t *testing.T) {
	// THE BUG THIS COVERS. `openssl rand -hex 32` appends a newline, and several deployment
	// platforms set a secret from a file verbatim. The bridge trims its own copy, so an untrimmed
	// server value differed by one byte and the constant-time compare failed on LENGTH — a flat 401
	// on every claim, with two values that look identical wherever you print them.
	base := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for _, stored := range []string{base, base + "\n", base + "\r\n", " " + base + "  ", base + "\t"} {
		t.Run("stored with surrounding whitespace", func(t *testing.T) {
			t.Setenv("AGENCY_WORKER_TOKEN", stored)
			t.Setenv("AGENCY_OWNER_UIDS", agencyOwner)
			cfg := loadConfig()
			if cfg.AgencyWorkerToken != base {
				t.Fatalf("configured token = %q, want it trimmed to %q", cfg.AgencyWorkerToken, base)
			}

			cfg.TradesDir = t.TempDir()
			cfg.Secret = testSecret
			cfg.CookieName = "nvda_session"
			store, err := openAgencyStore(cfg.TradesDir, cfg.AgencyOwnerUIDs)
			if err != nil {
				t.Fatal(err)
			}
			srv := &Server{cfg: cfg, agency: store, http: &http.Client{}}
			mux := http.NewServeMux()
			srv.registerSubscriptionRoutes(mux)

			// The worker sends the TRIMMED value, which is what the bridge's readToken produces.
			code, _ := workerCall(t, mux, "/_internal/agency/claim",
				`{"workerId":"w","workflows":["company_research_v1"]}`, base)
			if code == http.StatusUnauthorized {
				t.Fatalf("a token stored as %q rejected the trimmed value the bridge sends", stored)
			}
		})
	}
}

func TestAnEmptyOrWhitespaceOnlyTokenStillDisablesTheWorkerRoutes(t *testing.T) {
	// Trimming must not turn "   " into a usable credential, nor into one that matches an empty
	// header. Whitespace-only is unconfigured.
	t.Setenv("AGENCY_WORKER_TOKEN", "   \n\t ")
	cfg := loadConfig()
	if cfg.AgencyWorkerToken != "" {
		t.Fatalf("a whitespace-only token became %q", cfg.AgencyWorkerToken)
	}

	cfg.TradesDir = t.TempDir()
	cfg.Secret = testSecret
	cfg.CookieName = "nvda_session"
	cfg.AgencyOwnerUIDs = []string{agencyOwner}
	store, err := openAgencyStore(cfg.TradesDir, cfg.AgencyOwnerUIDs)
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{cfg: cfg, agency: store, http: &http.Client{}}
	mux := http.NewServeMux()
	srv.registerSubscriptionRoutes(mux)

	code, out := workerCall(t, mux, "/_internal/agency/claim",
		`{"workerId":"w","workflows":["company_research_v1"]}`, "anything")
	if code != http.StatusForbidden {
		t.Fatalf("a whitespace-only token = %d, want 403 (the lane is unconfigured)", code)
	}
	if out["missingConfiguration"] != "AGENCY_WORKER_TOKEN" {
		t.Fatalf("the refusal must name the variable, got %v", out)
	}
}
