package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// agency_routes.go — the HTTP surface of the research-agency lane.
//
// TWO AUDIENCES, TWO CREDENTIALS, AND THEY DO NOT OVERLAP.
//
//	OWNER  /agency/...            session cookie + the AGENCY_OWNER_UIDS allowlist
//	WORKER /_internal/agency/...  the AGENCY_WORKER_TOKEN header, and a session cookie is a 404
//
// THE WORKER CREDENTIAL IS NOT `AUTH_SECRET`, AND THAT IS THE MOST IMPORTANT LINE IN THIS FILE.
// Every other internal seam in this repository authenticates with `X-Internal-Secret: $AUTH_SECRET`
// (services/events/app/automation.py, journal/internal_theses.go, alerts/routes_extra.go). That is
// defensible between containers on one private compose network. It is NOT defensible here: this
// credential lives on a laptop, and `AUTH_SECRET` is the key that signs session cookies —
// paper/auth.go::systemCookie shows how few lines it takes to mint a session for any uid from it.
// A worker token that could be turned into a session for an arbitrary user is a worker token whose
// compromise is an account compromise. So this lane has its own secret, it grants exactly the five
// worker routes below — status, claim, heartbeat, complete and fail — and it can mint nothing. It
// authorises no browser route, no other service, and no session.
//
// FAIL CLOSED ON MISSING CONFIGURATION. With no AGENCY_WORKER_TOKEN the worker routes answer 403
// NAMING the missing variable — not 200, and not an open route. With an empty AGENCY_OWNER_UIDS
// nobody can create a run. Both are the posture paper/auth.go::requireSession already takes, and
// both mean that an unconfigured deployment has this lane switched OFF rather than switched open.
//
// NO GENERIC REMOTE-COMMAND API LIVES HERE. `POST /agency/runs` accepts a workflow name, a ticker
// and a question. There is no field for a profile, a toolset, a model, a provider, a path, a shell
// command or a system prompt, and `agencyCreateRequest` is decoded with DisallowUnknownFields so
// adding one to the payload is a 400 rather than a silently ignored key.

func init() {
	registerSubscriptionRoute(func(s *Server, mux *http.ServeMux) {
		// Owner surface. Every route requires a session; the allowlist is checked inside.
		mux.HandleFunc("POST /agency/runs", s.requireAuth(s.handleAgencyCreate))
		mux.HandleFunc("GET /agency/runs", s.requireAuth(s.handleAgencyList))
		mux.HandleFunc("GET /agency/runs/{id}", s.requireAuth(s.handleAgencyGet))
		mux.HandleFunc("POST /agency/runs/{id}/cancel", s.requireAuth(s.handleAgencyCancel))

		// Worker surface. Not proxied by the gateway under any prefix.
		//
		// `status` is a READ: it takes no lease, changes nothing, and cannot cause a Hermes
		// invocation. It exists so the bridge's `-check` can prove the URL, the reverse-proxy
		// prefix, TLS and the credential all work WITHOUT claiming a job — a preflight that has to
		// claim work in order to tell you it is configured correctly is not a preflight.
		mux.HandleFunc("GET /_internal/agency/status", s.handleAgencyWorkerStatus)
		mux.HandleFunc("POST /_internal/agency/claim", s.handleAgencyClaim)
		mux.HandleFunc("POST /_internal/agency/runs/{id}/heartbeat", s.handleAgencyHeartbeat)
		mux.HandleFunc("POST /_internal/agency/runs/{id}/complete", s.handleAgencyComplete)
		mux.HandleFunc("POST /_internal/agency/runs/{id}/fail", s.handleAgencyFail)
	})
}

// agencyBodyCap bounds any request body this lane will read. The artifact cap is the largest of
// them; anything past it is refused before it is parsed.
const agencyBodyCap = agencyMaxArtifactBytes + (16 << 10)

// constantTimeStringEqual compares two strings without leaking where they differ. Length is
// compared first because subtle.ConstantTimeCompare requires equal lengths — the length itself is
// not a secret here (both sides are fixed-width hex).
func constantTimeStringEqual(a, b string) bool {
	x, y := []byte(a), []byte(b)
	if len(x) != len(y) {
		return false
	}
	return subtle.ConstantTimeCompare(x, y) == 1
}

// ────────────────────────────────────────────────────────────────────────────────── owner surface

// requireAgencyOwner resolves the caller and confirms they are on the allowlist.
//
// A signed-in non-owner gets 403 with a stated reason rather than 404: they are a real, known user
// of this deployment and telling them the feature is owner-only is honest. What they must NOT be
// able to learn is anything about the owner's runs, and they cannot — every store call below is
// keyed by the CALLER's uid, so a non-owner reading `/agency/runs` would see their own empty list
// even if the allowlist check were removed.
func (s *Server) requireAgencyOwner(w http.ResponseWriter, r *http.Request) (string, bool) {
	uid := userID(r)
	if s.agency == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "the research agency lane is not available on this deployment",
		})
		return "", false
	}
	if !s.agency.isOwner(uid) {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error": "the research agency lane is owner-only on this deployment",
			// The variable is named so an operator can fix it; the VALUE is never echoed.
			"missingConfiguration": "AGENCY_OWNER_UIDS",
		})
		return "", false
	}
	return uid, true
}

