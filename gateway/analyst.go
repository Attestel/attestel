package main

// analyst.go — Wave 3 Lane 3B. The user-initiated route in front of the agentic analyst pipeline
// (`services/llm/app/analyst.py`, EVENT_CONTRACTS.md §7).
//
// INVARIANT #4, STATED HERE BECAUSE THE NEXT AGENT WILL OTHERWISE WIRE THIS INTO A POLL.
// This route is USER-INITIATED ONLY. No page load, ticker change, quote poll, alert poll,
// evaluator tick, cron, scheduler or prefetch may reach it. One request costs up to eight
// sequential local-model generations, each of which takes the cross-process model lease
// (`{READS_DIR}/.model.lock`, contract §9.21) and therefore blocks every other generation on the
// machine for its duration. A poll on this route does not merely waste tokens — it starves the
// interactive path it shares the lease with.
//
// It is a POST for the same reason. A GET invites a browser prefetch, a link prefetch, a
// speculative-navigation hint and an over-eager `useEffect`; each of those is an invariant #4
// violation that nobody wrote a line of code to cause. `handleAnalyst` therefore also refuses any
// non-POST method explicitly rather than relying on the mux, and does no work before it does so.
//
// IT IS ASYNCHRONOUS, AND THAT IS ALSO AN INVARIANT #4 STORY. A run is up to eight sequential
// generations; it does not fit inside an HTTP request, and pretending otherwise is what made the
// flagship flow return 504 in production while the run carried on unobserved. So this POST STARTS
// the run and answers `202` with a `runId`, and `GET /api/analyst/runs/{runID}`
// (`analystjobs.go`) reports it. The status read is the one thing in this lane that may be polled:
// it is a map lookup, it reaches no upstream, and it can never cause a generation. The POST's rules
// are untouched — user-initiated, POST-only, one run per (ticker, horizon, asOf, uid) at a time.
//
// Stdlib only (invariant #5): `s.getJSON` / `s.postJSON` from clients.go, `s.cache` from cache.go.
// No third HTTP helper and no new module — `gateway/go.mod` still has zero `require` entries.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

// analystDisclaimerGateway is NOT declared here. `gateway/explore.go` (Lane 2C) already declares
// `analystDisclaimer` at package scope with the byte-for-byte text of §9.18's ANALYST_DISCLAIMER,
// and gateway is one flat `package main` — a second declaration is a compile error. This file
// reuses that constant, which is what "reconciled" means in a single-package program.

// analystHorizons is §9.11's request-token vocabulary. The gateway validates against it and never
// translates: the `1d -> 1_trading_day` mapping lives in exactly one place, and that place is
// `services/llm/app/analyst.py::HORIZON_FIELD`. Restating it here would be the third vocabulary
// §9.11 forbids.
var analystHorizons = map[string]bool{"1d": true, "5d": true, "20d": true, "60d": true}

// analystTimeout bounds one analyst run. Up to eight sequential local-model generations at
// `llm.py`'s 120 s per-call timeout, plus lease waits behind an in-flight background generation.
// The same 10-minute shape `committee.go` already uses for the same reason.
const analystTimeout = 10 * time.Minute

// analystContextProvider is a test seam around the production point-in-time context assembler.
// Keeping the seam lets route tests avoid sockets, but the default MUST remain
// `buildContextEnvelope`: `committeeContextFor` reads live providers and cannot honour `asOf`.
func productionAnalystContextProvider(s *Server, ctx context.Context, ticker, horizon, asOf string, id identity) map[string]any {
	contextHorizon := horizon
	if contextHorizon == "" {
		contextHorizon = defaultHorizon
	}
	return s.buildContextEnvelope(ctx, ticker, asOf, contextHorizon, id)
}

var analystContextProvider = productionAnalystContextProvider

// registerAnalystRoutes wires this lane's single route.
//
// It is NOT attached through `routeseams.go`'s `registerEventRoute`, and that is a measured
// decision rather than an oversight: `gateway/routeseams_test.go` pins `wantEventRegistrars = 1`
// and its own header says the file "is in no lane's ownership" — attaching a second registrar here
// would turn a green suite red in a file this lane may not edit. The brief's Phase 6 and its
// handoff template both specify this method instead, so the integrator's change is one line in
// `gateway/main.go` and nothing else moves. See the handoff's REQUIRED CHANGES section.
func (s *Server) registerAnalystRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/analyst/{ticker}", s.handleAnalyst)
	// The status read. A GET on purpose, and safe as a GET for the same reason the run route is not:
	// it starts nothing, it touches no upstream, and a prefetch of it costs a map lookup.
	mux.HandleFunc("GET /api/analyst/runs/{runID}", s.handleAnalystRun)
}

