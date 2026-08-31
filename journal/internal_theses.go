package main

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"
)

// internal_theses.go — `GET /_internal/theses`, Wave 5 Lane 5B.
//
// The one question the per-user thesis endpoints structurally cannot answer: **which theses exist
// across every user, and when was each last reviewed?** Lane 5B's monitor (`alerts/
// thesis_monitor.go`) sweeps all of them on a schedule, and it cannot do that through
// `GET /theses`, which is scoped to one session.
//
// IT IS MODELLED EXACTLY ON `/_internal/tickers` (§9.4), INCLUDING THE PART THAT MATTERS MOST:
//
//   - It is refused outright when the request carries a session cookie, valid or not. An internal
//     path is not a back door onto somebody else's data, and 404 (rather than 403) keeps its
//     existence undisclosed to a browser that stumbles onto it.
//   - The gateway does not proxy it, under any prefix. `gateway/routeseams_test.go` asserts that for
//     `/_internal/tickers` and now asserts it here too.
//   - It is bound to the internal network in `docker-compose.yml` like every other service port.
//
// WHAT IT RETURNS, AND WHAT IT REFUSES TO RETURN
// ----------------------------------------------
// A NARROW, CONTENT-FREE PROJECTION: id, owner, ticker, timestamps, and the due-dates the user put
// on their catalysts. Nothing else. Specifically **not** the claim, the notes, the assumptions, the
// risks, the invalidation conditions, or any item text — none of which the monitor needs, and all of
// which are the user's research. D-13's forbidden-field list (thesis text, evidence excerpts, user
// notes, email, credentials) is about shipping content to third-party analytics; this route ships
// nothing to anybody outside the compose network, and it ships no content at all either way.
//
// `userId` IS returned, and that is the one place this route differs from `/_internal/tickers`.
// That route deliberately returns a bare SET so it is not a follow graph. Here the owner is
// load-bearing: a stale marker has to be scoped to a user, and a notification has to reach the
// person whose thesis it is. `alerts` already stores rules and events keyed by uid, so it learns no
// identity it did not already hold. It is recorded in the handoff as a clause, not slipped in.

// internalThesis is the projection. Every field on it is a timestamp, an id or a symbol.
type internalThesis struct {
	ID     string `json:"id"`
	UserID string `json:"userId"`
	Ticker string `json:"ticker"`
	Status string `json:"status"`

	CreatedAt  int64 `json:"createdAt"`
	UpdatedAt  int64 `json:"updatedAt"`
	LastReview *struct {
		At int64 `json:"at"`
	} `json:"lastReview"`

	// CatalystsDue is the monitor's second staleness signal: the user named what they were waiting
	// for and gave it a date. Dates only — never the catalyst's text.
	CatalystsDue []int64 `json:"catalystsDue"`

	// NotificationLevel is resolved from the owner's subscription for this ticker, so the monitor
	// can apply doc §16.11's matrix without a second cross-user route. "" when they follow the
	// ticker implicitly (a thesis without a follow), which `shouldNotify` reads as the shipped
	// default `material`.
	NotificationLevel string `json:"notificationLevel"`
}

// registerInternalThesesAPI mounts the route. Called from the same place the other thesis routes
// are registered; it needs the thesis store and, optionally, the subscription store.
func (s *Server) registerInternalThesesAPI(mux *http.ServeMux, theses *ThesisStore,
	levelFor func(uid, ticker string) string) {
	mux.HandleFunc("GET /_internal/theses", func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie(s.cfg.CookieName); err == nil {
			// §9.4's rule, restated: a request carrying a session is a browser, and a browser has
			// no business here whatever its session says.
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"theses": internalTheses(theses, levelFor),
		})
	})

	// The re-synthesis worker needs the CURRENT claim and a narrowly scoped way to persist one
	// system-authored check. These are separate from the content-free sweep above, require the
	// shared server secret, and are not proxied by the gateway.
	mux.HandleFunc("GET /_internal/thesis-resynth/{id}", s.handleInternalResynthGet)
	mux.HandleFunc("PATCH /_internal/thesis-resynth/{id}", s.handleInternalResynthPatch)
}

