package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// main_test.go — the EXIT-STATUS CONTRACT.
//
// Something will be scheduling this program, so the exit code is how a failure gets noticed at all.
// An earlier version logged every error and returned nil, which would have made a cron wrapper
// report success forever while nothing worked. These tests pin the contract from the file header:
//
//	0  the queue was empty, or every claimed job reached a reported conclusion
//	1  everything else, except a lost lease — which is the protocol working, not this bridge failing

// fakeClient is a scripted hostedClient. Each field defaults to "succeed", so a test overrides only
// the call whose failure it is about.
type fakeClient struct {
	status     workerStatus
	statusErr  error
	claims     []claimStep
	claimIdx   int
	heartbeat  error
	complete   error
	fail       error
	failCalls  int
	claimCalls int
}

// claimStep is one scripted answer to Claim: a job, an empty queue, or an error.
type claimStep struct {
	job *Job
	err error
}

func (f *fakeClient) Status(context.Context) (workerStatus, error) {
	return f.status, f.statusErr
}

func (f *fakeClient) Claim(context.Context, Config) (*Job, bool, error) {
	f.claimCalls++
	if f.claimIdx >= len(f.claims) {
		return nil, false, nil // queue empty
	}
	step := f.claims[f.claimIdx]
	f.claimIdx++
	if step.err != nil {
		return nil, false, step.err
	}
	if step.job == nil {
		return nil, false, nil
	}
	return step.job, true, nil
}

func (f *fakeClient) Heartbeat(context.Context, *Job, string, Config) error { return f.heartbeat }
func (f *fakeClient) Complete(context.Context, *Job, *Artifact) error       { return f.complete }
func (f *fakeClient) Fail(context.Context, *Job, string, bool) error {
	f.failCalls++
	return f.fail
}

func fakeJob() *Job {
	return &Job{
		SchemaVersion: jobSchemaVersion, RunID: "agr_fake", UserID: "owner",
		WorkflowVersion: workflowCompanyResearch, Ticker: "NVDA",
		Question: "why did gross margin move", AsOf: "2026-09-02T00:00:00Z",
		LeaseToken: "lease-token",
	}
}

// workableConfig is a config a full four-stage stub run can actually execute against.
func workableConfig(t *testing.T) Config {
	t.Helper()
	return Config{
		BaseURL:            "https://example.invalid",
		Token:              "t",
		WorkerID:           "wkr_test",
		StateDir:           t.TempDir(),
		PromptDir:          "prompts",
		LeaseSeconds:       900,
		StageBudgetSeconds: 600,
		RunBudgetSeconds:   2400,
		MaxTurns:           defaultMaxTurns,
		DryRunHermes:       true,
	}
}

func depsFor(cfg Config, client hostedClient, runner hermesRunner) deps {
	return deps{
		loadConfig: func() (Config, error) { return cfg, nil },
		newClient:  func(Config) hostedClient { return client },
		newRunner:  func(Config) hermesRunner { return runner },
	}
}

// ───────────────────────────────────────────────────────────────────── the only successful zero

func TestAnEmptyQueueSucceeds(t *testing.T) {
	cfg := workableConfig(t)
	client := &fakeClient{} // no scripted claims -> queue empty
	if err := run(context.Background(), options{}, depsFor(cfg, client, stubRunner{})); err != nil {
		t.Fatalf("an empty queue returned %v; it is the one condition that must exit 0", err)
	}
	if client.claimCalls != 1 {
		t.Fatalf("claim was called %d times for a single pass, want 1", client.claimCalls)
	}
}

func TestACompletedRunSucceeds(t *testing.T) {
	cfg := workableConfig(t)
	client := &fakeClient{claims: []claimStep{{job: fakeJob()}}}
	if err := run(context.Background(), options{}, depsFor(cfg, client, stubRunner{})); err != nil {
		t.Fatalf("a run that completed returned %v", err)
	}
}

// ─────────────────────────────────────────────────────────────── every failure must be non-zero

func TestConfigurationFailureIsAnError(t *testing.T) {
	d := deps{
		loadConfig: func() (Config, error) { return Config{}, errf("ATTESTEL_URL is not set") },
		newClient:  func(Config) hostedClient { return &fakeClient{} },
		newRunner:  func(Config) hermesRunner { return stubRunner{} },
	}
	if err := run(context.Background(), options{}, d); err == nil {
		t.Fatal("a configuration failure exited 0")
	}
}

