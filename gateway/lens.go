package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// lens.go — contextual research lenses (PARALLEL_CONTRACTS.md §3, Step 07).
//
// THE SERVER RESOLVES EVERY ID. That sentence is the whole design of this file. The browser sends
// ids and nothing else that matters; this handler loads the thesis and each evidence record from
// `journal` WITH THE CALLER'S COOKIE, computes the chart context from `analysis` itself, and only
// then hands a normalized bundle to the llm. Consequences that are deliberate, not incidental:
//
//   · Evidence TEXT supplied by a client is never read and never trusted. Ownership is proved by the
//     journal returning the record for that cookie, nothing else.
//   · A target or evidence id the caller does not own is a 404 and the run NEVER STARTS. No model
//     call, no partial context, no "it was probably theirs".
//   · The deterministic chart context in the saved review is the value THIS handler computed. The
//     model's copy is discarded (§3.5.5), exactly as main.py re-imposes keyLevels.
//
// Persistence is the journal (`{TRADES_DIR}/{uid}/reviews.json`) — reviews are per-user research
// records, so they live with theses and evidence rather than in the llm's read store.
//
// On-demand ONLY. Nothing here is reachable from a poll, a scheduler, a ticker change, or a page
// load (invariant #4); the route is a POST and every caller is a click on a named task button.

const (
	lensTimeout      = 90 * time.Second
	maxEvidencePerRun = 24
	maxUserInstruct   = 1000
)

// lensRequest is the browser-facing body (§3.1). Note what is absent: any evidence content, any
// thesis content, and any chart numbers. Only ids and the requested method.
type lensRequest struct {
	LensID          string   `json:"lensId"`
	Task            string   `json:"task"`
	Target          lensTgt  `json:"target"`
	Ticker          string   `json:"ticker"`
	Timeframe       string   `json:"timeframe"`
	EvidenceIDs     []string `json:"evidenceIds"`
	PriorReviewID   string   `json:"priorReviewId"`
	UserInstruction string   `json:"userInstruction"`
}

type lensTgt struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

var lensTasks = map[string]bool{
	"challenge_thesis":         true,
	"explain_evidence":         true,
	"review_chart_context":     true,
	"teach_step":               true,
	"apply_custom_framework":   true,
	"review_with_perspectives": true,
}

var lensTargetTypes = map[string]bool{
	"thesis": true, "evidence": true, "chart_context": true, "decision": true,
}

// registerLensRoutes mounts the lens surface. One registration line in main.go keeps this lane's
// footprint in the shared file minimal (PARALLEL_EXECUTION.md § shared-file rule).
func (s *Server) registerLensRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/lens", s.handleLens)
	mux.HandleFunc("GET /api/lenses", s.handleLenses)
	// Saved reviews. Pass-throughs: the journal owns the store and its per-user scoping, and Step 10
	// (Library) reads exactly these two GETs and nothing else from this lane (§3.7).
	mux.HandleFunc("GET /api/reviews", s.handleReviewsList)
	mux.HandleFunc("GET /api/reviews/{id}", s.handleReviewGet)
	mux.HandleFunc("PATCH /api/reviews/{id}", s.handleReviewPatch)
	mux.HandleFunc("DELETE /api/reviews/{id}", s.handleReviewDelete)
}

