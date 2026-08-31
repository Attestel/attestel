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

const maxReviewsPerPortfolio = 100

type reviewBucket struct {
	loaded bool
	items  []PortfolioReview
	err    error
}

type PortfolioReviewStore struct {
	base  string
	mu    sync.Mutex
	cache map[string]*reviewBucket
	docs  *documentRepository
}

func openPortfolioReviewStore(base string) (*PortfolioReviewStore, error) {
	return openPortfolioReviewStoreWithRepository(base, nil)
}

func openPortfolioReviewStoreWithRepository(base string, docs *documentRepository) (*PortfolioReviewStore, error) {
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, err
	}
	return &PortfolioReviewStore{base: base, cache: map[string]*reviewBucket{}, docs: docs}, nil
}

func (s *PortfolioReviewStore) path(uid string) string {
	return filepath.Join(s.base, uid, "portfolio_reviews.json")
}

func (s *PortfolioReviewStore) loadLocked(uid string) (*reviewBucket, error) {
	if uid == "" {
		return nil, errors.New("user id is required")
	}
	if bucket, ok := s.cache[uid]; ok && bucket.loaded {
		return bucket, bucket.err
	}
	bucket := &reviewBucket{loaded: true, items: []PortfolioReview{}}
	var b []byte
	var err error
	if s.docs != nil {
		b, _, err = s.docs.load(uid, "portfolio_reviews", s.path(uid))
	} else {
		b, err = os.ReadFile(s.path(uid))
	}
	if err != nil {
		if !os.IsNotExist(err) {
			bucket.err = fmt.Errorf("read portfolio reviews: %w", err)
		}
		s.cache[uid] = bucket
		return bucket, bucket.err
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &bucket.items); err != nil {
			bucket.err = fmt.Errorf("decode portfolio reviews: %w", err)
		}
	}
	if bucket.items == nil {
		bucket.items = []PortfolioReview{}
	}
	s.cache[uid] = bucket
	return bucket, bucket.err
}

func (s *PortfolioReviewStore) persistLocked(uid string, bucket *reviewBucket) error {
	if bucket == nil || bucket.err != nil {
		return errors.New("portfolio review store is unreadable")
	}
	if s.docs != nil {
		return s.docs.save(uid, "portfolio_reviews", bucket.items)
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

func clonePortfolioReviews(src []PortfolioReview) []PortfolioReview {
	b, _ := json.Marshal(src)
	out := []PortfolioReview{}
	_ = json.Unmarshal(b, &out)
	return out
}

func (s *PortfolioReviewStore) List(uid, portfolioID string, limit int) ([]PortfolioReview, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, err := s.loadLocked(uid)
	if err != nil {
		return nil, err
	}
	out := []PortfolioReview{}
	// Append order is authoritative when two reviews share one second; ids are random.
	for i := len(bucket.items) - 1; i >= 0; i-- {
		review := bucket.items[i]
		if review.PortfolioID == portfolioID {
			out = append(out, review)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return clonePortfolioReviews(out), nil
}

func (s *PortfolioReviewStore) ByContext(uid, portfolioID, contextVersion string) (PortfolioReview, bool, error) {
	items, err := s.List(uid, portfolioID, maxReviewsPerPortfolio)
	if err != nil {
		return PortfolioReview{}, false, err
	}
	for _, review := range items {
		if review.ContextVersion == contextVersion {
			return review, true, nil
		}
	}
	return PortfolioReview{}, false, nil
}

func (s *PortfolioReviewStore) Add(uid string, review PortfolioReview) (PortfolioReview, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, err := s.loadLocked(uid)
	if err != nil {
		return PortfolioReview{}, err
	}
	prior := append([]PortfolioReview(nil), bucket.items...)
	bucket.items = append(bucket.items, review)
	indices := []int{}
	for i, row := range bucket.items {
		if row.PortfolioID == review.PortfolioID {
			indices = append(indices, i)
		}
	}
	if len(indices) > maxReviewsPerPortfolio {
		sort.Slice(indices, func(i, j int) bool {
			a, b := bucket.items[indices[i]], bucket.items[indices[j]]
			if a.CreatedAt != b.CreatedAt {
				return a.CreatedAt < b.CreatedAt
			}
			return a.ID < b.ID
		})
		remove := map[int]bool{}
		for _, index := range indices[:len(indices)-maxReviewsPerPortfolio] {
			remove[index] = true
		}
		kept := make([]PortfolioReview, 0, len(bucket.items)-len(remove))
		for i, row := range bucket.items {
			if !remove[i] {
				kept = append(kept, row)
			}
		}
		bucket.items = kept
	}
	if err := s.persistLocked(uid, bucket); err != nil {
		bucket.items = prior
		return PortfolioReview{}, err
	}
	return clonePortfolioReviews([]PortfolioReview{review})[0], nil
}
