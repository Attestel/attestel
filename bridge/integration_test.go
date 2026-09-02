package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// integration_test.go — ONE COMPLETE RUN, from the owner pressing a button to the artifact
// rendering, against the REAL hosted service.
//
// WHAT IS REAL HERE AND WHAT IS NOT.
//
//	REAL: the journal binary, compiled from source and run as a process; its HTTP routes; its
//	      durable store; the session cookie; the owner allowlist; the worker credential; the lease;
//	      the artifact validator; this bridge's claim, heartbeat, assembly, validation and upload;
//	      every schema on both sides of the wire.
//	FAKE: the four Hermes invocations, replaced by `stubRunner` (stub.go).
//
// Only the model is stubbed, and that is the point: this test can run on a laptop with no
// PostgreSQL, no Docker, no network, no provider credential and no Hermes installation, which is
// what makes it a test somebody will actually run. The journal is started with no DATABASE_URL so
// it uses its own documented file backend — the same fallback every other journal store has.
//
// The stubbed stages still exercise the whole validator: they write a query file, read it back,
// declare a real source URL, use every provenance label, raise a veto with a reason, and produce a
// chair conclusion. An artifact that survives this path is an artifact the server accepts.

const (
	e2eOwner  = "e2e-owner-uid"
	e2eSecret = "e2e-auth-secret-not-a-real-one"
	e2eToken  = "e2e-worker-token-0123456789abcdef0123456789abcdef"
	e2eCookie = "nvda_session"
)

// e2eSession mints a session cookie for the journal.
//
// The signer is copied from journal/auth.go, which is itself a byte-identical copy of
// auth/token.go — the pattern this repository uses for tiny verifiers shared across independent
// stdlib-only modules ("If you change it, change every copy"). A test that could not mint a session
// could not exercise the owner routes at all.
func e2eSession(uid string) *http.Cookie {
	b64 := base64.RawURLEncoding
	raw, _ := json.Marshal(map[string]any{
		"uid": uid,
		"iat": time.Now().Unix(),
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	body := b64.EncodeToString(raw)
	mac := hmac.New(sha256.New, []byte(e2eSecret))
	mac.Write([]byte(body))
	return &http.Cookie{Name: e2eCookie, Value: body + "." + b64.EncodeToString(mac.Sum(nil))}
}

// freePort reserves a port by binding and releasing it.
func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("cannot reserve a port: %v", err)
	}
	defer l.Close()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	return port
}

// startJournal compiles and runs the real hosted service, returning its base URL.
func startJournal(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("the Go toolchain is not on PATH; the end-to-end test needs it to build the journal")
	}

	bin := filepath.Join(t.TempDir(), "journal-e2e")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = "../journal"
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("cannot build the journal service: %v\n%s", err, out)
	}

	port := freePort(t)
	dataDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	cmd := exec.CommandContext(ctx, bin)
	// A DELIBERATELY MINIMAL ENVIRONMENT. `Env` is set rather than inherited so this test cannot
	// accidentally pick up a real DATABASE_URL, a real AUTH_SECRET or a real worker token from the
	// developer's shell and write test rows into something that matters.
	cmd.Env = []string{
		"PORT=" + port,
		"TRADES_DIR=" + dataDir,
		"AUTH_SECRET=" + e2eSecret,
		"COOKIE_NAME=" + e2eCookie,
		"AGENCY_OWNER_UIDS=" + e2eOwner,
		"AGENCY_WORKER_TOKEN=" + e2eToken,
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
	}
	var logs bytes.Buffer
	cmd.Stdout = &logs
	cmd.Stderr = &logs
	if err := cmd.Start(); err != nil {
		t.Fatalf("cannot start the journal service: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("journal service log:\n%s", logs.String())
		}
	})

	base := "http://127.0.0.1:" + port
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return base
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the journal service did not become healthy:\n%s", logs.String())
	return ""
}

// ownerRequest issues a request as the signed-in owner and decodes the response.
func ownerRequest(t *testing.T, method, url, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(e2eSession(e2eOwner))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

// e2eBridgeConfig builds the bridge configuration pointed at the running journal.
func e2eBridgeConfig(t *testing.T, base string) Config {
	t.Helper()
	t.Setenv("ATTESTEL_URL", base)
	t.Setenv("ATTESTEL_ALLOW_INSECURE_URL", "1") // loopback only; validateBaseURL enforces that
	t.Setenv("ATTESTEL_WORKER_TOKEN", e2eToken)
	t.Setenv("ATTESTEL_BRIDGE_STATE_DIR", t.TempDir())
	t.Setenv("ATTESTEL_BRIDGE_PROMPT_DIR", "prompts")
	t.Setenv("ATTESTEL_BRIDGE_DRY_RUN", "1")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("cannot configure the bridge: %v", err)
	}
	return cfg
}

