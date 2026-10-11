package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// client.go — the ONE place this bridge talks to the network on the hosted side.
//
// EVERY CONNECTION IS OUTBOUND AND THIS PROCESS LISTENS ON NOTHING. There is no `http.Server`
// anywhere in this module, no port is bound, and nothing on the home network is reachable from
// outside as a consequence of running it. The hosted deployment has no address for this machine and
// never learns one: it answers claims, it does not make them.
//
// REDIRECTS ARE REFUSED. `CheckRedirect` returns an error, so a hosted deployment that has been
// compromised or misconfigured cannot bounce a request — with the worker credential attached — to a
// host of its choosing. A bearer token that follows a redirect is a bearer token you have given
// away.
//
// THE CREDENTIAL IS ATTACHED HERE AND NOWHERE ELSE, and it never appears in an error: `errf`
// redacts, and `truncateBody` caps and redacts any upstream body quoted back in a message.

const maxResponseBytes = 1 << 20 // 1 MiB; every response in this protocol is small JSON

// The addressing headers for the lease-scoped snapshot read. KEEP IN STEP WITH
// journal/experiment_routes.go's copies — a mismatch is a 400 with no obvious cause.
const (
	agencyUserHeader  = "X-Agency-User-Id"
	agencyLeaseHeader = "X-Agency-Lease-Token"
)

// hostedClient is the seam the tests drive. `*apiClient` is the only production implementation;
// the interface exists so `run()` and `drainQueue()` can be exercised against a fake that returns
// an auth failure, a transport failure or a stale lease on demand. Those are precisely the paths
// that must produce a non-zero exit, and they are unreachable from a test that needs a real server.
type hostedClient interface {
	Status(ctx context.Context) (workerStatus, error)
	Claim(ctx context.Context, cfg Config) (*claimedJob, bool, error)
	Heartbeat(ctx context.Context, ref jobRef, stage string, cfg Config) error
	Complete(ctx context.Context, job *Job, artifact *Artifact) error
	Fail(ctx context.Context, ref jobRef, reason string, retryable bool) error

	// The review lane. `Snapshot` is a READ — it takes no lease, extends none, and changes nothing
	// — and it is the ONLY method on this interface that fetches a document. Note its parameters:
	// a run id and a lease. There is no URL argument anywhere in this interface, so a compromised
	// server cannot direct this worker at an address of its choosing.
	Snapshot(ctx context.Context, job *ReviewJob) (*ExperimentSnapshot, error)
	CompleteReview(ctx context.Context, job *ReviewJob, review *ExperimentReviewArtifact) error
}

// jobRef is the addressing block every post-claim call needs: whose run, which run, and the lease
// that proves we still hold it. Both job kinds reduce to it, so `Heartbeat` and `Fail` are written
// once rather than twice.
type jobRef struct {
	RunID      string
	UserID     string
	LeaseToken string
}

func (j *Job) ref() jobRef {
	return jobRef{RunID: j.RunID, UserID: j.UserID, LeaseToken: j.LeaseToken}
}

func (j *ReviewJob) ref() jobRef {
	return jobRef{RunID: j.RunID, UserID: j.UserID, LeaseToken: j.LeaseToken}
}

// claimedJob is what one claim produced: EXACTLY ONE of the two job kinds, never both and never
// neither. `runOnce` switches on which one is set, so a server that returned an envelope this
// bridge does not recognise produces a refusal rather than a half-understood run.
type claimedJob struct {
	Research *Job
	Review   *ReviewJob
}

func (c *claimedJob) ref() jobRef {
	if c.Review != nil {
		return c.Review.ref()
	}
	if c.Research != nil {
		return c.Research.ref()
	}
	return jobRef{}
}

// workflowVersion names which workflow was claimed, for logging and for the heartbeat's stage
// validation.
func (c *claimedJob) workflowVersion() string {
	if c.Review != nil {
		return c.Review.WorkflowVersion
	}
	if c.Research != nil {
		return c.Research.WorkflowVersion
	}
	return ""
}

var _ hostedClient = (*apiClient)(nil)