// handleLenses lists the caller's available lenses (built-ins + their own personas) and the task
// table. Read-only and cheap; it makes NO model call.
func (s *Server) handleLenses(w http.ResponseWriter, r *http.Request) {
	id := s.identityFrom(r)
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	res, err := s.getJSONUser(ctx, s.cfg.LLMURL+"/lenses", id.userID)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": "lens catalogue unavailable: " + err.Error(), "code": "llm_unreachable"})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleLens(w http.ResponseWriter, r *http.Request) {
	id := s.identityFrom(r)
	if id.userID == "" {
		// Reviews are per-user records, so running one requires a session. The UI routes a guest to
		// sign-in before offering the button; this is the backstop.
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": "sign in required", "code": "auth_required"})
		return
	}

	var req lensRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<18)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "invalid JSON: " + err.Error(), "code": "validation_failed"})
		return
	}
	if !lensTasks[req.Task] {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "unknown task: " + req.Task, "code": "validation_failed", "field": "task"})
		return
	}
	if !lensTargetTypes[req.Target.Type] {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "unknown target type: " + req.Target.Type, "code": "validation_failed", "field": "target"})
		return
	}
	if len(req.EvidenceIDs) > maxEvidencePerRun {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "at most 24 evidence records per review", "code": "validation_failed", "field": "evidenceIds"})
		return
	}

	ticker := strings.ToUpper(strings.TrimSpace(req.Ticker))
	timeframe := normalizeTimeframe(req.Timeframe)

	ctx, cancel := context.WithTimeout(r.Context(), lensTimeout)
	defer cancel()

	// ---- resolve the target. A foreign or missing id ends the request here, before any model call.
	var thesis map[string]any
	switch req.Target.Type {
	case "thesis":
		t, err := s.loadThesis(ctx, req.Target.ID, id.cookie)
		if err != nil || t == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{
				"error": "thesis not found", "code": "not_found"})
			return
		}
		thesis = t
		if ticker == "" {
			ticker, _ = t["ticker"].(string)
		}
	case "decision":
		// Step 09 owns decision records. The contract requires this lane to ACCEPT the type and 404
		// until that store exists, rather than inventing a shape for it (§3.2).
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "decision records are not available yet", "code": "not_found"})
		return
	}

	// ---- resolve evidence. Every id must come back from the journal FOR THIS COOKIE.
	authorized, missing, err := s.loadEvidence(ctx, req.EvidenceIDs, id.cookie)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": "journal unreachable: " + err.Error(), "code": "journal_unreachable"})
		return
	}
	if len(missing) > 0 {
		// Foreign or deleted ids are a 404 naming the ids, never a silent drop — a review that
		// quietly ignored half the user's selection would be worse than one that failed.
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "evidence not found", "code": "not_found", "evidenceIds": missing})
		return
	}

	// If the target IS an evidence record, it is part of its own authorized context.
	if req.Target.Type == "evidence" {
		found := false
		for _, e := range authorized {
			if idOf(e) == req.Target.ID {
				found = true
				break
			}
		}
		if !found {
			one, m, err2 := s.loadEvidence(ctx, []string{req.Target.ID}, id.cookie)
			if err2 != nil || len(m) > 0 || len(one) == 0 {
				writeJSON(w, http.StatusNotFound, map[string]any{
					"error": "evidence not found", "code": "not_found"})
				return
			}
			authorized = append(one, authorized...)
		}
		if ticker == "" {
			ticker, _ = authorized[0]["ticker"].(string)
		}
	}

	// ---- deterministic chart context, computed HERE. Only for tasks that use it, so a thesis
	// challenge does not silently pull a chart the user did not ask about.
	var chart map[string]any
	if req.Target.Type == "chart_context" || req.Task == "review_chart_context" {
		if ticker == "" {
			ticker = strings.ToUpper(strings.TrimSpace(req.Target.ID))
		}
		chart = s.chartContextFor(ctx, ticker, timeframe)
		if chart == nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"error": "chart context unavailable: the analysis service is not responding",
				"code": "analysis_unreachable"})
			return
		}
	}

	// ---- prior review, only when a comparison was requested.
	var prior map[string]any
	if strings.TrimSpace(req.PriorReviewID) != "" {
		if p, e := s.journalGet(ctx, s.cfg.JournalURL+"/reviews/"+url.PathEscape(req.PriorReviewID), id.cookie); e == nil {
			prior, _ = p["review"].(map[string]any)
		}
	}

	lensCtx := map[string]any{
		"lens":            nil, // filled by the llm from lensId + the caller's persona store
		"thesis":          thesis,
		"evidence":        authorized,
		"chartContext":    chart,
		"priorReview":     prior,
		"userInstruction": truncateString(req.UserInstruction, maxUserInstruct),
	}

	// ---- "Review with perspectives" runs the committee pipeline, unchanged.
	if req.Task == "review_with_perspectives" {
		s.lensViaCommittee(ctx, w, r, id, ticker, req, lensCtx)
		return
	}

	lensCtx["lens"] = s.resolveLens(ctx, req.LensID, req.Task, id.userID)

	res, err := s.postJSONUser(ctx, s.cfg.LLMURL+"/lens", map[string]any{
		"lensId":          req.LensID,
		"task":            req.Task,
		"target":          map[string]any{"type": req.Target.Type, "id": req.Target.ID},
		"ticker":          ticker,
		"timeframe":       timeframe,
		"userInstruction": lensCtx["userInstruction"],
		"context":         lensCtx,
	}, id.userID)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": "lens service unavailable: " + err.Error(), "code": "llm_unreachable"})
		return
	}

	// The server's deterministic chart context wins, ALWAYS (§3.5.5) — including when the server has
	// none, in which case the field is nulled rather than left holding whatever the model invented.
	// Assigning only when non-nil would let a fabricated level survive on every task that does not
	// use chart context, which is most of them.
	res["chartContext"] = chart

	s.persistReview(ctx, w, res, id.cookie)
}

