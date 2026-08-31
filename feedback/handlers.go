package main

import (
	"encoding/json"
	"log"
	"net/http"
)

// handlers.go — the HTTP surface. Credential-safe CORS (exact allow-list + credentials, never "*"),
// JSON in/out, and the D-16 access-control policy:
//
//	GET    /health           PUBLIC — liveness only, reveals no feedback data
//	POST   /feedback         any signed-in tester; the server stamps the owner from the cookie
//	GET    /feedback         signed-in: your OWN submissions. Admin: all of them (incl. legacy).
//	                         ?status= and ?category= filter WITHIN what you are allowed to see.
//	GET    /feedback/summary ADMIN ONLY — counts by status/category + average rating
//	PATCH  /feedback/{id}    ADMIN ONLY — triage: {status: new|reviewed|resolved}
//	DELETE /feedback/{id}    ADMIN ONLY — remove (spam / test noise)
//
// Authorization is decided from the verified session cookie alone. No header, query parameter, body
// field or client flag can grant it, and no response carries the persisted owner id.

type Server struct {
	cfg   Config
	store *Store
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("POST /feedback", s.requireAuth(s.handleCreate))
	mux.HandleFunc("GET /feedback", s.requireAuth(s.handleList))
	mux.HandleFunc("GET /feedback/summary", s.requireAdmin(s.handleSummary))
	mux.HandleFunc("PATCH /feedback/{id}", s.requireAdmin(s.handleUpdate))
	mux.HandleFunc("DELETE /feedback/{id}", s.requireAdmin(s.handleDelete))
	return s.withCORS(mux)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Check(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "unavailable", "service": "feedback"})
		return
	}
	storage := "files"
	if s.store.db != nil {
		storage = "postgresql"
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "service": "feedback", "storage": storage})
}

func writeFeedbackStoreError(w http.ResponseWriter, err error) {
	log.Printf("feedback storage: %v", err)
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "feedback storage is unavailable"})
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var f Feedback
	if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON: " + err.Error()})
		return
	}
	// Server owns identity, ownership and lifecycle fields — never trust the client's. Zeroing Owner
	// before stamping it is what makes {"owner":"someone-else"} in the body inert.
	f.ID, f.Status, f.CreatedAt, f.UpdatedAt, f.Owner = "", "", 0, 0, ""
	if err := validateFeedback(&f); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	f.Owner = userID(r) // the verified session subject, never a request field
	created, err := s.store.AddE(f)
	if err != nil {
		writeFeedbackStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"feedback": public(created)})
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	uid := userID(r)
	admin := s.isAdmin(uid)

	status := r.URL.Query().Get("status")     // new | reviewed | resolved | "" (all)
	category := r.URL.Query().Get("category") // bug | idea | ux | other | "" (all)

	items := s.store.List()
	out := make([]publicFeedback, 0, len(items))
	unread := 0
	for _, f := range items {
		// Ownership scoping FIRST, so the query filters can only ever narrow an already-permitted
		// set. A normal user sees their own records and nothing else — a legacy record (Owner == "")
		// is never theirs, because a verified uid is never "".
		if !admin && f.Owner != uid {
			continue
		}
		if f.Status == "new" {
			unread++
		}
		if status != "" && f.Status != status {
			continue
		}
		if category != "" && f.Category != category {
			continue
		}
		out = append(out, public(f))
	}
	// `admin` tells the UI whether to render triage controls. It is a rendering hint derived from the
	// server's own allow-list, not a grant: every privileged route re-checks it.
	writeJSON(w, http.StatusOK, map[string]any{"feedback": out, "unread": unread, "admin": admin})
}

func (s *Server) handleSummary(w http.ResponseWriter, r *http.Request) {
	items := s.store.List()
	byStatus := map[string]int{}
	byCategory := map[string]int{}
	ratingSum, ratingN := 0, 0
	for _, f := range items {
		byStatus[f.Status]++
		byCategory[f.Category]++
		if f.Rating > 0 {
			ratingSum += f.Rating
			ratingN++
		}
	}
	var avg float64
	if ratingN > 0 {
		avg = float64(ratingSum) / float64(ratingN)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total": len(items), "byStatus": byStatus, "byCategory": byCategory,
		"avgRating": avg, "rated": ratingN,
	})
}

type patchReq struct {
	Status *string `json:"status"`
}

func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	f, ok := s.store.Get(r.PathValue("id"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "feedback not found"})
		return
	}
	var p patchReq
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON: " + err.Error()})
		return
	}
	if p.Status != nil {
		if !validStatus[*p.Status] {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "status must be new, reviewed, or resolved"})
			return
		}
		f.Status = *p.Status
	}
	updated, ok, err := s.store.ReplaceE(f)
	if err != nil {
		writeFeedbackStoreError(w, err)
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "feedback not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"feedback": public(updated)})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	deleted, err := s.store.DeleteE(r.PathValue("id"))
	if err != nil {
		writeFeedbackStoreError(w, err)
		return
	}
	if !deleted {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "feedback not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