// workerStatus is the read-only answer from `GET /_internal/agency/status`. It proves the URL, the
// TLS path and the credential all work, and it says how much is queued — without claiming anything.
type workerStatus struct {
	OK                    bool     `json:"ok"`
	Workflows             []string `json:"workflows"`
	JobSchemaVersion      string   `json:"jobSchemaVersion"`
	ArtifactSchemaVersion string   `json:"artifactSchemaVersion"`
	// POINTERS, so an ABSENT field is distinguishable from a zero one. `queuedRuns: 0` is a
	// perfectly ordinary answer (nothing is waiting) and an omitted `queuedRuns` is a response that
	// is not this API — an `int` would collapse the two into the same value, which is exactly the
	// silence the fail-closed rule below exists to reject.
	QueuedRuns      *int `json:"queuedRuns"`
	MaxLeaseSeconds *int `json:"maxLeaseSeconds"`

	// The review lane's schema pair. OPTIONAL, and deliberately so: a deployment that predates the
	// review workflow states neither, and this bridge must still be able to run research against
	// it. They are checked only when the server actually offers `experiment_review_v1`.
	ReviewJobSchemaVersion      string `json:"reviewJobSchemaVersion"`
	ReviewArtifactSchemaVersion string `json:"reviewArtifactSchemaVersion"`
}

// offersReview reports whether the hosted deployment dispatches the review workflow.
func (s workerStatus) offersReview() bool {
	return containsString(s.Workflows, workflowExperimentReview)
}

type apiClient struct {
	base  string
	token string
	http  *http.Client
}

func newAPIClient(cfg Config) *apiClient {
	return &apiClient{
		base:  cfg.BaseURL,
		token: cfg.Token,
		http: &http.Client{
			Timeout: httpTimeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return errf("the hosted deployment attempted a redirect; refusing to forward the " +
					"worker credential to another host")
			},
		},
	}
}

// apiError carries the HTTP status so the caller can act on 409 (a lost lease) differently from a
// transport failure. `body` has already been redacted and capped.
type apiError struct {
	status int
	body   string
}

func (e *apiError) Error() string {
	return redact("hosted API returned " + itoa(e.status) + ": " + e.body)
}

