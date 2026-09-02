package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
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

// hostedClient is the seam the tests drive. `*apiClient` is the only production implementation;
// the interface exists so `run()` and `drainQueue()` can be exercised against a fake that returns
// an auth failure, a transport failure or a stale lease on demand. Those are precisely the paths
// that must produce a non-zero exit, and they are unreachable from a test that needs a real server.
type hostedClient interface {
	Status(ctx context.Context) (workerStatus, error)
	Claim(ctx context.Context, cfg Config) (*Job, bool, error)
	Heartbeat(ctx context.Context, job *Job, stage string, cfg Config) error
	Complete(ctx context.Context, job *Job, artifact *Artifact) error
	Fail(ctx context.Context, job *Job, reason string, retryable bool) error
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
func (c *apiClient) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return errf("cannot build the request: %v", err)
	}
	req.Header.Set("X-Worker-Token", c.token)
	req.Header.Set("User-Agent", bridgeVersion)

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
func (c *apiClient) Claim(ctx context.Context, cfg Config) (*Job, bool, error) {
	var res claimResponse
	err := c.post(ctx, "/_internal/agency/claim", map[string]any{
		"workerId":     cfg.WorkerID,
		"workflows":    []string{workflowCompanyResearch},
		"leaseSeconds": cfg.LeaseSeconds,
	}, &res)
	if err != nil {
		return nil, false, err
	}
	if !res.Claimed {
		return nil, false, nil
	}
	if err := res.Job.validate(); err != nil {
		return nil, false, err
	}
	return res.Job, true, nil
}

// Heartbeat extends the lease and reports which stage is running. A failed heartbeat is fatal to
// the run in progress: if we cannot prove we still hold the lease, continuing would mean spending
// the owner's machine on work that can no longer be delivered.
func (c *apiClient) Heartbeat(ctx context.Context, job *Job, stage string, cfg Config) error {
	return c.post(ctx, "/_internal/agency/runs/"+job.RunID+"/heartbeat", map[string]any{
		"userId":       job.UserID,
		"leaseToken":   job.LeaseToken,
		"stage":        stage,
		"leaseSeconds": cfg.LeaseSeconds,
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
func (c *apiClient) Fail(ctx context.Context, job *Job, reason string, retryable bool) error {
	return c.post(ctx, "/_internal/agency/runs/"+job.RunID+"/fail", map[string]any{
		"userId":     job.UserID,
		"leaseToken": job.LeaseToken,
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