// ─────────────────────────────────────────────────────────────────────────── the end-to-end test

func TestOneCompleteCompanyResearchRunFromQueueToArtifact(t *testing.T) {
	base := startJournal(t)
	cfg := e2eBridgeConfig(t, base)
	client := newAPIClient(cfg)

	// ── 1. The owner asks a question. ────────────────────────────────────────────────────────
	question := "Why did reported gross margin move between the last two quarters?"
	code, created := ownerRequest(t, http.MethodPost, base+"/agency/runs",
		fmt.Sprintf(`{"ticker":"NVDA","question":%q}`, question))
	if code != http.StatusAccepted {
		t.Fatalf("create = %d, want 202: %v", code, created)
	}
	run := created["run"].(map[string]any)
	runID := run["id"].(string)
	if run["status"] != "queued" {
		t.Fatalf("a new run is %v, want queued", run["status"])
	}
	// The response is immediate and carries what a client needs to poll.
	if run["pollAfterMs"] == nil || created["href"] == nil {
		t.Fatalf("the 202 does not tell the client how to follow the run: %v", created)
	}

	// ── 2. The local bridge claims it and works it. ───────────────────────────────────────────
	worked, err := runOnce(context.Background(), cfg, client, stubRunner{})
	if err != nil {
		t.Fatalf("the bridge could not complete the run: %v", err)
	}
	if !worked {
		t.Fatal("the bridge claimed nothing although a run was queued")
	}

	// ── 3. The owner reads the finished artifact. ─────────────────────────────────────────────
	code, out := ownerRequest(t, http.MethodGet, base+"/agency/runs/"+runID, "")
	if code != http.StatusOK {
		t.Fatalf("read = %d: %v", code, out)
	}
	if out["status"] != "completed" {
		t.Fatalf("status = %v (error: %v), want completed", out["status"], out["error"])
	}

	artifact, ok := out["artifact"].(map[string]any)
	if !ok {
		t.Fatalf("the completed run carries no artifact: %v", out)
	}

	// Identity and provenance of the run itself.
	if artifact["schemaVersion"] != artifactSchemaVersion {
		t.Fatalf("artifact schemaVersion = %v", artifact["schemaVersion"])
	}
	if artifact["runId"] != runID || artifact["ticker"] != "NVDA" {
		t.Fatalf("the artifact does not belong to this run: %v", artifact)
	}
	if artifact["question"] != question {
		t.Fatalf("the artifact lost the question: %v", artifact["question"])
	}
	if artifact["asOf"] != run["asOf"] {
		t.Fatalf("the artifact's cutoff (%v) is not the run's server-assigned one (%v)",
			artifact["asOf"], run["asOf"])
	}

	// All four profiles ran, in order.
	stages := artifact["stages"].([]any)
	if len(stages) != len(companyResearchChain) {
		t.Fatalf("the artifact has %d stages, want %d", len(stages), len(companyResearchChain))
	}
	for i, s := range stages {
		stage := s.(map[string]any)
		if stage["profile"] != companyResearchChain[i].Profile {
			t.Fatalf("stage %d is %v, want %s", i, stage["profile"], companyResearchChain[i].Profile)
		}
	}

	// Citations survived, with a real URL and an explicit date.
	sources := artifact["sources"].([]any)
	if len(sources) == 0 {
		t.Fatal("the artifact cites nothing")
	}
	src := sources[0].(map[string]any)
	if !strings.HasPrefix(src["url"].(string), "https://") || src["publishedAt"] == "" {
		t.Fatalf("a source is not usable as a citation: %v", src)
	}

	// The research fields the owner reads.
	for _, field := range []string{"thesis", "antiThesis", "chairConclusion", "veto", "identity"} {
		if artifact[field] == nil {
			t.Fatalf("the artifact has no %s", field)
		}
	}
	if artifact["researchPriority"] == nil {
		t.Fatal("the artifact carries no research priority")
	}

	// A dry run is LABELLED. This is the property that stops a stubbed artifact from ever being
	// mistaken for real research.
	degraded, _ := artifact["degraded"].([]any)
	labelled := false
	for _, d := range degraded {
		if strings.Contains(fmt.Sprint(d), "dry-run") {
			labelled = true
		}
	}
	if !labelled {
		t.Fatalf("a stubbed run was not labelled degraded: %v", degraded)
	}

	// ── 4. The safety properties, on the artifact that actually crossed the wire. ─────────────
	raw, _ := json.Marshal(artifact)
	body := string(raw)
	for _, forbidden := range []string{
		`"direction"`, `"signal"`, `"priceTarget"`, `"expectedReturn"`, `"confidence"`,
		`"positionSize"`, `"modelUsed"`, `"provider"`, `"estimatedCost"`, `"sessionId"`,
		`"hostname"`, "/Users/", "/home/", ".hermes",
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("the stored artifact contains %s", forbidden)
		}
	}

	// And the run still reports NO_SIGNAL — research completing is not evidence of anything.
	act := out["actionability"].(map[string]any)
	if act["evidenceState"] != "NO_SIGNAL" || act["action"] != "NO_ACTION" {
		t.Fatalf("a completed research run reports %v/%v, want NO_SIGNAL/NO_ACTION",
			act["evidenceState"], act["action"])
	}
	if act["vetoRaised"] != true {
		t.Fatal("the stub raised a veto and the view did not report it")
	}

	// ── 5. A second bridge pass finds nothing. The queue is drained, not looping. ─────────────
	worked, err = runOnce(context.Background(), cfg, client, stubRunner{})
	if err != nil {
		t.Fatalf("the second pass errored: %v", err)
	}
	if worked {
		t.Fatal("the bridge claimed a second run; a completed run must not be re-claimable")
	}
}

