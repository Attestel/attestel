package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// reviews.go — saved lens reviews (PARALLEL_CONTRACTS.md §3.7, Step 07).
//
// A review is the SYSTEM'S output about a user-owned object: "this lens, applied to this thesis,
// found these things, citing this evidence". It lives here with theses and evidence because it is a
// per-user research record, and it obeys three rules that make it trustworthy to look back at:
//
//  1. IMMUTABLE CONTENT. There is no PATCH of findings, citations, or any other body field — a
//     review is a snapshot of what a lens said at a moment. The ONLY mutable parts are the user's
//     own reaction to it: `rating` and `citationFeedback`. If the analysis should change, you run a
//     new review; you do not edit the old one into agreeing with you.
//  2. A REVIEW NEVER WRITES A THESIS FIELD. Nothing in this file opens the thesis store. Accepting a
//     finding as a next question is a separate, explicit PATCH /theses/{id} that the user performs
//     with the text in front of them (§3.7). Model text never lands in `claim`, `assumptions`,
//     `risks`, `nextQuestions`, or `notes` by any path.
//  3. PER-USER PARTITION. `{TRADES_DIR}/{uid}/reviews.json`, same lazy-load + atomic-write discipline
//     as theses and evidence. A foreign id is 404, never 403.
//
// Step 10 (Library) consumes GET /reviews and GET /reviews/{id} and nothing else from this store.

const (
	maxReviewsPerUser = 2000 // oldest dropped past this; a research log, not an archive
	defaultReviewLim  = 50
	maxReviewLim      = 200
	maxRatingLen      = 16
)

var validReviewRating = map[string]bool{"useful": true, "not_useful": true}

// Review is stored as a decoded map rather than a struct on purpose.
//
// The BODY is authored by the llm against a frozen schema and validated by the gateway before it
// ever reaches this service. Re-declaring every field here would create a second, drifting copy of
// that schema in a service that has no business interpreting findings — and a field this store did
// not know about would be silently dropped on the next write. Storing the validated document whole
// keeps the llm's schema the single source of truth. The fields this store DOES own (id, savedAt,
// rating, citationFeedback) are set and policed explicitly below.
type Review map[string]any

// CitationFeedback records whether the user found a specific finding's citation sound. It is the
// "citation acceptance" metric this step is measured on, and it is user-authored.
type CitationFeedback struct {
	FindingID string `json:"findingId"`
	Accepted  bool   `json:"accepted"`
}

type reviewStore struct {
	dir    string
	mu     sync.Mutex
	byUser map[string][]Review
	docs   *documentRepository
}

func openReviewStore(dir string) (*reviewStore, error) {
	return openReviewStoreWithRepository(dir, nil)
}

func openReviewStoreWithRepository(dir string, docs *documentRepository) (*reviewStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &reviewStore{dir: dir, byUser: map[string][]Review{}, docs: docs}, nil
}

func (s *reviewStore) path(uid string) string {
	return filepath.Join(s.dir, uid, "reviews.json")
}

// load reads a user's reviews lazily, once. A missing file is an empty list; an UNPARSEABLE file is
// an error, so a write can refuse rather than silently replacing records we failed to read.
func (s *reviewStore) load(uid string) ([]Review, error) {
	if cached, ok := s.byUser[uid]; ok {
		return cached, nil
	}
	var b []byte
	var err error
	var found bool
	if s.docs != nil {
		b, found, err = s.docs.load(uid, "reviews", s.path(uid))
	} else {
		b, err = os.ReadFile(s.path(uid))
		found = err == nil
	}
	if !found && err == nil {
		s.byUser[uid] = []Review{}
		return s.byUser[uid], nil
	}
	if os.IsNotExist(err) {
		s.byUser[uid] = []Review{}
		return s.byUser[uid], nil
	}
	if err != nil {
		return nil, err
	}
	var out []Review
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	s.byUser[uid] = out
	return out, nil
}

