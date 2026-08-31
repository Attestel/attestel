package main

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// subscriptions.go — the per-user FOLLOW GRAPH. Contract: EVENT_CONTRACTS.md §2, §2.1, §2.2, §9.4,
// §9.39.
//
// Before this file a user had exactly one company (`settings.activeTicker`, a single string held by
// the `auth` service) and the universe was a global `TICKERS` env shared by everybody. A follow is a
// different thing from an open tab, so it gets its own durable, per-user, erasable record.
//
// Four decisions shape everything below:
//
//  1. STORAGE IS THE JOURNAL'S EXISTING IDIOM, NOT A NEW ONE. Plain JSON at
//     `{TRADES_DIR}/{uid}/subscriptions.json`, one mutex, lazy per-user load, atomic `.tmp` +
//     `os.Rename` — identical to `evidence_store.go` and `trades.go`. D-26 put PostgreSQL in PYTHON
//     services for the GLOBAL corpus; per-user state stays here (AD-1), because D-06's erasure story
//     is "`rm -rf {TRADES_DIR}/{uid}/` removes everything this user owns" and a database breaks it.
//     A Go dependency would also break invariant #5.
//  2. FOLLOWING IS IDEMPOTENT. Following a company you already follow is not an error and not a
//     second row — it is the state you asked for. `POST /subscriptions` returns the stored record.
//  3. THERE IS NO CAP. Free-tier pressure is a provider-budget problem (AD-10, in the ingestion
//     service), not a reason to limit what a user may follow. A followed ticker also need not be in
//     the gateway's `TICKERS` — that env gates the NVDA-only seeded surfaces, not the follow graph.
//  4. NOTHING HERE IS A POSITION. A subscription says "tell me about this company". It carries no
//     size, no side, no price and no action (invariant #2).
//
// Zero keys, zero network: every endpoint is filesystem-only, which is why the whole surface works
// with an empty `.env` and no other service running.

const subscriptionsFile = "subscriptions.json"

// ---- enums (closed sets; a value outside them is a 400, never a stored surprise) ----

var validNotificationLevel = map[string]bool{
	"material": true, "all": true, "earnings": true, "macro": true, "none": true,
}

// validSubscriptionSource is closed on purpose: `migrated` is the audit trail for the one-time
// §2.2 migration, which `migrate` below now mints itself (§9.39). §2.1 still lists `source?` on the
// create body, so the value stays accepted there — closing the enum is what stops a typo becoming a
// stored surprise, and an explicitly-passed `migrated` is a claim about provenance, not a privilege.
var validSubscriptionSource = map[string]bool{
	"migrated": true, "manual": true, "explore": true,
}

// tickerPattern is the "malformed ticker" rule. Uppercased and trimmed before matching, so `nvda`
// is fine and ` NVDA ` is fine. Dots and hyphens are allowed for class shares (BRK.B, RDS-A); 12
// characters is longer than any listed US symbol.
var tickerPattern = regexp.MustCompile(`^[A-Z0-9][A-Z0-9.\-]{0,11}$`)

// Subscription is one followed company. Field order mirrors §2.
type Subscription struct {
	ID                   string `json:"id"` // sub_ + 16 hex
	Ticker               string `json:"ticker"`
	CreatedAt            int64  `json:"createdAt"` // unix seconds
	Priority             int    `json:"priority"`  // 0 = normal; lower sorts first; user-orderable
	NotificationsEnabled bool   `json:"notificationsEnabled"`
	NotificationLevel    string `json:"notificationLevel"` // material | all | earnings | macro | none
	Source               string `json:"source"`            // migrated | manual | explore
}

func newSubscriptionID() string { return "sub_" + newID() }

// writeFollowError emits {error, code}. `code` is what the frontend switches on — an unknown enum
// and an unknown id need different UI responses. Shared with `event_state.go`.
func writeFollowError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": msg, "code": code})
}

// Store-level failures shared by this lane's two stores.
var (
	errSubscriptionUnreadable  = errors.New("stored per-user state could not be read")
	errSubscriptionReservedUID = errors.New("reserved user partition")
)