func TestTheBridgeReportsAFailureRatherThanLeavingARunHanging(t *testing.T) {
	// The other half of the loop: when a stage cannot produce a valid result, the owner must see a
	// failed run with a reason — not a run that silently stops moving until its lease expires.
	base := startJournal(t)
	cfg := e2eBridgeConfig(t, base)
	client := newAPIClient(cfg)

	code, created := ownerRequest(t, http.MethodPost, base+"/agency/runs",
		`{"ticker":"NVDA","question":"a question whose stages will fail"}`)
	if code != http.StatusAccepted {
		t.Fatalf("create = %d", code)
	}
	runID := created["run"].(map[string]any)["id"].(string)

	if _, err := runOnce(context.Background(), cfg, client, brokenRunner{}); err == nil {
		t.Fatal("a broken stage produced no error")
	}

	code, out := ownerRequest(t, http.MethodGet, base+"/agency/runs/"+runID, "")
	if code != http.StatusOK {
		t.Fatalf("read = %d", code)
	}
	// A permanent (schema) failure is not retryable, so the run is terminal on the first attempt.
	if out["status"] != "failed" {
		t.Fatalf("status = %v, want failed", out["status"])
	}
	reason, _ := out["error"].(string)
	if reason == "" {
		t.Fatal("the failed run carries no reason")
	}
	if !strings.Contains(reason, "schema") {
		t.Fatalf("the reason does not explain the failure: %q", reason)
	}
	if out["artifact"] != nil {
		t.Fatal("a failed run stored an artifact")
	}
}

func TestACancelledRunCannotBeCompletedByTheWorkerThatHeldIt(t *testing.T) {
	// Cancellation is the owner's stop button, and it has to actually stop things: a worker that
	// was mid-chain when the owner cancelled must not be able to land its result afterwards.
	base := startJournal(t)
	cfg := e2eBridgeConfig(t, base)
	client := newAPIClient(cfg)

	code, created := ownerRequest(t, http.MethodPost, base+"/agency/runs",
		`{"ticker":"NVDA","question":"a question the owner changes their mind about"}`)
	if code != http.StatusAccepted {
		t.Fatalf("create = %d", code)
	}
	runID := created["run"].(map[string]any)["id"].(string)

	// The bridge claims it.
	job, ok, err := client.Claim(context.Background(), cfg)
	if err != nil || !ok {
		t.Fatalf("claim failed (ok=%v err=%v)", ok, err)
	}

	// The owner cancels while the worker holds the lease.
	if code, out := ownerRequest(t, http.MethodPost, base+"/agency/runs/"+runID+"/cancel", ""); code != http.StatusOK {
		t.Fatalf("cancel = %d: %v", code, out)
	}

	// Every remaining worker call on that lease is refused with 409.
	if err := client.Heartbeat(context.Background(), job, "stock-scout", cfg); !isStaleLease(err) {
		t.Fatalf("heartbeat after cancellation = %v, want a 409 stale-lease refusal", err)
	}
	if err := client.Complete(context.Background(), job, &Artifact{}); !isStaleLease(err) {
		t.Fatalf("complete after cancellation = %v, want a 409 stale-lease refusal", err)
	}

	code, out := ownerRequest(t, http.MethodGet, base+"/agency/runs/"+runID, "")
	if code != http.StatusOK || out["status"] != "cancelled" {
		t.Fatalf("status after cancel = %v (code %d), want cancelled", out["status"], code)
	}
}

