package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// smoke_test.go — ONE REAL REVIEW, against a REAL deployment, with REAL Hermes profiles.
//
// ─────────────────────────────────────────────────────────────────────────────────────────────
// WHY THIS EXISTS ALONGSIDE THE STUBBED END-TO-END TEST
// ─────────────────────────────────────────────────────────────────────────────────────────────
// `review_test.go`'s end-to-end case proves the PIPELINE: routes, credentials, leases, schemas,
// validators, the citation checker and the round trip. It stubs the model, which is what makes it
// runnable on any laptop with no provider credential — and it is therefore blind to every failure
// that only a real model can produce:
//
//   - a profile that does not exist, or whose wrapper is not on PATH;
//   - a profile with no working model configured, which fails at the first token;
//   - a model that ignores the output contract and returns prose, a fenced block, or JSON with a
//     field the closed schema does not declare;
//   - a model that invents an `evidencePaths` entry rather than copying one — the single most
//     likely real failure, and one no stub will ever reproduce because the stub reads the paths out
//     of the document it was handed;
//   - a stage that runs past its turn cap or its wall-clock budget on a real chain of thought;
//   - an artifact that trips the leak or prescriptive-language scan on wording a model chose.
//
// Every one of those is a REAL defect that ships if only the stub is ever exercised. So this test
// invokes `execRunner` — the production path, no dry run — and asserts the same properties.
//
// ─────────────────────────────────────────────────────────────────────────────────────────────
// IT EXERCISES THE REAL PRODUCT PATH, WHICH MEANS TWO DIFFERENT BASE URLS
// ─────────────────────────────────────────────────────────────────────────────────────────────
// The owner and the worker reach this deployment through DIFFERENT front doors, and a smoke test
// that used one URL for both would prove the wrong thing:
//
//	OWNER   https://<host>/api/experiments/snapshots     nginx -> gateway -> journal
//	        https://<host>/api/agency/reviews            (session cookie)
//	WORKER  https://<host>/svc/journal/_internal/...     nginx -> journal directly
//	        (worker token; deliberately NOT proxied by the gateway)
//
// An earlier version pointed the owner calls straight at `/svc/journal`, bypassing nginx and the
// gateway entirely. That skipped the two layers most likely to be misconfigured — the gateway's
// dedicated snapshot client and nginx's location-specific timeout are BOTH new, and both sit only
// on the `/api/` path. The test would have passed while the real product route 502'd.
//
//	ATTESTEL_SMOKE_APP_URL     the APPLICATION origin the browser uses (e.g. https://<host>).
//	                           Owner calls go to <app>/api/... through nginx and the gateway.
//	ATTESTEL_SMOKE_JOURNAL_URL the journal's WORKER base, including the reverse-proxy prefix
//	                           (e.g. https://<host>/svc/journal). Bridge calls go here.
//	ATTESTEL_SMOKE_COOKIE      the owner's session cookie, as `name=value`.
//	ATTESTEL_WORKER_TOKEN      the worker credential, as for any normal run.
//
//	cd bridge && ATTESTEL_SMOKE_APP_URL=https://<host> \
//	             ATTESTEL_SMOKE_JOURNAL_URL=https://<host>/svc/journal \
//	             ATTESTEL_SMOKE_COOKIE='nvda_session=…' \
//	             ATTESTEL_WORKER_TOKEN=… \
//	             go test -run TestRealHermesReviewSmoke -v -timeout 20m ./...
//
// It costs real model calls and real time, so it is SKIPPED unless all four are set, and it refuses
// to run in dry-run mode — a "real Hermes smoke test" that silently used the stub would be worse
// than no test at all, because it would report success for the thing it did not do.
//
// WHAT IT WRITES, so nobody is surprised: one evidence snapshot and one completed review run on the
// owner's own account. Both are read-only with respect to the experiment — the snapshot is
// assembled from GETs and the review has no field that could change anything. It resets nothing,
// trains nothing and books nothing.

const (
	smokeAppURLEnv     = "ATTESTEL_SMOKE_APP_URL"
	smokeJournalURLEnv = "ATTESTEL_SMOKE_JOURNAL_URL"
	smokeCookieEnv     = "ATTESTEL_SMOKE_COOKIE"
)