func (s *Server) handleAgencyCreate(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.requireAgencyOwner(w, r)
	if !ok {
		return
	}
	var req agencyCreateRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, agencyBodyCap))
	// An unknown field is a 400. A request that tried to name a profile, a model or a command must
	// be REFUSED, not silently stripped: silently stripping teaches a caller the field works.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "invalid request body: " + redactAgencyText(err.Error()),
		})
		return
	}
	ticker, question, err := req.normalise()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	run, created, err := s.agency.Create(uid, ticker, question, time.Now().UTC())
	if err != nil {
		log.Printf("agency: create failed: %s", redactAgencyText(err.Error()))
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "could not enqueue the research run",
		})
		return
	}

	// 202 in both cases. `created:false` says a live run for this exact request already existed and
	// this call attached to it rather than starting a second one.
	body := map[string]any{
		"run":     agencyView(run),
		"created": created,
		"href":    "/agency/runs/" + run.ID,
	}
	writeJSON(w, http.StatusAccepted, body)
}

func (s *Server) handleAgencyList(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.requireAgencyOwner(w, r)
	if !ok {
		return
	}
	limit := 25
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= agencyRunsPerUser {
			limit = n
		}
	}
	runs, err := s.agency.List(uid, limit, time.Now().UTC())
	if err != nil {
		log.Printf("agency: list failed: %s", redactAgencyText(err.Error()))
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "could not read the research runs",
		})
		return
	}
	views := make([]agencyRunView, 0, len(runs))
	for _, run := range runs {
		// The list omits artifacts: a listing of twenty-five 256 KiB documents is a download, not a
		// list. The detail route serves the artifact.
		run.Artifact = nil
		views = append(views, agencyView(run))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"runs":       views,
		"workflow":   agencyWorkflowCompanyResearch,
		"disclaimer": agencyDisclaimer,
	})
}

func (s *Server) handleAgencyGet(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.requireAgencyOwner(w, r)
	if !ok {
		return
	}
	// THIS ROUTE STARTS NOTHING. It is a store read, it reaches no upstream, and it can never cause
	// a Hermes invocation — which is exactly why it is the route the browser is allowed to poll.
	// gateway/analystjobs.go makes the same split for the same reason.
	run, found, err := s.agency.Get(uid, r.PathValue("id"), time.Now().UTC())
	if err != nil {
		log.Printf("agency: read failed: %s", redactAgencyText(err.Error()))
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "could not read the research run",
		})
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "no such research run", "code": "unknown_run",
		})
		return
	}
	writeJSON(w, http.StatusOK, agencyView(run))
}

func (s *Server) handleAgencyCancel(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.requireAgencyOwner(w, r)
	if !ok {
		return
	}
	run, found, err := s.agency.Cancel(uid, r.PathValue("id"), time.Now().UTC())
	if err != nil {
		log.Printf("agency: cancel failed: %s", redactAgencyText(err.Error()))
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "could not cancel the research run",
		})
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "no such research run", "code": "unknown_run",
		})
		return
	}
	writeJSON(w, http.StatusOK, agencyView(run))
}

// ───────────────────────────────────────────────────────────────────────────────── worker surface

// requireAgencyWorker gates all five worker routes — status, claim, heartbeat, complete and fail.
//
// Three refusals, in this order and for these reasons:
//  1. A request carrying a session cookie is a BROWSER, and a browser has no business here whatever
//     its session says. 404 keeps the route's existence undisclosed. This is journal/
//     internal_theses.go's rule, restated.
//  2. No configured token means the lane is off. 403 names the variable so the operator can fix it.
//  3. A wrong token is 401, compared in constant time.
func (s *Server) requireAgencyWorker(w http.ResponseWriter, r *http.Request) bool {
	if _, err := r.Cookie(s.cfg.CookieName); err == nil {
		http.NotFound(w, r)
		return false
	}
	if s.agency == nil || s.cfg.AgencyWorkerToken == "" {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error": "the research agency worker API is disabled: no worker credential is " +
				"configured for this deployment",
			"missingConfiguration": "AGENCY_WORKER_TOKEN",
		})
		return false
	}
	if !constantTimeStringEqual(s.cfg.AgencyWorkerToken, r.Header.Get("X-Worker-Token")) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": "worker authentication required",
		})
		return false
	}
	return true
}

