package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Store keeps a small in-process working set. PostgreSQL is authoritative in production; the JSON
// files are the explicit no-database fallback and the one-time import source during cutover.

type Store struct {
	dir string
	mu  sync.Mutex

	rules          []Rule
	readWatermarks map[string]int64           // per user: events with ts <= this are "read" for that user
	readIDs        map[string]map[string]bool // per user: individually marked-read event ids (click-to-read)
	db             *sql.DB
	schema         string
	lastErr        error
}

type persistedState struct {
	ReadWatermarks map[string]int64           `json:"readWatermarks"`
	ReadIDs        map[string]map[string]bool `json:"readIds"`
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
	s := &Store{dir: dir, readWatermarks: map[string]int64{}, readIDs: map[string]map[string]bool{}}
	s.loadRules()
	s.migrateLegacyRules()
	s.loadState()
	return s, nil
}

func (s *Store) Check(ctx context.Context) error {
	if s.db == nil {
		return nil
	}
	return s.db.PingContext(ctx)
}

// migrateLegacyRules reassigns any pre-auth rule (empty UserID, from before accounts existed) to a
// "_legacy" owner so it isn't served to guests (uid ""). No real account has that id, so these rules
// keep being evaluated but become invisible/immutable — preserved, not silently dropped or leaked.
func (s *Store) migrateLegacyRules() {
	s.mu.Lock()
	defer s.mu.Unlock()
	before := append([]Rule(nil), s.rules...)
	changed := false
	for i := range s.rules {
		if s.rules[i].UserID == "" {
			s.rules[i].UserID = "_legacy"
			changed = true
		}
	}
	if changed {
		if err := s.persistRulesLocked(); err != nil {
			s.rules = before
			s.lastErr = err
		}
	}
}

func (s *Store) rulesPath() string  { return filepath.Join(s.dir, "rules.json") }
func (s *Store) eventsPath() string { return filepath.Join(s.dir, "events.jsonl") }
func (s *Store) statePath() string  { return filepath.Join(s.dir, "state.json") }

func (s *Store) loadRules() {
	b, err := os.ReadFile(s.rulesPath())
	if err != nil {
		return // no file yet -> empty rule set
	}
	var rules []Rule
	if json.Unmarshal(b, &rules) == nil {
		s.rules = rules
	}
}

func (s *Store) loadState() {
	b, err := os.ReadFile(s.statePath())
	if err != nil {
		return
	}
	var st persistedState
	if json.Unmarshal(b, &st) == nil {
		if st.ReadWatermarks != nil {
			s.readWatermarks = st.ReadWatermarks
		}
		if st.ReadIDs != nil {
			s.readIDs = st.ReadIDs
		}
	}
}

// persistStateLocked writes state.json (read watermarks + per-event read ids). Caller holds s.mu.
func (s *Store) persistStateLocked(uid string) error {
	if s.db != nil {
		return s.persistStatePostgresLocked(uid)
	}
	b, _ := json.Marshal(persistedState{ReadWatermarks: s.readWatermarks, ReadIDs: s.readIDs})
	return writeFileAtomic(s.statePath(), b)
}

// writeFileAtomic writes via a temp file + rename so a crash mid-write can't corrupt the JSON.
func writeFileAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// persistRulesLocked writes rules.json. Caller must hold s.mu.
func (s *Store) persistRulesLocked() error {
	if s.db != nil {
		return s.persistRulesPostgresLocked()
	}
	b, err := json.MarshalIndent(s.rules, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.rulesPath(), b)
}

// ---- rules API ----

// Rules returns ALL rules — used by the evaluator, which must keep evaluating every user's rules.
func (s *Store) Rules() []Rule {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Rule, len(s.rules))
	copy(out, s.rules)
	return out
}

// RulesForUser returns only the caller's rules (for the API — a user never sees another's rules).
func (s *Store) RulesForUser(uid string) []Rule {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Rule{}
	for _, r := range s.rules {
		if r.UserID == uid {
			out = append(out, r)
		}
	}
	return out
}

func (s *Store) GetRule(id string) (Rule, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.rules {
		if s.rules[i].ID == id {
			return s.rules[i], true
		}
	}
	return Rule{}, false
}

func (s *Store) AddRule(r Rule) Rule {
	created, _ := s.AddRuleE(r)
	return created
}

func (s *Store) AddRuleE(r Rule) (Rule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.ID = newID()
	r.CreatedAt = time.Now().Unix()
	r.LastTriggered = 0
	r.LastState = nil
	s.rules = append(s.rules, r)
	if err := s.persistRulesLocked(); err != nil {
		s.rules = s.rules[:len(s.rules)-1]
		s.lastErr = err
		return Rule{}, err
	}
	return r, nil
}

// ReplaceRule writes an already-VALIDATED rule over the stored one, preserving the fields the server
// owns: identity, owner, creation time, and (unless the caller deliberately re-baselined it) the
// edge-trigger memory. Used by PATCH, which validates a simulated merge first so a bad edit is
// refused before anything is persisted.
func (s *Store) ReplaceRule(id string, next Rule) (Rule, bool) {
	replaced, ok, _ := s.ReplaceRuleE(id, next)
	return replaced, ok
}

func (s *Store) ReplaceRuleE(id string, next Rule) (Rule, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.rules {
		if s.rules[i].ID != id {
			continue
		}
		cur := s.rules[i]
		next.ID = cur.ID
		next.UserID = cur.UserID
		next.CreatedAt = cur.CreatedAt
		next.LastTriggered = cur.LastTriggered
		s.rules[i] = next
		if err := s.persistRulesLocked(); err != nil {
			s.rules[i] = cur
			s.lastErr = err
			return Rule{}, false, err
		}
		return s.rules[i], true, nil
	}
	return Rule{}, false, nil
}

