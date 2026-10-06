package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// experiment_snapshot_store.go — durable, owner-scoped persistence for evidence snapshots.
//
// IT IS APPEND-AND-READ. There is no Update, no Patch and no Delete method on this type, and that
// absence is the whole point: a snapshot is evidence, and evidence a later process can rewrite is
// not evidence. A stored snapshot is only ever removed by the retention trim, which drops the
// OLDEST records wholesale and never edits one.
//
// It is modelled on AgencyStore (agency_store.go), which is itself modelled on
// PortfolioSnapshotStore: one JSON collection per user, written through `documentRepository` when a
// database is configured and to `{base}/{uid}/experiment_snapshots.json` when one is not. Every
// mutation goes through one mutex, for the reason agency_store.go's header gives.

const experimentSnapshotCollection = "experiment_snapshots"

var errSnapshotNotFound = errors.New("no such experiment snapshot")

type experimentSnapshotBucket struct {
	loaded bool
	items  []ExperimentSnapshot
	err    error
}

// ExperimentSnapshotStore holds the per-user snapshot lists.
//
// `owners` is the same allowlist the agency lane uses. It is held here so the WORKER path can
// resolve a snapshot across the configured owners when a bridge asks for the one attached to a run
// it holds — the identical sweep `AgencyStore.Claim` performs, for the identical reason: a lookup
// can never reach a record belonging to a user the operator did not name.
type ExperimentSnapshotStore struct {
	base   string
	mu     sync.Mutex
	cache  map[string]*experimentSnapshotBucket
	docs   *documentRepository
	owners []string

	// pinned reports the snapshot ids a user's LIVE runs still need. Retention never drops one.
	//
	// WHY THIS EXISTS. Retention kept the newest N snapshots and dropped the rest — including,
	// eventually, the one a queued review had been created against. A worker would then claim that
	// review, ask for its evidence, and get a 404 for a snapshot the owner had legitimately queued
	// twenty minutes earlier. Worse, the failure is time-dependent: it only appears once a busy
	// owner has taken N newer snapshots, so it would never show up in testing and would show up on
	// the one day somebody was iterating.
	//
	// LOCK ORDERING, AND IT IS LOAD-BEARING. This callback reaches into the AGENCY store, so it is
	// invoked while THIS store's mutex is held. Nothing in the agency store calls back into this
	// one, so the order is always snapshots → agency and there is no cycle. If that ever changes,
	// this is the line that has to change with it.
	pinned func(uid string) map[string]bool
}

func openExperimentSnapshotStore(base string, owners []string, docs *documentRepository) (*ExperimentSnapshotStore, error) {
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, err
	}
	return &ExperimentSnapshotStore{
		base:   base,
		cache:  map[string]*experimentSnapshotBucket{},
		docs:   docs,
		owners: append([]string(nil), owners...),
	}, nil
}

// pinnedFor resolves the callback, treating an unset one as "nothing is pinned". A store wired
// without it retains exactly as it did before — which is the correct fallback for a test that does
// not care, and the wrong one for production, so main.go always sets it.
func (s *ExperimentSnapshotStore) pinnedFor(uid string) map[string]bool {
	if s.pinned == nil {
		return nil
	}
	if ids := s.pinned(uid); ids != nil {
		return ids
	}
	return nil
}

func (s *ExperimentSnapshotStore) path(uid string) string {
	return filepath.Join(s.base, uid, experimentSnapshotCollection+".json")
}

func (s *ExperimentSnapshotStore) loadLocked(uid string) (*experimentSnapshotBucket, error) {
	if uid == "" {
		return nil, errors.New("user id is required")
	}
	if bucket, ok := s.cache[uid]; ok && bucket.loaded {
		return bucket, bucket.err
	}
	bucket := &experimentSnapshotBucket{loaded: true, items: []ExperimentSnapshot{}}
	var b []byte
	var err error
	if s.docs != nil {
		b, _, err = s.docs.load(uid, experimentSnapshotCollection, s.path(uid))
	} else {
		b, err = os.ReadFile(s.path(uid))
	}
	if err != nil {
		if !os.IsNotExist(err) {
			bucket.err = fmt.Errorf("read experiment snapshots: %w", err)
		}
		s.cache[uid] = bucket
		return bucket, bucket.err
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &bucket.items); err != nil {
			bucket.err = fmt.Errorf("decode experiment snapshots: %w", err)
		}
	}
	if bucket.items == nil {
		bucket.items = []ExperimentSnapshot{}
	}
	s.cache[uid] = bucket
	return bucket, bucket.err
}