// analystRequest is the POST body. Both fields are optional: absent `asOf` means "now" (§1), absent
// `horizon` means all four (§9.11).
type analystRequest struct {
	Horizon string `json:"horizon"`
	AsOf    string `json:"asOf"`
}

// handleAnalyst runs the analyst pipeline for one ticker.
func (s *Server) handleAnalyst(w http.ResponseWriter, r *http.Request) {
	// Refuse anything that is not a POST before touching the cache, the body or an upstream. The
	// mux pattern already restricts the method; this is the belt to that braces, so a future
	// registration under a bare path cannot silently make the route prefetchable (invariant #4).
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{
			"error":      "analyst runs are POST-only: they are expensive and user-initiated",
			"disclaimer": analystDisclaimer,
		})
		return
	}

	ticker := strings.ToUpper(r.PathValue("ticker"))
	if ticker == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing ticker"})
		return
	}

	var req analystRequest
	if body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16)); err == nil && len(body) > 0 {
		_ = json.Unmarshal(body, &req) // an unparseable body degrades to "all horizons, now"
	}
	horizon := strings.TrimSpace(req.Horizon)
	if horizon != "" && !analystHorizons[horizon] {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "horizon must be one of 1d, 5d, 20d, 60d (EVENT_CONTRACTS.md §9.11)",
		})
		return
	}
	asOf := strings.TrimSpace(req.AsOf)
	if asOf != "" {
		if _, err := time.Parse(time.RFC3339, asOf); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "asOf must be an RFC3339 timestamp: " + asOf,
			})
			return
		}
	}
	id := s.identityFrom(r)

	// CACHE KEY. `uid` is in the key because the envelope can carry the caller's subscriptions, and
	// `asOf` is in the key because a HISTORICAL run and a LIVE run must never share an entry — a
	// live answer served for a past cutoff is a leak of the present into the past, which is the one
	// failure §1 exists to prevent. Empty `asOf` (live) is a DIFFERENT key from any timestamp, so
	// the two spaces cannot collide.
	key := analystCacheKey(ticker, horizon, asOf, id.userID)
	if r.URL.Query().Get("refresh") == "" {
		if cached, ok := s.cache.get(key); ok {
			// A cache hit is a FINISHED run, so it answers 200 with the envelope and says
			// `status: "done"`. That single field is what lets the client tell the two POST outcomes
			// apart without inspecting the payload: `202 + status running` means poll, `200 + status
			// done` means render.
			var env map[string]any
			if err := json.Unmarshal(cached, &env); err == nil {
				env["status"] = analystDone
				env["runId"] = analystRunID(key)
				w.Header().Set("X-Cache", "HIT")
				writeJSON(w, http.StatusOK, env)
				return
			}
		}
	}

	// START THE RUN AND ANSWER NOW.
	//
	// Everything expensive — the point-in-time context assembly and the model calls — happens in the
	// background goroutine under a `context.Background()` deadline, because the request that asked
	// for the run is about to end and must not cancel it. `begin` is what keeps one run per key: a
	// second POST arriving while the first is in flight gets the SAME `runId` and starts nothing, so
	// two runs can never queue against each other for the cross-process model lease.
	job, owned := s.analystRuns().begin(key, ticker, horizon, asOf)
	if owned {
		s.startAnalystRun(job, id)
	}

	w.Header().Set("X-Cache", "MISS")
	writeJSON(w, http.StatusAccepted, map[string]any{
		"ticker":      ticker,
		"runId":       job.ID,
		"status":      analystRunning,
		"href":        "/api/analyst/runs/" + job.ID,
		"startedAt":   job.StartedAt.UTC().Format(time.RFC3339),
		"attached":    !owned,
		"pollAfterMs": analystRunPollMs,
		"disclaimer":  analystDisclaimer,
	})
}

// analystCacheKey is a function rather than an inline concatenation so the live/historical
// isolation above is testable without a server.
func analystCacheKey(ticker, horizon, asOf, uid string) string {
	if horizon == "" {
		horizon = "all"
	}
	if asOf == "" {
		asOf = "live"
	}
	if uid == "" {
		uid = "guest"
	}
	return "analyst:" + ticker + "|" + horizon + "|" + asOf + "|" + uid
}

// AnalystTTL is the cache lifetime for one analyst run, in the style of `COMMITTEE_TTL`.
//
// It is a METHOD on Config rather than a field, because adding a field means editing
// `gateway/config.go` — a file this lane does not own. The default matches `CommitteeTTL`'s 30
// minutes for the same reason: a run is many sequential local-model calls, so it is cached hard,
// and `?refresh=1` is the explicit escape hatch. `envDur` is config.go's existing helper, so
// `ANALYST_TTL` in the environment works exactly like every other TTL in this service.
func (c Config) AnalystTTL() time.Duration { return envDur("ANALYST_TTL", 30*time.Minute) }
