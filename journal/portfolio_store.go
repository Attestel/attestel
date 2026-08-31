package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// PortfolioStore follows the journal's per-user, atomic JSON persistence pattern while fixing one
// known weakness of the older stores: an unreadable collection is surfaced as an error on reads and
// writes, never silently presented as an empty portfolio list.

type portfolioBucket struct {
	loaded bool
	items  []Portfolio
	err    error
}

type PortfolioStore struct {
	base  string
	mu    sync.Mutex
	cache map[string]*portfolioBucket
	docs  *documentRepository
}

func openPortfolioStore(base string) (*PortfolioStore, error) {
	return openPortfolioStoreWithRepository(base, nil)
}

func openPortfolioStoreWithRepository(base string, docs *documentRepository) (*PortfolioStore, error) {
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, err
	}
	return &PortfolioStore{base: base, cache: map[string]*portfolioBucket{}, docs: docs}, nil
}

func (s *PortfolioStore) path(uid string) string {
	return filepath.Join(s.base, uid, "portfolios.json")
}

func (s *PortfolioStore) loadLocked(uid string) (*portfolioBucket, error) {
	if uid == "" {
		return nil, errors.New("user id is required")
	}
	if bucket, ok := s.cache[uid]; ok && bucket.loaded {
		return bucket, bucket.err
	}
	bucket := &portfolioBucket{loaded: true, items: []Portfolio{}}
	var b []byte
	var err error
	if s.docs != nil {
		b, _, err = s.docs.load(uid, "portfolios", s.path(uid))
	} else {
		b, err = os.ReadFile(s.path(uid))
	}
	if err != nil {
		if !os.IsNotExist(err) {
			bucket.err = fmt.Errorf("read portfolios: %w", err)
		}
		s.cache[uid] = bucket
		return bucket, bucket.err
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &bucket.items); err != nil {
			bucket.err = fmt.Errorf("decode portfolios: %w", err)
		}
	}
	if bucket.items == nil {
		bucket.items = []Portfolio{}
	}
	s.cache[uid] = bucket
	return bucket, bucket.err
}

func (s *PortfolioStore) persistLocked(uid string, bucket *portfolioBucket) error {
	if bucket == nil || bucket.err != nil {
		return errors.New("portfolio store is unreadable")
	}
	if s.docs != nil {
		return s.docs.save(uid, "portfolios", bucket.items)
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

func clonePortfolios(src []Portfolio) []Portfolio {
	// JSON round-trip keeps nested slices/pointers isolated without a fragile manual copy routine.
	b, _ := json.Marshal(src)
	out := []Portfolio{}
	_ = json.Unmarshal(b, &out)
	return out
}

func (s *PortfolioStore) List(uid string) ([]Portfolio, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, err := s.loadLocked(uid)
	if err != nil {
		return nil, err
	}
	out := clonePortfolios(bucket.items)
	sort.Slice(out, func(i, j int) bool {
		if out[i].UpdatedAt != out[j].UpdatedAt {
			return out[i].UpdatedAt > out[j].UpdatedAt
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (s *PortfolioStore) Get(uid, id string) (Portfolio, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, err := s.loadLocked(uid)
	if err != nil {
		return Portfolio{}, false, err
	}
	for _, portfolio := range bucket.items {
		if portfolio.ID == id {
			return clonePortfolios([]Portfolio{portfolio})[0], true, nil
		}
	}
	return Portfolio{}, false, nil
}

func (s *PortfolioStore) Add(uid string, portfolio Portfolio) (Portfolio, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, err := s.loadLocked(uid)
	if err != nil {
		return Portfolio{}, err
	}
	now := time.Now().Unix()
	portfolio.ID = "pf_" + newID()
	portfolio.SchemaVersion = portfolioSchemaVersion
	portfolio.Version = 1
	portfolio.CreatedAt = now
	portfolio.UpdatedAt = now
	bucket.items = append(bucket.items, portfolio)
	if err := s.persistLocked(uid, bucket); err != nil {
		bucket.items = bucket.items[:len(bucket.items)-1]
		return Portfolio{}, err
	}
	return clonePortfolios([]Portfolio{portfolio})[0], nil
}

func (s *PortfolioStore) Replace(uid string, portfolio Portfolio) (Portfolio, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, err := s.loadLocked(uid)
	if err != nil {
		return Portfolio{}, false, err
	}
	for i := range bucket.items {
		if bucket.items[i].ID != portfolio.ID {
			continue
		}
		prior := bucket.items[i]
		portfolio.ID = prior.ID
		portfolio.SchemaVersion = portfolioSchemaVersion
		portfolio.CreatedAt = prior.CreatedAt
		portfolio.UpdatedAt = time.Now().Unix()
		portfolio.Version = prior.Version + 1
		bucket.items[i] = portfolio
		if err := s.persistLocked(uid, bucket); err != nil {
			bucket.items[i] = prior
			return Portfolio{}, false, err
		}
		return clonePortfolios([]Portfolio{portfolio})[0], true, nil
	}
	return Portfolio{}, false, nil
}

func (s *PortfolioStore) Delete(uid, id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, err := s.loadLocked(uid)
	if err != nil {
		return false, err
	}
	for i := range bucket.items {
		if bucket.items[i].ID != id {
			continue
		}
		prior := append([]Portfolio(nil), bucket.items...)
		bucket.items = append(bucket.items[:i], bucket.items[i+1:]...)
		if err := s.persistLocked(uid, bucket); err != nil {
			bucket.items = prior
			return false, err
		}
		return true, nil
	}
	return false, nil
}
