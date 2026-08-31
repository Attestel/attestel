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

const maxSnapshotsPerPortfolio = 180

type snapshotBucket struct {
	loaded bool
	items  []PortfolioSnapshot
	err    error
}

type PortfolioSnapshotStore struct {
	base  string
	mu    sync.Mutex
	cache map[string]*snapshotBucket
	docs  *documentRepository
}

func openPortfolioSnapshotStore(base string) (*PortfolioSnapshotStore, error) {
	return openPortfolioSnapshotStoreWithRepository(base, nil)
}

func openPortfolioSnapshotStoreWithRepository(base string, docs *documentRepository) (*PortfolioSnapshotStore, error) {
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, err
	}
	return &PortfolioSnapshotStore{base: base, cache: map[string]*snapshotBucket{}, docs: docs}, nil
}

func (s *PortfolioSnapshotStore) path(uid string) string {
	return filepath.Join(s.base, uid, "portfolio_snapshots.json")
}

func (s *PortfolioSnapshotStore) loadLocked(uid string) (*snapshotBucket, error) {
	if uid == "" {
		return nil, errors.New("user id is required")
	}
	if bucket, ok := s.cache[uid]; ok && bucket.loaded {
		return bucket, bucket.err
	}
	bucket := &snapshotBucket{loaded: true, items: []PortfolioSnapshot{}}
	var b []byte
	var err error
	if s.docs != nil {
		b, _, err = s.docs.load(uid, "portfolio_snapshots", s.path(uid))
	} else {
		b, err = os.ReadFile(s.path(uid))
	}
	if err != nil {
		if !os.IsNotExist(err) {
			bucket.err = fmt.Errorf("read portfolio snapshots: %w", err)
		}
		s.cache[uid] = bucket
		return bucket, bucket.err
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &bucket.items); err != nil {
			bucket.err = fmt.Errorf("decode portfolio snapshots: %w", err)
		}
	}
	if bucket.items == nil {
		bucket.items = []PortfolioSnapshot{}
	}
	s.cache[uid] = bucket
	return bucket, bucket.err
}

func (s *PortfolioSnapshotStore) persistLocked(uid string, bucket *snapshotBucket) error {
	if bucket == nil || bucket.err != nil {
		return errors.New("portfolio snapshot store is unreadable")
	}
	if s.docs != nil {
		return s.docs.save(uid, "portfolio_snapshots", bucket.items)
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
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path(uid)); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func cloneSnapshots(src []PortfolioSnapshot) []PortfolioSnapshot {
	b, _ := json.Marshal(src)
	out := []PortfolioSnapshot{}
	_ = json.Unmarshal(b, &out)
	return out
}

func (s *PortfolioSnapshotStore) List(uid, portfolioID string, limit int) ([]PortfolioSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, err := s.loadLocked(uid)
	if err != nil {
		return nil, err
	}
	out := make([]PortfolioSnapshot, 0)
	// Writes append. Walk backward so two explicit checkpoints inside one wall-clock second still
	// return in creation order; a random id is not a chronology surrogate.
	for i := len(bucket.items) - 1; i >= 0; i-- {
		snapshot := bucket.items[i]
		if snapshot.PortfolioID == portfolioID {
			out = append(out, snapshot)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return cloneSnapshots(out), nil
}

func (s *PortfolioSnapshotStore) Latest(uid, portfolioID string) (PortfolioSnapshot, bool, error) {
	items, err := s.List(uid, portfolioID, 1)
	if err != nil || len(items) == 0 {
		return PortfolioSnapshot{}, false, err
	}
	return items[0], true, nil
}

func (s *PortfolioSnapshotStore) Add(uid string, snapshot PortfolioSnapshot) (PortfolioSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, err := s.loadLocked(uid)
	if err != nil {
		return PortfolioSnapshot{}, err
	}
	prior := append([]PortfolioSnapshot(nil), bucket.items...)
	bucket.items = append(bucket.items, snapshot)

	// Bound each portfolio independently. Other portfolios' review history is untouched.
	indices := make([]int, 0)
	for i, row := range bucket.items {
		if row.PortfolioID == snapshot.PortfolioID {
			indices = append(indices, i)
		}
	}
	if len(indices) > maxSnapshotsPerPortfolio {
		sort.Slice(indices, func(i, j int) bool {
			a, b := bucket.items[indices[i]], bucket.items[indices[j]]
			if a.CreatedAt != b.CreatedAt {
				return a.CreatedAt < b.CreatedAt
			}
			return a.ID < b.ID
		})
		remove := map[int]bool{}
		for _, index := range indices[:len(indices)-maxSnapshotsPerPortfolio] {
			remove[index] = true
		}
		kept := make([]PortfolioSnapshot, 0, len(bucket.items)-len(remove))
		for i, row := range bucket.items {
			if !remove[i] {
				kept = append(kept, row)
			}
		}
		bucket.items = kept
	}
	if err := s.persistLocked(uid, bucket); err != nil {
		bucket.items = prior
		return PortfolioSnapshot{}, err
	}
	return cloneSnapshots([]PortfolioSnapshot{snapshot})[0], nil
}