// lensViaCommittee runs the analyst committee and saves its projection into the review shape. The
// pipeline, the snapshot, and the deterministic consensus are untouched (§3.6, invariant #6).
func (s *Server) lensViaCommittee(ctx context.Context, w http.ResponseWriter, r *http.Request,
	id identity, ticker string, req lensRequest, lensCtx map[string]any) {

	facts := s.committeeContextFor(ctx, ticker)
	res, err := s.postJSONUser(ctx, s.cfg.LLMURL+"/committee", map[string]any{
		"ticker":      ticker,
		"context":     facts,
		"persist":     true,
		"target":      map[string]any{"type": req.Target.Type, "id": req.Target.ID},
		"lensContext": lensCtx,
	}, id.userID)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": "committee unavailable: " + err.Error(), "code": "llm_unreachable"})
		return
	}
	review, _ := res["review"].(map[string]any)
	if review == nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": "committee returned no review", "code": "llm_unreachable"})
		return
	}
	s.persistReview(ctx, w, review, id.cookie)
}

// persistReview writes the validated review to the journal and returns the STORED record, so the
// client renders what was actually saved (with its server-assigned id and savedAt) rather than a
// hopeful local copy. A store failure is surfaced, not swallowed: a review the user cannot find
// again is worse than an error they can retry.
func (s *Server) persistReview(ctx context.Context, w http.ResponseWriter, review map[string]any, cookie string) {
	status, body, err := s.journalPost(ctx, s.cfg.JournalURL+"/reviews", review, cookie)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": "review could not be saved: " + err.Error(), "code": "journal_unreachable",
			"review": review})
		return
	}
	if status >= 400 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write(body)
}

// ---- resolution helpers -------------------------------------------------------------------------

// loadThesis fetches one thesis for the CALLER. nil when the journal says 404 — which is what a
// foreign id returns, so ownership needs no separate check here.
func (s *Server) loadThesis(ctx context.Context, id, cookie string) (map[string]any, error) {
	res, err := s.journalGet(ctx, s.cfg.JournalURL+"/theses/"+url.PathEscape(id), cookie)
	if err != nil {
		return nil, err
	}
	t, _ := res["thesis"].(map[string]any)
	return t, nil
}

// loadEvidence resolves the requested ids against the caller's OWN evidence and reports which ones
// did not come back. It lists the caller's records and intersects, so a foreign id is simply absent
// — the journal never has to confirm that someone else's record exists.
func (s *Server) loadEvidence(ctx context.Context, ids []string, cookie string) ([]map[string]any, []string, error) {
	want := map[string]bool{}
	order := []string{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id != "" && !want[id] {
			want[id] = true
			order = append(order, id)
		}
	}
	if len(order) == 0 {
		return []map[string]any{}, nil, nil
	}

	res, err := s.journalGet(ctx, s.cfg.JournalURL+"/evidence?limit=200", cookie)
	if err != nil {
		return nil, nil, err
	}
	raw, _ := res["evidence"].([]any)
	byID := map[string]map[string]any{}
	for _, item := range raw {
		if m, ok := item.(map[string]any); ok {
			if rid := idOf(m); rid != "" {
				byID[rid] = m
			}
		}
	}

	out := make([]map[string]any, 0, len(order))
	missing := []string{}
	for _, id := range order {
		if rec, ok := byID[id]; ok {
			out = append(out, rec)
		} else {
			missing = append(missing, id)
		}
	}
	return out, missing, nil
}

