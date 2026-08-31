package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
)

// handlers.go — the HTTP surface. Permissive CORS (additive service; the frontend fails silent if
// it's unreachable). JSON in/out. Nothing here places trades — the API only manages descriptive
// rules and reads triggered events.

type API struct {
	store *Store
	cfg   Config
	// monitor is Wave 5 Lane 5B's sweep, or nil when it was never constructed. Nil is a real state
	// and the route above handles it explicitly: "no sweep has run" and "nothing went stale" are
	// different answers and must not render alike.
	monitor *ThesisMonitor
}

func newAPI(store *Store, cfg Config) *API { return &API{store: store, cfg: cfg} }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (a *API) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", a.handleHealth)
	// Reads are scoped to the caller (optionalAuth: a guest sees no rules/events). Every mutation
	// requires a signed-in user (requireAuth is the 401 backstop behind the guest-routing UI).
	mux.HandleFunc("GET /rules", a.optionalAuth(a.handleListRules))
	mux.HandleFunc("POST /rules", a.requireAuth(a.handleCreateRule))
	mux.HandleFunc("PATCH /rules/{id}", a.requireAuth(a.handleUpdateRule))
	mux.HandleFunc("DELETE /rules/{id}", a.requireAuth(a.handleDeleteRule))
	mux.HandleFunc("GET /events", a.optionalAuth(a.handleListEvents))
	mux.HandleFunc("POST /events/read", a.requireAuth(a.handleMarkRead))
	// Per-notification read: clicking one notification in the bell modal marks just that event read.
	mux.HandleFunc("POST /events/{id}/read", a.requireAuth(a.handleMarkEventRead))
	// Monitoring routes live in routes_extra.go (PARALLEL_CONTRACTS.md §7.1's route seam), so this
	// lane adds one line here instead of a block of registrations in a shared file.
	a.registerExtraRoutes(mux)
	return a.withCORS(mux)
}

func (a *API) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := a.store.Check(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "unavailable", "service": "alerts"})
		return
	}
	storage := "files"
	if a.store.db != nil {
		storage = "postgresql"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "service": "alerts",
		"tickers": a.cfg.Tickers, "evalInterval": a.cfg.EvalInterval.String(), "storage": storage,
	})
}

func writeStoreUnavailable(w http.ResponseWriter, err error) {
	log.Printf("alerts storage: %v", err)
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "alerts storage is unavailable"})
}

func (a *API) handleListRules(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"rules": a.store.RulesForUser(userID(r))})
}

func (a *API) handleCreateRule(w http.ResponseWriter, r *http.Request) {
	var rule Rule
	if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON: " + err.Error()})
		return
	}
	rule.UserID = userID(r) // server-owned; never trust a client-supplied owner
	rule.Active = true      // new rules start active; toggle off via PATCH
	// D-10's precondition is derived here, never read off the wire: this request, from this signed-in
	// user, named the thesis item. That is the assertion — not a flag a client can set.
	rule.UserCreatedFromItem = strings.TrimSpace(rule.ThesisItemID) != ""
	if err := validateAndNormalize(&rule); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	created, err := a.store.AddRuleE(rule)
	if err != nil {
		writeStoreUnavailable(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"rule": created})
}

func (a *API) handleUpdateRule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var patch map[string]any
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON: " + err.Error()})
		return
	}
	current, ok := a.store.GetRule(id)
	if !ok || current.UserID != userID(r) {
		// Another user's rule is invisible — 404, never reveal it exists (ownership by scope).
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "rule not found"})
		return
	}
	// Validate a simulated merge BEFORE persisting, so a bad params/cooldown edit is rejected
	// without corrupting the stored rule. `active` toggles never need validation.
	sim := current
	if p, ok := patch["params"].(map[string]any); ok {
		sim.Params = p
	}
	if v, ok := patch["cooldownSec"].(float64); ok {
		sim.CooldownSec = int(v)
	}
	// Thesis linkage is patchable, and doing so is a USER assertion just as creating from an item is
	// — so UserCreatedFromItem is re-derived from this body rather than inherited. Clearing the item
	// clears the assertion with it, which is what makes revoking an auto-link possible.
	if v, present := patch["thesisId"]; present {
		sim.ThesisID, _ = v.(string)
	}
	if v, present := patch["thesisItemId"]; present {
		sim.ThesisItemID, _ = v.(string)
		sim.UserCreatedFromItem = strings.TrimSpace(sim.ThesisItemID) != ""
	}
	if v, present := patch["intent"]; present {
		sim.Intent, _ = v.(string)
	}
	if v, present := patch["reviewAt"]; present {
		f, _ := v.(float64)
		sim.ReviewAt = int64(f)
	}
	if err := validateAndNormalize(&sim); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if v, ok := patch["active"].(bool); ok {
		sim.Active = v
	}
	// Changing the condition re-baselines the edge memory, exactly as it did before Step 08.
	if _, ok := patch["params"].(map[string]any); ok {
		sim.LastState = nil
	}
	updated, ok, err := a.store.ReplaceRuleE(id, sim)
	if err != nil {
		writeStoreUnavailable(w, err)
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "rule not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rule": updated})
}

func (a *API) handleDeleteRule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if cur, ok := a.store.GetRule(id); !ok || cur.UserID != userID(r) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "rule not found"})
		return
	}
	deleted, err := a.store.DeleteRuleE(id)
	if err != nil {
		writeStoreUnavailable(w, err)
		return
	}
	if !deleted {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "rule not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (a *API) handleListEvents(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if q := r.URL.Query().Get("limit"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 {
			limit = n
		}
	}
	events, unread, err := a.store.ListEventsE(userID(r), limit)
	if err != nil {
		writeStoreUnavailable(w, err)
		return
	}
	// Optional, additive filters. Absent params reproduce the pre-Step-08 response exactly; `unread`
	// deliberately stays the caller's TOTAL unread count so the bell badge is unaffected by a filter.
	if thesisID := strings.TrimSpace(r.URL.Query().Get("thesisId")); thesisID != "" {
		events = filterEvents(events, func(ev Event) bool { return ev.ThesisID == thesisID })
	}
	if q := r.URL.Query().Get("since"); q != "" {
		if since, err := strconv.ParseInt(q, 10, 64); err == nil {
			// Strictly after: `since` is a review-checkpoint boundary, and the checkpoint itself is
			// not a change that happened after it (§4.4).
			events = filterEvents(events, func(ev Event) bool { return ev.TS > since })
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events, "unread": unread})
}

func (a *API) handleMarkRead(w http.ResponseWriter, r *http.Request) {
	if err := a.store.MarkEventsReadE(userID(r)); err != nil {
		writeStoreUnavailable(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "unread": 0})
}

// handleMarkEventRead marks one event read for the caller and returns the caller's remaining unread
// count, so the bell badge can update immediately.
func (a *API) handleMarkEventRead(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "event id required"})
		return
	}
	uid := userID(r)
	if err := a.store.MarkEventReadE(uid, id); err != nil {
		writeStoreUnavailable(w, err)
		return
	}
	_, unread, err := a.store.ListEventsE(uid, 1) // unread is counted over ALL the user's events
	if err != nil {
		writeStoreUnavailable(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "unread": unread})
}