func TestRealHermesReviewSmoke(t *testing.T) {
	appURL := strings.TrimRight(strings.TrimSpace(os.Getenv(smokeAppURLEnv)), "/")
	journalURL := strings.TrimRight(strings.TrimSpace(os.Getenv(smokeJournalURLEnv)), "/")
	cookie := strings.TrimSpace(os.Getenv(smokeCookieEnv))
	token := strings.TrimSpace(os.Getenv("ATTESTEL_WORKER_TOKEN"))
	if appURL == "" || journalURL == "" || cookie == "" || token == "" {
		t.Skipf("skipping the real-Hermes smoke test: set %s, %s, %s and ATTESTEL_WORKER_TOKEN to "+
			"run it (it makes real model calls on this machine and writes a real snapshot and "+
			"review)", smokeAppURLEnv, smokeJournalURLEnv, smokeCookieEnv)
	}
	// The owner path must be the PRODUCT path. Pointing it at /svc/journal would bypass nginx and
	// the gateway, which is where the snapshot route's dedicated client and location-specific
	// timeout live — the two layers most likely to be wrong, and the two this test exists to cover.
	if strings.Contains(appURL, "/svc/") {
		t.Fatalf("%s is %q; it must be the APPLICATION origin so owner calls traverse nginx and the "+
			"gateway. The journal worker base belongs in %s", smokeAppURLEnv, appURL,
			smokeJournalURLEnv)
	}
	// A "real Hermes" test that quietly used the stub would report success for the one thing it
	// exists to check. Refuse rather than skip: the operator asked for this test by setting three
	// variables, and telling them it passed would be a lie.
	if envBool("ATTESTEL_BRIDGE_DRY_RUN") {
		t.Fatal("ATTESTEL_BRIDGE_DRY_RUN is set; this test must invoke the real Hermes profiles, " +
			"and a stubbed run would prove nothing it is here to prove")
	}

	// ── 0. The wrappers must exist before anything is queued. ────────────────────────────────
	//
	// Checked FIRST so a missing profile is a clear failure here rather than a queued review that
	// fails three attempts later with the same message buried in a run record.
	for _, spec := range experimentReviewChain {
		if _, err := resolveProfileBinary(spec.Profile); err != nil {
			t.Fatalf("the %s wrapper is not on PATH: %v\n\nCreate the three review profiles and "+
				"alias them:\n  hermes profile alias experiment-auditor\n"+
				"  hermes profile alias evidence-skeptic\n  hermes profile alias experiment-chair",
				spec.Profile, err)
		}
	}

	cfg := smokeConfig(t, journalURL, token)
	client := newAPIClient(cfg)

	// The deployment must actually offer the workflow, checked before any work is done.
	statusCtx, cancelStatus := withTimeout(context.Background())
	status, err := client.Status(statusCtx)
	cancelStatus()
	if err != nil {
		t.Fatalf("the deployment is unreachable or the worker credential was rejected: %v", err)
	}
	if !status.offersReview() {
		t.Fatalf("this deployment does not offer %s; it serves %v",
			workflowExperimentReview, status.Workflows)
	}

	// ── 1. Freeze the evidence, exactly as the button does. ──────────────────────────────────
	code, created := smokeOwnerCall(t, http.MethodPost, appURL+"/api/experiments/snapshots", cookie, "")
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("snapshot = %d: %v", code, created)
	}
	snapshot, _ := created["snapshot"].(map[string]any)
	snapshotID, _ := snapshot["id"].(string)
	if snapshotID == "" {
		t.Fatalf("the deployment returned no snapshot id: %v", created)
	}
	derived, _ := snapshot["derived"].(map[string]any)
	t.Logf("snapshot %s · generation %v · revision %v", snapshotID,
		snapshot["generation"], snapshot["revision"])
	t.Logf("derived: paper=%v candidate=%v operational=%v",
		derived["paperStatus"], derived["candidateStatus"], derived["operationalStatus"])

	// ── 2. Queue the review. ─────────────────────────────────────────────────────────────────
	code, queued := smokeOwnerCall(t, http.MethodPost, appURL+"/api/agency/reviews", cookie,
		fmt.Sprintf(`{"workflow":%q,"snapshotId":%q}`, workflowExperimentReview, snapshotID))
	if code != http.StatusAccepted {
		t.Fatalf("review = %d: %v", code, queued)
	}
	runID, _ := queued["run"].(map[string]any)["id"].(string)
	if runID == "" {
		t.Fatalf("the deployment returned no run id: %v", queued)
	}
	if queued["created"] == false {
		t.Fatalf("the review attached to an existing in-flight run (%s) rather than creating one. "+
			"This test must work a run it created, so it cannot assert anything about one it did "+
			"not; cancel the existing run or wait for it to finish, then retry", runID)
	}
	t.Logf("queued review %s; invoking the real profiles now", runID)

	// ── 3. Work it with the REAL runner. This is the point of the test. ──────────────────────
	//
	// CLAIM ONLY THE RUN THIS TEST CREATED.
	//
	// `runOnce` claims the OLDEST claimable run across every configured owner. On a deployment that
	// is actually in use, that is very likely somebody else's queued research job — and the test
	// would then spend the operator's machine on unrelated work, report on a run it knows nothing
	// about, and leave its own review sitting in the queue. Worse, it would hold a lease on a job
	// its own assertions are about to abandon.
	//
	// `claimUntil` therefore claims repeatedly until it gets THIS run, and releases anything else
	// back to the queue immediately via `Fail(retryable)` — which returns the run to `queued` with
	// its attempt count intact, exactly as a transport failure would. Nothing is consumed, nothing
	// is worked, and the queue is left as it was found.
	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(cfg.RunBudgetSeconds+300)*time.Second)
	defer cancel()

	started := time.Now()
	if err := workWithProviderRetries(ctx, t, cfg, client, runID, func() error {
		return claimAndWorkExactly(ctx, t, cfg, client, runID)
	}); err != nil {
		t.Fatalf("the real review failed after %s: %v", time.Since(started).Round(time.Second), err)
	}
	t.Logf("the real chain completed in %s", time.Since(started).Round(time.Second))

	// ── 4. Read it back and assert the properties that matter. ───────────────────────────────
	code, out := smokeOwnerCall(t, http.MethodGet, appURL+"/api/agency/runs/"+runID, cookie, "")
	if code != http.StatusOK {
		t.Fatalf("read = %d: %v", code, out)
	}
	if out["status"] != "completed" {
		t.Fatalf("status = %v (error: %v); a real model failed the pipeline the stub passes",
			out["status"], out["error"])
	}
	review, ok := out["review"].(map[string]any)
	if !ok {
		t.Fatalf("the completed run carries no review: %v", out)
	}

	// THE STATUSES CAME FROM THE SNAPSHOT, not from the model. A real model that tried to restate
	// one would have been refused by the server; this asserts the values that survived match the
	// ones this deployment derived for itself.
	for _, field := range []string{"paperStatus", "candidateStatus", "operationalStatus"} {
		got, _ := review[field].(map[string]any)
		if got == nil {
			t.Fatalf("the review carries no %s", field)
		}
		if got["value"] != derived[field] {
			t.Errorf("%s = %v but the snapshot derived %v; a worker may not restate a status",
				field, got["value"], derived[field])
		}
		if rationale, _ := got["rationale"].(string); strings.TrimSpace(rationale) == "" {
			t.Errorf("%s carries no rationale; a real model produced an empty explanation", field)
		}
	}

	// EVERY CITATION RESOLVED. The server already refused any that did not — reaching `completed`
	// is the proof — but asserting the review actually cites something catches a model that
	// satisfied the schema with empty lists.
	refs, _ := review["evidenceReferences"].([]any)
	if len(refs) == 0 {
		t.Error("the review cites no evidence at all")
	}

	// A REAL REVIEW IS STILL NO_SIGNAL, and still has nowhere to put one.
	action, _ := out["actionability"].(map[string]any)
	if action == nil || action["evidenceState"] != "NO_SIGNAL" || action["action"] != "NO_ACTION" {
		t.Errorf("a completed real review reports %v, want NO_SIGNAL / NO_ACTION", action)
	}
	encoded, _ := json.Marshal(review)
	for _, forbidden := range []string{
		`"direction"`, `"signal"`, `"priceTarget"`, `"expectedReturn"`, `"probability"`,
		`"positionSize"`, `"recommendation"`, `"confidence"`,
	} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("a real review carries %s", forbidden)
		}
	}

	// AND IT IS NOT LABELLED DEGRADED. The stub always is; a real run must not be, or the two are
	// indistinguishable in the stored record.
	if degraded, _ := review["degraded"].([]any); len(degraded) > 0 {
		t.Errorf("a real review is labelled degraded (%v); that label belongs to stubbed runs",
			degraded)
	}

	if summary, _ := review["summary"].(string); len(strings.TrimSpace(summary)) < 40 {
		t.Errorf("the chair's summary is %d characters; a real review should say something",
			len(summary))
	} else {
		t.Logf("summary: %s", summary)
	}
}

