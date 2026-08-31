package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// event_state.go — what ONE user has done with a GLOBAL event. Contract: EVENT_CONTRACTS.md §2.3.
//
// The events themselves live in `services/events` and belong to nobody: one row per event for the
// whole system, deduplicated across providers. "I have read this", "I saved this" and "I dismissed
// this" are the opposite kind of fact — they are the user's, they are erasable, and there is one per
// (user, event) pair. So they live here, in the user's own partition.
//
// The single rule that makes this file safe is what it does NOT store:
//
//	{ "eventId": …, "read": …, "saved": …, "dismissed": …, "updatedAt": … }
//
// and nothing else. No title, no summary, no URL, no importance, no ticker. Copying any of those
// would create a second source of truth for a global object that legitimately mutates as more
// documents join its cluster — and would turn a user's partition into a store of content that
// D-06's `rm -rf {TRADES_DIR}/{uid}/` was never meant to be the only copy of. An event id is a
// pointer; a cached headline is a liability.
//
// ABSENCE IS NOT AN ERROR. An event a user has never touched simply has no row. The client treats a
// missing id as unread, unsaved and undismissed, which is why `GET` omits unknown ids rather than
// returning a skeleton for each — a skeleton would be indistinguishable from a real "read: false"
// the user actually set, and it would grow the response to the size of the feed.

const eventStateFile = "event_state.json"

// maxEventStateIDs bounds one batch read. It is the same bound `gateway/changes.go` already uses for
// `maxChangeItems`, so the two do not drift into two different ideas of "a page of events".
// Exceeding it is a 400, never a silent truncation: a caller that asked about 500 events and got 200
// answers would record 300 events as unread that the user had read.
const maxEventStateIDs = 200

// eventIDPattern is the §Conventions id shape: `evt_` + 16 lowercase hex.
var eventIDPattern = regexp.MustCompile(`^evt_[0-9a-f]{16}$`)

// EventState is one user's relationship to one global event. Field order mirrors §2.3.
type EventState struct {
	EventID   string `json:"eventId"`
	Read      bool   `json:"read"`
	Saved     bool   `json:"saved"`
	Dismissed bool   `json:"dismissed"`
	UpdatedAt int64  `json:"updatedAt"` // unix seconds
}

// ---- store ----

// eventStateStore mirrors `subscriptionStore` exactly — same idiom, same locking, same
// refuse-to-clobber guard — because the journal has one storage pattern and this is it.
type eventStateStore struct {
	base    string
	mu      sync.Mutex
	states  map[string][]EventState
	loadErr map[string]error
	docs    *documentRepository
}

func openEventStateStore(base string) (*eventStateStore, error) {
	return openEventStateStoreWithRepository(base, nil)
}

func openEventStateStoreWithRepository(base string, docs *documentRepository) (*eventStateStore, error) {
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, err
	}
	return &eventStateStore{
		base:    base,
		states:  map[string][]EventState{},
		loadErr: map[string]error{},
		docs:    docs,
	}, nil
}

func (s *eventStateStore) path(uid string) string {
	return filepath.Join(s.base, uid, eventStateFile)
}

func (s *eventStateStore) loadLocked(uid string) []EventState {
	if v, ok := s.states[uid]; ok {
		return v
	}
	states := []EventState{}
	var err error
	if s.docs != nil {
		var payload []byte
		payload, _, err = s.docs.load(uid, "event_state", s.path(uid))
		if err == nil && len(payload) > 0 {
			err = json.Unmarshal(payload, &states)
		}
	} else {
		_, err = loadJSON(s.path(uid), &states)
	}
	if err != nil {
		s.loadErr[uid] = err
		states = []EventState{}
	}
	s.states[uid] = states
	return states
}

func (s *eventStateStore) writableLocked(uid string) bool {
	s.loadLocked(uid)
	return s.loadErr[uid] == nil
}

