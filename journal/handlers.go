package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"
)

// handlers.go — the HTTP surface. Permissive CORS (additive service; the frontend fails silent if
// it's unreachable). JSON in/out. Nothing here executes a trade — the API only manages the user's
// own records and computes descriptive statistics from them.

type Server struct {
	cfg                Config
	store              *Store
	theses             *ThesisStore
	portfolios         *PortfolioStore
	portfolioSnapshots *PortfolioSnapshotStore
	portfolioReviews   *PortfolioReviewStore
	documents          *documentRepository
	// agency holds the owner-scoped Hermes research runs (agency_store.go). Nil when the lane is
	// not configured, which every route in agency_routes.go treats as "switched off", never as
	// "open".
	agency *AgencyStore
	http   *http.Client
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	// Reads are scoped to the caller (optionalAuth: a guest sees an empty journal). Every mutation
	// requires a signed-in user (requireAuth is the 401 backstop behind the guest-routing UI).
	mux.HandleFunc("GET /trades", s.optionalAuth(s.handleListTrades))
	mux.HandleFunc("POST /trades", s.requireAuth(s.handleCreateTrade))
	mux.HandleFunc("PATCH /trades/{id}", s.requireAuth(s.handleUpdateTrade))
	mux.HandleFunc("DELETE /trades/{id}", s.requireAuth(s.handleDeleteTrade))
	mux.HandleFunc("GET /stats", s.optionalAuth(s.handleStats))
	// Theses: the user's structured, versioned per-ticker hypotheses + the latest evidence check.
	// The four v1 routes keep their exact paths and status codes; the three below are the minimum
	// additions the Research workspace needs (PARALLEL_CONTRACTS §1.9). Advancing a review checkpoint
	// is a write, so it takes requireAuth like every other mutation.
	mux.HandleFunc("GET /theses", s.optionalAuth(s.handleListTheses))
	mux.HandleFunc("POST /theses", s.requireAuth(s.handleCreateThesis))
	mux.HandleFunc("GET /theses/{id}", s.optionalAuth(s.handleGetThesis))
	// Phase 3. The deterministic thesis review queue: which upcoming events touch a condition the
	// user wrote down. Registered BEFORE the `{id}` patterns would matter; Go's ServeMux prefers
	// the more specific literal path either way, and `event-reviews` is not a thesis id.
	mux.HandleFunc("GET /theses/event-reviews", s.optionalAuth(s.handleThesisEventReviews))
	mux.HandleFunc("GET /theses/{id}/versions", s.optionalAuth(s.handleThesisVersions))
	mux.HandleFunc("POST /theses/{id}/review-checkpoint", s.requireAuth(s.handleThesisReviewCheckpoint))
	mux.HandleFunc("PATCH /theses/{id}", s.requireAuth(s.handleUpdateThesis))
	mux.HandleFunc("DELETE /theses/{id}", s.requireAuth(s.handleDeleteThesis))
	// Evidence + thesis↔evidence links. Registered from evidence.go so that lane keeps its routes,
	// types, and store in its own files (PARALLEL_CONTRACTS.md §7.1's route seam).
	s.registerEvidenceRoutes(mux)
	// Saved lens reviews (Step 07). Immutable snapshots; only the user's own rating and citation
	// feedback are mutable, and nothing here ever writes a thesis field.
	s.registerReviewRoutes(mux)
	// Decision records + outcome reviews (Step 09). Same seam: the routes, the write-once snapshot,
	// and the reasoning/P&L separation all live in decisions.go / outcomes.go.
	s.registerDecisionRoutes(mux)
	// Thesis-first guided onboarding (Step 11). Same seam again; the state model, the server-verified
	// activation rule, and the guest path all live in onboarding.go.
	s.registerOnboardingRoutes(mux)
	// Portfolio Intelligence. Holdings, cash, user targets and policy are durable research records;
	// there is no brokerage or execution surface behind these routes.
	s.registerPortfolioRoutes(mux)
	// Wave 0 seam. Empty today; Wave 1 Lane A attaches /subscriptions (contract §2.1) and
	// /event-state (§2.3) from its own files so it never edits handlers.go. See routeseams.go.
	s.registerSubscriptionRoutes(mux)
	return s.withCORS(mux)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Check(r.Context()); err != nil {
		log.Printf("journal trade store health: %v", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "unavailable", "service": "journal"})
		return
	}
	storage := "files"
	if s.store.db != nil {
		storage = "postgresql"
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "service": "journal", "tradeStorage": storage})
}

func writeTradeStoreError(w http.ResponseWriter, err error) {
	log.Printf("journal trade store: %v", err)
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "trade storage is unavailable"})
}

var errTradeStoreUnavailable = &apiError{
	Status: http.StatusServiceUnavailable, Code: "store_unavailable",
	Msg: "trade storage is unavailable; no journal change was made",
}

// ---- view builders (compute pnl etc. on read; fetch quotes for open trades once per ticker) ----

