package main

// agency.go — the browser-facing surface of the Hermes research agency lane.
//
// It is a THIN PROXY and nothing else. The journal owns the runs, the state machine, the lease and
// the artifact validation (journal/agency*.go); this file forwards four owner routes to it with the
// browser's session cookie attached, exactly as the thesis routes already do. There is no cache, no
// upstream fan-out and no logic here, deliberately — a second implementation of the state machine
// in a second language is how two services come to disagree about what "running" means.
//
// WHAT IS DELIBERATELY NOT PROXIED, AND WHY.
//
// The journal's `/_internal/agency/*` routes — claim, heartbeat, complete, fail — are NOT reachable
// through this gateway under any prefix, and must never be. They mutate lease state, they carry the
// worker credential, and putting them behind a browser route would make a page load able to claim a
// research job. `gateway/automation.go` states the same rule for the same reason ("The lease and
// complete routes are NOT proxied… putting them behind a browser route would make a page load able
// to start a background job"). `agency_test.go` asserts the mux answers 404 for them.
//
// THE POST HERE CANNOT CAUSE A MODEL CALL ON THIS MACHINE. It writes a queued row and returns. The
// Hermes invocation happens later, on the owner's own computer, only because a local worker chose
// to claim the row. That is a stronger property than invariant #4 asks for: the hosted deployment
// has no path to the model at all in this lane.
//
// Stdlib only (invariant #5): `proxyJournal` from research.go, `writeJSON` from handlers.go. This
// file adds no import the gateway did not already have.

import (
	"context"
	"io"
	"net/http"
	"time"
)

func init() {
	registerEventRoute(func(s *Server, mux *http.ServeMux) {
		mux.HandleFunc("POST /api/agency/runs", s.handleAgencyRuns)
		mux.HandleFunc("GET /api/agency/runs", s.handleAgencyRuns)
		mux.HandleFunc("GET /api/agency/runs/{id}", s.handleAgencyRun)
		mux.HandleFunc("POST /api/agency/runs/{id}/cancel", s.handleAgencyCancel)

		// The experiment-review lane. Same thin-proxy rule: the journal owns the snapshot
		// assembly, the immutability, the derived verdicts and the review validation
		// (journal/experiment_*.go), and this file forwards with the browser's cookie attached.
		//
		// `POST /api/experiments/snapshots` is the one route here that causes an upstream READ —
		// the journal reads the paper service over GET and stores what it found. It cannot mutate
		// the experiment: the journal has no method that could (journal/paper_client.go), so
		// neither does anything reachable through this proxy.
		mux.HandleFunc("POST /api/experiments/snapshots", s.handleExperimentSnapshots)
		mux.HandleFunc("GET /api/experiments/snapshots", s.handleExperimentSnapshots)
		mux.HandleFunc("GET /api/experiments/snapshots/{id}", s.handleExperimentSnapshot)
		mux.HandleFunc("POST /api/agency/reviews", s.handleAgencyReviews)
	})
}

// handleAgencyRuns serves the collection: POST enqueues, GET lists. Both are session-scoped by the
// journal, which resolves the caller from the forwarded cookie and checks its own owner allowlist —
// the gateway does not duplicate that check, because two allowlists drift and the one that matters
// is the one next to the data.
func (s *Server) handleAgencyRuns(w http.ResponseWriter, r *http.Request) {
	// A guest is refused here as well as at the journal. Not redundant: the gateway can answer
	// without a network hop, and the browser gets the same 401 shape every other lane returns.
	if s.userIDFrom(r) == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "sign in required"})
		return
	}
	s.proxyJournal(w, r, r.Method, "/agency/runs"+agencyQuery(r))
}

// handleAgencyRun serves one run's status and, once there is one, its artifact.
//
// IT STARTS NOTHING. This is the route the browser polls, and it is safe to poll for the same
// reason `GET /api/analyst/runs/{id}` is: it reaches a stored row, it cannot claim, resume, retry
// or extend a run, and no Hermes profile can be invoked by reading it.
func (s *Server) handleAgencyRun(w http.ResponseWriter, r *http.Request) {
	if s.userIDFrom(r) == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "sign in required"})
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing run id"})
		return
	}
	s.proxyJournal(w, r, http.MethodGet, "/agency/runs/"+id)
}

func (s *Server) handleAgencyCancel(w http.ResponseWriter, r *http.Request) {
	if s.userIDFrom(r) == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "sign in required"})
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing run id"})
		return
	}
	s.proxyJournal(w, r, http.MethodPost, "/agency/runs/"+id+"/cancel")
}

// agencyQuery forwards ONLY the one query parameter this lane understands.
//
// Forwarding `r.URL.RawQuery` wholesale would let a caller append arbitrary parameters to an
// internal request, which is a small hole today and an injection surface the moment the journal
// grows a parameter the gateway does not know about. An allowlist of one is the right size for a
// surface with one parameter.
func agencyQuery(r *http.Request) string {
	if v := r.URL.Query().Get("limit"); v != "" {
		return "?limit=" + urlQueryEscape(v)
	}
	return ""
}

