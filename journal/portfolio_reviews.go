package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type PortfolioReview struct {
	ID             string         `json:"id"`
	PortfolioID    string         `json:"portfolioId"`
	ContextVersion string         `json:"contextVersion"`
	Structured     map[string]any `json:"structured"`
	ModelUsed      string         `json:"modelUsed"`
	Warnings       []string       `json:"warnings"`
	Retried        bool           `json:"retried"`
	Disclaimer     string         `json:"disclaimer"`
	CreatedAt      int64          `json:"createdAt"`
}

type portfolioLLMResponse struct {
	ContextVersion string         `json:"contextVersion"`
	Question       string         `json:"question,omitempty"`
	Structured     map[string]any `json:"structured"`
	ModelUsed      string         `json:"modelUsed"`
	Warnings       []string       `json:"warnings"`
	Retried        bool           `json:"retried"`
	Disclaimer     string         `json:"disclaimer"`
}

func (s *Server) handleCreatePortfolioReview(w http.ResponseWriter, r *http.Request) {
	uid := userID(r)
	portfolio, ok, err := s.portfolios.Get(uid, r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "portfolio store is unreadable"})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "portfolio not found"})
		return
	}
	intel, err := s.buildPortfolioIntelligence(r.Context(), uid, portfolio)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "portfolio intelligence is unavailable"})
		return
	}
	if cached, ok, err := s.portfolioReviews.ByContext(uid, portfolio.ID, intel.ContextVersion); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "portfolio review store is unreadable"})
		return
	} else if ok {
		writeJSON(w, http.StatusOK, map[string]any{"review": cached, "reused": true})
		return
	}
	var llm portfolioLLMResponse
	if err := s.postJSON(r.Context(), s.cfg.LLMURL+"/portfolio-review", map[string]any{"context": intel}, &llm); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "portfolio review explanation is unavailable"})
		return
	}
	if llm.Structured == nil || strings.TrimSpace(llm.ModelUsed) == "" {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "portfolio review returned an invalid response"})
		return
	}
	review := PortfolioReview{
		ID: "pr_" + newID(), PortfolioID: portfolio.ID, ContextVersion: intel.ContextVersion,
		Structured: llm.Structured, ModelUsed: llm.ModelUsed, Warnings: llm.Warnings,
		Retried: llm.Retried, Disclaimer: llm.Disclaimer, CreatedAt: time.Now().Unix(),
	}
	review, err = s.portfolioReviews.Add(uid, review)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "portfolio review could not be persisted"})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"review": review, "reused": false})
}

func (s *Server) handleListPortfolioReviews(w http.ResponseWriter, r *http.Request) {
	uid := userID(r)
	portfolioID := r.PathValue("id")
	if _, ok, err := s.portfolios.Get(uid, portfolioID); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "portfolio store is unreadable"})
		return
	} else if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "portfolio not found"})
		return
	}
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 1 && n <= 100 {
			limit = n
		} else {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "limit must be between 1 and 100"})
			return
		}
	}
	reviews, err := s.portfolioReviews.List(uid, portfolioID, limit)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "portfolio review store is unreadable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reviews": reviews})
}

func (s *Server) handlePortfolioScenario(w http.ResponseWriter, r *http.Request) {
	uid := userID(r)
	portfolio, ok, err := s.portfolios.Get(uid, r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "portfolio store is unreadable"})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "portfolio not found"})
		return
	}
	var body struct {
		Question string `json:"question"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON: " + err.Error()})
		return
	}
	body.Question = strings.TrimSpace(body.Question)
	if body.Question == "" || len([]rune(body.Question)) > 1500 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "question is required and must be at most 1500 characters"})
		return
	}
	intel, err := s.buildPortfolioIntelligence(r.Context(), uid, portfolio)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "portfolio intelligence is unavailable"})
		return
	}
	var scenario portfolioLLMResponse
	if err := s.postJSON(r.Context(), s.cfg.LLMURL+"/portfolio-scenario", map[string]any{
		"question": body.Question, "context": intel,
	}, &scenario); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "portfolio scenario is unavailable"})
		return
	}
	if scenario.Structured == nil || strings.TrimSpace(scenario.ModelUsed) == "" {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "portfolio scenario returned an invalid response"})
		return
	}
	// The journal owns the actual context version. Never trust a downstream echo for audit identity.
	scenario.ContextVersion = intel.ContextVersion
	scenario.Question = body.Question
	writeJSON(w, http.StatusOK, map[string]any{"scenario": scenario})
}
