package main

// subscriptions.go — the follow-graph pass-through, and the one-time `activeTicker` migration.
// Contract: EVENT_CONTRACTS.md §9.3 (cookie forwarding), §9.5 + §9.39 (the migration), §9.39a.
//
// Two jobs, and the split between them is the whole design:
//
//  1. PROXY. `/api/subscriptions*` and `/api/event-state` are journal's surface. The gateway adds
//     nothing to them — no reshaping, no caching, no merging — it forwards the session cookie and
//     copies the upstream status and body back verbatim. `handleJournalProxy` (evidence.go) already
//     does exactly this and is reused rather than re-implemented.
//
//  2. THE MIGRATION, and only the half the gateway alone can do. §9.39 is explicit: the gateway
//     does NOT decide whether to migrate. It cannot — the marker is "does `subscriptions.json`
//     exist?", which lives inside journal and is invisible through §2.1's response shape, where an
//     empty array means both *never migrated* and *deliberately unfollowed everything*. A gateway
//     that seeded on empty would hand a user back, on every page load, the one ticker they had
//     explicitly removed.
//
//     So the gateway supplies the single fact it alone holds — `activeTicker`, read from auth with
//     the caller's cookie — and journal decides. `POST /subscriptions/migrate` is idempotent by
//     construction (the second call sees the file the first one wrote), which is what lets the
//     gateway call it without tracking whether it already has.
//
// WHY THERE IS NO PER-USER STATE HERE. The obvious optimisation — remember which users we have
// migrated — is forbidden: the gateway writes nothing to disk by design, and per-user state here
// would be a second place D-06 erasure has to reach. `migratedThisSession` below is a bounded
// in-memory set that exists only to skip a redundant round trip; losing it on restart costs one
// extra idempotent call and nothing else. It is deliberately NOT durable.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// init attaches this file's routes through the Wave 0 seam, so neither `main.go` nor `routeseams.go`
// is edited here. Both files' doc comments say a lane adds its own file with an init(); this is that.
func init() {
	registerSubscriptionRoute(func(s *Server, mux *http.ServeMux) {
		s.registerSubscriptionProxy(mux)
	})
}

// registerSubscriptionProxy mounts journal's follow-graph surface under /api.
//
// `POST /api/subscriptions/migrate` is deliberately ABSENT. The migration is a server-side
// consequence of the first list call, not a browser-callable endpoint: exposing it would let a page
// re-seed a ticker the user had removed, which is the exact bug §9.39 exists to prevent.
func (s *Server) registerSubscriptionProxy(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/subscriptions", s.handleSubscriptionsList)
	mux.HandleFunc("POST /api/subscriptions", s.handleJournalProxy)
	mux.HandleFunc("PATCH /api/subscriptions/{id}", s.handleJournalProxy)
	mux.HandleFunc("DELETE /api/subscriptions/{id}", s.handleJournalProxy)
	mux.HandleFunc("GET /api/event-state", s.handleJournalProxy)
	mux.HandleFunc("POST /api/event-state", s.handleJournalProxy)
	// Wave 5 Lane 5B: the server-side "when did you last look at what changed" boundary, moved out
	// of the browser's `localStorage` and into journal's per-user partition (§2.3). Registered
	// explicitly rather than as a prefix, for the same reason every other route here is: a prefix
	// proxy on `/api/event-state` would forward paths nobody has audited.
	mux.HandleFunc("GET /api/event-state/last-check", s.handleJournalProxy)
	mux.HandleFunc("POST /api/event-state/last-check", s.handleJournalProxy)
	// §9.4: /_internal/tickers is NOT proxied and must not be. It is bound to the internal network
	// for Wave 5's monitor, and journal refuses it outright when a session cookie is present — a
	// rule a server-side caller that drops the cookie would trivially bypass.
}

// migrationLedger remembers which users this PROCESS has already asked journal about.
//
// Not a migration record — journal owns that, on disk, in the file whose existence is the marker.
// This only avoids a redundant round trip per session. It is capped so a long-running gateway
// cannot grow it without bound, and eviction is safe by construction: the worst case is one more
// idempotent call that journal answers `{"migrated": false}`.
type migrationLedger struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