// save writes atomically (.tmp + rename) so a crash mid-write cannot truncate the log.
func (s *reviewStore) save(uid string, list []Review) error {
	if len(list) > maxReviewsPerUser {
		list = list[len(list)-maxReviewsPerUser:]
	}
	if s.docs != nil {
		if err := s.docs.save(uid, "reviews", list); err != nil {
			delete(s.byUser, uid)
			return err
		}
		s.byUser[uid] = list
		return nil
	}
	dir := filepath.Join(s.dir, uid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(list, "", " ")
	if err != nil {
		return err
	}
	tmp := s.path(uid) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path(uid)); err != nil {
		return err
	}
	s.byUser[uid] = list
	return nil
}

// ---- API ----------------------------------------------------------------------------------------

type reviewAPI struct {
	srv   *Server
	store *reviewStore
}

// registerReviewRoutes mounts the saved-review surface. One line in handlers.go.
func (s *Server) registerReviewRoutes(mux *http.ServeMux) {
	store, err := openReviewStoreWithRepository(s.cfg.TradesDir, s.documents)
	if err != nil {
		// Additive: if the store cannot open, theses/trades/evidence must still serve.
		log.Printf("journal: review store unavailable in %q: %v (review routes not mounted)", s.cfg.TradesDir, err)
		return
	}
	api := &reviewAPI{srv: s, store: store}

	mux.HandleFunc("GET /reviews", s.optionalAuth(api.handleList))
	mux.HandleFunc("POST /reviews", s.requireAuth(api.handleCreate))
	mux.HandleFunc("GET /reviews/{id}", s.optionalAuth(api.handleGet))
	mux.HandleFunc("PATCH /reviews/{id}", s.requireAuth(api.handlePatch))
	mux.HandleFunc("DELETE /reviews/{id}", s.requireAuth(api.handleDelete))
}

func reviewStr(r Review, key string) string {
	s, _ := r[key].(string)
	return s
}

func reviewInt64(r Review, key string) int64 {
	switch v := r[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	}
	return 0
}

// handleList returns the caller's reviews, newest first by savedAt, with the filters Step 10's
// Library needs. Reads are optionalAuth: a guest simply has none.
func (a *reviewAPI) handleList(w http.ResponseWriter, r *http.Request) {
	uid := userID(r)
	a.store.mu.Lock()
	all, err := a.store.load(uid)
	a.store.mu.Unlock()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "stored reviews could not be read", "code": "store_unreadable"})
		return
	}

	q := r.URL.Query()
	ticker := strings.ToUpper(strings.TrimSpace(q.Get("ticker")))
	thesisID := strings.TrimSpace(q.Get("thesisId"))
	lensID := strings.TrimSpace(q.Get("lensId"))
	task := strings.TrimSpace(q.Get("task"))
	from := parseUnixParam(q.Get("from"))
	to := parseUnixParam(q.Get("to"))

	// Walk the log BACKWARDS so the newest insertion comes first before sorting. savedAt has
	// one-second resolution, so two reviews saved in the same second tie — and a stable sort would
	// otherwise surface the older one first, which reads as wrong in a "newest first" list.
	out := make([]Review, 0, len(all))
	for i := len(all) - 1; i >= 0; i-- {
		rev := all[i]
		if ticker != "" && reviewStr(rev, "ticker") != ticker {
			continue
		}
		if thesisID != "" && reviewStr(rev, "thesisId") != thesisID {
			continue
		}
		if lensID != "" {
			lens, _ := rev["lens"].(map[string]any)
			if lens == nil {
				continue
			}
			if s, _ := lens["id"].(string); s != lensID {
				continue
			}
		}
		if task != "" && reviewStr(rev, "task") != task {
			continue
		}
		at := reviewInt64(rev, "savedAt")
		if from > 0 && at < from {
			continue
		}
		if to > 0 && at > to {
			continue
		}
		out = append(out, rev)
	}

	sort.SliceStable(out, func(i, j int) bool {
		return reviewInt64(out[i], "savedAt") > reviewInt64(out[j], "savedAt")
	})

	limit := parseReviewLimit(q.Get("limit"))
	truncated := false
	if len(out) > limit {
		out = out[:limit]
		truncated = true
	}
	writeJSON(w, http.StatusOK, map[string]any{"reviews": out, "truncated": truncated})
}