func TestAWrongWorkerCredentialClaimsNothing(t *testing.T) {
	// The credential boundary, exercised over a real socket rather than a handler call.
	base := startJournal(t)
	cfg := e2eBridgeConfig(t, base)

	code, _ := ownerRequest(t, http.MethodPost, base+"/agency/runs",
		`{"ticker":"NVDA","question":"a queued question"}`)
	if code != http.StatusAccepted {
		t.Fatalf("create = %d", code)
	}

	wrong := cfg
	wrong.Token = strings.Repeat("z", len(e2eToken))
	if _, ok, err := newAPIClient(wrong).Claim(context.Background(), wrong); ok || err == nil {
		t.Fatalf("a wrong worker credential claimed a job (ok=%v err=%v)", ok, err)
	}
}

// brokenRunner returns output that cannot satisfy any stage schema. It stands in for a model that
// ignored its instructions, a truncated response, or a stage that was talked into free prose.
type brokenRunner struct{}

func (brokenRunner) Run(_ context.Context, _ stageSpec, _, _ string, _ Config) (string, error) {
	return `{"sources":[],"findings":[],"notes":[],"unresolvedQuestions":[],"direction":"Buy"}`, nil
}

// ─────────────────────────────────────────────────────────────────── the PRODUCTION URL shape