// handleAgencyWorkerStatus answers the bridge's preflight.
//
// WHAT IT DELIBERATELY DOES NOT RETURN. No run ids, no tickers, no questions, no user ids and no
// artifacts — a preflight needs to know that the pipe works, not what is in it. `queuedRuns` is a
// COUNT across the configured owners, which is the one number that makes `-check` useful ("is
// anything waiting for me?") and which discloses nothing about what is waiting.
func (s *Server) handleAgencyWorkerStatus(w http.ResponseWriter, r *http.Request) {
	if !s.requireAgencyWorker(w, r) {
		return
	}
	queued, err := s.agency.QueuedCount(time.Now().UTC())
	if err != nil {
		// FAIL THE REQUEST RATHER THAN ANSWER `ok: true, queuedRuns: 0`.
		//
		// Those two answers look identical to a worker and mean opposite things: one is a healthy
		// idle queue, the other is a queue nobody can read. A preflight that cannot tell them apart
		// would send an operator away satisfied while their runs sat invisible.
		log.Printf("agency: status failed: %s", redactAgencyText(err.Error()))
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok":     false,
			"status": "degraded",
			"error": "the research queue could not be read; the lane is not usable until this is " +
				"resolved",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                    true,
		"workflows":             []string{agencyWorkflowCompanyResearch},
		"jobSchemaVersion":      agencyJobSchemaVersion,
		"artifactSchemaVersion": agencyArtifactSchemaVersion,
		"queuedRuns":            queued,
		// Served so a bridge can warn the operator that its configured lease will be clamped,
		// rather than discovering it as a surprise mid-run.
		"maxLeaseSeconds": int(agencyMaxLeaseDuration / time.Second),
	})
}

type agencyClaimRequest struct {
	// WorkerID is opaque and worker-chosen. It must not be a hostname, a username or a path — the
	// bridge generates a random one once and stores it locally. It is capped and redacted here
	// rather than trusted.
	WorkerID string `json:"workerId"`
	// Workflows is the worker's own allowlist, sent so the server can refuse to hand it work it did
	// not ask for. A worker that only knows company_research_v1 says so, and a future second
	// workflow cannot be dispatched to it by accident.
	Workflows    []string `json:"workflows"`
	LeaseSeconds int      `json:"leaseSeconds"`
}

func (s *Server) handleAgencyClaim(w http.ResponseWriter, r *http.Request) {
	if !s.requireAgencyWorker(w, r) {
		return
	}
	var req agencyClaimRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, agencyBodyCap))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "invalid claim body: " + redactAgencyText(err.Error()),
		})
		return
	}
	// The worker must declare that it can run this workflow. An empty list is a worker that named
	// nothing, and it gets nothing — fail closed.
	if !containsString(req.Workflows, agencyWorkflowCompanyResearch) {
		writeJSON(w, http.StatusOK, map[string]any{
			"claimed": false,
			"reason":  "this worker did not declare support for " + agencyWorkflowCompanyResearch,
		})
		return
	}

	lease := agencyLeaseDuration
	if req.LeaseSeconds > 0 {
		if d := time.Duration(req.LeaseSeconds) * time.Second; d <= agencyMaxLeaseDuration {
			lease = d
		}
	}
	workerID := redactAgencyText(req.WorkerID)
	if len(workerID) > 64 {
		workerID = workerID[:64]
	}

	run, ok, err := s.agency.Claim(workerID, lease, time.Now().UTC())
	if err != nil {
		log.Printf("agency: claim failed: %s", redactAgencyText(err.Error()))
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "could not claim a research run",
		})
		return
	}
	if !ok {
		// An empty queue is a normal answer, not an error.
		writeJSON(w, http.StatusOK, map[string]any{"claimed": false, "reason": "no runs are queued"})
		return
	}

	// The JOB envelope. Note what it carries and what it does not: a workflow NAME, a subject and a
	// cutoff. No prompt, no profile, no toolset, no model, no path, no command.
	writeJSON(w, http.StatusOK, map[string]any{
		"claimed": true,
		"job": map[string]any{
			"schemaVersion":   agencyJobSchemaVersion,
			"runId":           run.ID,
			"userId":          run.UserID,
			"workflowVersion": run.WorkflowVersion,
			"ticker":          run.Ticker,
			"question":        run.Question,
			"asOf":            run.AsOf,
			"attempt":         run.Attempts,
			"maxAttempts":     agencyMaxAttempts,
			"leaseToken":      run.LeaseToken,
			"leaseExpiresAt":  run.LeaseExpiresAt,
		},
	})
}

// agencyWorkerRef is the common addressing block on every post-claim worker call: which user's run,
// and the lease token that proves the caller still holds it.
type agencyWorkerRef struct {
	UserID     string `json:"userId"`
	LeaseToken string `json:"leaseToken"`
}

