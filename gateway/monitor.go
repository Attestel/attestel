package main

import (
	"context"
	"net/http"
	"time"
)

// monitor.go — `GET /api/monitor/theses`, Wave 5 Lane 5B.
//
// A COOKIE-FORWARDING READ-THROUGH to `alerts`' `GET /monitoring/theses`, exactly the shape
// `gateway/subscriptions.go` already uses for journal: the frontend goes through the gateway,
// always (§9.3), and never names :8095.
//
// §9.59 — A VERBATIM PROXY MAY ADD, NEVER ALTER. Everything `alerts` sent is forwarded untouched,
// including `enabled: false` and the `resynthQueue.drainer` note. The gateway adds exactly one key
// it owns, `available`, which says whether the upstream answered at all. It renames nothing,
// reorders nothing and drops nothing — a proxy that "tidied" the payload would be the second half
// of §9.60's failure, where a client renders a field whose disclosure never arrived.
//
// THE DEGRADED ANSWER IS 200, NOT 502. `alerts` is an additive service and the surface that reads
// this renders alongside others. A page that says "monitoring unavailable" is correct; a page
// broken by a 502 is not. What it must never do is render an outage as "nothing went stale" — so
// the degraded body carries `available: false` and a sentence saying exactly that, in the same
// posture as `WhatChangedPanel`'s "It is not a statement that nothing changed."
//
// This route is READ-ONLY and triggers nothing. The sweep is a schedule inside `alerts`; asking it
// for its results does not make it run, and nothing in the path reaches a model (invariant #4).

func init() {
	registerEventRoute(func(s *Server, mux *http.ServeMux) {
		s.registerMonitorRoutes(mux)
	})
}

func (s *Server) registerMonitorRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/monitor/theses", s.handleMonitorTheses)
}

const monitorProxyTimeout = 10 * time.Second

func (s *Server) handleMonitorTheses(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), monitorProxyTimeout)
	defer cancel()

	cookie := ""
	if c, err := r.Cookie(s.cfg.CookieName); err == nil {
		cookie = c.String()
	}
	res, err := s.journalGet(ctx, s.cfg.AlertsURL+"/monitoring/theses", cookie)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"available": false,
			"enabled":   false,
			"markers":   []any{},
			"note": "The thesis monitor could not be reached. This is not a statement that " +
				"nothing went stale.",
			"error": err.Error(),
		})
		return
	}
	// Verbatim, plus the one key this proxy owns (§9.59).
	res["available"] = true
	writeJSON(w, http.StatusOK, res)
}
