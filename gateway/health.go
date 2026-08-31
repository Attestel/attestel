package main

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// ───────────────────────────────────────────────────────────────────── service health
//
// WHY THIS FILE EXISTS.
//
// `/health` used to be `writeJSON(w, 200, {"status":"ok","service":"gateway"})` — a literal. It
// could not fail, so it could not diagnose. On 2026-08-22 PostgreSQL stopped accepting connections
// at the ClusterIP the whole stack shares (`DATABASE_URL` fans out to both `BARS_DATABASE_URL` and
// `EVENTS_DATABASE_URL` in `deploy/entrypoint.sh`); `services/analysis` threw
// `psycopg.OperationalError` on every request, `services/events` could not boot at all, and the
// frontend HealthDot cluster stayed green for the entire outage because all five Go services were
// answering their own constants. The only signal that reached a human was the
// `events:unreachable` banner in the feed, which is a §9.44 degradation marker in a 200 body and is
// deliberately never logged.
//
// So this endpoint now ASKS. It fans out to the four upstreams that can actually be broken and
// reports what each one said, including whether its PostgreSQL store answered — because both
// `analysis` and `events` return HTTP 200 from their own `/health` while their database is down,
// and a probe that only reads the status code would repeat exactly the failure it exists to catch.
//
// LIVENESS vs READINESS — point the platform at the right one:
//
//   - `/health` is LIVENESS. It is 200 whenever the gateway process itself can serve, even when
//     every dependency is down; the body carries `status: "degraded"` and names them. Wire the
//     container's liveness probe here. Wiring it to a dependency check would restart-loop a
//     perfectly healthy gateway for the duration of a database outage.
//   - `/ready` is READINESS. It is 503 when a dependency in `Config.ReadyRequire` is down, so the
//     platform stops routing traffic to an instance that cannot answer. Wire the readiness probe
//     here — and NOT the liveness probe.
//
// DISCLOSURE. `/health` is the one unauthenticated route (D-16), and error strings and database
// targets name internal hosts. Both are therefore withheld from anonymous callers: a signed-out
// probe gets liveness and per-dependency up/down, a signed-in operator gets the detail that makes
// the outage diagnosable. `up` is never withheld — hiding WHICH dependency is down would recreate
// the silence this file was written to end.

// healthProbeTimeout bounds ONE upstream probe. Far below the Server's 130s client timeout on
// purpose: a health endpoint that can hang cannot be polled, so an upstream that has not answered
// in three seconds is reported down rather than waited on.
const healthProbeTimeout = 3 * time.Second

// healthDeps is the fan-out set, in display order. These are the four services with a failure mode
// the gateway cannot see from its own process: the Go siblings (alerts/journal/paper/auth) are
// polled directly by the browser and own their own dots.
var healthDeps = []string{"analysis", "events", "llm", "prediction"}

// depHealth is one upstream's answer. `Up` means "this dependency can do its job" — not merely
// that a socket opened, and not merely that it returned 200.
type depHealth struct {
	Up        bool         `json:"up"`
	LatencyMS int64        `json:"latencyMs"`
	Status    string       `json:"status,omitempty"`   // upstream's own self-report, when not "ok"
	Error     string       `json:"error,omitempty"`    // withheld from anonymous callers
	Database  *depDatabase `json:"database,omitempty"` // only for the PostgreSQL-backed services
}

// depDatabase is the part a status code cannot tell you. `services/analysis` reports its store
// under `barStore` and `services/events` under `db`/`dbError`; both answer 200 either way.
type depDatabase struct {
	OK     bool   `json:"ok"`
	Target string `json:"target,omitempty"` // host:port/dbname — credential-free upstream, still withheld
	Error  string `json:"error,omitempty"`  // withheld from anonymous callers
}

// depURL maps a dependency name to its configured base URL.
func (s *Server) depURL(name string) string {
	switch name {
	case "analysis":
		return s.cfg.AnalysisURL
	case "events":
		return s.cfg.EventsURL
	case "llm":
		return s.cfg.LLMURL
	case "prediction":
		return s.cfg.PredictionURL
	}
	return ""
}

// probeDeps fans out to every dependency concurrently — the same `sync.WaitGroup` shape
// `handleDashboard` uses. One slow upstream costs healthProbeTimeout, never the sum.
func (s *Server) probeDeps(ctx context.Context) map[string]depHealth {
	out := make(map[string]depHealth, len(healthDeps))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, name := range healthDeps {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			d := s.probeDep(ctx, name)
			mu.Lock()
			out[name] = d
			mu.Unlock()
		}(name)
	}
	wg.Wait()
	return out
}