func (s *Server) buildViews(ctx context.Context, trades []Trade) []TradeView {
	openTickers := map[string]bool{}
	for _, t := range trades {
		if t.Exit == nil {
			openTickers[t.Ticker] = true
		}
	}
	marks := s.quoteBatch(ctx, openTickers)
	now := time.Now().UTC()
	views := make([]TradeView, 0, len(trades))
	for _, t := range trades {
		views = append(views, computeView(t, marks[t.Ticker], now))
	}
	return views
}

func (s *Server) buildView(ctx context.Context, t Trade) TradeView {
	var mark *quoteResp
	if t.Exit == nil {
		mark, _ = s.fetchQuote(ctx, t.Ticker)
	}
	return computeView(t, mark, time.Now().UTC())
}

func (s *Server) handleListTrades(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status == "" {
		status = "all"
	}
	mode := r.URL.Query().Get("mode") // paper | live | all (empty -> all)
	if mode == "" {
		mode = "all"
	}
	trades, err := s.store.List(userID(r))
	if err != nil {
		writeTradeStoreError(w, err)
		return
	}
	views := s.buildViews(r.Context(), trades)
	out := make([]TradeView, 0, len(views))
	for _, v := range views {
		if status != "all" && v.Status != status {
			continue
		}
		if mode != "all" && effectiveMode(v.Mode) != mode {
			continue
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"trades": out})
}

// createReq is a trade plus the request-only flag to snapshot the current read at entry.
type createReq struct {
	Trade
	CaptureRead bool `json:"captureRead"`
}

func (s *Server) handleCreateTrade(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON: " + err.Error()})
		return
	}
	t := req.Trade
	// Never trust a client-supplied read snapshot / ids — the server owns those.
	t.AttachedRead = nil
	if err := validateTrade(&t); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if req.CaptureRead {
		// best-effort: a failed fetch must not fail trade creation
		t.AttachedRead = s.captureRead(r.Context(), t.Ticker, t.Entry.Timeframe)
	}
	created, err := s.store.Add(userID(r), t)
	if err != nil {
		writeTradeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"trade": s.buildView(r.Context(), created)})
}

// tradePatch uses pointers so absent fields are left untouched. Setting `exit` closes the trade.
type tradePatch struct {
	Side      *string    `json:"side"`
	Entry     *Entry     `json:"entry"`
	Stop      *float64   `json:"stop"`
	Target    *float64   `json:"target"`
	Exit      *Exit      `json:"exit"`
	Thesis    *string    `json:"thesis"`
	Tags      *[]string  `json:"tags"`
	Risk      *RiskPlan  `json:"risk"`      // plan metadata only — never used to compute P&L
	Checklist *Checklist `json:"checklist"` // the user's own plan-completeness record; never P&L
}

func (s *Server) handleUpdateTrade(w http.ResponseWriter, r *http.Request) {
	uid := userID(r)
	id := r.PathValue("id")
	t, ok, err := s.store.Get(uid, id)
	if err != nil {
		writeTradeStoreError(w, err)
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "trade not found"})
		return
	}
	var p tradePatch
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON: " + err.Error()})
		return
	}
	if p.Side != nil {
		t.Side = *p.Side
	}
	if p.Entry != nil {
		t.Entry = *p.Entry
	}
	if p.Stop != nil {
		t.Stop = p.Stop
	}
	if p.Target != nil {
		t.Target = p.Target
	}
	if p.Exit != nil {
		t.Exit = p.Exit
	}
	if p.Thesis != nil {
		t.Thesis = *p.Thesis
	}
	if p.Tags != nil {
		t.Tags = *p.Tags
	}
	if p.Risk != nil {
		t.Risk = p.Risk
	}
	if p.Checklist != nil {
		t.Checklist = p.Checklist
	}
	if err := validateTrade(&t); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	updated, ok, err := s.store.Replace(uid, t)
	if err != nil {
		writeTradeStoreError(w, err)
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "trade not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"trade": s.buildView(r.Context(), updated)})
}

func (s *Server) handleDeleteTrade(w http.ResponseWriter, r *http.Request) {
	deleted, err := s.store.Delete(userID(r), r.PathValue("id"))
	if err != nil {
		writeTradeStoreError(w, err)
		return
	}
	if !deleted {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "trade not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	// Stats are computable per mode so SIMULATED paper P&L never contaminates real/live stats
	// (guardrail). Default "all"; the UI asks for mode=live to see only real trades.
	mode := r.URL.Query().Get("mode")
	if mode == "" {
		mode = "all"
	}
	trades, err := s.store.List(userID(r))
	if err != nil {
		writeTradeStoreError(w, err)
		return
	}
	views := s.buildViews(r.Context(), trades)
	if mode != "all" {
		filtered := make([]TradeView, 0, len(views))
		for _, v := range views {
			if effectiveMode(v.Mode) == mode {
				filtered = append(filtered, v)
			}
		}
		views = filtered
	}
	writeJSON(w, http.StatusOK, computeStats(views))
}