// startJournalBehindProductionProxy puts a reverse proxy in front of the journal that behaves the
// way deploy/nginx.conf.template does:
//
//	location /svc/journal/ { proxy_pass http://127.0.0.1:8096/; }
//
// The trailing slash on `proxy_pass` strips the `/svc/journal/` prefix, so a request for
// `/svc/journal/_internal/agency/claim` reaches the journal as `/_internal/agency/claim`.
//
// WHY THIS TEST EXISTS. Every other test here points the bridge straight at the journal's own port,
// which is the DEVELOPMENT shape. In a real deployment the journal is not exposed on its own port
// at all — it is behind that prefix — so a bridge configured with a bare `https://<host>` would
// have posted every claim to `https://<host>/_internal/agency/claim`, which the gateway serves and
// which does not exist. That is a 404 on the first real run and nothing else. This test exercises
// the shape the documentation now tells operators to configure.
func startJournalBehindProductionProxy(t *testing.T) (proxyBase string, journalBase string) {
	t.Helper()
	journalBase = startJournal(t)
	target, err := url.Parse(journalBase)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "/svc/journal/"
		if !strings.HasPrefix(r.URL.Path, prefix) {
			// Exactly like nginx: anything outside a configured location is not this service.
			http.NotFound(w, r)
			return
		}
		out := *r.URL
		out.Scheme, out.Host = target.Scheme, target.Host
		out.Path = "/" + strings.TrimPrefix(r.URL.Path, prefix)

		req, err := http.NewRequest(r.Method, out.String(), r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		req.Header = r.Header.Clone()
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(proxy.Close)
	return proxy.URL, journalBase
}

func TestACompleteRunThroughTheProductionProxyPath(t *testing.T) {
	proxyBase, journalBase := startJournalBehindProductionProxy(t)

	// The bridge is configured the way the documentation says: the base URL INCLUDES the prefix the
	// deployment serves the journal under.
	cfg := e2eBridgeConfig(t, proxyBase+"/svc/journal")
	client := newAPIClient(cfg)

	// The owner still creates the run through the journal's own routes (in production, through the
	// gateway); only the worker traffic goes over the proxied path.
	code, created := ownerRequest(t, http.MethodPost, journalBase+"/agency/runs",
		`{"ticker":"NVDA","question":"does the production path actually route?"}`)
	if code != http.StatusAccepted {
		t.Fatalf("create = %d: %v", code, created)
	}
	runID := created["run"].(map[string]any)["id"].(string)

	// -check must pass over the same path before anything is claimed.
	status, err := client.Status(context.Background())
	if err != nil {
		t.Fatalf("the preflight failed over the production path: %v", err)
	}
	if status.QueuedRuns == nil || *status.QueuedRuns != 1 {
		t.Fatalf("the preflight reported %v queued runs, want 1", status.QueuedRuns)
	}

	worked, err := runOnce(context.Background(), cfg, client, stubRunner{})
	if err != nil || !worked {
		t.Fatalf("the run did not complete over the production path (worked=%v err=%v)", worked, err)
	}

	code, out := ownerRequest(t, http.MethodGet, journalBase+"/agency/runs/"+runID, "")
	if code != http.StatusOK || out["status"] != "completed" {
		t.Fatalf("status = %v (code %d, error %v), want completed", out["status"], code, out["error"])
	}
	if out["artifact"] == nil {
		t.Fatal("no artifact came back over the production path")
	}
}

func TestABareHostWithoutThePrefixFailsThePreflight(t *testing.T) {
	// The misconfiguration this whole item is about, caught by -check instead of by a 404 on the
	// first real run.
	proxyBase, _ := startJournalBehindProductionProxy(t)
	cfg := e2eBridgeConfig(t, proxyBase) // no /svc/journal
	if _, err := newAPIClient(cfg).Status(context.Background()); err == nil {
		t.Fatal("the preflight passed against a base URL missing the reverse-proxy prefix")
	}
}

// ───────────────────────────────────────────────────────────── the lease-versus-stage invariant

// sleepyRunner takes almost the whole stage budget, the way a real four-stage research chain does.
type sleepyRunner struct{ delay time.Duration }

func (s sleepyRunner) Run(ctx context.Context, spec stageSpec, workdir, queryPath string, cfg Config) (string, error) {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return "", errf("stage %s exceeded its budget", spec.Profile)
	}
	return stubRunner{}.Run(ctx, spec, workdir, queryPath, cfg)
}

func TestAStageThatOutlastsItsLeaseLosesTheRun(t *testing.T) {
	// THE BUG THIS IS A REGRESSION TEST FOR. The defaults used to be a 600-second lease and a
	// 600-second stage budget. The bridge heartbeats BETWEEN stages, so a stage that ran to its
	// budget expired the lease at the exact moment it finished: the run was taken over, the
	// completion was refused with a 409, and everything the stage produced was discarded.
	//
	// This reproduces it directly — a one-second lease against a stage that takes longer — so the
	// failure mode is demonstrated rather than asserted about. The guard is the next test.
	base := startJournal(t)
	cfg := e2eBridgeConfig(t, base)
	cfg.LeaseSeconds = 1 // deliberately shorter than the stage; validateBudgets forbids this
	client := newAPIClient(cfg)

	code, _ := ownerRequest(t, http.MethodPost, base+"/agency/runs",
		`{"ticker":"NVDA","question":"a question whose stage outlasts its lease"}`)
	if code != http.StatusAccepted {
		t.Fatalf("create = %d", code)
	}

	_, err := runOnce(context.Background(), cfg, client, sleepyRunner{delay: 1500 * time.Millisecond})
	if err == nil {
		t.Fatal("a stage that outlasted its lease completed anyway; the reproduction is no longer valid")
	}
	if !isStaleLease(err) {
		t.Fatalf("expected a stale-lease refusal, got %v", err)
	}
}