// writeFollowStoreError maps them onto responses. `store_unreadable` is a 503, not a 500: the data
// is intact, we are refusing to overwrite it, and a retry after the file is fixed will work.
func writeFollowStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errSubscriptionUnreadable):
		writeFollowError(w, http.StatusServiceUnavailable, "store_unreadable",
			"stored per-user state could not be read; refusing to overwrite it")
	default:
		log.Printf("journal: subscription store error: %v", err)
		writeFollowError(w, http.StatusInternalServerError, "store_error", "per-user store unavailable")
	}
}

// ---- store ----

// subscriptionStore holds one collection per user behind one mutex.
//
// loadErr is the refuse-to-clobber guard copied from `evidence_store.go`: a file that EXISTS but
// does not parse is remembered, and every write is refused (503) rather than replacing a user's
// hand-edited or half-written follow graph with an empty array.
type subscriptionStore struct {
	base    string
	mu      sync.Mutex
	subs    map[string][]Subscription
	loadErr map[string]error
	docs    *documentRepository
}

func openSubscriptionStore(base string) (*subscriptionStore, error) {
	return openSubscriptionStoreWithRepository(base, nil)
}

func openSubscriptionStoreWithRepository(base string, docs *documentRepository) (*subscriptionStore, error) {
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, err
	}
	return &subscriptionStore{
		base:    base,
		subs:    map[string][]Subscription{},
		loadErr: map[string]error{},
		docs:    docs,
	}, nil
}

func (s *subscriptionStore) path(uid string) string {
	return filepath.Join(s.base, uid, subscriptionsFile)
}

// loadLocked lazily reads a user's follow graph. Caller must hold s.mu.
func (s *subscriptionStore) loadLocked(uid string) []Subscription {
	if v, ok := s.subs[uid]; ok {
		return v
	}
	subs := []Subscription{}
	var err error
	if s.docs != nil {
		var payload []byte
		payload, _, err = s.docs.load(uid, "subscriptions", s.path(uid))
		if err == nil && len(payload) > 0 {
			err = json.Unmarshal(payload, &subs)
		}
	} else {
		_, err = loadJSON(s.path(uid), &subs)
	}
	if err != nil {
		s.loadErr[uid] = err
		subs = []Subscription{}
	}
	s.subs[uid] = subs
	return subs
}

func (s *subscriptionStore) writableLocked(uid string) bool {
	s.loadLocked(uid)
	return s.loadErr[uid] == nil
}

