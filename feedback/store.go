package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Store keeps the feedback working set. PostgreSQL is authoritative in production; feedback.json
// is the explicit no-database fallback and one-time legacy import source.

type Store struct {
	dir   string
	mu    sync.Mutex
	items []Feedback
	db    *sql.DB
	table string
}

func openStore(dir string) (*Store, error) {
	return openStoreWithDatabase(dir, "", "")
}

func openStoreWithDatabase(dir, databaseURL, schema string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if databaseURL != "" {
		return openPostgresStore(dir, databaseURL, schema)
	}
	s := &Store{dir: dir}
	s.load()
	return s, nil
}

func (s *Store) Check(ctx context.Context) error {
	if s.db == nil {
		return nil
	}
	return s.db.PingContext(ctx)
}

func (s *Store) path() string { return filepath.Join(s.dir, "feedback.json") }

func (s *Store) load() {
	b, err := os.ReadFile(s.path())
	if err != nil {
		return // no file yet -> empty
	}
	var items []Feedback
	if json.Unmarshal(b, &items) == nil {
		s.items = items
	}
}

// persistLocked writes feedback.json via temp file + rename. Caller must hold s.mu.
func (s *Store) persistLocked() error {
	if s.db != nil {
		return s.persistPostgresLocked()
	}
	b, err := json.MarshalIndent(s.items, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path())
}

// List returns a copy, NEWEST first.
func (s *Store) List() []Feedback {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Feedback, len(s.items))
	copy(out, s.items)
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func (s *Store) Add(f Feedback) Feedback {
	created, _ := s.AddE(f)
	return created
}

func (s *Store) AddE(f Feedback) (Feedback, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().Unix()
	f.ID = newID()
	f.CreatedAt = now
	f.UpdatedAt = now
	f.Status = "new"
	s.items = append(s.items, f)
	if err := s.persistLocked(); err != nil {
		s.items = s.items[:len(s.items)-1]
		return Feedback{}, err
	}
	return f, nil
}

func (s *Store) Get(id string) (Feedback, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.items {
		if s.items[i].ID == id {
			return s.items[i], true
		}
	}
	return Feedback{}, false
}

// Replace overwrites the item with matching ID (already validated by the caller) and persists.
func (s *Store) Replace(f Feedback) (Feedback, bool) {
	updated, ok, _ := s.ReplaceE(f)
	return updated, ok
}

func (s *Store) ReplaceE(f Feedback) (Feedback, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.items {
		if s.items[i].ID == f.ID {
			prior := s.items[i]
			f.CreatedAt = s.items[i].CreatedAt // preserve
			f.UpdatedAt = time.Now().Unix()
			s.items[i] = f
			if err := s.persistLocked(); err != nil {
				s.items[i] = prior
				return Feedback{}, false, err
			}
			return f, true, nil
		}
	}
	return Feedback{}, false, nil
}

func (s *Store) Delete(id string) bool {
	deleted, _ := s.DeleteE(id)
	return deleted
}

func (s *Store) DeleteE(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.items {
		if s.items[i].ID == id {
			before := append([]Feedback(nil), s.items...)
			s.items = append(s.items[:i], s.items[i+1:]...)
			if err := s.persistLocked(); err != nil {
				s.items = before
				return false, err
			}
			return true, nil
		}
	}
	return false, nil
}
