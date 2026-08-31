package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Store persists the trade journal. Production uses PostgreSQL; the file fields implement the
// explicit no-DATABASE_URL local/test fallback and preserve the pre-cutover layout.
type Store struct {
	base   string
	mu     sync.Mutex
	cache  map[string][]Trade
	db     *sql.DB
	table  string
	schema string
}

const legacyBucket = "_legacy"

// openStore intentionally opens the file backend. Tests use it directly; main uses
// openStoreWithDatabase so a configured production database can never silently fall back.
func openStore(base string) (*Store, error) {
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, err
	}
	s := &Store{base: base, cache: map[string][]Trade{}}
	s.migrateLegacy()
	return s, nil
}

func openStoreWithDatabase(base, databaseURL, schema string) (*Store, error) {
	if databaseURL == "" {
		return openStore(base)
	}
	return openPostgresTradeStore(base, databaseURL, schema)
}

func (s *Store) userDir(uid string) string { return filepath.Join(s.base, uid) }
func (s *Store) path(uid string) string    { return filepath.Join(s.base, uid, "trades.json") }

// migrateLegacy moves a pre-auth {base}/trades.json into an unattributed bucket once.
func (s *Store) migrateLegacy() {
	old := filepath.Join(s.base, "trades.json")
	if _, err := os.Stat(old); err != nil {
		return
	}
	if _, err := os.Stat(s.path(legacyBucket)); err == nil {
		return
	}
	if err := os.MkdirAll(s.userDir(legacyBucket), 0o755); err != nil {
		return
	}
	if err := os.Rename(old, s.path(legacyBucket)); err == nil {
		log.Printf("journal: migrated pre-auth trades.json -> %s (legacy bucket, unattributed)", s.path(legacyBucket))
	}
}

// loadUser returns a copy-on-write source slice. Caller holds s.mu.
func (s *Store) loadUser(uid string) ([]Trade, error) {
	if t, ok := s.cache[uid]; ok {
		return t, nil
	}
	trades := []Trade{}
	b, err := os.ReadFile(s.path(uid))
	if err == nil {
		if err := json.Unmarshal(b, &trades); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	s.cache[uid] = trades
	return trades, nil
}

// persistLocked writes {uid}/trades.json via temp file + rename. Caller holds s.mu.
func (s *Store) persistLocked(uid string) error {
	if err := os.MkdirAll(s.userDir(uid), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s.cache[uid], "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path(uid) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(uid))
}

func (s *Store) Check(ctx context.Context) error {
	if s.db == nil {
		return nil
	}
	return s.db.PingContext(ctx)
}

func (s *Store) List(uid string) ([]Trade, error) {
	if s.db != nil {
		return s.listPostgres(uid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	src, err := s.loadUser(uid)
	if err != nil {
		return nil, err
	}
	out := make([]Trade, len(src))
	copy(out, src)
	return out, nil
}

func (s *Store) Add(uid string, t Trade) (Trade, error) {
	now := time.Now().Unix()
	t.ID = newID()
	t.CreatedAt = now
	t.UpdatedAt = now
	if s.db != nil {
		return t, s.addPostgres(uid, t)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	trades, err := s.loadUser(uid)
	if err != nil {
		return Trade{}, err
	}
	before := append([]Trade(nil), trades...)
	s.cache[uid] = append(trades, t)
	if err := s.persistLocked(uid); err != nil {
		s.cache[uid] = before
		return Trade{}, err
	}
	return t, nil
}

func (s *Store) Get(uid, id string) (Trade, bool, error) {
	if s.db != nil {
		return s.getPostgres(uid, id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	trades, err := s.loadUser(uid)
	if err != nil {
		return Trade{}, false, err
	}
	for _, t := range trades {
		if t.ID == id {
			return t, true, nil
		}
	}
	return Trade{}, false, nil
}

func (s *Store) Replace(uid string, t Trade) (Trade, bool, error) {
	if s.db != nil {
		return s.replacePostgres(uid, t)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	trades, err := s.loadUser(uid)
	if err != nil {
		return Trade{}, false, err
	}
	for i := range trades {
		if trades[i].ID == t.ID {
			before := append([]Trade(nil), trades...)
			t.CreatedAt = trades[i].CreatedAt
			t.UpdatedAt = time.Now().Unix()
			trades[i] = t
			s.cache[uid] = trades
			if err := s.persistLocked(uid); err != nil {
				s.cache[uid] = before
				return Trade{}, false, err
			}
			return t, true, nil
		}
	}
	return Trade{}, false, nil
}

func (s *Store) Delete(uid, id string) (bool, error) {
	if s.db != nil {
		return s.deletePostgres(uid, id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	trades, err := s.loadUser(uid)
	if err != nil {
		return false, err
	}
	for i := range trades {
		if trades[i].ID == id {
			before := append([]Trade(nil), trades...)
			s.cache[uid] = append(trades[:i], trades[i+1:]...)
			if err := s.persistLocked(uid); err != nil {
				s.cache[uid] = before
				return false, err
			}
			return true, nil
		}
	}
	return false, nil
}