func (s *Server) internalThesisAuthorized(r *http.Request) bool {
	if _, err := r.Cookie(s.cfg.CookieName); err == nil {
		return false
	}
	want, got := []byte(s.cfg.Secret), []byte(r.Header.Get("X-Internal-Secret"))
	return len(want) == len(got) && subtle.ConstantTimeCompare(want, got) == 1
}

func (s *Server) requireInternalThesis(w http.ResponseWriter, r *http.Request) bool {
	if _, err := r.Cookie(s.cfg.CookieName); err == nil {
		http.NotFound(w, r)
		return false
	}
	if !s.internalThesisAuthorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "internal authentication required"})
		return false
	}
	return true
}

func (s *Server) handleInternalResynthGet(w http.ResponseWriter, r *http.Request) {
	if !s.requireInternalThesis(w, r) {
		return
	}
	uid := strings.TrimSpace(r.URL.Query().Get("userId"))
	if uid == "" || r.PathValue("id") == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "userId and thesis id are required"})
		return
	}
	thesis, ok := s.theses.Get(uid, r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "thesis not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"thesis": map[string]any{
		"id": thesis.ID, "ticker": thesis.Ticker, "status": thesis.Status,
		"claim": thesis.Claim, "text": thesis.Text, "lastCheck": thesis.LastCheck,
	}})
}

func (s *Server) handleInternalResynthPatch(w http.ResponseWriter, r *http.Request) {
	if !s.requireInternalThesis(w, r) {
		return
	}
	uid := strings.TrimSpace(r.URL.Query().Get("userId"))
	if uid == "" || r.PathValue("id") == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "userId and thesis id are required"})
		return
	}
	var body struct {
		LastCheck *ThesisCheck `json:"lastCheck"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON: " + err.Error()})
		return
	}
	if body.LastCheck == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "lastCheck is required"})
		return
	}
	now := time.Now().Unix()
	updated, apiErr := s.theses.Mutate(uid, r.PathValue("id"), now,
		func(cur Thesis, all []Thesis) (thesisMutation, *apiError) {
			next, err := applyThesisPatch(cur, thesisPatch{LastCheck: body.LastCheck}, all, now)
			if err != nil {
				return thesisMutation{}, err
			}
			return thesisMutation{next: next, version: false, bumpUpdated: false}, nil
		})
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"thesis": updated})
}

// internalTheses walks every user partition under the thesis base directory.
//
// The walk is over DIRECTORY NAMES, which are user ids, because `ThesisStore` is partitioned per
// user by construction (`{base}/{uid}/theses.json`) and has no cross-user list method — deliberately,
// since every other caller is a session. Adding one to the store would give the per-user API a
// cross-user door it does not need.
func internalTheses(store *ThesisStore, levelFor func(uid, ticker string) string) []internalThesis {
	out := []internalThesis{}
	if store == nil {
		return out
	}
	for _, uid := range store.UserIDs() {
		// `_legacy` holds the pre-auth, UNATTRIBUTED migration bucket. Its theses belong to nobody,
		// so there is nobody to notify and no marker to scope — it is skipped rather than attributed
		// to a user named "_legacy".
		if strings.HasPrefix(uid, "_") {
			continue
		}
		for _, t := range store.List(uid) {
			row := internalThesis{
				ID: t.ID, UserID: uid, Ticker: strings.ToUpper(t.Ticker), Status: t.Status,
				CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
				CatalystsDue: []int64{},
			}
			if t.LastReview != nil {
				row.LastReview = &struct {
					At int64 `json:"at"`
				}{At: t.LastReview.At}
			}
			for _, c := range t.Catalysts {
				if c.DueAt != nil && *c.DueAt > 0 {
					row.CatalystsDue = append(row.CatalystsDue, *c.DueAt)
				}
			}
			sort.Slice(row.CatalystsDue, func(i, j int) bool {
				return row.CatalystsDue[i] < row.CatalystsDue[j]
			})
			if levelFor != nil {
				row.NotificationLevel = levelFor(uid, row.Ticker)
			}
			out = append(out, row)
		}
	}
	// Stable order so two sweeps over an unchanged store see the same list in the same sequence —
	// which is what makes a truncated sweep truncate the same tail rather than a random one.
	sort.Slice(out, func(i, j int) bool {
		if out[i].UserID != out[j].UserID {
			return out[i].UserID < out[j].UserID
		}
		return out[i].ID < out[j].ID
	})
	return out
}