func (c *apiClient) post(ctx context.Context, path string, payload any, out any) error {
	buf, err := json.Marshal(payload)
	if err != nil {
		return errf("cannot encode the request: %v", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(buf))
	if err != nil {
		return errf("cannot build the request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Worker-Token", c.token)
	req.Header.Set("User-Agent", bridgeVersion)

	resp, err := c.http.Do(req)
	if err != nil {
		// The URL is not quoted: it is configuration, and a transport error that echoes it into a
		// log adds nothing a reader does not already know.
		return errf("cannot reach the hosted deployment: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &apiError{status: resp.StatusCode, body: truncateBody(body)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return errf("the hosted deployment sent a response this bridge could not decode")
	}
	return nil
}

// get issues a read-only request. Only `Status` uses it: everything else in this protocol changes
// state and is a POST.
func (c *apiClient) get(ctx context.Context, path string, out any, headers ...map[string]string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return errf("cannot build the request: %v", err)
	}
	req.Header.Set("X-Worker-Token", c.token)
	req.Header.Set("User-Agent", bridgeVersion)
	for _, set := range headers {
		for k, v := range set {
			req.Header.Set(k, v)
		}
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return errf("cannot reach the hosted deployment: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &apiError{status: resp.StatusCode, body: truncateBody(body)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return errf("the hosted deployment sent a response this bridge could not decode")
	}
	return nil
}

// Status proves the whole hosted path works — the URL including any reverse-proxy prefix, TLS, and
// the worker credential — WITHOUT claiming a job. It is what makes `-check` an honest name.
//
// It is a read: it takes no lease, changes no state, and cannot cause a Hermes invocation. That is
// the same split the rest of this system draws between a run route and its status route.
func (c *apiClient) Status(ctx context.Context) (workerStatus, error) {
	var out workerStatus
	if err := c.get(ctx, "/_internal/agency/status", &out); err != nil {
		return workerStatus{}, err
	}
	// EVERY FIELD IS REQUIRED, AND A MISSING ONE IS A FAILURE.
	//
	// The earlier version treated an absent schema version as "fine, carry on" — so a response from
	// something that was not this API at all (a login page, a proxy error rendered as JSON, a
	// different service on the same host) could satisfy the preflight by simply not mentioning the
	// fields it was being checked on. A preflight that passes on silence is not a preflight; it is
	// a way of finding out at claim time instead.
	if !out.OK {
		return out, errf("the hosted deployment did not report the research agency lane as available")
	}
	if out.JobSchemaVersion == "" || out.ArtifactSchemaVersion == "" {
		return out, errf("the hosted deployment did not state its job and artifact schema " +
			"versions; this is not an agency worker endpoint this bridge can use")
	}
	if out.JobSchemaVersion != jobSchemaVersion {
		return out, errf("the hosted deployment issues %q jobs; this bridge understands %q",
			out.JobSchemaVersion, jobSchemaVersion)
	}
	if out.ArtifactSchemaVersion != artifactSchemaVersion {
		return out, errf("the hosted deployment expects %q artifacts; this bridge produces %q",
			out.ArtifactSchemaVersion, artifactSchemaVersion)
	}
	if len(out.Workflows) == 0 {
		return out, errf("the hosted deployment offered no workflows")
	}
	if !containsString(out.Workflows, workflowCompanyResearch) {
		return out, errf("the hosted deployment does not offer %q", workflowCompanyResearch)
	}
	// The review pair is checked ONLY when the deployment says it runs reviews. A server that does
	// not offer the workflow is not required to state its schemas, and demanding them would make
	// this bridge refuse to talk to a perfectly good older deployment — the fail-closed rule is
	// about not guessing, not about rejecting what does not exist.
	if out.offersReview() {
		if out.ReviewJobSchemaVersion != reviewJobSchemaVersion {
			return out, errf("the hosted deployment issues %q review jobs; this bridge understands %q",
				out.ReviewJobSchemaVersion, reviewJobSchemaVersion)
		}
		if out.ReviewArtifactSchemaVersion != reviewArtifactSchemaVersion {
			return out, errf("the hosted deployment expects %q reviews; this bridge produces %q",
				out.ReviewArtifactSchemaVersion, reviewArtifactSchemaVersion)
		}
	}
	if out.MaxLeaseSeconds == nil || *out.MaxLeaseSeconds <= 0 {
		return out, errf("the hosted deployment did not state a usable lease ceiling")
	}
	if out.QueuedRuns == nil {
		return out, errf("the hosted deployment did not state how many runs are queued")
	}
	if *out.QueuedRuns < 0 {
		return out, errf("the hosted deployment reported a negative queue depth")
	}
	return out, nil
}

// Claim asks for one job. `ok == false` with no error means the queue is empty, which is the
// ordinary answer and not a failure.
//
// The claim DECLARES this worker's allowlist. The server refuses to hand back anything not on it,
// and `Job.validate` refuses again on receipt — two checks on the same fact, on both sides of a
// trust boundary, because this is the boundary that decides what runs on the owner's machine.
func (c *apiClient) Claim(ctx context.Context, cfg Config) (*claimedJob, bool, error) {
	var res claimResponse
	err := c.post(ctx, "/_internal/agency/claim", map[string]any{
		"workerId": cfg.WorkerID,
		// BOTH workflows this bridge implements, and nothing else. The server hands back only what
		// was declared here, and the envelope that comes back is validated again below.
		"workflows":    workerWorkflows(),
		"leaseSeconds": cfg.LeaseSeconds,
	}, &res)
	if err != nil {
		return nil, false, err
	}
	if !res.Claimed {
		return nil, false, nil
	}

	// WHICH KIND CAME BACK IS DECIDED BY THE WORKFLOW NAME, not by which fields happen to be
	// populated. Sniffing for a `snapshotId` would mean a server could steer this bridge into the
	// review path by adding a field to a research job.
	switch res.Job.workflow() {
	case workflowCompanyResearch:
		job, err := res.Job.asResearch()
		if err != nil {
			return nil, false, err
		}
		return &claimedJob{Research: job}, true, nil
	case workflowExperimentReview:
		job, err := res.Job.asReview()
		if err != nil {
			return nil, false, err
		}
		return &claimedJob{Review: job}, true, nil
	default:
		return nil, false, errf("the server dispatched workflow %q, which is not on this bridge's "+
			"allowlist (%s)", res.Job.workflow(), strings.Join(workerWorkflows(), ", "))
	}
}

// workerWorkflows is this bridge's own allowlist, declared on every claim.
func workerWorkflows() []string {
	return []string{workflowCompanyResearch, workflowExperimentReview}
}

// Heartbeat extends the lease and reports which stage is running. A failed heartbeat is fatal to
// the run in progress: if we cannot prove we still hold the lease, continuing would mean spending
// the owner's machine on work that can no longer be delivered.
func (c *apiClient) Heartbeat(ctx context.Context, ref jobRef, stage string, cfg Config) error {
	return c.post(ctx, "/_internal/agency/runs/"+ref.RunID+"/heartbeat", map[string]any{
		"userId":       ref.UserID,
		"leaseToken":   ref.LeaseToken,
		"stage":        stage,
		"leaseSeconds": cfg.LeaseSeconds,
	}, nil)
}

// Snapshot reads the evidence this review is about.
//
// IT IS A READ, and it is the only fetch in this program. The path is built from a CONSTANT and the
// run id — never from a value the server supplied — and the lease rides in the query string because
// this is a GET. A lapsed lease reads nothing: the server checks it holds before answering.
func (c *apiClient) Snapshot(ctx context.Context, job *ReviewJob) (*ExperimentSnapshot, error) {
	var res struct {
		Snapshot *ExperimentSnapshot `json:"snapshot"`
	}
	// THE LEASE RIDES IN HEADERS, NOT IN THE QUERY STRING. A lease token is a bearer credential for
	// one run, and a query string is the part of a request that every reverse proxy, load balancer
	// and access log writes to disk by default. `X-Worker-Token` travels as a header for exactly
	// that reason; so does this.
	headers := map[string]string{
		agencyUserHeader:  job.UserID,
		agencyLeaseHeader: job.LeaseToken,
	}
	path := "/_internal/agency/runs/" + job.RunID + "/snapshot"
	if err := c.get(ctx, path, &res, headers); err != nil {
		return nil, err
	}
	if err := res.Snapshot.validate(job); err != nil {
		return nil, err
	}
	return res.Snapshot, nil
}

// CompleteReview uploads the validated review. Same 409 semantics as `Complete`: a lost lease means
// another attempt owns the run and our result is discarded rather than retried.
func (c *apiClient) CompleteReview(ctx context.Context, job *ReviewJob, review *ExperimentReviewArtifact) error {
	return c.post(ctx, "/_internal/agency/runs/"+job.RunID+"/complete-review", map[string]any{
		"userId":     job.UserID,
		"leaseToken": job.LeaseToken,
		"review":     review,
	}, nil)
}

// Complete uploads the validated artifact. A 409 means the lease is no longer ours — the run was
// taken over or cancelled — and the correct response is to DISCARD our result rather than retry.
// services/llm/app/automation.py handles the identical status the identical way.
func (c *apiClient) Complete(ctx context.Context, job *Job, artifact *Artifact) error {
	return c.post(ctx, "/_internal/agency/runs/"+job.RunID+"/complete", map[string]any{
		"userId":     job.UserID,
		"leaseToken": job.LeaseToken,
		"artifact":   artifact,
	}, nil)
}

// Fail reports why a run did not produce an artifact. The reason is redacted twice — once by `errf`
// where it was constructed and once here — because this is the string that ends up on a record the
// owner reads in a browser.
func (c *apiClient) Fail(ctx context.Context, ref jobRef, reason string, retryable bool) error {
	return c.post(ctx, "/_internal/agency/runs/"+ref.RunID+"/fail", map[string]any{
		"userId":     ref.UserID,
		"leaseToken": ref.LeaseToken,
		"error":      redact(reason),
		"retryable":  retryable,
	}, nil)
}

// isStaleLease reports whether an error is the server saying somebody else owns this run now.
func isStaleLease(err error) bool {
	var ae *apiError
	if !asAPIError(err, &ae) {
		return false
	}
	return ae.status == http.StatusConflict
}

func asAPIError(err error, target **apiError) bool {
	if ae, ok := err.(*apiError); ok {
		*target = ae
		return true
	}
	return false
}

// truncateBody caps and redacts an upstream body before it can be quoted into an error.
func truncateBody(b []byte) string {
	const cap = 300
	s := redact(string(b))
	if len(s) > cap {
		return s[:cap] + "…"
	}
	return s
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// withTimeout is a small helper so every hosted call is bounded even when the caller's context is
// the whole-run one.
func withTimeout(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, httpTimeout)
}