func TestEveryFailureModeReturnsAnError(t *testing.T) {
	// One case per failure class named in the brief: authentication, networking, claim,
	// Hermes-stage, validation, and failure-reporting. Each must be non-zero.
	cases := []struct {
		name   string
		client *fakeClient
		runner hermesRunner
		want   string
	}{
		{
			name: "a rejected credential",
			client: &fakeClient{claims: []claimStep{{
				err: &apiError{status: http.StatusUnauthorized, body: "worker authentication required"},
			}}},
			runner: stubRunner{},
			want:   "401",
		},
		{
			name:   "an unreachable deployment",
			client: &fakeClient{claims: []claimStep{{err: errf("cannot reach the hosted deployment")}}},
			runner: stubRunner{},
			want:   "cannot reach",
		},
		{
			name: "a claim the server refused",
			client: &fakeClient{claims: []claimStep{{
				err: &apiError{status: http.StatusInternalServerError, body: "could not claim"},
			}}},
			runner: stubRunner{},
			want:   "500",
		},
		{
			name:   "a Hermes stage that will not run",
			client: &fakeClient{claims: []claimStep{{job: fakeJob()}}},
			runner: refusingRunner{},
			want:   "no wrapper for profile",
		},
		{
			name:   "a stage whose output fails validation",
			client: &fakeClient{claims: []claimStep{{job: fakeJob()}}},
			runner: brokenRunner{},
			want:   "schema",
		},
		{
			name: "a heartbeat the server rejected",
			client: &fakeClient{
				claims:    []claimStep{{job: fakeJob()}},
				heartbeat: &apiError{status: http.StatusInternalServerError, body: "boom"},
			},
			runner: stubRunner{},
			want:   "500",
		},
		{
			name: "an upload the server refused",
			client: &fakeClient{
				claims:   []claimStep{{job: fakeJob()}},
				complete: &apiError{status: http.StatusBadRequest, body: "invalid artifact"},
			},
			runner: stubRunner{},
			want:   "400",
		},
		{
			name: "a failure we could not even report",
			client: &fakeClient{
				claims: []claimStep{{job: fakeJob()}},
				fail:   errf("cannot reach the hosted deployment"),
			},
			runner: brokenRunner{},
			want:   "could not be reported",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := workableConfig(t)
			err := run(context.Background(), options{}, depsFor(cfg, tc.client, tc.runner))
			if err == nil {
				t.Fatalf("%s exited 0; every failure must be non-zero", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%s reported %q, expected it to mention %q", tc.name, err, tc.want)
			}
		})
	}
}

func TestAFailedRunIsStillReportedToTheServer(t *testing.T) {
	// Exiting non-zero is not enough on its own: the owner's browser must show WHY, or the run sits
	// looking alive until its lease lapses.
	cfg := workableConfig(t)
	client := &fakeClient{claims: []claimStep{{job: fakeJob()}}}
	if err := run(context.Background(), options{}, depsFor(cfg, client, brokenRunner{})); err == nil {
		t.Fatal("a broken stage exited 0")
	}
	if client.failCalls != 1 {
		t.Fatalf("Fail was called %d times, want 1 — a failure must be reported, not just logged",
			client.failCalls)
	}
}

// ─────────────────────────────────────────────────────────────────── the one deliberate non-failure

func TestALostLeaseIsNotABridgeFailure(t *testing.T) {
	// A 409 means another attempt owns the run and ours is correctly discarded. That is the
	// protocol working. Exiting non-zero would make a healthy takeover page an operator.
	cfg := workableConfig(t)
	client := &fakeClient{
		claims:    []claimStep{{job: fakeJob()}},
		heartbeat: &apiError{status: http.StatusConflict, body: "stale_lease"},
	}
	if err := run(context.Background(), options{}, depsFor(cfg, client, stubRunner{})); err != nil {
		t.Fatalf("a lost lease returned %v; it must exit 0", err)
	}
	if client.failCalls != 0 {
		t.Fatal("a lost lease was reported as a failure; that would overwrite the newer attempt's result")
	}
}