// smokeConfig builds a bridge configuration pointed at the deployment under test, WITHOUT the dry
// run. `t.Setenv` restores the previous environment when the test ends.
func smokeConfig(t *testing.T, base, token string) Config {
	t.Helper()
	t.Setenv("ATTESTEL_URL", base)
	t.Setenv("ATTESTEL_WORKER_TOKEN", token)
	t.Setenv("ATTESTEL_BRIDGE_STATE_DIR", t.TempDir())
	t.Setenv("ATTESTEL_BRIDGE_DRY_RUN", "")
	if strings.HasPrefix(base, "http://") {
		// Only relevant when somebody points this at a local deployment; a hosted one is https and
		// `validateBaseURL` enforces that on its own.
		t.Setenv("ATTESTEL_ALLOW_INSECURE_URL", "1")
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("cannot configure the bridge: %v", err)
	}
	if cfg.DryRunHermes {
		t.Fatal("the resolved configuration is a dry run; this test must invoke real profiles")
	}
	return cfg
}

// smokeOwnerCall issues an owner request with the operator's own session cookie.
func smokeOwnerCall(t *testing.T, method, url, cookie, body string) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", cookie)

	// Its own client with a budget that covers snapshot assembly (the journal bounds itself at
	// 150s) rather than the default, which would give up before the deployment did.
	client := &http.Client{Timeout: 200 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

// ─────────────────────────────────────────────────────── the local real-Hermes variant

// localHermesSmokeEnv opts into the LOCAL real-model run.
const localHermesSmokeEnv = "ATTESTEL_LOCAL_HERMES_SMOKE"

// TestRealHermesReviewAgainstALocalJournal is the smoke test above WITHOUT a hosted deployment.
//
// WHY BOTH EXIST. `TestRealHermesReviewSmoke` needs a URL, an owner session cookie and the
// deployment's worker token — credentials that live with the operator and should not travel. That
// makes it the right test for "does this work against useCTL" and the wrong one for "does the
// real-model path work at all", because nobody can run it without those three secrets.
//
// This one needs neither: it starts the REAL journal binary on a loopback port with a temp data
// directory, points it at a fake paper service, mints its own session, and then runs `execRunner` —
// the production path, real profiles, real model calls. Everything the stub cannot cover is covered
// here: whether the wrappers resolve, whether the profiles have a working model, whether a real
// model honours the closed output schema, and — the one that matters most — whether it CITES REAL
// SNAPSHOT PATHS rather than inventing plausible-looking ones.
//
// It is opt-in because it spends real model calls. It writes nothing outside a temp directory.
//
//	cd bridge && ATTESTEL_LOCAL_HERMES_SMOKE=1 go test -run TestRealHermesReviewAgainstALocalJournal -v -timeout 20m ./...
func TestRealHermesReviewAgainstALocalJournal(t *testing.T) {
	if !envBool(localHermesSmokeEnv) {
		t.Skipf("skipping the local real-Hermes run: set %s=1 to invoke the real profiles (it "+
			"spends real model calls on this machine)", localHermesSmokeEnv)
	}
	for _, spec := range experimentReviewChain {
		if _, err := resolveProfileBinary(spec.Profile); err != nil {
			t.Fatalf("the %s wrapper is not on PATH: %v", spec.Profile, err)
		}
	}

	paperURL := fakePaperServer(t)
	base := startJournalWithPaper(t, paperURL)

	// The bridge config, WITHOUT the dry run — `e2eBridgeConfig` sets it, so this rebuilds the
	// environment rather than reusing it.
	t.Setenv("ATTESTEL_URL", base)
	t.Setenv("ATTESTEL_ALLOW_INSECURE_URL", "1") // loopback only
	t.Setenv("ATTESTEL_WORKER_TOKEN", e2eToken)
	t.Setenv("ATTESTEL_BRIDGE_STATE_DIR", t.TempDir())
	t.Setenv("ATTESTEL_BRIDGE_PROMPT_DIR", "prompts")
	t.Setenv("ATTESTEL_BRIDGE_DRY_RUN", "")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("cannot configure the bridge: %v", err)
	}
	if cfg.DryRunHermes {
		t.Fatal("the resolved configuration is a dry run; this test must invoke real profiles")
	}
	client := newAPIClient(cfg)

	code, created := ownerRequest(t, http.MethodPost, base+"/experiments/snapshots", "")
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("snapshot = %d: %v", code, created)
	}
	snapshot := created["snapshot"].(map[string]any)
	snapshotID := snapshot["id"].(string)
	derived := snapshot["derived"].(map[string]any)

	code, queued := ownerRequest(t, http.MethodPost, base+"/agency/reviews",
		fmt.Sprintf(`{"workflow":%q,"snapshotId":%q}`, workflowExperimentReview, snapshotID))
	if code != http.StatusAccepted {
		t.Fatalf("review = %d: %v", code, queued)
	}
	runID := queued["run"].(map[string]any)["id"].(string)
	t.Logf("queued review %s against snapshot %s; invoking the real profiles now", runID, snapshotID)

	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(cfg.RunBudgetSeconds+120)*time.Second)
	defer cancel()

	started := time.Now()
	err = workWithProviderRetries(ctx, t, cfg, client, runID, func() error {
		worked, runErr := runOnce(ctx, cfg, client, execRunner{})
		if runErr != nil {
			return runErr
		}
		if !worked {
			return errf("the bridge claimed nothing although a review was queued")
		}
		return nil
	})
	elapsed := time.Since(started).Round(time.Second)
	if err != nil {
		t.Fatalf("the real review failed after %s: %v", elapsed, err)
	}
	t.Logf("the real three-stage chain completed in %s", elapsed)

	code, out := ownerRequest(t, http.MethodGet, base+"/agency/runs/"+runID, "")
	if code != http.StatusOK {
		t.Fatalf("read = %d: %v", code, out)
	}
	if out["status"] != "completed" {
		t.Fatalf("status = %v (error: %v); a real model failed the pipeline the stub passes",
			out["status"], out["error"])
	}
	review := out["review"].(map[string]any)

	// The statuses came from the SNAPSHOT. A real model cannot restate one — the server refuses —
	// and reaching `completed` with matching values is the proof.
	for _, field := range []string{"paperStatus", "candidateStatus", "operationalStatus"} {
		got := review[field].(map[string]any)
		if got["value"] != derived[field] {
			t.Errorf("%s = %v but the snapshot derived %v", field, got["value"], derived[field])
		}
		if rationale, _ := got["rationale"].(string); strings.TrimSpace(rationale) == "" {
			t.Errorf("%s carries no rationale", field)
		}
	}

	// EVERY CITATION RESOLVED AGAINST THE REAL SNAPSHOT. This is the assertion the stub can never
	// make honestly: the stub reads its paths out of the document it was handed, so it cannot
	// invent one. A real model can, and this is where that shows up.
	refs, _ := review["evidenceReferences"].([]any)
	if len(refs) == 0 {
		t.Error("the real review cites no evidence at all")
	}
	for _, r := range refs {
		ref := r.(map[string]any)
		if ref["presence"] != "present" {
			t.Errorf("the real review cites %v, which is not present in the snapshot", ref["path"])
		}
	}

	// A real review is NOT degraded — that label belongs to stubbed runs, and the two must stay
	// distinguishable in the stored record.
	if degraded, _ := review["degraded"].([]any); len(degraded) > 0 {
		t.Errorf("a real review is labelled degraded (%v)", degraded)
	}

	action := out["actionability"].(map[string]any)
	if action["evidenceState"] != "NO_SIGNAL" || action["action"] != "NO_ACTION" {
		t.Errorf("a completed real review reports %v / %v, want NO_SIGNAL / NO_ACTION",
			action["evidenceState"], action["action"])
	}
	encoded, _ := json.Marshal(review)
	for _, forbidden := range []string{
		`"direction"`, `"signal"`, `"priceTarget"`, `"expectedReturn"`, `"probability"`,
		`"positionSize"`, `"recommendation"`, `"confidence"`,
	} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("a real review carries %s", forbidden)
		}
	}
	t.Logf("summary: %s", review["summary"])
	blockers, _ := review["blockers"].([]any)
	t.Logf("%d blockers, %d evidence references", len(blockers), len(refs))
}