func (s *eventStateStore) persistLocked(uid string, v []EventState) error {
	if s.docs != nil {
		if err := s.docs.save(uid, "event_state", v); err != nil {
			delete(s.states, uid)
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Join(s.base, uid), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path(uid) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(uid))
}

// get returns the stored states for the requested ids, in the order they were asked for, skipping
// ids with no stored state. Read-only: asking about an event does not create a row for it.
func (s *eventStateStore) get(uid string, ids []string) ([]EventState, error) {
	if reservedUID(uid) {
		return nil, errSubscriptionReservedUID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stored := s.loadLocked(uid)
	if s.loadErr[uid] != nil {
		return nil, errSubscriptionUnreadable
	}
	byID := make(map[string]EventState, len(stored))
	for _, st := range stored {
		byID[st.EventID] = st
	}
	out := make([]EventState, 0, len(ids))
	for _, id := range ids {
		if st, ok := byID[id]; ok {
			out = append(out, st)
		}
	}
	return out, nil
}

// upsert applies only the flags that were PRESENT in the body and always stamps `updatedAt`. A nil
// pointer leaves the stored flag alone, so "mark read" does not silently un-save.
func (s *eventStateStore) upsert(uid, eventID string, read, saved, dismissed *bool) (EventState, error) {
	if reservedUID(uid) {
		return EventState{}, errSubscriptionReservedUID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.writableLocked(uid) {
		return EventState{}, errSubscriptionUnreadable
	}
	now := time.Now().Unix()
	states := s.loadLocked(uid)
	for i := range states {
		if states[i].EventID != eventID {
			continue
		}
		if read != nil {
			states[i].Read = *read
		}
		if saved != nil {
			states[i].Saved = *saved
		}
		if dismissed != nil {
			states[i].Dismissed = *dismissed
		}
		states[i].UpdatedAt = now
		s.states[uid] = states
		if err := s.persistLocked(uid, states); err != nil {
			return EventState{}, err
		}
		return states[i], nil
	}
	// First touch: absent flags default to false, which is exactly the state the client already
	// assumed for an event with no row.
	st := EventState{EventID: eventID, UpdatedAt: now}
	if read != nil {
		st.Read = *read
	}
	if saved != nil {
		st.Saved = *saved
	}
	if dismissed != nil {
		st.Dismissed = *dismissed
	}
	s.states[uid] = append(states, st)
	if err := s.persistLocked(uid, s.states[uid]); err != nil {
		return EventState{}, err
	}
	return st, nil
}

// ---- HTTP surface ----

type eventStateAPI struct {
	srv   *Server
	store *eventStateStore
}

// init attaches this lane's second pair of routes through the same Wave 0 seam.
func init() {
	registerSubscriptionRoute(func(s *Server, mux *http.ServeMux) {
		s.registerEventStateAPI(mux)
	})
}

// registerEventStateAPI mounts §2.3's two endpoints. Both require a session: per-user event state
// has no meaning for a guest, and there is no partition to read.
func (s *Server) registerEventStateAPI(mux *http.ServeMux) {
	store, err := openEventStateStoreWithRepository(s.cfg.TradesDir, s.documents)
	if err != nil {
		log.Printf("journal: event-state store unavailable in %q: %v (event-state routes not mounted)", s.cfg.TradesDir, err)
		return
	}
	api := &eventStateAPI{srv: s, store: store}

	mux.HandleFunc("GET /event-state", s.requireAuth(api.handleGet))
	mux.HandleFunc("POST /event-state", s.requireAuth(api.handleUpsert))
}

// parseEventIDs splits and validates the `ids` query parameter. Duplicates collapse; a malformed id
// is a 400 rather than a skipped entry, because a client that mistyped an id must not be told
// "no state" — that is indistinguishable from a real answer.
func parseEventIDs(raw string) ([]string, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, ""
	}
	parts := strings.Split(raw, ",")
	if len(parts) > maxEventStateIDs {
		return nil, "too many ids"
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		id := strings.TrimSpace(p)
		if !eventIDPattern.MatchString(id) {
			return nil, "malformed event id: " + id
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out, ""
}

func (a *eventStateAPI) handleGet(w http.ResponseWriter, r *http.Request) {
	ids, problem := parseEventIDs(r.URL.Query().Get("ids"))
	if problem == "too many ids" {
		writeFollowError(w, http.StatusBadRequest, "too_many_ids",
			"at most 200 event ids per request")
		return
	}
	if problem != "" {
		writeFollowError(w, http.StatusBadRequest, "validation_failed", problem)
		return
	}
	if len(ids) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"states": []EventState{}})
		return
	}
	states, err := a.store.get(userID(r), ids)
	if err != nil {
		writeFollowStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"states": states})
}

// eventStateBody is §2.3's upsert body. Pointers again, for the same reason: `read: false` is a
// deliberate un-read and an absent `read` is "do not touch it".
type eventStateBody struct {
	EventID   string `json:"eventId"`
	Read      *bool  `json:"read"`
	Saved     *bool  `json:"saved"`
	Dismissed *bool  `json:"dismissed"`
}

func (a *eventStateAPI) handleUpsert(w http.ResponseWriter, r *http.Request) {
	var body eventStateBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeFollowError(w, http.StatusBadRequest, "invalid_json", "invalid JSON: "+err.Error())
		return
	}
	eventID := strings.TrimSpace(body.EventID)
	if eventID == "" {
		writeFollowError(w, http.StatusBadRequest, "validation_failed", "eventId is required")
		return
	}
	if !eventIDPattern.MatchString(eventID) {
		writeFollowError(w, http.StatusBadRequest, "validation_failed", "malformed event id: "+eventID)
		return
	}
	// The event's EXISTENCE is not checked here, and that is deliberate: journal makes no outbound
	// call of any kind (invariant #1/#2 of this lane), so verifying an id would mean either an HTTP
	// hop to `services/events` or a copy of the corpus. A state row for an id that never existed is
	// inert — it is returned only to a caller that asks for that exact id.
	st, err := a.store.upsert(userID(r), eventID, body.Read, body.Saved, body.Dismissed)
	if err != nil {
		writeFollowStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"state": st})
}
