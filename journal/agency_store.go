package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

// agency_store.go — durable, owner-scoped persistence for agency runs.
//
// It is modelled directly on PortfolioSnapshotStore: one JSON collection per user, written through
// `documentRepository` when a database is configured and to `{base}/{uid}/agency_runs.json` when
// one is not. That fallback is not a toy — it is the same explicit no-DATABASE_URL path every other
// journal store has, and it is what lets the whole lane be tested end to end without a database.
//
// EVERY MUTATION GOES THROUGH ONE LOCK. The transitions this store performs — claim, heartbeat,
// complete, fail, cancel — are read-modify-write over a user's run list, and two workers racing for
// one run must produce exactly one winner. In PostgreSQL that would be a conditional UPDATE
// (services/events/app/automation.py does it that way, and its comment explains why the row
// serialises the writers); here the journal process is the single writer for its own store, so the
// mutex is the serialisation point and `claimLocked` re-reads the run inside it. If this lane ever
// runs in more than one journal replica, THIS is the line that has to become a conditional UPDATE
// — it is called out here rather than discovered later.

const agencyCollection = "agency_runs"

type agencyBucket struct {
	loaded bool
	items  []AgencyRun
	err    error
}

// AgencyStore holds the per-user run lists. `owners` is the configured owner allowlist: the worker
// claim path sweeps exactly those users and nobody else, so a claim can never reach a run belonging
// to a user the operator did not name.
type AgencyStore struct {
	base   string
	mu     sync.Mutex
	cache  map[string]*agencyBucket
	docs   *documentRepository
	owners []string
}

func openAgencyStore(base string, owners []string) (*AgencyStore, error) {
	return openAgencyStoreWithRepository(base, owners, nil)
}

func openAgencyStoreWithRepository(base string, owners []string, docs *documentRepository) (*AgencyStore, error) {
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, err
	}
	return &AgencyStore{
		base:   base,
		cache:  map[string]*agencyBucket{},
		docs:   docs,
		owners: append([]string(nil), owners...),
	}, nil
}

func (s *AgencyStore) path(uid string) string {
	return filepath.Join(s.base, uid, agencyCollection+".json")
}

// isOwner reports whether `uid` is on the configured allowlist. An EMPTY allowlist means NOBODY,
// exactly as gateway's EvalAdminUIDs does: an unconfigured deployment must not have an owner-only
// capability reachable by whoever signs up first.
func (s *AgencyStore) isOwner(uid string) bool {
	if uid == "" {
		return false
	}
	return slices.Contains(s.owners, uid)
}

func (s *AgencyStore) loadLocked(uid string) (*agencyBucket, error) {
	if uid == "" {
		return nil, errors.New("user id is required")
	}
	if bucket, ok := s.cache[uid]; ok && bucket.loaded {
		return bucket, bucket.err
	}
	bucket := &agencyBucket{loaded: true, items: []AgencyRun{}}
	var b []byte
	var err error
	if s.docs != nil {
		b, _, err = s.docs.load(uid, agencyCollection, s.path(uid))
	} else {
		b, err = os.ReadFile(s.path(uid))
	}
	if err != nil {
		if !os.IsNotExist(err) {
			bucket.err = fmt.Errorf("read agency runs: %w", err)
		}
		s.cache[uid] = bucket
		return bucket, bucket.err
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &bucket.items); err != nil {
			bucket.err = fmt.Errorf("decode agency runs: %w", err)
		}
	}
	if bucket.items == nil {
		bucket.items = []AgencyRun{}
	}
	s.cache[uid] = bucket
	return bucket, bucket.err
}