// claimAndWorkExactly claims until it holds `wantRunID`, working only that run.
//
// Anything else claimed on the way is RELEASED, not worked: `Fail(retryable=true)` returns a run to
// `queued` without consuming an attempt's worth of real work, which is the same path a transport
// failure takes. The queue is left as it was found.
//
// `maxSkips` bounds the search so a busy deployment cannot turn this into an unbounded loop of
// claim-and-release against somebody else's queue.
func claimAndWorkExactly(ctx context.Context, t *testing.T, cfg Config, client hostedClient, wantRunID string) error {
	t.Helper()
	const maxSkips = 20
	for skipped := 0; skipped <= maxSkips; skipped++ {
		claimCtx, cancel := withTimeout(ctx)
		job, ok, err := client.Claim(claimCtx, cfg)
		cancel()
		if err != nil {
			return err
		}
		if !ok {
			return errf("the queue is empty but run %s was never claimed; another worker took it",
				wantRunID)
		}
		ref := job.ref()
		if ref.RunID == wantRunID {
			if job.Review == nil {
				return errf("run %s came back as a %s job, not a review", wantRunID,
					job.workflowVersion())
			}
			return workReview(ctx, cfg, client, execRunner{}, job.Review)
		}

		// Somebody else's run. Put it straight back.
		t.Logf("released unrelated run %s (%s) back to the queue", ref.RunID, job.workflowVersion())
		releaseCtx, cancelRelease := withTimeout(ctx)
		relErr := client.Fail(releaseCtx, ref,
			"released by the attestel smoke test, which claims only the run it created", true)
		cancelRelease()
		if relErr != nil {
			return errf("could not release unrelated run %s back to the queue: %v", ref.RunID, relErr)
		}
	}
	return errf("claimed %d unrelated runs without reaching %s; run this against an idle queue or "+
		"an isolated owner", maxSkips, wantRunID)
}