type agencyHeartbeatRequest struct {
	agencyWorkerRef
	// Stage is a bounded progress label. It is validated against the fixed chain rather than stored
	// as free text, so a worker cannot write arbitrary strings into a record the owner reads.
	Stage        string `json:"stage"`
	LeaseSeconds int    `json:"leaseSeconds"`
}

func (s *Server) handleAgencyHeartbeat(w http.ResponseWriter, r *http.Request) {
	if !s.requireAgencyWorker(w, r) {
		return
	}
	var req agencyHeartbeatRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, agencyBodyCap))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "invalid heartbeat body: " + redactAgencyText(err.Error()),
		})
		return
	}
	stage := strings.TrimSpace(req.Stage)
	if stage != "" && !containsString(agencyProfileChain, stage) {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "stage must be one of the workflow's profiles",
		})
		return
	}
	lease := agencyLeaseDuration
	if req.LeaseSeconds > 0 {
		if d := time.Duration(req.LeaseSeconds) * time.Second; d <= agencyMaxLeaseDuration {
			lease = d
		}
	}
	run, err := s.agency.Heartbeat(req.UserID, r.PathValue("id"), req.LeaseToken, stage, lease,
		time.Now().UTC())
	if err != nil {
		s.writeAgencyWorkerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "runId": run.ID, "status": run.Status, "leaseExpiresAt": run.LeaseExpiresAt,
	})
}

type agencyCompleteRequest struct {
	agencyWorkerRef
	Artifact *AgencyArtifact `json:"artifact"`
}

func (s *Server) handleAgencyComplete(w http.ResponseWriter, r *http.Request) {
	if !s.requireAgencyWorker(w, r) {
		return
	}
	var req agencyCompleteRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, agencyBodyCap))
	// Strict decoding is half the privacy boundary: a field the artifact struct does not declare —
	// a model name, a cost, a session id, a direction — cannot be stored, because it cannot be
	// decoded. The other half is the value scan inside validateAgencyArtifact.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "invalid artifact body: " + redactAgencyText(err.Error()),
		})
		return
	}

	run, err := s.agency.Complete(req.UserID, r.PathValue("id"), req.LeaseToken, req.Artifact,
		time.Now().UTC())
	if err != nil {
		var invalid agencyValidationError
		if errors.As(err, &invalid) {
			// A rejected artifact is a FAILED run with a stated reason, never a partial success.
			// The failure is recorded against the run so the owner sees why, using the same lease
			// the completion arrived on.
			if _, ferr := s.agency.Fail(req.UserID, r.PathValue("id"), req.LeaseToken,
				"artifact rejected: "+invalid.Error(), false, time.Now().UTC()); ferr != nil {
				log.Printf("agency: could not record a rejected artifact: %s",
					redactAgencyText(ferr.Error()))
			}
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": invalid.Error(), "code": "invalid_artifact",
			})
			return
		}
		s.writeAgencyWorkerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "runId": run.ID, "status": run.Status,
	})
}

type agencyFailRequest struct {
	agencyWorkerRef
	Error string `json:"error"`
	// Retryable says whether the worker believes another attempt could succeed. The server still
	// applies the attempt cap; this only distinguishes "the provider was down" from "this job is
	// malformed and will fail identically next time".
	Retryable bool `json:"retryable"`
}

func (s *Server) handleAgencyFail(w http.ResponseWriter, r *http.Request) {
	if !s.requireAgencyWorker(w, r) {
		return
	}
	var req agencyFailRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, agencyBodyCap))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "invalid failure body: " + redactAgencyText(err.Error()),
		})
		return
	}
	run, err := s.agency.Fail(req.UserID, r.PathValue("id"), req.LeaseToken, req.Error,
		req.Retryable, time.Now().UTC())
	if err != nil {
		s.writeAgencyWorkerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "runId": run.ID, "status": run.Status, "attempts": run.Attempts,
	})
}

// writeAgencyWorkerError maps the store's two sentinel errors onto the statuses the bridge acts on.
//
// 409 IS NOT AN ERROR THE WORKER SHOULD RETRY. It means the lease is no longer ours — the run
// expired and was taken over, or the owner cancelled it. The other run's result stands and ours is
// discarded. services/llm/app/automation.py handles the same status the same way and its comment
// says so: "The other run's result stands; ours is discarded rather than overwriting a newer one."
func (s *Server) writeAgencyWorkerError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errAgencyRunNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "no such research run", "code": "unknown_run",
		})
	case errors.Is(err, errAgencyStaleLease):
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "the lease is missing, expired, or no longer current — another attempt owns " +
				"this run, or the owner cancelled it",
			"code": "stale_lease",
		})
	default:
		log.Printf("agency: worker call failed: %s", redactAgencyText(err.Error()))
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "the research run could not be updated",
		})
	}
}

func containsString(haystack []string, needle string) bool {
	return slices.Contains(haystack, needle)
}