func (s *AgencyStore) persistLocked(uid string, bucket *agencyBucket) error {
	if bucket == nil || bucket.err != nil {
		return errors.New("agency run store is unreadable")
	}
	// Retain the newest N. A run is a durable record, but an unbounded list on one owner is an
	// unbounded document in one row.
	sortAgencyRunsNewestFirst(bucket.items)
	if len(bucket.items) > agencyRunsPerUser {
		bucket.items = bucket.items[:agencyRunsPerUser]
	}
	if s.docs != nil {
		return s.docs.save(uid, agencyCollection, bucket.items)
	}
	dir := filepath.Dir(s.path(uid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(bucket.items, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path(uid) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(uid))
}

// ─────────────────────────────────────────────────────────────────────────────── owner operations

// Create returns the run for this request. If a live run already exists for the same idempotency
// key it is returned UNCHANGED with `created == false`, and no second run is enqueued — the
// attach-don't-start rule gateway/analystjobs.go applies to analyst runs.
func (s *AgencyStore) Create(uid, ticker, question string, now time.Time) (AgencyRun, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, err := s.loadLocked(uid)
	if err != nil {
		return AgencyRun{}, false, err
	}
	s.reconcileLocked(bucket, now)

	key := agencyIdempotencyKey(uid, agencyWorkflowCompanyResearch, ticker, question)
	for _, run := range bucket.items {
		if run.IdempotencyKey == key && !run.terminal() {
			return run, false, nil
		}
	}

	id, err := newAgencyRunID()
	if err != nil {
		return AgencyRun{}, false, err
	}
	run := AgencyRun{
		ID:              id,
		UserID:          uid,
		SchemaVersion:   agencyJobSchemaVersion,
		WorkflowVersion: agencyWorkflowCompanyResearch,
		IdempotencyKey:  key,
		Ticker:          ticker,
		Question:        question,
		// Server-assigned, never worker-supplied. See AgencyRun.AsOf.
		AsOf:      now.UTC().Format(time.RFC3339),
		Status:    agencyQueued,
		CreatedAt: now.Unix(),
	}
	bucket.items = append(bucket.items, run)
	if err := s.persistLocked(uid, bucket); err != nil {
		return AgencyRun{}, false, err
	}
	return run, true, nil
}

// Get returns one run belonging to `uid`. A run owned by somebody else is NOT FOUND rather than
// forbidden: a 403 would confirm the id exists.
func (s *AgencyStore) Get(uid, id string, now time.Time) (AgencyRun, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, err := s.loadLocked(uid)
	if err != nil {
		return AgencyRun{}, false, err
	}
	if s.reconcileLocked(bucket, now) {
		if err := s.persistLocked(uid, bucket); err != nil {
			return AgencyRun{}, false, err
		}
	}
	for _, run := range bucket.items {
		if run.ID == id {
			return run, true, nil
		}
	}
	return AgencyRun{}, false, nil
}

// List returns the caller's runs, newest first.
func (s *AgencyStore) List(uid string, limit int, now time.Time) ([]AgencyRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, err := s.loadLocked(uid)
	if err != nil {
		return nil, err
	}
	if s.reconcileLocked(bucket, now) {
		if err := s.persistLocked(uid, bucket); err != nil {
			return nil, err
		}
	}
	out := append([]AgencyRun(nil), bucket.items...)
	sortAgencyRunsNewestFirst(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Cancel stops a run the owner asked for. Cancelling a terminal run is a no-op that reports the run
// unchanged, so a double-click is not an error.
func (s *AgencyStore) Cancel(uid, id string, now time.Time) (AgencyRun, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, err := s.loadLocked(uid)
	if err != nil {
		return AgencyRun{}, false, err
	}
	s.reconcileLocked(bucket, now)
	for i := range bucket.items {
		run := &bucket.items[i]
		if run.ID != id {
			continue
		}
		if run.terminal() {
			return *run, true, nil
		}
		run.Status = agencyCancelled
		run.FinishedAt = now.Unix()
		// The lease is dropped, so an in-flight worker's completion cannot land: `complete`
		// requires the CURRENT token and there no longer is one.
		run.LeaseToken = ""
		run.LeaseExpiresAt = 0
		if err := s.persistLocked(uid, bucket); err != nil {
			return AgencyRun{}, false, err
		}
		return *run, true, nil
	}
	return AgencyRun{}, false, nil
}

// ────────────────────────────────────────────────────────────────────────────── worker operations

// Claim takes the oldest claimable run across the configured owners and returns it with a fresh
// lease. `ok == false` means there is nothing to do, which is the ordinary answer.
func (s *AgencyStore) Claim(workerID string, lease time.Duration, now time.Time) (AgencyRun, bool, error) {
	if lease <= 0 || lease > agencyMaxLeaseDuration {
		lease = agencyLeaseDuration
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, uid := range s.owners {
		bucket, err := s.loadLocked(uid)
		if err != nil {
			// One unreadable owner must not stop the sweep; it is reported by the caller's logs and
			// the other owners still get service.
			continue
		}
		changed := s.reconcileLocked(bucket, now)

		// Oldest first: a queue that serves the newest request first starves the first one.
		oldest := -1
		for i := range bucket.items {
			if !bucket.items[i].claimable(now) {
				continue
			}
			if oldest < 0 || bucket.items[i].CreatedAt < bucket.items[oldest].CreatedAt {
				oldest = i
			}
		}
		if oldest < 0 {
			if changed {
				if err := s.persistLocked(uid, bucket); err != nil {
					return AgencyRun{}, false, err
				}
			}
			continue
		}

		token, err := newAgencyLeaseToken()
		if err != nil {
			return AgencyRun{}, false, err
		}
		run := &bucket.items[oldest]
		run.Status = agencyClaimed
		run.Attempts++
		run.LeaseToken = token
		run.LeaseExpiresAt = now.Add(lease).Unix()
		run.WorkerID = workerID
		run.ClaimedAt = now.Unix()
		run.HeartbeatAt = now.Unix()
		run.Stage = ""
		run.Error = ""
		if err := s.persistLocked(uid, bucket); err != nil {
			return AgencyRun{}, false, err
		}
		return *run, true, nil
	}
	return AgencyRun{}, false, nil
}

// QueuedCount reports how many runs are claimable across the configured owners.
//
// IT IS SIDE-EFFECT FREE, AND THAT IS THE WHOLE REASON IT IS SEPARATE FROM `List`.
//
// Its only caller is `GET /_internal/agency/status`, the worker preflight — a route whose entire
// value is that running it changes nothing. An earlier version reconciled and PERSISTED, so a
// `-check` could quietly re-queue a lapsed run, expire a stale one and rewrite the owner's stored
// document. That is a small change, but it is a change, and a preflight that mutates is a preflight
// you cannot run twice with confidence.
//
// So the count is computed WITHOUT mutating anything: `claimableAt` answers the question
// reconciliation would answer, on the value as it stands, by folding the two rules reconciliation
// applies (a lapsed lease returns to the queue; a run past its cutoff expires) into a pure
// predicate. The stored rows are corrected on the next call that is genuinely a write — `Claim`,
// `List`, `Get` or `Cancel` — which is where correcting them belongs.
//
// It returns a NUMBER, never a run, a ticker or a question.
//
// The one unavoidable effect is that `loadLocked` populates the in-memory read cache. That is a
// cache fill, not a state change: nothing is written and no row's value differs afterwards.
func (s *AgencyStore) QueuedCount(now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.owners) == 0 {
		// No configured owner is not "nothing queued" — it is a lane that is switched off. The
		// caller reports it as such rather than as a healthy empty queue.
		return 0, errAgencyNoOwners
	}
	total := 0
	for _, uid := range s.owners {
		bucket, err := s.loadLocked(uid)
		if err != nil {
			// AN UNREADABLE OWNER STORE IS A FAILURE, NOT A ZERO.
			//
			// This used to `continue`, so a deployment whose only owner's document was corrupt or
			// unreadable answered `ok: true, queuedRuns: 0` — the exact shape of a healthy, idle
			// queue. An operator running `-check` would have been told everything was fine while
			// their runs were invisible, and the bridge would have gone back to sleep. Reporting
			// nothing and reporting nothing-is-wrong are different answers.
			return 0, fmt.Errorf("owner store unreadable: %w", err)
		}
		for i := range bucket.items {
			// A COPY, so nothing here can accidentally write through the slice.
			if run := bucket.items[i]; run.claimableAt(now) {
				total++
			}
		}
	}
	return total, nil
}

// agencyLeaseError distinguishes "this lease is not yours" from "this run does not exist", so the
// route can answer 409 and 404 respectively rather than collapsing both into one status.
var (
	// errAgencyNoOwners marks a lane with no configured owner. It is distinct from an empty queue:
	// one is "switched off", the other is "nothing waiting".
	errAgencyNoOwners = errors.New("no owner is configured for the research agency lane")

	errAgencyRunNotFound = errors.New("no such agency run")
	errAgencyStaleLease  = errors.New("the lease is missing, expired, or no longer current")
)

// Heartbeat extends the CURRENT lease and moves `claimed` to `running`.
//
// It extends only a lease that is still live and still ours. An expired lease is not renewable —
// renewing one would let a worker that lost the run to a takeover silently take it back, which is
// precisely the overwrite this protocol exists to prevent.
func (s *AgencyStore) Heartbeat(uid, id, token, stage string, lease time.Duration, now time.Time) (AgencyRun, error) {
	if lease <= 0 || lease > agencyMaxLeaseDuration {
		lease = agencyLeaseDuration
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, err := s.loadLocked(uid)
	if err != nil {
		return AgencyRun{}, err
	}
	for i := range bucket.items {
		run := &bucket.items[i]
		if run.ID != id {
			continue
		}
		if !run.leaseHeld(now) || !agencyTokenEqual(run.LeaseToken, token) {
			return AgencyRun{}, errAgencyStaleLease
		}
		run.Status = agencyRunning
		run.HeartbeatAt = now.Unix()
		run.LeaseExpiresAt = now.Add(lease).Unix()
		if stage != "" {
			run.Stage = stage
		}
		if err := s.persistLocked(uid, bucket); err != nil {
			return AgencyRun{}, err
		}
		return *run, nil
	}
	return AgencyRun{}, errAgencyRunNotFound
}

// Complete stores a validated artifact.
//
// IDEMPOTENT FOR THE SAME LEASE, REFUSED FOR ANY OTHER. A repeat completion from the lease that
// produced the stored result returns that result unchanged. A completion from a lease that is no
// longer current — because it expired and the run was taken over, or because the owner cancelled —
// is `errAgencyStaleLease`, and the stored result is untouched. That is the rule "an expired worker
// cannot overwrite a later result", and it is the same takeover semantics
// services/events/app/automation.py::complete applies.
func (s *AgencyStore) Complete(uid, id, token string, artifact *AgencyArtifact, now time.Time) (AgencyRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, err := s.loadLocked(uid)
	if err != nil {
		return AgencyRun{}, err
	}
	for i := range bucket.items {
		run := &bucket.items[i]
		if run.ID != id {
			continue
		}
		if run.Status == agencyCompleted {
			if agencyTokenEqual(run.CompletedByLease, token) {
				return *run, nil // idempotent replay of OUR completion
			}
			return AgencyRun{}, errAgencyStaleLease
		}
		if !run.leaseHeld(now) || !agencyTokenEqual(run.LeaseToken, token) {
			return AgencyRun{}, errAgencyStaleLease
		}
		if err := validateAgencyArtifact(artifact, *run); err != nil {
			return AgencyRun{}, err
		}
		run.Status = agencyCompleted
		run.Artifact = artifact
		run.FinishedAt = now.Unix()
		run.CompletedByLease = token
		run.LeaseToken = ""
		run.LeaseExpiresAt = 0
		run.Error = ""
		run.Stage = ""
		if err := s.persistLocked(uid, bucket); err != nil {
			return AgencyRun{}, err
		}
		return *run, nil
	}
	return AgencyRun{}, errAgencyRunNotFound
}

// Fail records a worker-reported failure. `retryable` returns the run to the queue when attempts
// remain, so a provider outage does not consume the job; the attempt cap is what stops a
// permanently malformed run from being retried forever (alerts/thesis_monitor.go does the same).
func (s *AgencyStore) Fail(uid, id, token, reason string, retryable bool, now time.Time) (AgencyRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, err := s.loadLocked(uid)
	if err != nil {
		return AgencyRun{}, err
	}
	for i := range bucket.items {
		run := &bucket.items[i]
		if run.ID != id {
			continue
		}
		if run.Status == agencyFailed && agencyTokenEqual(run.CompletedByLease, token) {
			return *run, nil // idempotent replay
		}
		if !run.leaseHeld(now) || !agencyTokenEqual(run.LeaseToken, token) {
			return AgencyRun{}, errAgencyStaleLease
		}
		run.Error = redactAgencyText(reason)
		run.LeaseToken = ""
		run.LeaseExpiresAt = 0
		run.Stage = ""
		if retryable && run.Attempts < agencyMaxAttempts {
			run.Status = agencyQueued
			run.ClaimedAt = 0
			run.HeartbeatAt = 0
			run.WorkerID = ""
		} else {
			run.Status = agencyFailed
			run.FinishedAt = now.Unix()
			run.CompletedByLease = token
		}
		if err := s.persistLocked(uid, bucket); err != nil {
			return AgencyRun{}, err
		}
		return *run, nil
	}
	return AgencyRun{}, errAgencyRunNotFound
}

// ───────────────────────────────────────────────────────────────────────────────── reconciliation

// reconcileLocked ages the bucket forward to `now` and reports whether anything changed.
//
// Two rules, both fail-safe in the direction of "do less":
//
//   - A lease that lapsed returns the run to `queued` when attempts remain, and to `expired` when
//     they do not. The abandoned worker cannot complete it either way, because the token is gone.
//   - A run that has sat unclaimed past agencyMaxRunAge is `expired`. Research answered at a cutoff
//     six hours stale is not the research that was asked for.
func (s *AgencyStore) reconcileLocked(bucket *agencyBucket, now time.Time) bool {
	if bucket == nil || bucket.err != nil {
		return false
	}
	changed := false
	for i := range bucket.items {
		run := &bucket.items[i]
		if run.terminal() {
			continue
		}
		if (run.Status == agencyClaimed || run.Status == agencyRunning) && !run.leaseHeld(now) {
			run.LeaseToken = ""
			run.LeaseExpiresAt = 0
			run.WorkerID = ""
			run.Stage = ""
			if run.Attempts >= agencyMaxAttempts {
				run.Status = agencyExpired
				run.FinishedAt = now.Unix()
				if run.Error == "" {
					run.Error = "the worker lease expired after the maximum number of attempts"
				}
			} else {
				run.Status = agencyQueued
				run.ClaimedAt = 0
				run.HeartbeatAt = 0
				if run.Error == "" {
					run.Error = "a worker lease expired without a result; the run was re-queued"
				}
			}
			changed = true
			continue
		}
		if run.Status == agencyQueued && now.Sub(time.Unix(run.CreatedAt, 0)) > agencyMaxRunAge {
			run.Status = agencyExpired
			run.FinishedAt = now.Unix()
			run.Error = "no worker claimed this run before its point-in-time cutoff went stale"
			changed = true
		}
	}
	return changed
}

// agencyTokenEqual compares two lease tokens in constant time. Both are hex of the same length in
// practice, and a length mismatch is answered without leaking where they diverged.
func agencyTokenEqual(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return constantTimeStringEqual(a, b)
}
