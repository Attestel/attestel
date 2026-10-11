package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// experiment_routes.go — the HTTP surface of the experiment-review lane.
//
// It reuses the agency lane's two credentials unchanged (agency_routes.go): the owner routes take
// the session cookie plus `AGENCY_OWNER_UIDS`, the worker routes take `AGENCY_WORKER_TOKEN` and a
// session cookie on one is a 404. Nothing here introduces a third audience or a third secret.
//
// ─────────────────────────────────────────────────────────────────────────────────────────────
// WHAT THE OWNER ROUTES CAN AND CANNOT CAUSE
// ─────────────────────────────────────────────────────────────────────────────────────────────
// `POST /experiments/snapshots` is the only route in this file that reaches an upstream, and the
// only thing it can reach is the paper service, over GET (paper_client.go has no other verb). It
// freezes evidence and returns. It calls no model, on this machine or any other.
//
// `POST /agency/reviews` writes a QUEUED ROW and returns 202. The Hermes invocation happens later,
// on the owner's own computer, only because a local worker chose to claim that row — the same
// property `gateway/agency.go` records for the research lane, and it is stronger than "the browser
// cannot start a model call": the hosted deployment has no path to a model in this lane at all.
//
// The two GETs are store reads. They reach no upstream, they can start nothing, and they are
// therefore the only routes here a browser may poll.

// snapshotAssembleTimeout bounds the whole five-source assembly.
//
// IT IS DERIVED FROM THE PER-SOURCE BUDGETS RATHER THAN PICKED. The five sources are read
// sequentially with their own timeouts (paper_client.go), so the worst case is their sum — 145s,
// two live-upstream reads at 50s and three store reads at 15s — and this leaves 45 seconds on top
// for the assembly, the content hash and the store write.
//
// An overall budget SHORTER than that sum would cut off the last source and record a healthy
// deployment as `unavailable`, which is the exact false negative this lane exists to avoid. A much
// longer one would let a wedged upstream hold a browser request open for minutes.
//
// IT IS ONE LINK IN A FOUR-LAYER CHAIN, and every outer layer must strictly exceed the one inside:
//
//	sources (145s)  <  journal assembly (190s)  <  gateway (215s)  <  nginx location (240s)
//
// `TestTheSnapshotBudgetChainNests` asserts all four together, including the nginx template, so a
// change to any one of them fails rather than silently truncating an assembly at whichever layer
// gives up first.
const snapshotAssembleTimeout = 190 * time.Second

// The addressing headers for the lease-scoped worker snapshot read. Named constants because the
// bridge sets the same two strings and a typo on one side is a 400 nobody can explain.
const (
	agencyUserHeader  = "X-Agency-User-Id"
	agencyLeaseHeader = "X-Agency-Lease-Token"
)

// ────────────────────────────────────────────────────────────────────────────────── owner surface

// handleSnapshotCreate assembles and stores one evidence snapshot.
//
// USER-INITIATED ONLY. Nothing in this deployment calls it on a timer, on a page load or on a
// ticker change; the browser reaches it from a click and nowhere else.
//
// TWO REFUSALS ARE NOT ERRORS TO PAPER OVER:
//
//   - A MIXED GENERATION is a 409. The evidence straddles a reset, so there is no single experiment
//     for it to be evidence about, and storing it would create a record nobody could interpret. A
//     retry moments later normally succeeds, which is exactly the shape `paper/dashboard.go`
//     already uses inside its own composition.
//   - An UNREACHABLE PAPER SERVICE IS NOT A REFUSAL. It is a snapshot whose sources are
//     `unavailable`, stored as such, with `operationalStatus: unavailable` derived from it. "The
//     deployment could not be read" is a fact about the experiment and this lane exists to record
//     facts about the experiment.
func (s *Server) handleSnapshotCreate(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.requireExperimentOwner(w, r)
	if !ok {
		return
	}
	// The request takes NO BODY, and a body is not read. There is nothing for a caller to
	// parameterise: which endpoints are composed, in what order and under what cutoff are decisions
	// this server makes, not decisions a payload can influence.
	ctx, cancel := context.WithTimeout(r.Context(), snapshotAssembleTimeout)
	defer cancel()

	snap, err := s.assembleExperimentSnapshot(ctx, uid, time.Now().UTC())
	if err != nil {
		if errors.Is(err, errMixedGeneration) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error": "the paper deployment reported more than one experiment generation while " +
					"this snapshot was being assembled, so the evidence would straddle a reset. " +
					"Nothing was stored; retry.",
				"code": "mixed_generation",
			})
			return
		}
		log.Printf("experiment snapshot: assembly failed: %s", redactAgencyText(err.Error()))
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "the experiment evidence snapshot could not be assembled",
		})
		return
	}

	stored, created, err := s.snapshots.Put(uid, *snap)
	if err != nil {
		log.Printf("experiment snapshot: store failed: %s", redactAgencyText(err.Error()))
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "the experiment evidence snapshot could not be stored",
		})
		return
	}
	// 201 for a new record, 200 when this content was already stored. `created:false` says the
	// deployment's state is byte-identical to a snapshot already held — the id is a content
	// address, so there was nothing new to write.
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]any{
		"snapshot": experimentSnapshotViewOf(stored),
		"created":  created,
		"href":     "/experiments/snapshots/" + stored.ID,
	})
}