func TestTheBudgetInvariantRefusesTheConfigurationThatCausesThat(t *testing.T) {
	// The guard. Every configuration that could reproduce the test above is refused at startup,
	// where it is one message an operator reads once rather than an intermittent, load-dependent
	// loss of a run's work.

	cases := []struct {
		name  string
		lease int
		stage int
		run   int
	}{
		{"the old default pairing, lease == stage", 600, 600, 2400},
		{"a lease shorter than the stage", 300, 600, 2400},
		{"a lease inside the safety margin", 600 + leaseSafetyMarginSeconds - 1, 600, 2400},
		{"a lease past the server's cap", serverMaxLeaseSeconds + 1, 600, 4000},
		{"a run budget too small for four stages", 900, 600, 1200},
		// ITEM: the run budget needs a margin of its own. `4 × 600 = 2400` exactly — which was the
		// shipped default — leaves nothing for the four heartbeat round trips, the four decodes and
		// validations, the assembly, the content scans and the upload. A run whose stages each used
		// their budget would finish the artifact with no time left to send it.
		{"a run budget of exactly four stage budgets", 900, 600, 2400},
		{"a run budget inside the margin", 900, 600, 2400 + runBudgetMarginSeconds - 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{
				LeaseSeconds: tc.lease, StageBudgetSeconds: tc.stage, RunBudgetSeconds: tc.run,
			}
			if err := validateBudgets(cfg); err == nil {
				t.Fatalf("%s was accepted (lease=%d stage=%d run=%d)",
					tc.name, tc.lease, tc.stage, tc.run)
			}
		})
	}

	// Both boundaries are exactly the margin, not approximately it.
	atLeaseBoundary := Config{
		LeaseSeconds:       600 + leaseSafetyMarginSeconds,
		StageBudgetSeconds: 600,
		RunBudgetSeconds:   2400 + runBudgetMarginSeconds,
	}
	if err := validateBudgets(atLeaseBoundary); err != nil {
		t.Fatalf("a lease of exactly stage+margin was refused: %v", err)
	}
	atRunBoundary := Config{
		LeaseSeconds:       900,
		StageBudgetSeconds: 600,
		RunBudgetSeconds:   4*600 + runBudgetMarginSeconds,
	}
	if err := validateBudgets(atRunBoundary); err != nil {
		t.Fatalf("a run budget of exactly stages+margin was refused: %v", err)
	}

	// And the SHIPPED DEFAULTS satisfy both, which is the thing that actually has to be true.
	defaults := Config{
		LeaseSeconds:       defaultLeaseSeconds,
		StageBudgetSeconds: defaultStageBudgetSeconds,
		RunBudgetSeconds:   defaultRunBudgetSeconds,
	}
	if err := validateBudgets(defaults); err != nil {
		t.Fatalf("the shipped defaults violate the invariant: %v", err)
	}
	if defaultRunBudgetSeconds <= defaultStageBudgetSeconds*len(companyResearchChain) {
		t.Fatalf("the default run budget (%ds) is not greater than %d stage budgets (%ds)",
			defaultRunBudgetSeconds, len(companyResearchChain),
			defaultStageBudgetSeconds*len(companyResearchChain))
	}
}

func TestANearBudgetStageStillCompletesUnderAValidLease(t *testing.T) {
	// The positive case: with the invariant satisfied, a stage that runs close to its budget still
	// holds its lease when it finishes, and the run completes normally.
	base := startJournal(t)
	cfg := e2eBridgeConfig(t, base)
	cfg.StageBudgetSeconds = 2
	cfg.LeaseSeconds = 2 + leaseSafetyMarginSeconds
	// The run budget satisfies the same invariant production does — the margin is an absolute floor,
	// not a proportion, so a test with tiny stage budgets still has to clear it. Bending the
	// invariant for a test would be testing a configuration that cannot ship.
	cfg.RunBudgetSeconds = 4*cfg.StageBudgetSeconds + runBudgetMarginSeconds
	if err := validateBudgets(cfg); err != nil {
		t.Fatalf("the test's own configuration violates the invariant: %v", err)
	}
	client := newAPIClient(cfg)

	code, created := ownerRequest(t, http.MethodPost, base+"/agency/runs",
		`{"ticker":"NVDA","question":"a question whose stages run close to their budget"}`)
	if code != http.StatusAccepted {
		t.Fatalf("create = %d", code)
	}
	runID := created["run"].(map[string]any)["id"].(string)

	// Each of the four stages takes most of its two-second budget.
	worked, err := runOnce(context.Background(), cfg, client, sleepyRunner{delay: 1500 * time.Millisecond})
	if err != nil || !worked {
		t.Fatalf("a near-budget run failed under a valid lease (worked=%v err=%v)", worked, err)
	}
	code, out := ownerRequest(t, http.MethodGet, base+"/agency/runs/"+runID, "")
	if code != http.StatusOK || out["status"] != "completed" {
		t.Fatalf("status = %v (error %v), want completed", out["status"], out["error"])
	}
}