func (s *ExperimentSnapshotStore) persistLocked(uid string, bucket *experimentSnapshotBucket) error {
	if bucket == nil || bucket.err != nil {
		return errors.New("the experiment snapshot store is unreadable")
	}
	sortSnapshotsNewestFirst(bucket.items)
	bucket.items = applyRetention(bucket.items, s.pinnedFor(uid))
	if s.docs != nil {
		return s.docs.save(uid, experimentSnapshotCollection, bucket.items)
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

// Put stores a snapshot and reports whether it was newly written.
//
// A SNAPSHOT WITH AN ID THAT IS ALREADY STORED IS RETURNED UNCHANGED, and `created` is false. The
// id is a content address (experiment_snapshot.go), so an identical id means byte-identical
// evidence — there is nothing to write, and overwriting would replace a record with its own copy
// while resetting its capture time. The existing record's `capturedAt` is the one that stands.
func (s *ExperimentSnapshotStore) Put(uid string, snap ExperimentSnapshot) (ExperimentSnapshot, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, err := s.loadLocked(uid)
	if err != nil {
		return ExperimentSnapshot{}, false, err
	}
	for _, existing := range bucket.items {
		if existing.ID == snap.ID {
			return existing, false, nil
		}
	}
	bucket.items = append(bucket.items, snap)
	if err := s.persistLocked(uid, bucket); err != nil {
		return ExperimentSnapshot{}, false, err
	}
	return snap, true, nil
}

// Get returns one snapshot belonging to `uid`. A snapshot owned by somebody else is NOT FOUND
// rather than forbidden, exactly as AgencyStore.Get is: a 403 would confirm the id exists.
func (s *ExperimentSnapshotStore) Get(uid, id string) (ExperimentSnapshot, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, err := s.loadLocked(uid)
	if err != nil {
		return ExperimentSnapshot{}, false, err
	}
	for _, snap := range bucket.items {
		if snap.ID == id {
			return snap, true, nil
		}
	}
	return ExperimentSnapshot{}, false, nil
}

// List returns the caller's snapshots, newest first. Payloads are STRIPPED: a listing of fifty
// snapshots each carrying five upstream payloads is a download, not a list. The detail route serves
// the whole record.
func (s *ExperimentSnapshotStore) List(uid string, limit int) ([]ExperimentSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, err := s.loadLocked(uid)
	if err != nil {
		return nil, err
	}
	out := make([]ExperimentSnapshot, 0, len(bucket.items))
	for _, snap := range bucket.items {
		out = append(out, stripSnapshotPayloads(snap))
	}
	sortSnapshotsNewestFirst(out)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// applyRetention keeps the newest N snapshots PLUS every pinned one, whatever its age.
//
// `items` must already be newest-first. The result preserves that order, so a pinned old snapshot
// stays where it belongs in the listing rather than being promoted to the top.
//
// THE HARD CEILING IS A SEPARATE, LARGER NUMBER. Pins are bounded in practice — a user has at most
// `agencyRunsPerUser` runs and non-terminal ones expire within `agencyMaxRunAge` — but "bounded in
// practice" is not bounded. `experimentSnapshotsHardCap` is what stops a pathological pin set from
// growing one owner's document without limit; past it, the OLDEST pinned snapshots are dropped and
// the reviews that needed them fail cleanly with "the evidence snapshot is no longer stored", which
// is a legible failure rather than an unbounded row.
func applyRetention(items []ExperimentSnapshot, pinned map[string]bool) []ExperimentSnapshot {
	if len(items) <= experimentSnapshotsPerUser {
		return items
	}
	kept := make([]ExperimentSnapshot, 0, len(items))
	for i, snap := range items {
		if i < experimentSnapshotsPerUser || pinned[snap.ID] {
			kept = append(kept, snap)
		}
	}
	if len(kept) > experimentSnapshotsHardCap {
		kept = kept[:experimentSnapshotsHardCap]
	}
	return kept
}

// stripSnapshotPayloads returns a copy with the upstream payloads removed. Everything that makes a
// listing row legible — the state, the reason, the age, the generation, the derived verdicts —
// survives; only the bulk goes.
//
// The source map is REBUILT rather than mutated in place: `SnapshotSource` values live in a map
// that the caller's slice shares with the store's cache, and editing one through a copied struct
// would still write into the shared map.
func stripSnapshotPayloads(snap ExperimentSnapshot) ExperimentSnapshot {
	sources := make(map[string]SnapshotSource, len(snap.Sources))
	for name, src := range snap.Sources {
		src.Payload = nil
		sources[name] = src
	}
	snap.Sources = sources
	return snap
}

// WithSnapshotPinned verifies a snapshot exists and runs `fn` while that fact is still true.
//
// ─────────────────────────────────────────────────────────────────────────────────────────────
// THE RACE THIS CLOSES
// ─────────────────────────────────────────────────────────────────────────────────────────────
// Creating a review used to be two independent store operations:
//
//  1. the route asked THIS store "does snapshot X exist?"      -> yes
//  2. the route asked the AGENCY store to create a run for X
//
// Retention pins snapshots that live runs depend on — but at step 1 there is no run yet, so X is
// unpinned. A concurrent `Put` between the two steps could therefore evict X, and step 2 would
// happily create a review of evidence that no longer exists. The owner gets a 202, the worker
// claims the run, and the snapshot read 404s: a review orphaned at birth, and the window is exactly
// as wide as one HTTP handler.
//
// Holding this store's lock across BOTH steps closes it. Eviction happens in `persistLocked`, which
// requires this mutex, so no `Put` can run while `fn` does — and by the time the lock is released
// the run exists and the ordinary pin protects the snapshot from then on.
//
// LOCK ORDERING. `fn` takes the AGENCY store's lock while this one is held, which is the same
// snapshots → agency order `pinnedFor` already uses. Nothing in the agency store reaches back into
// this one, so there is no cycle. That ordering is the invariant; if it is ever broken, this method
// and `pinnedFor` are the two places that break.
func (s *ExperimentSnapshotStore) WithSnapshotPinned(uid, id string, fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, err := s.loadLocked(uid)
	if err != nil {
		return err
	}
	for _, snap := range bucket.items {
		if snap.ID == id {
			return fn()
		}
	}
	return errSnapshotNotFound
}

// findForOwner resolves a snapshot across the configured owners.
//
// ITS ONLY CALLER IS THE WORKER SNAPSHOT ROUTE, and that route has already verified the worker
// holds a live lease on a run whose `SnapshotID` is this one. The sweep is bounded to the
// configured allowlist, so a worker can never reach a snapshot belonging to a user the operator did
// not name — the same guarantee `AgencyStore.Claim` gives for runs.
func (s *ExperimentSnapshotStore) findForOwner(uid, id string) (ExperimentSnapshot, error) {
	if uid != "" {
		snap, found, err := s.Get(uid, id)
		if err != nil {
			return ExperimentSnapshot{}, err
		}
		if found {
			return snap, nil
		}
		return ExperimentSnapshot{}, errSnapshotNotFound
	}
	for _, owner := range s.owners {
		snap, found, err := s.Get(owner, id)
		if err != nil {
			continue
		}
		if found {
			return snap, nil
		}
	}
	return ExperimentSnapshot{}, errSnapshotNotFound
}

func sortSnapshotsNewestFirst(snaps []ExperimentSnapshot) {
	sort.SliceStable(snaps, func(i, j int) bool {
		if snaps[i].CapturedAt != snaps[j].CapturedAt {
			return snaps[i].CapturedAt > snaps[j].CapturedAt
		}
		return snaps[i].ID > snaps[j].ID
	})
}