// persistLocked writes the collection via temp file + rename, so a reader never sees a half-written
// file and no `.tmp` survives a successful write. Caller must hold s.mu.
//
// It returns the error rather than swallowing it because `migrate` must not answer `migrated: true`
// for a seed that never reached disk. The mutating handlers ignore it, as they always have: their
// answer is the record, and a failed write leaves the previous file intact.
func (s *subscriptionStore) persistLocked(uid string, v []Subscription) error {
	if s.docs != nil {
		if err := s.docs.save(uid, "subscriptions", v); err != nil {
			delete(s.subs, uid)
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Join(s.base, uid), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path(uid) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(uid))
}

// sortSubscriptions imposes the ONE order the whole system uses: priority ascending, then ticker
// ascending (§2.1). It is deterministic because the frontend does not sort this list again.
func sortSubscriptions(in []Subscription) {
	sort.SliceStable(in, func(i, j int) bool {
		if in[i].Priority != in[j].Priority {
			return in[i].Priority < in[j].Priority
		}
		return in[i].Ticker < in[j].Ticker
	})
}

// list returns a sorted copy. It is a pure read: it does NOT create the file.
//
// It used to, because under the pre-correction §2.2 the first GET was what laid down the migration
// marker. §9.39 moved the decision into `migrate` below, and the file is now "created on first
// write" — a GET is not a write. Creating it here would be worse than redundant: `migrate` reads the
// same marker, so a GET that ran first would permanently answer `migrated: false` and the one-time
// `activeTicker` seed could never fire at all.
func (s *subscriptionStore) list(uid string) ([]Subscription, error) {
	if reservedUID(uid) {
		return nil, errSubscriptionReservedUID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.loadLocked(uid)
	if s.loadErr[uid] != nil {
		return nil, errSubscriptionUnreadable
	}
	out := make([]Subscription, len(src))
	copy(out, src)
	sortSubscriptions(out)
	return out, nil
}

// migrate is §9.39's one-time `activeTicker` seed, decided where the marker already lives.
//
// The gateway knows the caller's `activeTicker` (it holds the cookie and can read auth settings);
// journal knows whether this user has ever had a follow graph. Neither fact is visible to the other
// service, so the gateway supplies the value and journal alone decides whether to use it. That is
// what closes the hole §9.39 describes: through §2.1's response shape an empty array means both
// "never migrated" and "deliberately unfollowed everything", and a gateway that seeded on empty
// would hand back, on every page load, the one ticker the user had explicitly removed.
//
// Returns (record, seeded). `seeded == false` is a no-op, not a failure.
func (s *subscriptionStore) migrate(uid, ticker string) (Subscription, bool, error) {
	if reservedUID(uid) {
		return Subscription{}, false, errSubscriptionReservedUID
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// EXISTENCE is the marker, and it is tested before anything is parsed. A file that exists but
	// does not parse still belongs to a migrated user, and seeding over it would be exactly the
	// clobber `errSubscriptionUnreadable` exists to prevent.
	if s.docs != nil {
		_, found, err := s.docs.load(uid, "subscriptions", s.path(uid))
		if err != nil {
			return Subscription{}, false, err
		}
		if found {
			return Subscription{}, false, nil
		}
	} else if _, err := os.Stat(s.path(uid)); err == nil {
		return Subscription{}, false, nil
	}

	// No `activeTicker` to carry over — the user reaches the follow graph with nothing to migrate.
	// The marker is still laid down (an empty file is the meaningful "migrated, follows nothing"
	// state of §9.39.2), so this stays a one-time event rather than a question re-asked every
	// session. The answer is `migrated: false`: nothing was seeded.
	if ticker == "" {
		if err := s.persistLocked(uid, []Subscription{}); err != nil {
			return Subscription{}, false, err
		}
		s.subs[uid] = []Subscription{}
		delete(s.loadErr, uid)
		return Subscription{}, false, nil
	}

	sub := Subscription{
		ID:                   newSubscriptionID(),
		Ticker:               ticker,
		CreatedAt:            time.Now().Unix(),
		Priority:             0,
		NotificationsEnabled: true,
		NotificationLevel:    "material",
		Source:               "migrated",
	}
	subs := []Subscription{sub}
	if err := s.persistLocked(uid, subs); err != nil {
		return Subscription{}, false, err
	}
	s.subs[uid] = subs
	delete(s.loadErr, uid)
	return sub, true, nil
}

// add follows a ticker, or returns the record that already follows it. The lookup and the append
// happen under ONE lock hold, so two simultaneous follows of the same ticker cannot both win.
// Returns (record, alreadyFollowed).
func (s *subscriptionStore) add(uid string, in Subscription) (Subscription, bool, error) {
	if reservedUID(uid) {
		return Subscription{}, false, errSubscriptionReservedUID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.writableLocked(uid) {
		return Subscription{}, false, errSubscriptionUnreadable
	}
	existing := s.loadLocked(uid)
	for _, sub := range existing {
		if sub.Ticker == in.Ticker {
			return sub, true, nil
		}
	}
	s.subs[uid] = append(existing, in)
	if err := s.persistLocked(uid, s.subs[uid]); err != nil {
		return Subscription{}, false, err
	}
	return in, false, nil
}

// patch applies only the fields that were PRESENT in the request body. A nil pointer is "leave
// alone", which is what makes `priority: 0` distinguishable from an absent `priority`.
func (s *subscriptionStore) patch(uid, id string, priority *int, enabled *bool, level *string) (Subscription, bool, error) {
	if reservedUID(uid) {
		return Subscription{}, false, errSubscriptionReservedUID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.writableLocked(uid) {
		return Subscription{}, false, errSubscriptionUnreadable
	}
	subs := s.loadLocked(uid)
	for i := range subs {
		if subs[i].ID != id {
			continue
		}
		if priority != nil {
			subs[i].Priority = *priority
		}
		if enabled != nil {
			subs[i].NotificationsEnabled = *enabled
		}
		if level != nil {
			subs[i].NotificationLevel = *level
		}
		s.subs[uid] = subs
		if err := s.persistLocked(uid, subs); err != nil {
			return Subscription{}, false, err
		}
		return subs[i], true, nil
	}
	return Subscription{}, false, nil
}

// remove unfollows. Unfollowing removes the follow and nothing else — no global event, no journal
// record and no evidence is touched by it.
//
// Removing the LAST subscription rewrites the file as `[]`; it never deletes it (§9.39.2). An empty
// file is a meaningful state — "this user has migrated and follows nothing" — and unlinking it would
// make the next `migrate` call re-seed a ticker the user just removed.
func (s *subscriptionStore) remove(uid, id string) (bool, error) {
	if reservedUID(uid) {
		return false, errSubscriptionReservedUID
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.writableLocked(uid) {
		return false, errSubscriptionUnreadable
	}
	subs := s.loadLocked(uid)
	for i := range subs {
		if subs[i].ID == id {
			s.subs[uid] = append(subs[:i:i], subs[i+1:]...)
			if err := s.persistLocked(uid, s.subs[uid]); err != nil {
				return false, err
			}
			return true, nil
		}
	}
	return false, nil
}

// allTickers is the §9.4 cross-user UNION: the set of followed symbols and nothing else.
//
// It reads the partitions off disk rather than the in-memory cache because a user who has not been
// touched since start-up is not in the cache, and Wave 5's monitor needs the whole set. It builds no
// index file and holds no cross-user state — D-06 stays "delete one directory". Reserved
// `_`-prefixed buckets are skipped, and a partition that does not parse is skipped rather than
// failing the whole answer.
func (s *subscriptionStore) allTickers() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	var userIDs []string
	if s.docs != nil {
		userIDs, _ = s.docs.userIDs("subscriptions", subscriptionsFile)
	} else {
		entries, err := os.ReadDir(s.base)
		if err != nil {
			return []string{}
		}
		for _, entry := range entries {
			if entry.IsDir() && !reservedUID(entry.Name()) {
				userIDs = append(userIDs, entry.Name())
			}
		}
	}
	for _, uid := range userIDs {
		var subs []Subscription
		if s.docs != nil {
			subs = s.loadLocked(uid)
			if s.loadErr[uid] != nil {
				continue
			}
		} else if _, err := loadJSON(filepath.Join(s.base, uid, subscriptionsFile), &subs); err != nil {
			continue
		}
		for _, sub := range subs {
			if t := strings.TrimSpace(sub.Ticker); t != "" {
				seen[t] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for t := range seen {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// ---- HTTP surface ----

// subscriptionAPI owns the follow-graph routes. Constructed inside the registration function rather
// than hung off Server, so this lane's footprint in shared files is zero lines.
type subscriptionAPI struct {
	srv   *Server
	store *subscriptionStore
}

// init attaches this lane's routes through the Wave 0 seam (`routeseams.go`) so neither
// `handlers.go` nor `evidence.go` is edited by this lane.
func init() {
	registerSubscriptionRoute(func(s *Server, mux *http.ServeMux) {
		s.registerSubscriptionAPI(mux)
	})
}

// registerSubscriptionAPI mounts §2.1's four endpoints plus §9.4's internal union.
//
// Every per-user route is requireAuth — including the reads. That differs from `/trades` and
// `/evidence`, which let a guest see an empty list: a follow graph is not a page the UI renders for
// a signed-out visitor, and the contract's own table calls all four session-authenticated.
func (s *Server) registerSubscriptionAPI(mux *http.ServeMux) {
	store, err := openSubscriptionStoreWithRepository(s.cfg.TradesDir, s.documents)
	if err != nil {
		// Additive service: if the directory cannot be opened, the journal must still serve trades
		// and theses. The routes are simply not mounted and the frontend degrades.
		log.Printf("journal: subscription store unavailable in %q: %v (subscription routes not mounted)", s.cfg.TradesDir, err)
		return
	}
	api := &subscriptionAPI{srv: s, store: store}

	mux.HandleFunc("GET /subscriptions", s.requireAuth(api.handleList))
	mux.HandleFunc("POST /subscriptions", s.requireAuth(api.handleCreate))
	// §9.39. A distinct path rather than a query parameter on the create route, because it is a
	// different question: create says "follow this", migrate says "have you ever followed anything?".
	mux.HandleFunc("POST /subscriptions/migrate", s.requireAuth(api.handleMigrate))
	mux.HandleFunc("PATCH /subscriptions/{id}", s.requireAuth(api.handlePatch))
	mux.HandleFunc("DELETE /subscriptions/{id}", s.requireAuth(api.handleDelete))

	// §9.4. Deliberately NOT wrapped in optionalAuth/requireAuth: it is not a per-user route wearing
	// an internal path, and it refuses any request that carries a session cookie at all.
	mux.HandleFunc("GET /_internal/tickers", api.handleInternalTickers)

	// Wave 5 Lane 5B's sibling internal route, mounted HERE rather than beside the thesis routes so
	// the subscription store is in scope: the projection resolves each thesis's notification level
	// from its owner's subscription, which spares the monitor a second cross-user route. Same
	// cookie refusal, same non-proxying, same content-free projection — see internal_theses.go.
	s.registerInternalThesesAPI(mux, s.theses, api.notificationLevelFor)
}

// notificationLevelFor answers "how loudly does this user want to hear about this company?" for one
// (uid, ticker). "" when they have no subscription for it — a thesis can exist without a follow, and
// `shouldNotify` reads "" as the shipped default (`material`) rather than escalating to `all`.
func (a *subscriptionAPI) notificationLevelFor(uid, ticker string) string {
	subs, err := a.store.list(uid)
	if err != nil {
		return ""
	}
	want := strings.ToUpper(strings.TrimSpace(ticker))
	for _, sub := range subs {
		if strings.ToUpper(sub.Ticker) == want {
			return sub.NotificationLevel
		}
	}
	return ""
}

func (a *subscriptionAPI) handleList(w http.ResponseWriter, r *http.Request) {
	subs, err := a.store.list(userID(r))
	if err != nil {
		writeFollowStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"subscriptions": subs})
}

// subscriptionCreateBody is exactly §2.1's create body. `priority` and `notificationsEnabled` are
// absent by design: a new follow is normal priority with notifications on, and re-ordering is a
// PATCH. Server-owned fields (id, createdAt) are never read from the client.
type subscriptionCreateBody struct {
	Ticker            string  `json:"ticker"`
	NotificationLevel *string `json:"notificationLevel"`
	Source            *string `json:"source"`
}

// handleCreate follows a ticker. Idempotent: a second follow of the same company returns the
// SAME record with 200, never a 409 and never a duplicate.
func (a *subscriptionAPI) handleCreate(w http.ResponseWriter, r *http.Request) {
	var body subscriptionCreateBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeFollowError(w, http.StatusBadRequest, "invalid_json", "invalid JSON: "+err.Error())
		return
	}
	ticker := strings.ToUpper(strings.TrimSpace(body.Ticker))
	if ticker == "" {
		writeFollowError(w, http.StatusBadRequest, "validation_failed", "ticker is required")
		return
	}
	if !tickerPattern.MatchString(ticker) {
		writeFollowError(w, http.StatusBadRequest, "validation_failed", "ticker is not a valid symbol: "+ticker)
		return
	}

	level := "material"
	if body.NotificationLevel != nil {
		level = strings.TrimSpace(*body.NotificationLevel)
		if !validNotificationLevel[level] {
			writeFollowError(w, http.StatusBadRequest, "validation_failed",
				"notificationLevel must be material, all, earnings, macro, or none")
			return
		}
	}
	source := "manual"
	if body.Source != nil {
		source = strings.TrimSpace(*body.Source)
		if !validSubscriptionSource[source] {
			writeFollowError(w, http.StatusBadRequest, "validation_failed",
				"source must be migrated, manual, or explore")
			return
		}
	}

	sub, _, err := a.store.add(userID(r), Subscription{
		ID:                   newSubscriptionID(),
		Ticker:               ticker,
		CreatedAt:            time.Now().Unix(),
		Priority:             0,
		NotificationsEnabled: true,
		NotificationLevel:    level,
		Source:               source,
	})
	if err != nil {
		writeFollowStoreError(w, err)
		return
	}
	// 200 whether the record was created now or already existed: the caller asked for a state, and
	// after this call the state holds. Distinguishing the two with 201 would make an idempotent
	// endpoint's status code depend on history the caller cannot see.
	writeJSON(w, http.StatusOK, map[string]any{"subscription": sub})
}

// subscriptionMigrateBody is §9.39's request body. The value is a FACT SUPPLIED BY THE CALLER, not
// something journal reads: this service still has no auth client, no `AUTH_URL` and makes no
// outbound call, and it never writes back to auth settings either. The legacy setting is left
// exactly where it is; migrating a follow graph does not clear it.
type subscriptionMigrateBody struct {
	ActiveTicker string `json:"activeTicker"`
}

// handleMigrate runs the one-time §2.2 seed, or declines to. It is idempotent by construction — the
// second call sees the file the first one wrote — so the gateway may call it once per session
// without tracking whether it already has.
func (a *subscriptionAPI) handleMigrate(w http.ResponseWriter, r *http.Request) {
	var body subscriptionMigrateBody
	// An absent body is "no ticker to carry over", not a malformed request: a gateway with nothing to
	// supply should still be able to ask the question.
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeFollowError(w, http.StatusBadRequest, "invalid_json", "invalid JSON: "+err.Error())
		return
	}
	ticker := strings.ToUpper(strings.TrimSpace(body.ActiveTicker))
	if ticker != "" && !tickerPattern.MatchString(ticker) {
		writeFollowError(w, http.StatusBadRequest, "validation_failed", "ticker is not a valid symbol: "+ticker)
		return
	}

	sub, seeded, err := a.store.migrate(userID(r), ticker)
	if err != nil {
		writeFollowStoreError(w, err)
		return
	}
	if !seeded {
		writeJSON(w, http.StatusOK, map[string]any{"migrated": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"migrated": true, "subscription": sub})
}

// subscriptionPatch uses pointers so an ABSENT field is "leave alone" and `priority: 0` is a real
// value. A presence map would work too; pointers are what `tradePatch` already uses.
type subscriptionPatch struct {
	Priority             *int    `json:"priority"`
	NotificationsEnabled *bool   `json:"notificationsEnabled"`
	NotificationLevel    *string `json:"notificationLevel"`
}

func (a *subscriptionAPI) handlePatch(w http.ResponseWriter, r *http.Request) {
	var p subscriptionPatch
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeFollowError(w, http.StatusBadRequest, "invalid_json", "invalid JSON: "+err.Error())
		return
	}
	if p.Priority == nil && p.NotificationsEnabled == nil && p.NotificationLevel == nil {
		writeFollowError(w, http.StatusBadRequest, "validation_failed",
			"nothing to update (priority, notificationsEnabled, notificationLevel)")
		return
	}
	if p.NotificationLevel != nil {
		level := strings.TrimSpace(*p.NotificationLevel)
		if !validNotificationLevel[level] {
			writeFollowError(w, http.StatusBadRequest, "validation_failed",
				"notificationLevel must be material, all, earnings, macro, or none")
			return
		}
		p.NotificationLevel = &level
	}
	// `ticker` and `source` are absent from the patch body on purpose: re-pointing a follow at a
	// different company is a follow of a different company, and rewriting `source` would erase the
	// audit trail the migration exists to leave.
	sub, ok, err := a.store.patch(userID(r), r.PathValue("id"), p.Priority, p.NotificationsEnabled, p.NotificationLevel)
	if err != nil {
		writeFollowStoreError(w, err)
		return
	}
	if !ok {
		writeFollowError(w, http.StatusNotFound, "not_found", "subscription not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"subscription": sub})
}

func (a *subscriptionAPI) handleDelete(w http.ResponseWriter, r *http.Request) {
	ok, err := a.store.remove(userID(r), r.PathValue("id"))
	if err != nil {
		writeFollowStoreError(w, err)
		return
	}
	if !ok {
		writeFollowError(w, http.StatusNotFound, "not_found", "subscription not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// handleInternalTickers answers "which companies does anybody follow?" (§9.4) — the one question the
// four per-user endpoints structurally cannot answer, and the one Wave 5's monitor needs.
//
// It returns a SET: no user ids, no counts, no per-user grouping, so it is not a follow graph and
// D-13's forbidden-field list is untouched. It is refused outright when the request carries a
// session cookie, valid or not — an internal path is not a back door onto somebody's own data, and
// 404 (rather than 403) keeps its existence undisclosed to a browser that stumbles onto it.
func (a *subscriptionAPI) handleInternalTickers(w http.ResponseWriter, r *http.Request) {
	if _, err := r.Cookie(a.srv.cfg.CookieName); err == nil {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tickers": a.store.allTickers()})
}
