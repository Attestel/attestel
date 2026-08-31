package main

// automation.go — the READ side of Phase 1's automation health, and nothing else.
//
// `services/events` owns the lane ledger. This file proxies exactly one of its endpoints,
// `GET /automation/status`, so an operator can see lane health in Settings instead of shelling into
// a container. Three properties are deliberate and are asserted by automation_test.go:
//
//  1. **Read-only.** The lease and complete routes are NOT proxied. They mutate lane state, they
//     are server-to-server, and they carry the internal secret — putting them behind a browser
//     route would make a page load able to start a background job, which is precisely the shape
//     invariant #4 forbids. There is no `POST` here.
//
//  2. **Signed-in only.** Automation health names which jobs a deployment runs and when they last
//     failed. That is operational detail, not public content, so a guest gets 401 — not a redacted
//     body, which invites the redaction to drift.
//
//  3. **It reaches the store, never a provider and never the model.** Its single upstream is
//     eventsGet, the same client every other store read uses.
//
// A degraded upstream produces an honest empty envelope with a stated reason, never a fabricated
// "all green" — a monitoring surface that cannot fail closed is worse than no monitoring surface.

import (
	"encoding/json"
	"net/http"
)

func init() {
	registerEventRoute(func(s *Server, mux *http.ServeMux) {
		mux.HandleFunc("GET /api/automation/status", s.handleAutomationStatus)
	})
}

func (s *Server) handleAutomationStatus(w http.ResponseWriter, r *http.Request) {
	if s.userIDFrom(r) == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": "sign in to view automation health",
		})
		return
	}

	body, err := s.eventsGet(r.Context(), "/automation/status?limit=20")
	if err != nil {
		// Honest degradation: the lanes list is empty and the reason is stated. A caller must not
		// be able to read this as "nothing is wrong".
		writeJSON(w, http.StatusOK, map[string]any{
			"automationEnabled": false,
			"lanes":             []any{},
			"recentRuns":        []any{},
			"providerQuotas":    []any{},
			"degraded":          []string{"events:unreachable"},
			"error":             err.Error(),
		})
		return
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"automationEnabled": false,
			"lanes":             []any{},
			"recentRuns":        []any{},
			"providerQuotas":    []any{},
			"degraded":          []string{"events:unreadable"},
		})
		return
	}
	if payload["degraded"] == nil {
		payload["degraded"] = []string{}
	}
	writeJSON(w, http.StatusOK, payload)
}