// chartContextFor computes the deterministic chart context for this request. It is never taken from
// the client and never taken from the model.
func (s *Server) chartContextFor(ctx context.Context, ticker, timeframe string) map[string]any {
	an, err := s.getJSON(ctx, s.cfg.AnalysisURL+"/analysis/"+ticker+"?timeframe="+url.QueryEscape(timeframe))
	if err != nil {
		return nil
	}
	regime, _ := an["regime"].(map[string]any)
	if regime == nil {
		return nil
	}
	out := map[string]any{"timeframe": timeframe}
	for _, k := range []string{"trend", "shortTermTrend", "momentum", "macdState", "volatility",
		"priceVsBands", "volume", "trendStrength", "stochastic", "volumeTrend", "vwapState",
		"lastClose", "movingAverages"} {
		if v, ok := regime[k]; ok {
			out[k] = v
		}
	}
	if kl, ok := regime["keyLevels"].(map[string]any); ok {
		out["support"] = kl["support"]
		out["resistance"] = kl["resistance"]
	}
	// dataState travels with the numbers so a review built on SYNTHETIC bars is labelled as such
	// wherever it is later read (invariant #1's "nobody trades on synthetic data" rule).
	if src, ok := an["source"].(string); ok {
		out["dataState"] = src
	}
	return out
}

// resolveLens turns a lensId into the persona the llm should apply. The llm re-resolves it against
// the caller's own persona store; this is the display identity plus the task's default when the
// client sent nothing.
func (s *Server) resolveLens(ctx context.Context, lensID, task, userID string) map[string]any {
	if strings.TrimSpace(lensID) == "" {
		lensID = defaultLensForTask(task)
	}
	res, err := s.getJSONUser(ctx, s.cfg.LLMURL+"/lenses", userID)
	if err == nil {
		if list, ok := res["lenses"].([]any); ok {
			for _, item := range list {
				m, ok := item.(map[string]any)
				if !ok {
					continue
				}
				if s, _ := m["id"].(string); s == lensID {
					return m
				}
			}
		}
	}
	return map[string]any{"id": lensID, "name": lensID, "builtin": strings.HasPrefix(lensID, "builtin:")}
}

func defaultLensForTask(task string) string {
	switch task {
	case "challenge_thesis":
		return "builtin:skeptic"
	case "explain_evidence":
		return "builtin:plain"
	case "review_chart_context":
		return "builtin:technician"
	case "teach_step":
		return "builtin:instructor"
	}
	return "builtin:plain"
}

// getJSONUser GETs like getJSON but stamps the gateway-verified X-User-Id, so the llm resolves the
// CALLER'S own personas. The GET counterpart of postJSONUser (auth.go); a guest ("") gets built-ins.
func (s *Server) getJSONUser(ctx context.Context, url, userID string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-User-Id", userID)
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("upstream %s -> %d: %s", url, resp.StatusCode, truncate(body, 200))
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode %s: %w", url, err)
	}
	return out, nil
}

func idOf(m map[string]any) string {
	s, _ := m["id"].(string)
	return s
}

func truncateString(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ---- saved-review pass-throughs (§3.7) ----------------------------------------------------------

func (s *Server) handleReviewsList(w http.ResponseWriter, r *http.Request) {
	path := "/reviews"
	if q := r.URL.RawQuery; q != "" {
		path += "?" + q
	}
	s.proxyJournal(w, r, http.MethodGet, path)
}

func (s *Server) handleReviewGet(w http.ResponseWriter, r *http.Request) {
	s.proxyJournal(w, r, http.MethodGet, "/reviews/"+url.PathEscape(r.PathValue("id")))
}

func (s *Server) handleReviewPatch(w http.ResponseWriter, r *http.Request) {
	s.proxyJournal(w, r, http.MethodPatch, "/reviews/"+url.PathEscape(r.PathValue("id")))
}

func (s *Server) handleReviewDelete(w http.ResponseWriter, r *http.Request) {
	s.proxyJournal(w, r, http.MethodDelete, "/reviews/"+url.PathEscape(r.PathValue("id")))
}