func (s *Store) DeleteRule(id string) bool {
	deleted, _ := s.DeleteRuleE(id)
	return deleted
}

func (s *Store) DeleteRuleE(id string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.rules {
		if s.rules[i].ID == id {
			before := append([]Rule(nil), s.rules...)
			s.rules = append(s.rules[:i], s.rules[i+1:]...)
			if err := s.persistRulesLocked(); err != nil {
				s.rules = before
				s.lastErr = err
				return false, err
			}
			return true, nil
		}
	}
	return false, nil
}

// evalUpdate carries the per-rule state changes produced by one evaluation pass.
type evalUpdate struct {
	lastTriggered int64
	lastState     any
}

// ApplyEvalUpdates merges evaluation results back onto the live rules by ID (so a concurrent
// create/delete during the pass isn't clobbered) and persists once.
func (s *Store) ApplyEvalUpdates(updates map[string]evalUpdate) {
	_ = s.ApplyEvalUpdatesE(updates)
}

func (s *Store) ApplyEvalUpdatesE(updates map[string]evalUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	before := append([]Rule(nil), s.rules...)
	changed := false
	for i := range s.rules {
		if u, ok := updates[s.rules[i].ID]; ok {
			s.rules[i].LastTriggered = u.lastTriggered
			s.rules[i].LastState = u.lastState
			changed = true
		}
	}
	if changed {
		if err := s.persistRulesLocked(); err != nil {
			s.rules = before
			s.lastErr = err
			return err
		}
	}
	return nil
}

// ---- events API ----

// AppendEvent writes one event as a JSON line. Append-only; never rewritten.
func (s *Store) AppendEvent(ev Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db != nil {
		err := s.appendEventPostgres(ev)
		s.lastErr = err
		return err
	}
	f, err := os.OpenFile(s.eventsPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

// ListEvents returns up to `limit` most-recent events FOR THE CALLER (newest first) plus that
// user's unread count. `read` is derived from the user's watermark. Malformed lines are skipped.
func (s *Store) ListEvents(uid string, limit int) ([]Event, int) {
	events, unread, _ := s.ListEventsE(uid, limit)
	return events, unread
}

func (s *Store) ListEventsE(uid string, limit int) ([]Event, int, error) {
	s.mu.Lock()
	watermark := s.readWatermarks[uid]
	readSet := make(map[string]bool, len(s.readIDs[uid]))
	for id, read := range s.readIDs[uid] {
		readSet[id] = read
	}
	s.mu.Unlock()

	if s.db != nil {
		all, err := s.listEventsPostgres(uid)
		if err != nil {
			s.lastErr = err
			return []Event{}, 0, err
		}
		return finishEvents(all, watermark, readSet, limit), countUnread(all, watermark, readSet), nil
	}
	f, err := os.Open(s.eventsPath())
	if err != nil {
		return []Event{}, 0, nil
	}
	defer f.Close()

	var all []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev Event
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		if ev.UserID != uid {
			continue // only the caller's events
		}
		all = append(all, ev)
	}

	unread := countUnread(all, watermark, readSet)
	for i := range all {
		all[i].Read = all[i].TS <= watermark || readSet[all[i].ID]
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].TS > all[j].TS })
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	if all == nil {
		all = []Event{}
	}
	return all, unread, nil
}

func countUnread(events []Event, watermark int64, readSet map[string]bool) int {
	n := 0
	for _, event := range events {
		if event.TS > watermark && !readSet[event.ID] {
			n++
		}
	}
	return n
}
func finishEvents(events []Event, watermark int64, readSet map[string]bool, limit int) []Event {
	for i := range events {
		events[i].Read = events[i].TS <= watermark || readSet[events[i].ID]
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].TS > events[j].TS })
	if limit > 0 && len(events) > limit {
		events = events[:limit]
	}
	if events == nil {
		return []Event{}
	}
	return events
}

// MarkEventsRead advances the CALLER'S read watermark to now, so their existing events count as read.
func (s *Store) MarkEventsRead(uid string) {
	_ = s.MarkEventsReadE(uid)
}

func (s *Store) MarkEventsReadE(uid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	previous, existed := s.readWatermarks[uid]
	s.readWatermarks[uid] = time.Now().Unix()
	if err := s.persistStateLocked(uid); err != nil {
		if existed {
			s.readWatermarks[uid] = previous
		} else {
			delete(s.readWatermarks, uid)
		}
		s.lastErr = err
		return err
	}
	return nil
}

// MarkEventRead marks a SINGLE event read for the caller (adds its id to that user's read set), so
// clicking one notification clears just that one. Idempotent; other users are unaffected.
func (s *Store) MarkEventRead(uid, id string) {
	_ = s.MarkEventReadE(uid, id)
}

func (s *Store) MarkEventReadE(uid, id string) error {
	if id == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	set := s.readIDs[uid]
	createdSet := false
	if set == nil {
		set = map[string]bool{}
		s.readIDs[uid] = set
		createdSet = true
	}
	previous, existed := set[id]
	set[id] = true
	if err := s.persistStateLocked(uid); err != nil {
		if existed {
			set[id] = previous
		} else {
			delete(set, id)
		}
		if createdSet && len(set) == 0 {
			delete(s.readIDs, uid)
		}
		s.lastErr = err
		return err
	}
	return nil
}
