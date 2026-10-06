package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// paper_client.go — the journal's READ-ONLY window onto the live paper experiment.
//
// ─────────────────────────────────────────────────────────────────────────────────────────────
// THERE IS NO WRITE METHOD HERE, AND THERE MUST NEVER BE ONE
// ─────────────────────────────────────────────────────────────────────────────────────────────
// The paper service has exactly two mutating routes — `POST /paper/reset`, which deletes every
// paper trade in the journal and establishes a new experiment generation, and `POST /paper/config`,
// which rewrites what the engine trades. Both are session-gated on that service (paper/handlers.go).
//
// This client is the ONLY path from the review lane to the paper service, and it can issue nothing
// but `GET`. `fetch` hard-codes `http.MethodGet`; there is no parameter for a method, no body
// argument, and no exported helper that takes a path plus a verb. A compromised worker that talked
// its way past every other check still has no function to call: resetting the experiment clock and
// writing to the paper ledger are not refused here, they are *unexpressible*.
//
// That is the same argument journal/agency.go makes about the artifact having no field for a
// signal, applied to the other direction of the boundary. `experiment_review_test.go` asserts it
// against the source text.
//
// ─────────────────────────────────────────────────────────────────────────────────────────────
// AN UNREACHABLE SOURCE IS AN ANSWER, NOT AN ERROR TO PAPER OVER
// ─────────────────────────────────────────────────────────────────────────────────────────────
// `fetch` returns the decoded body OR a reason, and the caller records the reason as a source
// state (experiment_snapshot.go). It never falls back to a cached copy, a file on disk or a
// previous snapshot: substituting stale local state for live state is precisely the failure the
// snapshot exists to make impossible, so there is no code path in this file that could.

// paperSourceEndpoints are the five live payloads a snapshot is composed from, in a fixed order so
// two assemblies of an unchanged deployment produce identical bytes.
//
// `days=30` bounds the dashboard's equity series. The default is 252 points plus a 100-decision
// tail, which is a page, not a record: a durable snapshot should carry the experiment's state, not
// a year of chart data.
//
// EACH SOURCE CARRIES ITS OWN TIMEOUT, AND WHICH BUDGET IT GETS FOLLOWS FROM WHAT IT ACTUALLY DOES.
//
// TWO of the five reach live upstreams, not the store, and both cap themselves at 45 seconds on the
// paper side:
//
//   - `/paper/readiness` re-evaluates every gate against prediction and analysis, per config;
//   - `/paper/provenance` calls `/predict` once PER CONFIG, sequentially (paper/provenance.go).
//     It was briefly given the fast budget on the assumption that it was a store read. It is not —
//     with three configs it is three sequential prediction calls, and a 15-second ceiling would
//     have recorded a healthy deployment's model provenance as `unavailable` under any real load.
//     That is the same false negative the dedicated client below exists to prevent, arrived at from
//     a different direction.
//
// The other three are store reads that return in milliseconds.
//
// Worst case is 2×50 + 3×15 = 145s. `snapshotAssembleTimeout` must exceed it, the gateway's
// `experimentSnapshotProxyTimeout` must exceed that, and nginx's location-specific
// `proxy_read_timeout` must exceed that. Each layer strictly contains the one inside it, and
// `TestTheSnapshotBudgetChainNests` asserts the whole chain rather than one link of it.
var paperSourceEndpoints = []struct {
	name    string
	path    string
	timeout time.Duration
}{
	{snapshotSourceReadiness, "/paper/readiness", paperUpstreamTimeout},
	{snapshotSourceDashboard, "/paper/dashboard?days=30", paperReadTimeout},
	{snapshotSourceExperiments, "/paper/experiments", paperReadTimeout},
	{snapshotSourceStatus, "/paper/status", paperReadTimeout},
	{snapshotSourceProvenance, "/paper/provenance", paperUpstreamTimeout},
}