// ─────────────────────────────────────────────────────────────────────────────────── draining

func TestDrainStopsAtTheEmptyQueueAndReportsSuccess(t *testing.T) {
	cfg := workableConfig(t)
	client := &fakeClient{claims: []claimStep{{job: fakeJob()}, {job: fakeJob()}}}
	worked, err := drainQueue(context.Background(), cfg, client, stubRunner{}, true)
	if err != nil {
		t.Fatalf("draining two workable runs returned %v", err)
	}
	if worked != 2 {
		t.Fatalf("drained %d runs, want 2", worked)
	}
}

func TestDrainStopsOnTheFirstRealFailureAndReportsWhatItDid(t *testing.T) {
	// The count matters: "nothing worked" and "two worked then the third failed" are different
	// problems, and an operator reading one log line should be able to tell them apart.
	cfg := workableConfig(t)
	client := &fakeClient{claims: []claimStep{
		{job: fakeJob()},
		{err: errf("cannot reach the hosted deployment")},
		{job: fakeJob()},
	}}
	worked, err := drainQueue(context.Background(), cfg, client, stubRunner{}, true)
	if err == nil {
		t.Fatal("a drain that hit a transport failure returned no error")
	}
	if worked != 1 {
		t.Fatalf("worked = %d, want 1 before the failure", worked)
	}

	client2 := &fakeClient{claims: []claimStep{
		{job: fakeJob()},
		{err: errf("cannot reach the hosted deployment")},
	}}
	err = run(context.Background(), options{drain: true}, depsFor(cfg, client2, stubRunner{}))
	if err == nil || !strings.Contains(err.Error(), "worked 1 run(s) before failing") {
		t.Fatalf("run() reported %v; it must say how much succeeded before the failure", err)
	}
}

func TestDrainNeverExceedsItsCap(t *testing.T) {
	cfg := workableConfig(t)
	steps := make([]claimStep, maxDrainJobs+5)
	for i := range steps {
		steps[i] = claimStep{job: fakeJob()}
	}
	client := &fakeClient{claims: steps}
	worked, err := drainQueue(context.Background(), cfg, client, stubRunner{}, true)
	if err != nil {
		t.Fatal(err)
	}
	if worked != maxDrainJobs {
		t.Fatalf("drained %d runs, want the cap of %d", worked, maxDrainJobs)
	}
}