func (s *Server) handleSnapshotList(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.requireExperimentOwner(w, r)
	if !ok {
		return
	}
	limit := 20
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= experimentSnapshotsPerUser {
			limit = n
		}
	}
	snaps, err := s.snapshots.List(uid, limit)
	if err != nil {
		log.Printf("experiment snapshot: list failed: %s", redactAgencyText(err.Error()))
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "the experiment evidence snapshots could not be read",
		})
		return
	}
	views := make([]experimentSnapshotView, 0, len(snaps))
	for _, snap := range snaps {
		views = append(views, experimentSnapshotViewOf(snap))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"snapshots": views,
		"workflow":  agencyWorkflowExperimentReview,
		"note":      experimentSnapshotNote,
	})
}

func (s *Server) handleSnapshotGet(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.requireExperimentOwner(w, r)
	if !ok {
		return
	}
	snap, found, err := s.snapshots.Get(uid, r.PathValue("id"))
	if err != nil {
		log.Printf("experiment snapshot: read failed: %s", redactAgencyText(err.Error()))
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "the experiment evidence snapshot could not be read",
		})
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "no such experiment snapshot", "code": "unknown_snapshot",
		})
		return
	}
	writeJSON(w, http.StatusOK, experimentSnapshotViewOf(snap))
}

// handleReviewCreate enqueues an `experiment_review_v1` run against a stored snapshot.
//
// The request has TWO FIELDS and is decoded with DisallowUnknownFields. See
// `experimentReviewRequest` for why that field list is the security property rather than a
// convenience.
func (s *Server) handleReviewCreate(w http.ResponseWriter, r *http.Request) {
	uid, ok := s.requireExperimentOwner(w, r)
	if !ok {
		return
	}
	var req experimentReviewRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, agencyBodyCap))
	// A request that tried to name a prompt, a profile, a model, a path or a command is a 400 —
	// never a silently stripped field, which would teach a caller the field works.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "invalid request body: " + redactAgencyText(err.Error()),
		})
		return
	}
	snapshotID, err := req.normalise()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	// THE SNAPSHOT MUST ALREADY EXIST, IT MUST BE THIS OWNER'S, AND IT MUST STILL EXIST WHEN THE
	// RUN IS CREATED.
	//
	// Those are three requirements, not two, and the third is why this is one atomic operation
	// rather than a check followed by a create. Retention pins snapshots that live runs depend on —
	// but between a bare existence check and the run's creation there IS no run, so nothing pins it
	// and a concurrent snapshot write could evict it. The review would then be enqueued against
	// evidence that no longer exists, and the worker would discover that only after claiming it.
	//
	// `WithSnapshotPinned` holds the snapshot store's lock across both steps, which is the lock
	// eviction also needs. See its header.
	var (
		run     AgencyRun
		created bool
	)
	err = s.snapshots.WithSnapshotPinned(uid, snapshotID, func() error {
		var createErr error
		run, created, createErr = s.agency.CreateReview(uid, snapshotID, time.Now().UTC())
		return createErr
	})
	if errors.Is(err, errSnapshotNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "no such experiment snapshot", "code": "unknown_snapshot",
		})
		return
	}
	if err != nil {
		log.Printf("experiment review: create failed: %s", redactAgencyText(err.Error()))
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "could not enqueue the experiment review",
		})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"run":     agencyView(run),
		"created": created,
		"href":    "/agency/runs/" + run.ID,
	})
}

// requireExperimentOwner is `requireAgencyOwner` plus the snapshot store's availability. The two
// stores are opened together in main.go, so a deployment with one and not the other is a bug rather
// than a configuration — but a nil dereference on a route is a worse way to find out.
func (s *Server) requireExperimentOwner(w http.ResponseWriter, r *http.Request) (string, bool) {
	uid, ok := s.requireAgencyOwner(w, r)
	if !ok {
		return "", false
	}
	if s.snapshots == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "the experiment review lane is not available on this deployment",
		})
		return "", false
	}
	return uid, true
}

// ───────────────────────────────────────────────────────────────────────────────── worker surface