func parseReviewLimit(raw string) int {
	n := defaultReviewLim
	if raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			n = v
		}
	}
	if n > maxReviewLim {
		n = maxReviewLim
	}
	return n
}

// parseUnixParam reads a unix-seconds query bound. 0 means "no bound".
func parseUnixParam(raw string) int64 {
	if raw == "" {
		return 0
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		return 0
	}
	return v
}

func (a *reviewAPI) handleGet(w http.ResponseWriter, r *http.Request) {
	uid := userID(r)
	id := r.PathValue("id")
	a.store.mu.Lock()
	all, err := a.store.load(uid)
	a.store.mu.Unlock()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "stored reviews could not be read", "code": "store_unreadable"})
		return
	}
	for _, rev := range all {
		if reviewStr(rev, "id") == id {
			writeJSON(w, http.StatusOK, map[string]any{"review": rev})
			return
		}
	}
	// A foreign id is 404, never 403 — existence is not confirmed across users.
	writeJSON(w, http.StatusNotFound, map[string]any{"error": "review not found", "code": "not_found"})
}

// handleCreate stores a validated review. The gateway is the only intended caller: it has already
// resolved the authorized context and validated every citation against it. This service does not
// re-run that validation (it has no access to the caller's evidence to check against) — it owns
// identity, timestamps, and the immutability rule.
func (a *reviewAPI) handleCreate(w http.ResponseWriter, r *http.Request) {
	uid := userID(r)
	var rev Review
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&rev); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "invalid JSON: " + err.Error(), "code": "validation_failed"})
		return
	}
	if rev == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "empty review", "code": "validation_failed"})
		return
	}
	if _, ok := rev["findings"]; !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "review must carry findings", "code": "validation_failed", "field": "findings"})
		return
	}

	now := time.Now().Unix()
	rev["id"] = "rev_" + newID()
	rev["savedAt"] = now
	// The user's own reaction starts empty and is the only thing they can later change.
	rev["rating"] = nil
	rev["citationFeedback"] = []any{}

	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	all, err := a.store.load(uid)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "stored reviews could not be read; refusing to overwrite them",
			"code":  "store_unreadable"})
		return
	}
	all = append(all, rev)
	if err := a.store.save(uid, all); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "review could not be saved: " + err.Error(), "code": "store_unwritable"})
		return
	}
	// §6.2: a CHALLENGE review is the third activation condition and the north-star numerator. The
	// explain / teach / chart-context tasks are real work but they are not a challenge, so they do not
	// count — measuring them here would let activation be satisfied without anyone's belief being
	// tested. Findings, agreements, and open questions never leave this service (§6.4).
	if task := reviewStr(rev, "task"); challengeTasks[task] {
		in := eventInput{
			Event:         evChallengeReviewCompleted,
			UID:           uid,
			TransitionKey: "created",
			Ticker:        reviewStr(rev, "ticker"),
			ObjectID:      reviewStr(rev, "id"),
			ThesisID:      reviewStr(rev, "thesisId"),
			Task:          task,
		}
		if lens, ok := rev["lens"].(map[string]any); ok {
			in.LensID, _ = lens["id"].(string)
		}
		analytics.emit(in)
	}
	writeJSON(w, http.StatusCreated, map[string]any{"review": rev})
}

// reviewPatch is the ENTIRE mutable surface of a saved review (§3.7). Anything else a client sends
// is ignored rather than merged — that is what makes the record an immutable snapshot in practice
// and not just in the docs.
type reviewPatch struct {
	Rating           *string             `json:"rating"`
	CitationFeedback *[]CitationFeedback `json:"citationFeedback"`
}

