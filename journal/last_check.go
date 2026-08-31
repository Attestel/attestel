package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// last_check.go — the SERVER-SIDE "when did you last look at what changed" boundary.
// Wave 5 Lane 5B, extending §2.3's per-user event state.
//
// WHY IT MOVED HERE. Wave 4 Lane 4A put it in `localStorage`
// (`web/src/components/events/lastCheck.js`) and said in the same breath that this was a limitation
// rather than a design: the boundary does not survive a new device, a cleared profile or a second
// browser, so "since your last check" silently becomes "in the last 24 hours" for a returning user
// on a second machine. 4A's handoff, GAPS.md and the Wave 4 integration report all recorded the
// same thing — the server-side home is journal's `user_event_state` (§2.3), and moving it is a
// CONTRACT AMENDMENT rather than a lane decision. 5B lands `/api/changed`, which is the surface
// that reads the boundary, so this is the wave that owes the move.
//
// WHAT IT IS, EXACTLY: one unix second per user. Not per event, not per ticker, not per surface.
// It lives beside `event_state.json` in the same partition, so D-06 still holds — `rm -rf
// {TRADES_DIR}/{uid}/` removes it with everything else of theirs.
//
// THE COPY RULE SURVIVES THE MOVE, AND THAT IS THE POINT. A default 24-hour window may NEVER be
// labelled "since your last check" (Wave 4 Lane 4A's locked decision 6). Server-side, that means:
//
//   - `GET` returns `{"lastCheck": null}` when the user has never recorded one. NULL, not "now",
//     not "24 hours ago" — a boundary the server invented would be indistinguishable at the client
//     from one the user earned, and the heading would then lie.
//   - The client omits `since` when it is null, and `/api/changed` answers `basis: "default24h"`,
//     which is the field the heading turns on.
//
// IT IS NEVER ADVANCED BY A READ. `GET` does not stamp; only an explicit `POST` does, and the
// client sends it only once the surface has actually rendered an answer. Advancing the boundary on
// a read would silently skip past events the user never saw, which is the one failure the whole
// object exists to prevent.

const lastCheckFile = "last_check.json"

// lastCheckRecord is deliberately a struct rather than a bare int, so a later field (a per-surface
// boundary, say) is an additive change rather than a format migration.
type lastCheckRecord struct {
	At        int64 `json:"at"`
	UpdatedAt int64 `json:"updatedAt"`
}

type lastCheckStore struct {
	base string
	mu   sync.Mutex
	docs *documentRepository
}

func openLastCheckStore(base string) (*lastCheckStore, error) {
	return openLastCheckStoreWithRepository(base, nil)
}

func openLastCheckStoreWithRepository(base string, docs *documentRepository) (*lastCheckStore, error) {
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, err
	}
	return &lastCheckStore{base: base, docs: docs}, nil
}

func (s *lastCheckStore) path(uid string) string {
	return filepath.Join(s.base, uid, lastCheckFile)
}

// get returns `(at, ok)`. `ok` is false when the user has never recorded a boundary — the state the
// copy rule turns on, and the reason this is not a bare `int64` return.
func (s *lastCheckStore) get(uid string) (int64, bool) {
	if reservedUID(uid) {
		return 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var rec lastCheckRecord
	var b []byte
	var err error
	if s.docs != nil {
		b, _, err = s.docs.load(uid, "last_check", s.path(uid))
	} else {
		b, err = os.ReadFile(s.path(uid))
	}
	if err != nil || json.Unmarshal(b, &rec) != nil || rec.At <= 0 {
		return 0, false
	}
	return rec.At, true
}

// set records the boundary. MONOTONIC: a later stamp wins and an earlier one is ignored rather than
// applied. Two tabs racing must not move a user's reading position BACKWARDS — that would re-show
// events they had already been shown, which erodes the same trust as skipping them.
func (s *lastCheckStore) set(uid string, at int64) (int64, error) {
	if reservedUID(uid) {
		return 0, errSubscriptionReservedUID
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	current := lastCheckRecord{}
	var b []byte
	var loadErr error
	if s.docs != nil {
		b, _, loadErr = s.docs.load(uid, "last_check", s.path(uid))
	} else {
		b, loadErr = os.ReadFile(s.path(uid))
	}
	if loadErr == nil {
		_ = json.Unmarshal(b, &current)
	}
	if at <= current.At {
		return current.At, nil
	}
	next := lastCheckRecord{At: at, UpdatedAt: time.Now().Unix()}
	if s.docs != nil {
		if err := s.docs.save(uid, "last_check", next); err != nil {
			return current.At, err
		}
		return next.At, nil
	}
	if err := os.MkdirAll(filepath.Join(s.base, uid), 0o755); err != nil {
		return current.At, err
	}
	b, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return current.At, err
	}
	tmp := s.path(uid) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return current.At, err
	}
	if err := os.Rename(tmp, s.path(uid)); err != nil {
		return current.At, err
	}
	return next.At, nil
}

// ---- HTTP surface ----

// init attaches the pair through the same Wave 0 seam `event_state.go` uses.
func init() {
	registerSubscriptionRoute(func(s *Server, mux *http.ServeMux) {
		s.registerLastCheckAPI(mux)
	})
}

func (s *Server) registerLastCheckAPI(mux *http.ServeMux) {
	store, err := openLastCheckStoreWithRepository(s.cfg.TradesDir, s.documents)
	if err != nil {
		// Additive: the journal must still serve everything else. The routes are simply not
		// mounted and the client falls back to its local boundary, which is the pre-Wave-5
		// behaviour and is honest about itself.
		return
	}

	// Both require a session. A guest has no partition, and a guest boundary is exactly what
	// `localStorage` is still for.
	mux.HandleFunc("GET /event-state/last-check", s.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		at, ok := store.get(userID(r))
		if !ok {
			// NULL, not a substituted default. See the header: a server-invented boundary would be
			// indistinguishable at the client from one the user earned.
			writeJSON(w, http.StatusOK, map[string]any{"lastCheck": nil})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"lastCheck": at})
	}))

	mux.HandleFunc("POST /event-state/last-check", s.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			At *int64 `json:"at"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		at := time.Now().Unix()
		if body.At != nil && *body.At > 0 {
			at = *body.At
		}
		stored, err := store.set(userID(r), at)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": err.Error(), "code": "last_check_unwritable"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"lastCheck": stored})
	}))
}