// handleAgencyWorkerSnapshot serves the snapshot attached to a run the caller currently HOLDS.
//
// LEASE-SCOPED, AND THAT IS THE WHOLE DESIGN. There is no route anywhere that serves a snapshot by
// id to a worker credential alone, so possessing the worker token does not let a compromised bridge
// enumerate the owner's evidence: it lets it read the evidence for the job it was actually given,
// while it still holds the lease on that job. A lease that has lapsed reads nothing.
//
// It is a READ. It takes no lease, extends none, and changes no state.
func (s *Server) handleAgencyWorkerSnapshot(w http.ResponseWriter, r *http.Request) {
	if !s.requireAgencyWorker(w, r) {
		return
	}
	if s.snapshots == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "the experiment review lane is not available on this deployment",
		})
		return
	}
	// THE LEASE TOKEN RIDES IN A HEADER, NOT IN THE QUERY STRING.
	//
	// A query string is the single most-logged part of a request: reverse proxies, load balancers,
	// CDNs and access logs all record the full path by default, and several of them do it at a
	// different retention than the request body. A lease token is a bearer credential for one run —
	// putting it where every hop writes it to disk is how a short-lived secret becomes a long-lived
	// one. `X-Worker-Token` already travels as a header for exactly this reason; so does this.
	//
	// It stays a GET. Moving the credential does not make this a mutation, and turning a read into a
	// POST to hide a value would have made the route's safety harder to see, not easier.
	uid := strings.TrimSpace(r.Header.Get(agencyUserHeader))
	token := strings.TrimSpace(r.Header.Get(agencyLeaseHeader))
	if uid == "" || token == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "this route takes the run's owner and lease token in the " + agencyUserHeader +
				" and " + agencyLeaseHeader + " headers",
			"code": "missing_lease",
		})
		return
	}

	run, found, err := s.agency.Get(uid, r.PathValue("id"), time.Now().UTC())
	if err != nil {
		s.writeAgencyWorkerError(w, err)
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "no such research run", "code": "unknown_run",
		})
		return
	}
	if !run.leaseHeld(time.Now().UTC()) || !agencyTokenEqual(run.LeaseToken, token) {
		s.writeAgencyWorkerError(w, errAgencyStaleLease)
		return
	}
	if !run.isReview() || run.SnapshotID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "this run carries no evidence snapshot", "code": "not_a_review",
		})
		return
	}

	snap, err := s.snapshots.findForOwner(uid, run.SnapshotID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "the evidence snapshot for this run is no longer stored",
			"code":  "unknown_snapshot",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshot": snap})
}

type agencyCompleteReviewRequest struct {
	agencyWorkerRef
	Review *ExperimentReviewArtifact `json:"review"`
}

// handleAgencyCompleteReview stores a validated review.
//
// Strict decoding is half the boundary: a field `ExperimentReviewArtifact` does not declare — a
// model name, a cost, a session id, a DIRECTION — cannot be stored, because it cannot be decoded.
// The other half is `validateExperimentReview`, which checks every citation against the stored
// snapshot and refuses any status that disagrees with the one this server derived.
func (s *Server) handleAgencyCompleteReview(w http.ResponseWriter, r *http.Request) {
	if !s.requireAgencyWorker(w, r) {
		return
	}
	if s.snapshots == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "the experiment review lane is not available on this deployment",
		})
		return
	}
	var req agencyCompleteReviewRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, agencyBodyCap))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "invalid review body: " + redactAgencyText(err.Error()),
		})
		return
	}

	runID := r.PathValue("id")
	run, found, err := s.agency.Get(req.UserID, runID, time.Now().UTC())
	if err != nil {
		s.writeAgencyWorkerError(w, err)
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "no such research run", "code": "unknown_run",
		})
		return
	}
	// Loaded WITHOUT trusting the artifact's own `snapshotId`: the run says which snapshot this
	// review is about, and the validator then requires the artifact to agree with the run.
	snap, snapErr := s.snapshots.findForOwner(req.UserID, run.SnapshotID)
	if snapErr != nil {
		snap = ExperimentSnapshot{}
	}

	stored, err := s.agency.CompleteReview(req.UserID, runID, req.LeaseToken, req.Review, snap,
		time.Now().UTC())
	if err != nil {
		var invalid agencyValidationError
		if errors.As(err, &invalid) {
			// A rejected review is a FAILED run with a stated reason, never a partial success —
			// the same rule `handleAgencyComplete` applies to a rejected research artifact.
			if _, ferr := s.agency.Fail(req.UserID, runID, req.LeaseToken,
				"review rejected: "+invalid.Error(), false, time.Now().UTC()); ferr != nil {
				log.Printf("experiment review: could not record a rejected review: %s",
					redactAgencyText(ferr.Error()))
			}
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": invalid.Error(), "code": "invalid_review",
			})
			return
		}
		s.writeAgencyWorkerError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "runId": stored.ID, "status": stored.Status,
	})
}
