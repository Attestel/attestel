package main

// opportunities.go exposes the events service's stored Early Opportunity Radar snapshot.
//
// This is a read-through only. A page load cannot start a scan, refresh bars, call prediction,
// call Qwen, or touch paper trading. The events service owns the versioned detector and every
// explanation; the gateway preserves that payload verbatim.

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

func init() {
	registerEventRoute(func(s *Server, mux *http.ServeMux) {
		mux.HandleFunc("GET /api/opportunities", s.handleOpportunities)
	})
}

const opportunityDefaultLimit = 20

func (s *Server) handleOpportunities(w http.ResponseWriter, r *http.Request) {
	limit := opportunityDefaultLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if value, err := strconv.Atoi(raw); err == nil && value > 0 {
			limit = min(value, 100)
		}
	}
	query := url.Values{}
	query.Set("limit", strconv.Itoa(limit))
	if ticker := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("ticker"))); ticker != "" {
		query.Set("ticker", ticker)
	}
	if state := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("state"))); state != "" {
		query.Set("state", state)
	}

	key := "opportunities:" + query.Encode()
	if cached, ok := s.cache.get(key); ok {
		w.Header().Set("X-Cache", "HIT")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(cached)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), feedsTimeout)
	defer cancel()
	body, err := s.eventsGet(ctx, "opportunities?"+query.Encode())
	if err != nil {
		marker := degradedEventsUnreachable
		if err == errEventsUnconfigured {
			marker = degradedEventsUnconfigured
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"runId":            nil,
			"coverage":         map[string]any{"state": "insufficient", "reason": marker},
			"candidates":       []any{},
			"degraded":         []string{marker},
			"paperEligibility": map[string]any{"state": "not-assessed"},
			"disclaimer":       "Research lead, not an investment recommendation.",
		})
		return
	}
	s.cache.set(key, body, s.cfg.CalendarTTL)
	w.Header().Set("X-Cache", "MISS")
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}