func TestASingleShotClaimsAtMostOneJob(t *testing.T) {
	cfg := workableConfig(t)
	client := &fakeClient{claims: []claimStep{{job: fakeJob()}, {job: fakeJob()}}}
	worked, err := drainQueue(context.Background(), cfg, client, stubRunner{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if worked != 1 {
		t.Fatalf("a non-drain pass worked %d runs, want 1", worked)
	}
}

// ────────────────────────────────────────────────────────────────────────────────────── -check

func TestCheckFailsWhenTheDeploymentCannotBeReached(t *testing.T) {
	// The failure `-check` most needs to catch. A preflight that passes on a bridge which cannot
	// talk to anything is worse than no preflight, because it moves the discovery to the first
	// real run.
	cfg := workableConfig(t)
	client := &fakeClient{statusErr: errf("cannot reach the hosted deployment")}
	err := run(context.Background(), options{check: true}, depsFor(cfg, client, stubRunner{}))
	if err == nil {
		t.Fatal("-check passed against an unreachable deployment")
	}
	if !strings.Contains(err.Error(), "hosted connectivity") {
		t.Fatalf("-check reported %q, expected it to name the connectivity problem", err)
	}
}

func TestCheckFailsOnARejectedCredential(t *testing.T) {
	cfg := workableConfig(t)
	client := &fakeClient{statusErr: &apiError{
		status: http.StatusUnauthorized, body: "worker authentication required",
	}}
	if err := run(context.Background(), options{check: true}, depsFor(cfg, client, stubRunner{})); err == nil {
		t.Fatal("-check passed with a credential the server rejects")
	}
}

func TestCheckPassesAndClaimsNothing(t *testing.T) {
	cfg := workableConfig(t)
	client := &fakeClient{status: workerStatus{
		OK: true, Workflows: []string{workflowCompanyResearch},
		JobSchemaVersion: jobSchemaVersion, ArtifactSchemaVersion: artifactSchemaVersion,
		QueuedRuns: intPtr(3), MaxLeaseSeconds: intPtr(serverMaxLeaseSeconds),
	}}
	if err := run(context.Background(), options{check: true}, depsFor(cfg, client, stubRunner{})); err != nil {
		t.Fatalf("-check failed against a healthy deployment: %v", err)
	}
	// THE POINT OF -check: it proves the configuration without spending the owner's machine.
	if client.claimCalls != 0 {
		t.Fatalf("-check claimed %d job(s); it must claim none", client.claimCalls)
	}
}

func TestCheckFailsOnASchemaVersionMismatch(t *testing.T) {
	// A server that issues a different job version cannot be worked against, and finding that out
	// at -check time rather than mid-run is the entire point of having a -check.
	// The version comparison itself lives in apiClient.Status and is exercised over a real socket
	// in integration_test.go; here the fake proves runCheck surfaces whatever Status reports rather
	// than swallowing it.
	client := &fakeClient{statusErr: errf(
		"the hosted deployment issues %q jobs; this bridge understands %q",
		"attestel.agency.job/99", jobSchemaVersion)}
	cfg := workableConfig(t)
	err := run(context.Background(), options{check: true}, depsFor(cfg, client, stubRunner{}))
	if err == nil {
		t.Fatal("-check passed against a server speaking a different schema version")
	}
}

// refusingRunner stands in for a machine with no Hermes profile wrappers installed.
type refusingRunner struct{}

func (refusingRunner) Run(context.Context, stageSpec, string, string, Config) (string, error) {
	// Matches the real message from resolveProfileBinary, which names the PROFILE — the
	// wrapper `hermes profile alias` creates is named after it, not `hermes-<profile>`.
	return "", errf("no wrapper for profile %q is on PATH", "stock-scout")
}

// intPtr is the pointer helper the status fixtures need: `workerStatus` uses pointers so an absent
// field is distinguishable from a zero one.
func intPtr(n int) *int { return &n }

func TestCheckFailsWhenTheServerWouldClampTheLease(t *testing.T) {
	// `validateBudgets` refuses a lease shorter than one stage plus its margin, because a stage that
	// outlasts its lease loses the run to a takeover. If the SERVER then clamps the lease back down,
	// that guarantee is void — the bridge believes it holds 900 seconds and actually holds whatever
	// the server allowed.
	//
	// This used to print a NOTE and finish with "all checks passed", which told the operator their
	// configuration was fine when it was not.
	cfg := workableConfig(t)
	cfg.LeaseSeconds = 900
	client := &fakeClient{status: workerStatus{
		OK: true, Workflows: []string{workflowCompanyResearch},
		JobSchemaVersion: jobSchemaVersion, ArtifactSchemaVersion: artifactSchemaVersion,
		QueuedRuns:      intPtr(0),
		MaxLeaseSeconds: intPtr(600), // below the bridge's configured lease
	}}
	err := run(context.Background(), options{check: true}, depsFor(cfg, client, stubRunner{}))
	if err == nil {
		t.Fatal("-check passed against a server that would clamp the lease")
	}
	if !strings.Contains(err.Error(), "lease compatibility") {
		t.Fatalf("-check reported %q, expected it to name the lease incompatibility", err)
	}
}

func TestCheckPassesWhenTheLeaseFitsUnderTheServerCap(t *testing.T) {
	// The boundary is "not greater than", so a lease exactly at the cap is fine.
	cfg := workableConfig(t)
	cfg.LeaseSeconds = 600
	client := &fakeClient{status: workerStatus{
		OK: true, Workflows: []string{workflowCompanyResearch},
		JobSchemaVersion: jobSchemaVersion, ArtifactSchemaVersion: artifactSchemaVersion,
		QueuedRuns:      intPtr(0),
		MaxLeaseSeconds: intPtr(600),
	}}
	if err := run(context.Background(), options{check: true}, depsFor(cfg, client, stubRunner{})); err != nil {
		t.Fatalf("a lease exactly at the server cap was rejected: %v", err)
	}
}
