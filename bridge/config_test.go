package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// config_test.go — the transport and credential rules.

func nowForTest() time.Time { return time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC) }

// testAsOf is the cutoff every artifact fixture answers at. Real artifacts always carry one — the
// server assigns it and the worker echoes it back — so a fixture without one is not a smaller test,
// it is an unrealistic one.
const testAsOf = "2026-09-02T10:00:00Z"

func TestPlainHTTPIsRefusedExceptToLoopback(t *testing.T) {
	// The claim carries a bearer credential. Over plain HTTP it is readable on the path, so HTTPS is
	// required — and the escape hatch cannot quietly become "insecure to the internet".
	if err := validateBaseURL("http://attestel.example.com", false); err == nil {
		t.Fatal("plain http to a remote host was accepted without the explicit override")
	}
	if err := validateBaseURL("http://attestel.example.com", true); err == nil {
		t.Fatal("the insecure override permitted a NON-loopback host")
	}
	if err := validateBaseURL("http://localhost:8096", true); err != nil {
		t.Fatalf("loopback http with the explicit override was refused: %v", err)
	}
	if err := validateBaseURL("https://attestel.example.com", false); err != nil {
		t.Fatalf("https was refused: %v", err)
	}
}

func TestAURLCarryingCredentialsOrAnOddSchemeIsRefused(t *testing.T) {
	for _, bad := range []string{
		"https://user:pw@attestel.example.com",
		"ftp://attestel.example.com",
		"file:///etc/passwd",
		"https://",
	} {
		if err := validateBaseURL(bad, false); err == nil {
			t.Fatalf("ATTESTEL_URL %q was accepted", bad)
		}
	}
}

func TestTheCredentialIsRemovedFromTheEnvironmentOnceRead(t *testing.T) {
	// Every Hermes stage inherits this process's environment. A credential left in it is one `env`
	// tool call away from a model's context, and one prompt injection away from a transcript.
	t.Setenv("ATTESTEL_WORKER_TOKEN", "super-secret-worker-token")
	token, err := readToken()
	if err != nil {
		t.Fatal(err)
	}
	if token != "super-secret-worker-token" {
		t.Fatalf("token = %q", token)
	}
	if v, present := os.LookupEnv("ATTESTEL_WORKER_TOKEN"); present {
		t.Fatalf("the credential is still in the environment (%q); child processes would inherit it", v)
	}
}

func TestACredentialFileMustNotBeReadableByOthers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("a-token\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ATTESTEL_WORKER_TOKEN", "")
	t.Setenv("ATTESTEL_WORKER_TOKEN_FILE", path)
	if _, err := readToken(); err == nil {
		t.Fatal("a world-readable credential file was accepted")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	// readToken removes BOTH credential variables on every path, including the failing one above, so
	// a second call has to be configured again. That is the point of the unset, not a wrinkle in it.
	t.Setenv("ATTESTEL_WORKER_TOKEN_FILE", path)
	got, err := readToken()
	if err != nil {
		t.Fatalf("a 0600 credential file was refused: %v", err)
	}
	if got != "a-token" {
		t.Fatalf("token = %q", got)
	}
}

func TestThereIsNoCredentialWithoutConfiguration(t *testing.T) {
	// No default, no fallback to AUTH_SECRET or anything else, and nothing read from the repository.
	t.Setenv("ATTESTEL_WORKER_TOKEN", "")
	t.Setenv("ATTESTEL_WORKER_TOKEN_FILE", "")
	if _, err := readToken(); err == nil {
		t.Fatal("a credential was resolved from nowhere")
	}
}

func TestACredentialFileErrorNeverEchoesItsPath(t *testing.T) {
	// The path is under the owner's home directory. An error that quotes it discloses their layout
	// to anyone they paste the message to.
	dir := t.TempDir()
	secret := filepath.Join(dir, "very-identifying-name")
	t.Setenv("ATTESTEL_WORKER_TOKEN", "")
	t.Setenv("ATTESTEL_WORKER_TOKEN_FILE", secret)
	_, err := readToken()
	if err == nil {
		t.Fatal("a missing credential file was accepted")
	}
	if strings.Contains(err.Error(), "very-identifying-name") || strings.Contains(err.Error(), dir) {
		t.Fatalf("the error leaked the credential path: %v", err)
	}
}

func TestEveryErrorThisModuleBuildsIsRedacted(t *testing.T) {
	// `errf` redacts at construction, so no call site has to remember to. This asserts the property
	// on the constructor itself rather than on a sample of its callers.
	err := errf("failed reading %s with api_key=%s", "/Users/someone/.hermes/config.yaml", "abc123def456")
	msg := err.Error()
	for _, leaked := range []string{"someone", "abc123def456"} {
		if strings.Contains(msg, leaked) {
			t.Fatalf("errf leaked %q: %s", leaked, msg)
		}
	}
}