// urlQueryEscape is a tiny local escaper for the single numeric parameter above. It keeps digits and
// rejects everything else by dropping it, so a non-numeric `limit` becomes an absent one and the
// journal applies its own default rather than parsing whatever arrived.
func urlQueryEscape(v string) string {
	out := make([]byte, 0, len(v))
	for i := 0; i < len(v) && i < 4; i++ {
		if v[i] >= '0' && v[i] <= '9' {
			out = append(out, v[i])
		}
	}
	return string(out)
}

// handleExperimentSnapshots serves the snapshot collection: POST assembles and stores one, GET
// lists them. Both are session-scoped by the journal, which resolves the caller from the forwarded
// cookie and checks its own owner allowlist.
func (s *Server) handleExperimentSnapshots(w http.ResponseWriter, r *http.Request) {
	if s.userIDFrom(r) == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "sign in required"})
		return
	}
	// A LONGER PROXY BUDGET FOR THE ASSEMBLY, and only for it.
	//
	// `proxyJournal`'s 15-second ceiling is right for the thesis and agency pass-throughs it was
	// built for, all of which are store reads. Freezing evidence is not: the journal reads five live
	// paper payloads sequentially and bounds itself at 150s (journal/experiment_routes.go). Proxied
	// through the shorter budget, every assembly that touched a busy readiness check would have died
	// at 15 seconds with a 502 — while the journal went on to store the snapshot the browser was
	// just told it did not get.
	//
	// The GET stays on the default budget: it is a store read like every other one here.
	if r.Method == http.MethodPost {
		s.proxySnapshotAssembly(w, r)
		return
	}
	s.proxyJournal(w, r, r.Method, "/experiments/snapshots"+agencyQuery(r))
}

// handleExperimentSnapshot serves one stored snapshot.
//
// IT STARTS NOTHING and it reaches no upstream beyond the journal's own store — it cannot assemble,
// re-assemble or refresh a snapshot, which is what makes it safe for a browser to read repeatedly.
// A snapshot is immutable, so re-reading one can never return different evidence.
func (s *Server) handleExperimentSnapshot(w http.ResponseWriter, r *http.Request) {
	if s.userIDFrom(r) == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "sign in required"})
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing snapshot id"})
		return
	}
	s.proxyJournal(w, r, http.MethodGet, "/experiments/snapshots/"+id)
}

// handleAgencyReviews enqueues an experiment review against a stored snapshot.
//
// LIKE `POST /api/agency/runs`, THIS CANNOT CAUSE A MODEL CALL ON THIS MACHINE. It writes a queued
// row and returns. The Hermes invocation happens later, on the owner's own computer, only because a
// local worker chose to claim the row.
func (s *Server) handleAgencyReviews(w http.ResponseWriter, r *http.Request) {
	if s.userIDFrom(r) == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "sign in required"})
		return
	}
	s.proxyJournal(w, r, http.MethodPost, "/agency/reviews")
}

// experimentSnapshotProxyTimeout must exceed the journal's own `snapshotAssembleTimeout` (190s) with
// margin for the store write and the response. It is one link in a four-layer chain — sources <
// journal < gateway < nginx — and `journal/experiment_review_test.go::TestTheSnapshotBudgetChainNests`
// asserts the whole chain, including this constant, read from this file.
const experimentSnapshotProxyTimeout = 215 * time.Second

// snapshotAssemblyClient is the DEDICATED client for the one route that can legitimately take
// minutes.
//
// IT EXISTS BECAUSE `Server.http` CANNOT BE USED HERE. That client carries a 130-second
// whole-request `Timeout` (main.go), and a `Timeout` on an `http.Client` is a hard ceiling on the
// entire request — it wins over any longer context. Proxying the assembly through it would have cut
// a 190-second journal budget off at 130 and returned a 502 while the journal went on to store the
// snapshot the browser was just told it did not get. That is worse than a plain timeout: the
// deployment and the user disagree about whether the evidence exists.
//
// It sets no `Timeout` of its own, so the per-request context is the only bound and the budget
// lives in exactly one place.
var snapshotAssemblyClient = &http.Client{}

// proxySnapshotAssembly forwards the assembly POST on its own client and its own budget.
//
// It is deliberately a separate function rather than a parameter on `proxyJournalWithin`: every
// other caller of that helper is a store read that SHOULD be bounded by the shared client, and a
// flag would make it possible to opt any of them out by accident.
func (s *Server) proxySnapshotAssembly(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), experimentSnapshotProxyTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.cfg.JournalURL+"/experiments/snapshots", r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": "bad journal request: " + err.Error(),
		})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if cookie := r.Header.Get("Cookie"); cookie != "" {
		req.Header.Set("Cookie", cookie)
	}

	resp, err := snapshotAssemblyClient.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": "the journal could not be reached to freeze the experiment evidence",
		})
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	// The journal's own status and `code` are copied straight back — `mixed_generation` is a 409 the
	// browser must be able to act on, and collapsing it into a generic gateway error would leave the
	// user with a shrug instead of a retry.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}