const (
	// paperUpstreamTimeout covers the two sources that fan out to LIVE upstreams and cap themselves
	// at 45s on the paper side. The extra 5s is transport and encoding, not slack.
	paperUpstreamTimeout = 50 * time.Second
	// paperReadTimeout covers the three genuine store reads.
	paperReadTimeout = 15 * time.Second
)

// worstCaseSourceBudget is the sum of every source's own timeout — the longest an assembly can
// legitimately take before anything above it may give up. Computed rather than written down, so the
// chain assertion cannot drift from the table above.
func worstCaseSourceBudget() time.Duration {
	var total time.Duration
	for _, ep := range paperSourceEndpoints {
		total += ep.timeout
	}
	return total
}

// newPaperClient builds the DEDICATED client the snapshot lane uses.
//
// IT IS NOT `Server.http`, AND THAT IS A FIX RATHER THAN A PREFERENCE. The shared journal client is
// `&http.Client{Timeout: 15 * time.Second}` (main.go) — a sensible bound for the quote and dashboard
// reads it was built for, and a wrong one here. A `Timeout` on the client is a hard ceiling on the
// WHOLE request, so a 50-second context on a readiness read would have been silently clamped to 15
// and a healthy-but-busy deployment would have been recorded `unavailable` — manufacturing exactly
// the false negative this lane exists to avoid.
//
// This client sets no `Timeout` of its own: the per-source context is the only bound, which keeps
// the budget in one place instead of two that can disagree.
//
// REDIRECTS ARE REFUSED, for the reason bridge/client.go gives about its own: a redirect this
// service followed would let a compromised or misconfigured paper deployment point the snapshot at
// a host of its choosing, and the stored evidence would name a service nobody audited.
func newPaperClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return fmt.Errorf("the paper service attempted a redirect; refusing to read experiment " +
				"evidence from another host")
		},
	}
}

// maxPaperSourceBytes caps one source payload. A source that overruns is recorded `unavailable`
// with that reason rather than truncated — half a payload is not evidence, and a truncated JSON
// document would fail to decode anyway.
const maxPaperSourceBytes = 512 << 10

// fetchPaperSource issues ONE read-only GET and returns the raw JSON body.
//
// The body is returned as `json.RawMessage` rather than a decoded map because the snapshot stores
// what the service actually said. Re-encoding a decoded map would silently reorder and renumber it,
// and a snapshot whose bytes are not the bytes that were served is not a snapshot.
func (s *Server) fetchPaperSource(ctx context.Context, path string, timeout time.Duration) (json.RawMessage, error) {
	if s.cfg.PaperURL == "" {
		return nil, fmt.Errorf("no PAPER_URL is configured for this deployment")
	}
	client := s.paperHTTP
	if client == nil {
		// A Server built without one (a test that constructs the struct directly) gets the same
		// client production uses rather than falling back to the shared 15-second one, which would
		// make the fallback quietly stricter than the real path.
		client = newPaperClient()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// GET, hard-coded. See the header.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.PaperURL+path, nil)
	if err != nil {
		return nil, fmt.Errorf("could not build the request for %s", path)
	}
	resp, err := client.Do(req)
	if err != nil {
		// The URL is configuration; quoting it into an error a browser reads adds nothing.
		return nil, fmt.Errorf("the paper service could not be reached for %s: %w", path,
			shortErr(err))
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxPaperSourceBytes+1))
	if readErr != nil {
		return nil, fmt.Errorf("the response for %s could not be read", path)
	}
	if len(body) > maxPaperSourceBytes {
		return nil, fmt.Errorf("the response for %s exceeds %d bytes; a truncated payload is not "+
			"evidence", path, maxPaperSourceBytes)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("the paper service answered %d for %s", resp.StatusCode, path)
	}
	if !json.Valid(body) {
		return nil, fmt.Errorf("the response for %s is not valid JSON", path)
	}
	return json.RawMessage(body), nil
}

// shortErr caps a transport error so a long dial message cannot dominate a stored source reason.
// The result still goes through `redactAgencyText` before it is stored.
func shortErr(err error) error {
	msg := err.Error()
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	return fmt.Errorf("%s", msg)
}