func TestTheWorkerIdIsOpaqueAndStable(t *testing.T) {
	// Not a hostname, not a username, not a MAC. The hosted side needs to tell two workers apart and
	// needs to learn nothing else about either.
	dir := t.TempDir()
	first, err := resolveWorkerID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first, "wkr_") {
		t.Fatalf("worker id %q does not have the opaque form", first)
	}
	host, _ := os.Hostname()
	if host != "" && strings.Contains(first, host) {
		t.Fatalf("the worker id embeds the hostname: %q", first)
	}
	second, err := resolveWorkerID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("the worker id is not stable across runs: %q then %q", first, second)
	}
}

func TestTheJobValidatorRefusesAnythingOffTheAllowlist(t *testing.T) {
	// The worker re-validates a job it was handed. The server's limits are the server's; this is
	// the check that holds if the server is wrong or compromised.
	valid := Job{
		SchemaVersion: jobSchemaVersion, RunID: "agr_1", UserID: "u",
		WorkflowVersion: workflowCompanyResearch, Ticker: "NVDA",
		Question: "why did gross margin move", AsOf: "2026-09-02T00:00:00Z",
		LeaseToken: "tok",
	}
	if err := valid.validate(); err != nil {
		t.Fatalf("a valid job was refused: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*Job)
	}{
		{"a future schema version", func(j *Job) { j.SchemaVersion = "attestel.agency.job/2" }},
		{"a workflow off the allowlist", func(j *Job) { j.WorkflowVersion = "exfiltrate_v1" }},
		{"no lease token", func(j *Job) { j.LeaseToken = "" }},
		{"no user id", func(j *Job) { j.UserID = "" }},
		{"a ticker that is a path", func(j *Job) { j.Ticker = "../../etc/passwd" }},
		{"an empty question", func(j *Job) { j.Question = "" }},
		{"an over-long question", func(j *Job) { j.Question = strings.Repeat("x", maxQuestionLen+1) }},
		{"a question with control characters", func(j *Job) { j.Question = "why\x00stop" }},
		{"an unparseable cutoff", func(j *Job) { j.AsOf = "yesterday" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			j := valid
			tc.mutate(&j)
			if err := j.validate(); err == nil {
				t.Fatalf("a job with %s was accepted", tc.name)
			}
		})
	}
}

func TestTheSchemaVersionsMatchTheServer(t *testing.T) {
	// These strings are duplicated across two independent modules on purpose (see schema.go). This
	// test is the thing that makes the duplication safe: if the server's copy is edited without
	// this one, CI fails here rather than in production with a silently mis-shaped envelope.
	if jobSchemaVersion != "attestel.agency.job/1" {
		t.Fatalf("jobSchemaVersion = %q; journal/agency.go pins attestel.agency.job/1", jobSchemaVersion)
	}
	if artifactSchemaVersion != "attestel.agency.artifact/1" {
		t.Fatalf("artifactSchemaVersion = %q; journal/agency.go pins attestel.agency.artifact/1",
			artifactSchemaVersion)
	}
	if workflowCompanyResearch != "company_research_v1" {
		t.Fatalf("workflow = %q", workflowCompanyResearch)
	}
	if vetoScope != "new_exposure_only" {
		t.Fatalf("vetoScope = %q; a veto may only ever withhold new exposure", vetoScope)
	}
}

func TestARetryableFailureIsDistinguishedFromAPermanentOne(t *testing.T) {
	// Burning the attempt cap on a failure that will recur identically only delays the honest
	// answer; refusing to retry a transient one loses work that would have succeeded.
	permanent := []string{
		"the stage's output does not match its schema: x",
		"the stage produced no JSON object",
		"the agents produced prescriptive language (\"price target\")",
		"the artifact contains an absolute macOS home path and will not be uploaded",
		"finding 0: a finding cites a source the run never declared",
		"no wrapper for profile \"stock-scout\" is on PATH",
	}
	for _, msg := range permanent {
		if retryableFailure(errf("%s", msg)) {
			t.Fatalf("%q was treated as retryable", msg)
		}
	}
	transient := []string{
		"cannot reach the hosted deployment: connection refused",
		"stage stock-scout exceeded its 600s wall-clock budget",
	}
	for _, msg := range transient {
		if !retryableFailure(errf("%s", msg)) {
			t.Fatalf("%q was treated as permanent", msg)
		}
	}
}
