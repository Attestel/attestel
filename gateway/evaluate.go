package main

// evaluate.go — the authenticated trigger for the offline edge-evaluation harness.
//
// Production is the single-container supervisord deploy: the operator has no shell and no docker
// access, so `python -m app.evaluate` could not be run there at all and a verdict could not be
// produced in production. Retraining was already reachable (POST /api/train/{ticker}); this closes
// the other half, so the whole of docs/VALIDATION_AND_GO_LIVE.md §2 can be worked from the app.
//
// Three properties, all deliberate, all asserted by evaluate_test.go:
//
//  1. **Fail-closed auth, the FEEDBACK_ADMIN_UIDS pattern.** `status` needs a signed-in session;
//     `run` additionally needs membership of EVAL_ADMIN_UIDS, which is EMPTY BY DEFAULT — so out of
//     the box nobody may trigger a run. Starting a minutes-long, CPU-heavy job that degrades
//     /predict latency for everyone must never be an anonymous (or any-user) action on a public box.
//     The allow-list holds opaque auth USER IDS (`GET /auth/me` -> `user.id`), never emails.
//
//  2. **Verbatim pass-through, no gateway opinion.** The upstream's status and body are copied
//     back unchanged, including its 409 (a run is already in flight) and its 400 (parameters were
//     supplied). In particular the caller's query string and body are FORWARDED rather than
//     stripped, so the prediction service's no-parameters rule is enforced in exactly one place and
//     a caller who sends parameters is told they were refused instead of silently ignored.
//
//  3. **Never cached.** A cached run trigger is a lost run and a cached status is a lie about a job
//     in flight.
//
// Nothing here executes an order, reaches a broker or moves money, and nothing here can influence
// what the evaluator concludes — it starts the harness and reads what the harness recorded.

import (
	"context"
	"io"
	"net/http"
	"time"
)

// registerEvaluateRoutes mounts the surface. One line in main.go, like the other lanes.
func (s *Server) registerEvaluateRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/evaluate/run", s.handleEvaluateRun)
	mux.HandleFunc("GET /api/evaluate/status", s.handleEvaluateStatus)
	mux.HandleFunc("POST /api/evaluate-events/run", s.handleEvaluateEventsRun)
	mux.HandleFunc("GET /api/evaluate-events/status", s.handleEvaluateEventsStatus)
	mux.HandleFunc("POST /api/estimate-snapshots/run", s.handleEstimateSnapshotsRun)
	mux.HandleFunc("GET /api/estimate-snapshots/status", s.handleEstimateSnapshotsStatus)
}

// handleEstimateSnapshotsRun makes the bounded forward collector reachable on the shell-less
// production deployment. It shares the fail-closed evaluator-admin boundary because every run
// spends provider calls and appends experiment evidence. It takes no request parameters upstream.
func (s *Server) handleEstimateSnapshotsRun(w http.ResponseWriter, r *http.Request) {
	uid := s.userIDFrom(r)
	if uid == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "sign in required"})
		return
	}
	if !s.isEvalAdmin(uid) {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error": "evaluation admin access required — this deployment's EVAL_ADMIN_UIDS " +
				"allow-list does not include your user id",
		})
		return
	}
	s.proxyPrediction(w, r, http.MethodPost, withQuery("/estimate-snapshots/run", r), 20*time.Second)
}

func (s *Server) handleEstimateSnapshotsStatus(w http.ResponseWriter, r *http.Request) {
	if s.userIDFrom(r) == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "sign in required"})
		return
	}
	s.proxyPrediction(w, r, http.MethodGet, "/estimate-snapshots/status", 15*time.Second)
}

// handleEvaluateEventsRun uses the same operator allow-list but a separate upstream harness. PEAD
// reports are research evidence only; this route cannot create a price-model verdict or open paper
// positions.
func (s *Server) handleEvaluateEventsRun(w http.ResponseWriter, r *http.Request) {
	uid := s.userIDFrom(r)
	if uid == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "sign in required"})
		return
	}
	if !s.isEvalAdmin(uid) {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error": "evaluation admin access required — this deployment's EVAL_ADMIN_UIDS " +
				"allow-list does not include your user id",
		})
		return
	}
	s.proxyPrediction(w, r, http.MethodPost, withQuery("/evaluate-events/run", r), 20*time.Second)
}

func (s *Server) handleEvaluateEventsStatus(w http.ResponseWriter, r *http.Request) {
	if s.userIDFrom(r) == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "sign in required"})
		return
	}
	s.proxyPrediction(w, r, http.MethodGet, "/evaluate-events/status", 15*time.Second)
}

// isEvalAdmin reports whether uid is on the EVAL_ADMIN_UIDS allow-list. An empty allow-list means
// nobody may trigger a run, and a guest ("") is never an admin — fail closed by construction.
func (s *Server) isEvalAdmin(uid string) bool {
	if uid == "" {
		return false
	}
	for _, a := range s.cfg.EvalAdminUIDs {
		if a == uid {
			return true
		}
	}
	return false
}

// handleEvaluateRun starts ONE evaluation upstream. The upstream answers 202 as soon as the
// subprocess exists, so a short timeout is right: a slow answer here means the prediction service is
// in trouble, not that the harness is thinking.
func (s *Server) handleEvaluateRun(w http.ResponseWriter, r *http.Request) {
	uid := s.userIDFrom(r)
	if uid == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "sign in required"})
		return
	}
	if !s.isEvalAdmin(uid) {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error": "evaluation admin access required — this deployment's EVAL_ADMIN_UIDS " +
				"allow-list does not include your user id",
		})
		return
	}
	s.proxyPrediction(w, r, http.MethodPost, withQuery("/evaluate/run", r), 20*time.Second)
}

// handleEvaluateStatus is a normal quick GET. Signed-in only: what this deployment has evaluated,
// which verdicts it holds and what its last run printed is operational detail, not public content.
func (s *Server) handleEvaluateStatus(w http.ResponseWriter, r *http.Request) {
	if s.userIDFrom(r) == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "sign in required"})
		return
	}
	s.proxyPrediction(w, r, http.MethodGet, "/evaluate/status", 15*time.Second)
}

// proxyPrediction forwards one request to the prediction service and copies its status and body back
// verbatim. Unlike getJSON it does NOT collapse upstream statuses into a gateway error: the operator
// needs the upstream's own 409 (already running) and 400 (parameters refused, with the rule named)
// to know what to do next. The session is NOT forwarded — the prediction service is internal and has
// no auth of its own; the gateway is the boundary, and this file is where it is enforced.
func (s *Server) proxyPrediction(w http.ResponseWriter, r *http.Request, method, path string, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	var body io.Reader
	if r.Body != nil {
		body = r.Body
	}
	req, err := http.NewRequestWithContext(ctx, method, s.cfg.PredictionURL+path, body)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "bad prediction request: " + err.Error()})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	// The prediction service is not browser-exposed and trusts this gateway-stamped identity for
	// model promotion audit records. A caller-supplied header is never forwarded.
	if uid := s.userIDFrom(r); uid != "" {
		req.Header.Set("X-Operator-Uid", uid)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "prediction unreachable: " + err.Error()})
		return
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	w.Header().Set("Content-Type", "application/json")
	// Never cached: a cached trigger is a lost run, a cached status is a lie about a job in flight.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(out)
}