// probeDep asks ONE upstream and decides whether its answer means "working".
func (s *Server) probeDep(ctx context.Context, name string) depHealth {
	base := strings.TrimRight(strings.TrimSpace(s.depURL(name)), "/")
	if base == "" {
		return depHealth{Up: false, Status: "unconfigured", Error: name + ": no URL configured"}
	}

	probeCtx, cancel := context.WithTimeout(ctx, healthProbeTimeout)
	defer cancel()

	started := time.Now()
	body, err := s.getJSON(probeCtx, base+"/health")
	d := depHealth{LatencyMS: time.Since(started).Milliseconds()}
	if err != nil {
		// Transport failure or a non-2xx. Either way this dependency is not serving.
		d.Error = err.Error()
		return d
	}

	d.Up = true

	// The upstream's own verdict. `services/events` reports `status: "degraded"` when its store is
	// unreachable but the process is fine — that is a down dependency for our purposes, because a
	// PostgreSQL-only service without PostgreSQL cannot answer a single real query.
	if st, ok := body["status"].(string); ok && st != "" && st != "ok" {
		d.Status = st
		d.Up = false
	}

	if db := extractDatabase(name, body); db != nil {
		d.Database = db
		if !db.OK {
			d.Up = false
		}
	}
	return d
}

// extractDatabase reads the store block out of an upstream health body. Returns nil for services
// that own no database, so their entry simply has no `database` key rather than a misleading one.
func extractDatabase(name string, body map[string]any) *depDatabase {
	switch name {
	case "analysis":
		// services/analysis/app/store.py:store_health — always present, carries `error` on failure.
		raw, ok := body["barStore"].(map[string]any)
		if !ok {
			return nil
		}
		db := &depDatabase{OK: true}
		if target, ok := raw["target"].(string); ok {
			db.Target = target
		}
		if msg, ok := raw["error"].(string); ok && msg != "" {
			db.OK = false
			db.Error = msg
		}
		if db.Target == "" && db.Error == "" {
			// No target and no error means the store never resolved a URL at all. Reporting that as
			// healthy is the silence this file exists to remove.
			db.OK = false
			db.Error = "bar store reported no database target"
		}
		return db

	case "events":
		// services/events/app/main.py:health — `db` is the credential-free target, `dbError` is set
		// when the connection or the migration check failed.
		db := &depDatabase{OK: true}
		if target, ok := body["db"].(string); ok {
			db.Target = target
		}
		if msg, ok := body["dbError"].(string); ok && msg != "" {
			db.OK = false
			db.Error = msg
		}
		return db
	}
	return nil
}

// redactDeps strips operator-only detail for anonymous callers. `up`, `latencyMs`, `status` and the
// existence of a `database` block survive; hostnames, driver messages and connection strings do not.
func redactDeps(deps map[string]depHealth) map[string]depHealth {
	out := make(map[string]depHealth, len(deps))
	for name, d := range deps {
		d.Error = ""
		if d.Database != nil {
			d.Database = &depDatabase{OK: d.Database.OK}
		}
		out[name] = d
	}
	return out
}

// downNames lists the dependencies that failed, sorted so the field is stable across polls.
func downNames(deps map[string]depHealth) []string {
	var down []string
	for name, d := range deps {
		if !d.Up {
			down = append(down, name)
		}
	}
	sort.Strings(down)
	if down == nil {
		down = []string{}
	}
	return down
}

// handleHealth is LIVENESS: always 200 while the gateway can serve, with the truth in the body.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	deps := s.probeDeps(r.Context())
	down := downNames(deps)

	status := "ok"
	if len(down) > 0 {
		status = "degraded"
	}
	if s.userIDFrom(r) == "" {
		deps = redactDeps(deps)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":       status,
		"service":      "gateway",
		"checkedAt":    time.Now().UTC().Format(time.RFC3339),
		"dependencies": deps,
		"down":         down,
	})
}

// handleReady is READINESS: 503 when a dependency this instance cannot serve without is down.
//
// Only the names in `Config.ReadyRequire` (READY_REQUIRE, default "analysis,events") count. `llm`
// is deliberately not among them — invariant #1b: the llm service answers `stub:offline` with no
// network and no weights, so a missing model runtime is a documented shape, not an outage.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	deps := s.probeDeps(r.Context())

	var notReady []string
	for _, name := range s.cfg.ReadyRequire {
		if d, ok := deps[name]; !ok || !d.Up {
			notReady = append(notReady, name)
		}
	}
	sort.Strings(notReady)

	code, status := http.StatusOK, "ready"
	if len(notReady) > 0 {
		code, status = http.StatusServiceUnavailable, "unavailable"
	} else {
		notReady = []string{}
	}
	if s.userIDFrom(r) == "" {
		deps = redactDeps(deps)
	}

	writeJSON(w, code, map[string]any{
		"status":       status,
		"service":      "gateway",
		"checkedAt":    time.Now().UTC().Format(time.RFC3339),
		"required":     s.cfg.ReadyRequire,
		"notReady":     notReady,
		"dependencies": deps,
	})
}