// maxMigrationLedgerEntries bounds the set. Well above any plausible concurrent user count for a
// single-user tool, and small enough that the map cannot become a leak.
const maxMigrationLedgerEntries = 4096

func newMigrationLedger() *migrationLedger {
	return &migrationLedger{seen: map[string]time.Time{}}
}

// claim reports whether this caller still needs the migration question asked. It returns true at
// most once per user per process.
func (m *migrationLedger) claim(uid string) bool {
	if uid == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, done := m.seen[uid]; done {
		return false
	}
	if len(m.seen) >= maxMigrationLedgerEntries {
		// Drop the oldest rather than refusing to record: an unbounded map is a real leak, a
		// redundant idempotent POST is not.
		var oldestUID string
		var oldest time.Time
		for k, v := range m.seen {
			if oldestUID == "" || v.Before(oldest) {
				oldestUID, oldest = k, v
			}
		}
		delete(m.seen, oldestUID)
	}
	m.seen[uid] = time.Now()
	return true
}

// subscriptionMigrations is process-wide because Server is constructed once. A package-level var
// rather than a Server field so this lane's footprint in shared files stays at zero lines.
var subscriptionMigrations = newMigrationLedger()

// handleSubscriptionsList runs the one-time migration, then proxies the list verbatim.
//
// ORDER MATTERS, but not for correctness: §9.39a made `GET /subscriptions` stop creating the file,
// so a read can no longer race the marker. It matters for the user — migrating first means the
// seeded ticker appears on the FIRST page load rather than the second.
func (s *Server) handleSubscriptionsList(w http.ResponseWriter, r *http.Request) {
	if uid := s.userIDFrom(r); uid != "" && subscriptionMigrations.claim(uid) {
		// Best-effort and deliberately un-surfaced. A failed migration must not turn a working list
		// call into an error: the user sees their follow graph, and the next session tries again.
		s.migrateActiveTicker(r)
	}
	s.handleJournalProxy(w, r)
}

// activeTickerOf reads the caller's `activeTicker` from auth, with their own cookie so the answer is
// scoped server-side. "" on any failure, which journal treats as "nothing to carry over".
//
// This is the ONLY reason the gateway holds an AUTH_URL. It reads and never writes: §2.2 keeps
// `activeTicker` as the "currently open company" pointer, and following a company is a different
// state from having it open.
func (s *Server) activeTickerOf(ctx context.Context, cookie string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.AuthURL+"/auth/settings", nil)
	if err != nil {
		return ""
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var body struct {
		Settings struct {
			ActiveTicker string `json:"activeTicker"`
		} `json:"settings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return ""
	}
	return strings.ToUpper(strings.TrimSpace(body.Settings.ActiveTicker))
}

// migrateActiveTicker asks journal the question only journal can answer, supplying the one fact only
// the gateway can supply. Fire-and-forget: nothing is read back, because there is nothing the
// gateway would do differently either way.
//
// It runs on its own short timeout rather than the request's context. The request is about to be
// proxied and must not be delayed by a slow auth service; and a migration that times out is simply
// retried next session, since journal's answer is idempotent.
func (s *Server) migrateActiveTicker(r *http.Request) {
	cookie := r.Header.Get("Cookie")
	if cookie == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), migrationTimeout)
	defer cancel()

	ticker := s.activeTickerOf(ctx, cookie)
	// An empty ticker is still worth sending: journal lays down the marker for a user with nothing
	// to carry over, so the question stays a one-time event rather than one asked every session.
	payload, err := json.Marshal(map[string]string{"activeTicker": ticker})
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.cfg.JournalURL+"/subscriptions/migrate", strings.NewReader(string(payload)))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cookie", cookie)
	resp, err := s.http.Do(req)
	if err != nil {
		return
	}
	_ = resp.Body.Close()
}

// migrationTimeout bounds the two hops (auth settings, then journal migrate) so a slow dependency
// cannot hold up the list call the user is actually waiting for.
const migrationTimeout = 3 * time.Second
