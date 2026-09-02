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

import "net/http"

func init() {
	registerEventRoute(func(s *Server, mux *http.ServeMux) {
		mux.HandleFunc("POST /api/agency/runs", s.handleAgencyRuns)
		mux.HandleFunc("GET /api/agency/runs", s.handleAgencyRuns)
		mux.HandleFunc("GET /api/agency/runs/{id}", s.handleAgencyRun)
		mux.HandleFunc("POST /api/agency/runs/{id}/cancel", s.handleAgencyCancel)
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