func (a *reviewAPI) handlePatch(w http.ResponseWriter, r *http.Request) {
	uid := userID(r)
	id := r.PathValue("id")

	var p reviewPatch
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&p); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "invalid JSON: " + err.Error(), "code": "validation_failed"})
		return
	}
	if p.Rating != nil && *p.Rating != "" && !validReviewRating[*p.Rating] {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "rating must be useful, not_useful, or null",
			"code":  "validation_failed", "field": "rating"})
		return
	}

	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	all, err := a.store.load(uid)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "stored reviews could not be read; refusing to overwrite them",
			"code":  "store_unreadable"})
		return
	}
	for i, rev := range all {
		if reviewStr(rev, "id") != id {
			continue
		}
		if p.Rating != nil {
			if *p.Rating == "" {
				all[i]["rating"] = nil
			} else {
				all[i]["rating"] = *p.Rating
			}
		}
		if p.CitationFeedback != nil {
			// Feedback may only reference findings that exist on THIS review, so it can never become
			// a side channel for storing arbitrary text against a record.
			known := findingIDs(rev)
			clean := make([]CitationFeedback, 0, len(*p.CitationFeedback))
			for _, cf := range *p.CitationFeedback {
				if known[cf.FindingID] {
					clean = append(clean, cf)
				}
			}
			all[i]["citationFeedback"] = clean
		}
		if err := a.store.save(uid, all); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error": "review could not be saved: " + err.Error(), "code": "store_unwritable"})
			return
		}
		emitReviewFeedback(uid, all[i], p)
		writeJSON(w, http.StatusOK, map[string]any{"review": all[i]})
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]any{"error": "review not found", "code": "not_found"})
}

// emitReviewFeedback records the user's reaction to a lens review: the usefulness verdict, and one
// event per finding whose citation they accepted or rejected (§6.2's `review_rated` and
// `citation_feedback`). The verdict is the transition key, so re-saving the same reaction is
// idempotent while changing their mind is a genuine second event — that is exactly what the quality
// metric needs. The finding's STATEMENT is never sent; only its opaque id.
func emitReviewFeedback(uid string, rev Review, p reviewPatch) {
	base := eventInput{
		UID:      uid,
		Ticker:   reviewStr(rev, "ticker"),
		ThesisID: reviewStr(rev, "thesisId"),
		Task:     reviewStr(rev, "task"),
	}
	if lens, ok := rev["lens"].(map[string]any); ok {
		base.LensID, _ = lens["id"].(string)
	}
	if p.Rating != nil && *p.Rating != "" {
		in := base
		in.Event, in.ObjectID, in.TransitionKey, in.Rating = evReviewRated, reviewStr(rev, "id"), *p.Rating, *p.Rating
		analytics.emit(in)
	}
	if p.CitationFeedback == nil {
		return
	}
	known := findingIDs(rev)
	for _, cf := range *p.CitationFeedback {
		if !known[cf.FindingID] {
			continue // the store dropped it too; an unknown finding id is not a measurable action
		}
		verdict := "rejected"
		if cf.Accepted {
			verdict = "accepted"
		}
		in := base
		in.Event, in.ObjectID, in.TransitionKey, in.Rating = evCitationFeedback, cf.FindingID, verdict, verdict
		analytics.emit(in)
	}
}

func findingIDs(rev Review) map[string]bool {
	out := map[string]bool{}
	list, _ := rev["findings"].([]any)
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			if s, _ := m["id"].(string); s != "" {
				out[s] = true
			}
		}
	}
	return out
}

func (a *reviewAPI) handleDelete(w http.ResponseWriter, r *http.Request) {
	uid := userID(r)
	id := r.PathValue("id")

	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	all, err := a.store.load(uid)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "stored reviews could not be read; refusing to overwrite them",
			"code":  "store_unreadable"})
		return
	}
	for i, rev := range all {
		if reviewStr(rev, "id") == id {
			all = append(all[:i], all[i+1:]...)
			if err := a.store.save(uid, all); err != nil {
				writeJSON(w, http.StatusServiceUnavailable, map[string]any{
					"error": "review could not be deleted: " + err.Error(), "code": "store_unwritable"})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
			return
		}
	}
	writeJSON(w, http.StatusNotFound, map[string]any{"error": "review not found", "code": "not_found"})
}
