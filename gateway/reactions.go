package main

// reactions.go — Phase 4's read-through: stored before/after evidence, and the aggregate that
// refuses to exist until it has earned the right to.
//
// TWO ROUTES, BOTH READS, BOTH STORE-BACKED
//
//	GET /api/reactions?eventId=…|ticker=…   what the market did after a stored event
//	GET /api/sensitivity?ticker=…&kind=…    the aggregate over matured, non-synthetic reactions
//
// Neither fetches a bar and neither calls a model. `services/events` computed these from bars the
// analysis service had already stored; the gateway forwards what it computed and adds nothing.
//
// THE ONE RULE THIS FILE ENFORCES ON ITS OWN. An unreachable upstream produces
// `sufficient: false` with a stated reason — never a missing field a client might render as a
// hopeful blank, and never a cached "last known good" number. "We could not check" and "there is
// not enough history" are both honest; "here is a percentage" is not.

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
)

func init() {
	registerEventRoute(func(s *Server, mux *http.ServeMux) {
		mux.HandleFunc("GET /api/reactions", s.handleReactions)
		mux.HandleFunc("GET /api/sensitivity", s.handleSensitivity)
	})
}

// reactionQuery copies through only the parameters the events service understands. An allowlist
// rather than a passthrough: a proxied query string is a place where an unexpected parameter can
// change an upstream's meaning.
func reactionQuery(r *http.Request, allowed ...string) url.Values {
	out := url.Values{}
	for _, name := range allowed {
		if value := strings.TrimSpace(r.URL.Query().Get(name)); value != "" {
			out.Set(name, value)
		}
	}
	return out
}

func (s *Server) handleReactions(w http.ResponseWriter, r *http.Request) {
	query := reactionQuery(r, "eventId", "ticker", "limit")
	if query.Get("eventId") == "" && query.Get("ticker") == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "eventId or ticker is required",
		})
		return
	}

	body, err := s.eventsGet(r.Context(), "/reactions?"+query.Encode())
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"reactions": []any{}, "degraded": []string{"events:unavailable"},
		})
		return
	}
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"reactions": []any{}, "degraded": []string{"events:invalid-response"},
		})
		return
	}
	if payload["degraded"] == nil {
		payload["degraded"] = []string{}
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *Server) handleSensitivity(w http.ResponseWriter, r *http.Request) {
	query := reactionQuery(r, "ticker", "kind", "series", "horizon", "as_of")

	body, err := s.eventsGet(r.Context(), "/sensitivity?"+query.Encode())
	if err != nil {
		// NOT a blank and NOT a stale number. An aggregate we could not read is reported as
		// insufficient with the reason stated, which is the only answer that cannot be rendered as
		// a percentage.
		writeJSON(w, http.StatusOK, map[string]any{
			"sufficient": false, "reason": "sensitivity unavailable",
			"raw": nil, "excess": nil, "sampleCount": nil,
			"degraded": []string{"events:unavailable"},
		})
		return
	}
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"sufficient": false, "reason": "sensitivity unavailable",
			"raw": nil, "excess": nil, "sampleCount": nil,
			"degraded": []string{"events:invalid-response"},
		})
		return
	}
	if payload["degraded"] == nil {
		payload["degraded"] = []string{}
	}
	writeJSON(w, http.StatusOK, payload)
}