// workWithProviderRetries re-attempts ONLY the failures the bridge itself classifies as retryable.
//
// WHY THIS IS NOT MASKING A DEFECT. `retryableFailure` (run.go) is the same classifier the bridge
// uses in production to decide whether to hand a run back to the queue. It returns false for every
// failure this test exists to catch — a schema violation, an invented citation, prescriptive
// language, a missing wrapper, a forbidden toolset — and true only for transport and provider
// conditions: a dropped connection, a budget overrun, an `overloaded_error` from the model host.
//
// Retrying the second category is what the production bridge does anyway, on a run the server
// re-queued. NOT retrying it would make this test a monitor of the provider's uptime rather than of
// this code, and a red suite that means "Anthropic was busy for ten seconds" is a red suite people
// learn to ignore.
//
// The cap matches the server's own `agencyMaxAttempts`, so the test cannot outlast the attempt
// budget the run actually has. Every retry is logged with its reason, so a run that only passed on
// the third attempt is visible rather than silently green.
func workWithProviderRetries(ctx context.Context, t *testing.T, cfg Config, client hostedClient, runID string, attempt func() error) error {
	t.Helper()
	const maxAttempts = 3
	var last error
	for i := 1; i <= maxAttempts; i++ {
		last = attempt()
		if last == nil {
			if i > 1 {
				t.Logf("the review succeeded on attempt %d", i)
			}
			return nil
		}
		if !retryableFailure(last) {
			// A defect in this code, not a hiccup in somebody's datacentre. Fail immediately and
			// say so, rather than burning the attempt budget on a result that will not change.
			return errf("permanent failure (not retried): %v", last)
		}
		if i < maxAttempts {
			t.Logf("attempt %d failed with a retryable condition, retrying: %v", i, last)
			// The server re-queued the run on a retryable failure, so the next claim can reach it.
			// A short pause, because the usual cause is an overloaded provider.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(10 * time.Second):
			}
		}
	}
	return errf("still failing after %d attempts, last retryable error: %v", maxAttempts, last)
}
