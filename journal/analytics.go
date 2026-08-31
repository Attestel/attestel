package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// analytics.go — self-hosted product events. Contract: PARALLEL_CONTRACTS.md §6 (FROZEN), D-13 (a).
//
// The whole design follows from one decision: the product's differentiator is that a conclusion can
// be traced back to its evidence, so shipping the user's research to a third party to measure whether
// they are researching would contradict the product. Hence:
//
//  1. SELF-HOSTED, SERVER-SIDE ONLY. No SDK, no client library, no hosted SaaS, no new service. The
//     service that performs the DURABLE WRITE appends one JSON line to
//     {TRADES_DIR}/_analytics/{YYYY-MM-DD}.jsonl. `_analytics` uses the reserved leading-underscore
//     convention, so it can never collide with a uid or an uppercase ticker.
//  2. EVERY EVENT IS A DURABLE STATE TRANSITION. The catalog is a CLOSED set of 14 (§6.2); adding a
//     fifteenth is a contract amendment. Renders and clicks are unmeasurable here by construction,
//     which is the point — "opened the panel" is not progress.
//  3. THE PROPERTY SET IS AN ALLOW-LIST, NOT A DENY-LIST. `analyticsEvent` IS the allow-list: it is a
//     closed struct, so a forbidden field (§6.4 — thesis text, excerpts, notes, rationale, email,
//     raw uid, cookies…) has nowhere to go even if a future caller tries. Every string that does land
//     is additionally validated against a closed vocabulary or an opaque-id shape, so free text
//     cannot ride in through a field that happens to be permitted.
//  4. IDENTITY IS ALWAYS HASHED. `actor.idHash = sha256(uid|sessionId + salt)[:16]`. The raw uid never
//     reaches the log even though the log never leaves the host — that is what keeps the log safe to
//     copy to a laptop to run the readout.
//  5. APPEND NEVER FAILS THE USER'S WRITE. emit() cannot return an error and cannot panic out; a
//     broken analytics dir degrades to no measurement, never to a failed save.
//
// The reader (analytics_report.go) de-duplicates on `eventId`, so a retried or replayed durable write
// is idempotent by construction and the writer needs no cross-process coordination.

const (
	analyticsSchemaVersion = 1
	analyticsDirName       = "_analytics"

	// maxAnalyticsIDLen bounds an opaque server id (ev_ + 16 hex is 19; a guest hash is 16). It exists
	// so a caller cannot smuggle prose through an id-shaped field.
	maxAnalyticsIDLen = 72
	maxTickerLen      = 12
	maxLensIDLen      = 80
)

// ---- research-loop stages (§6.3) ----

const (
	stageOrient     = "orient"
	stageUnderstand = "understand"
	stageForm       = "form"
	stageChallenge  = "challenge"
	stageMonitor    = "monitor"
	stageDecide     = "decide"
	stageReview     = "review"
)

var validAnalyticsStage = map[string]bool{
	stageOrient: true, stageUnderstand: true, stageForm: true, stageChallenge: true,
	stageMonitor: true, stageDecide: true, stageReview: true,
}

// ---- the closed event catalog: exactly 14 (§6.2) ----

const (
	evOnboardingStarted        = "onboarding_started"
	evOnboardingResumed        = "onboarding_resumed"
	evOnboardingCompleted      = "onboarding_completed"
	evThesisCreated            = "thesis_created"
	evThesisActivated          = "thesis_activated"
	evThesisClosed             = "thesis_closed"
	evEvidenceSaved            = "evidence_saved"
	evEvidenceLinked           = "evidence_linked"
	evChallengeReviewCompleted = "challenge_review_completed"
	evThesisReviewed           = "thesis_reviewed"
	evDecisionRecorded         = "decision_recorded"
	evOutcomeReviewed          = "outcome_reviewed"
	evReviewRated              = "review_rated"
	evCitationFeedback         = "citation_feedback"
)

// analyticsCatalogEntry documents, IN CODE, what §6.2 requires of every event before it is
// instrumented: the durable write that authorises it, the research-loop stage it belongs to, and the
// metric it feeds. `authority` is the exact durable write — if that write does not happen, the event
// must not be emitted.
type analyticsCatalogEntry struct {
	Event      string
	Stage      string
	ObjectType string
	Authority  string // the durable write that is the sole authority for this event
	Metric     string // the product metric it feeds (guide §13.1)
}

// analyticsCatalog is the allow-list of event names. An emit for a name absent from this map is
// dropped — which is what makes "closed set of 14" true in code and not only in the document.
var analyticsCatalog = map[string]analyticsCatalogEntry{
	evOnboardingStarted: {
		Event: evOnboardingStarted, Stage: stageOrient, ObjectType: "onboarding",
		Authority: "first onboarding-state write (journal POST /onboarding)",
		Metric:    "activation conversion (denominator) · time to value (start)",
	},
	evOnboardingResumed: {
		Event: evOnboardingResumed, Stage: stageOrient, ObjectType: "onboarding",
		Authority: "onboarding-state write after a gap (journal POST /onboarding)",
		Metric:    "resumability of the guided first session",
	},
	evOnboardingCompleted: {
		Event: evOnboardingCompleted, Stage: stageChallenge, ObjectType: "onboarding",
		Authority: "onboarding-state write once thesis + source-backed evidence link + challenge review all exist durably",
		Metric:    "ACTIVATION · time to value (end)",
	},
	evThesisCreated: {
		Event: evThesisCreated, Stage: stageForm, ObjectType: "thesis",
		Authority: "journal POST /theses 201",
		Metric:    "activation condition 1 · depth denominator",
	},
	evThesisActivated: {
		Event: evThesisActivated, Stage: stageForm, ObjectType: "thesis",
		Authority: "journal thesis write transitioning into status:\"active\"",
		Metric:    "distinct active theses (north-star denominator)",
	},
	evThesisClosed: {
		Event: evThesisClosed, Stage: stageReview, ObjectType: "thesis",
		Authority: "journal thesis write transitioning into status:\"closed\"",
		Metric:    "thesis lifecycle completion · close-reason mix",
	},
	evEvidenceSaved: {
		Event: evEvidenceSaved, Stage: stageUnderstand, ObjectType: "evidence",
		Authority: "journal POST /evidence 201 (NOT on a dedupe hit, which writes nothing new)",
		Metric:    "research depth",
	},
	evEvidenceLinked: {
		Event: evEvidenceLinked, Stage: stageForm, ObjectType: "evidence_link",
		Authority: "journal evidence-link create, or a bearing change on an existing link",
		Metric:    "activation condition 2 · research depth · contradicting evidence per thesis",
	},
	evChallengeReviewCompleted: {
		Event: evChallengeReviewCompleted, Stage: stageChallenge, ObjectType: "review",
		Authority: "journal POST /reviews 201 for a challenge task",
		Metric:    "ACTIVATION condition 3 · NORTH STAR (theses reviewed per week)",
	},
	evThesisReviewed: {
		Event: evThesisReviewed, Stage: stageMonitor, ObjectType: "thesis",
		Authority: "journal POST /theses/{id}/review-checkpoint",
		Metric:    "NORTH STAR (theses reviewed per week) · retention",
	},
	evDecisionRecorded: {
		Event: evDecisionRecorded, Stage: stageDecide, ObjectType: "decision",
		Authority: "journal POST /decisions 201",
		Metric:    "decision-loop completion (denominator)",
	},
	evOutcomeReviewed: {
		Event: evOutcomeReviewed, Stage: stageReview, ObjectType: "outcome_review",
		Authority: "journal POST /outcomes 201 (one per decision, §5.4)",
		Metric:    "decision-loop completion (numerator)",
	},
	evReviewRated: {
		Event: evReviewRated, Stage: stageChallenge, ObjectType: "review",
		Authority: "journal PATCH /reviews/{id} carrying `rating`",
		Metric:    "lens usefulness",
	},
	evCitationFeedback: {
		Event: evCitationFeedback, Stage: stageChallenge, ObjectType: "finding",
		Authority: "journal PATCH /reviews/{id} carrying `citationFeedback` (one event per finding)",
		Metric:    "citation acceptance",
	},
}

// ---- closed vocabularies for the enum-shaped properties ----
//
// Anything not in these sets is DROPPED (rendered as null), never stored verbatim. Several of these
// mirror a validator that already exists in this package; they are repeated rather than referenced so
// that widening a product enum can never silently widen what telemetry accepts.

var validAnalyticsLevel = map[string]bool{"beginner": true, "standard": true}

var validAnalyticsTask = map[string]bool{
	"challenge_thesis": true, "explain_evidence": true, "review_chart_context": true,
	"teach_step": true, "apply_custom_framework": true, "review_with_perspectives": true,
}

// challengeTasks are the lens tasks that count as "a challenge review" for activation and for the
// north-star metric: the skeptic attacking a thesis, and the bull/bear perspectives review. Both
// exist to find what the user is missing; the explain/teach/chart tasks do not.
var challengeTasks = map[string]bool{
	"challenge_thesis": true, "review_with_perspectives": true,
}

// validAnalyticsRating covers both rating-shaped properties: a review's usefulness verdict and a
// citation's accept/reject. The event name disambiguates which vocabulary applies.
var validAnalyticsRating = map[string]bool{
	"useful": true, "not_useful": true, "accepted": true, "rejected": true,
}

// ---- the event record: this struct IS the property allow-list (§6.3) ----

type analyticsActor struct {
	Type   string `json:"type"` // "user" | "guest"
	IDHash string `json:"idHash"`
}

// analyticsEvent is written as one JSON line. Nullable properties are pointers so an unknown value is
// an explicit null rather than a misleading "" or 0 — "we did not record this" and "this was empty"
// are different facts.
type analyticsEvent struct {
	Event         string         `json:"event"`
	EventID       string         `json:"eventId"`
	At            int64          `json:"at"`
	SchemaVersion int            `json:"schemaVersion"`
	Actor         analyticsActor `json:"actor"`

	Ticker     string `json:"ticker"`
	ObjectType string `json:"objectType"`
	ObjectID   string `json:"objectId"`

	// ThesisID stays server-side: the log never leaves the host (D-13 a), which is the only reason
	// §6.3 permits it at all. It is what makes "distinct active theses reviewed" countable.
	ThesisID string `json:"thesisId"`

	Stage string  `json:"stage"`
	Level *string `json:"level"`

	Bearing     *string `json:"bearing"`
	Intent      *string `json:"intent"`
	CloseReason *string `json:"closeReason"`
	LensID      *string `json:"lensId"`
	Task        *string `json:"task"`
	Kind        *string `json:"kind"`
	Provider    *string `json:"provider"`

	DataState *string `json:"dataState"`
	Synthetic bool    `json:"synthetic"`

	Rating           *string `json:"rating"`
	ReasoningQuality *int    `json:"reasoningQuality"`
	DurationMs       *int64  `json:"durationMs"`
}

// eventInput is what a call site hands the sink. It is deliberately NOT the stored shape: every field
// here is validated, normalized, or dropped on the way in.
type eventInput struct {
	Event string

	// UID is the signed-in user. Empty means a guest, and then GuestSession identifies the browser
	// session. Neither ever reaches the log unhashed.
	UID          string
	GuestSession string

	// TransitionKey is the durable state being ENTERED. It is the third component of eventId, which
	// is what makes a replayed write produce the same id and de-duplicate away.
	TransitionKey string

	Ticker     string
	ObjectType string
	ObjectID   string
	ThesisID   string

	Bearing     string
	Intent      string
	CloseReason string
	LensID      string
	Task        string
	Kind        string
	Provider    string
	DataState   string
	Rating      string

	ReasoningQuality *int
	DurationMs       *int64

	// Synthetic forces the exclusion flag on for a record whose data state is not itself "synthetic"
	// but whose underlying prices were (an outcome review marked to a synthetic quote, for example).
	Synthetic bool
}

// ---- the sink ----

// analyticsSink appends events. One per process, wired in main().
type analyticsSink struct {
	dir  string // {TRADES_DIR}/_analytics
	salt string
	mu   sync.Mutex
	now  func() time.Time

	// demo marks accounts whose activity is DEMO activity — flagged `synthetic: true` and excluded
	// from the default readout. Server-side configuration only (ANALYTICS_DEMO_UIDS): a browser can
	// never mark its own activity, in either direction.
	demo map[string]bool

	// levelOf resolves the caller's experience level (beginner|standard) without this package growing
	// a client for the auth service. Wired to the onboarding store, which caches the level the client
	// reported on its own onboarding writes. Returns "" when unknown, and unknown stays null.
	levelOf func(uid string) string

	// warned keeps a broken analytics directory from filling the log with one line per user write.
	warned bool
	docs   *documentRepository
}

// openAnalyticsSink prepares {base}/_analytics. A failure here is NOT fatal: the journal must serve
// research records whether or not it can measure them.
func openAnalyticsSink(base, salt string, demoUIDs []string) *analyticsSink {
	return openAnalyticsSinkWithRepository(base, salt, demoUIDs, nil)
}

func openAnalyticsSinkWithRepository(base, salt string, demoUIDs []string, docs *documentRepository) *analyticsSink {
	dir := filepath.Join(base, analyticsDirName)
	s := &analyticsSink{
		dir:  dir,
		salt: salt,
		now:  time.Now,
		demo: map[string]bool{},
		docs: docs,
	}
	for _, u := range demoUIDs {
		if u = strings.TrimSpace(u); u != "" {
			s.demo[u] = true
		}
	}
	if docs != nil {
		if err := docs.importAnalyticsFiles(); err != nil {
			log.Printf("journal: analytics PostgreSQL import unavailable: %v (new events still attempt PostgreSQL)", err)
		}
		return s
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("journal: analytics dir %q unavailable: %v (events will not be recorded)", dir, err)
		s.dir = ""
	}
	return s
}

// analytics is the process-wide sink, set in main(). Every method on it is nil-safe so a build that
// never called main() (a unit test of another lane's handler) cannot panic on a missing sink.
var analytics *analyticsSink

// useLevelSource wires the experience-level lookup. Nil-safe so a test harness that never built a
// sink can still register routes.
func (s *analyticsSink) useLevelSource(fn func(uid string) string) {
	if s == nil {
		return
	}
	s.levelOf = fn
}

// hashIdentity is sha256(identity + salt)[:16] as hex. Never reversible from the log alone, and
// stable for one identity across restarts so retention cohorts hold.
func (s *analyticsSink) hashIdentity(id string) string {
	if s == nil || id == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(id + "\x00" + s.salt))
	return hex.EncodeToString(sum[:])[:16]
}

// eventID = sha256(event + "\x00" + objectId + "\x00" + transitionKey)[:32] (§6.3). Deterministic, so
// the same durable transition always yields the same id and the reader can de-duplicate.
func analyticsEventID(event, objectID, transitionKey string) string {
	sum := sha256.Sum256([]byte(event + "\x00" + objectID + "\x00" + transitionKey))
	return hex.EncodeToString(sum[:])[:32]
}

// ---- property sanitizers ----

// opaqueID accepts only an id-shaped string: the ids this system mints are a short lowercase prefix
// plus hex, and a guest hash is hex. Anything else — a sentence, a URL, an email — is dropped, which
// is the mechanical guarantee behind §6.4 for the id-shaped fields.
func opaqueID(s string) string {
	if s == "" || len(s) > maxAnalyticsIDLen {
		return ""
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return ""
		}
	}
	return s
}

// analyticsTicker keeps a ticker symbol only if it looks like one. A ticker is not personal content
// (§6.4) and it is what makes per-company metrics possible.
func analyticsTicker(s string) string {
	s = strings.ToUpper(strings.TrimSpace(s))
	if s == "" || len(s) > maxTickerLen {
		return ""
	}
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-':
		default:
			return ""
		}
	}
	return s
}

// analyticsLensID accepts `builtin:<id>` / `persona:<id>` and nothing else. A custom persona's
// INSTRUCTIONS are user-authored text and are on the forbidden list; its opaque id is not.
func analyticsLensID(s string) string {
	if s == "" || len(s) > maxLensIDLen {
		return ""
	}
	scheme, rest, ok := strings.Cut(s, ":")
	if !ok || (scheme != "builtin" && scheme != "persona") || opaqueID(rest) == "" {
		return ""
	}
	return s
}

// enumOrEmpty keeps a value only if the closed vocabulary contains it.
func enumOrEmpty(v string, allowed map[string]bool) string {
	if allowed[v] {
		return v
	}
	return ""
}

// optString renders "" as JSON null: absent and empty are different facts.
func optString(v string) *string {
	if v == "" {
		return nil
	}
	out := v
	return &out
}

// ---- emit ----

// build turns an input into the stored record, dropping everything that is not allow-listed. Split
// from emit so the sanitizing rules are testable without touching the filesystem.
func (s *analyticsSink) build(in eventInput) (analyticsEvent, bool) {
	entry, ok := analyticsCatalog[in.Event]
	if !ok {
		return analyticsEvent{}, false // closed catalog: an unknown name is never written
	}

	actor := analyticsActor{Type: "user", IDHash: s.hashIdentity(in.UID)}
	if in.UID == "" {
		actor = analyticsActor{Type: "guest", IDHash: s.hashIdentity(in.GuestSession)}
	}
	if actor.IDHash == "" {
		return analyticsEvent{}, false // an unattributable event is noise, not measurement
	}
	// The unattributed pre-accounts bucket is read-never / write-never (§1.10) and is nobody's
	// activity — it must not appear in a product metric either.
	if reservedUID(in.UID) {
		return analyticsEvent{}, false
	}

	objectID := opaqueID(in.ObjectID)
	dataState := enumOrEmpty(in.DataState, validDataState)

	// Synthetic/demo exclusion (§6.3): synthetic data state, an explicit synthetic signal from the
	// call site, or a server-flagged demo account. All three are server-side facts.
	synthetic := in.Synthetic || dataState == "synthetic" || s.demo[in.UID]

	level := ""
	if s.levelOf != nil && in.UID != "" {
		level = enumOrEmpty(s.levelOf(in.UID), validAnalyticsLevel)
	}

	ev := analyticsEvent{
		Event:         in.Event,
		EventID:       analyticsEventID(in.Event, objectID, in.TransitionKey),
		At:            s.now().Unix(),
		SchemaVersion: analyticsSchemaVersion,
		Actor:         actor,
		Ticker:        analyticsTicker(in.Ticker),
		ObjectType:    entry.ObjectType,
		ObjectID:      objectID,
		ThesisID:      opaqueID(in.ThesisID),
		Stage:         entry.Stage,
		Level:         optString(level),

		Bearing:     optString(enumOrEmpty(in.Bearing, validEvidenceBearing)),
		Intent:      optString(enumOrEmpty(in.Intent, validDecisionIntent)),
		CloseReason: optString(enumOrEmpty(in.CloseReason, validCloseReason)),
		LensID:      optString(analyticsLensID(in.LensID)),
		Task:        optString(enumOrEmpty(in.Task, validAnalyticsTask)),
		Kind:        optString(enumOrEmpty(in.Kind, evidenceKindAllowed(in.Kind))),
		Provider:    optString(enumOrEmpty(in.Provider, validEvidenceProvider)),

		DataState: optString(dataState),
		Synthetic: synthetic,

		Rating:     optString(enumOrEmpty(in.Rating, validAnalyticsRating)),
		DurationMs: in.DurationMs,
	}
	if in.ReasoningQuality != nil && *in.ReasoningQuality >= 1 && *in.ReasoningQuality <= 5 {
		q := *in.ReasoningQuality
		ev.ReasoningQuality = &q
	}
	return ev, true
}

// evidenceKindAllowed adapts the kind spec map (whose value type is not bool) to the enum checker,
// so the analytics vocabulary stays pinned to the kinds evidence.go actually accepts.
func evidenceKindAllowed(kind string) map[string]bool {
	if _, ok := evidenceKinds[kind]; ok {
		return map[string]bool{kind: true}
	}
	return map[string]bool{}
}

// emit appends one event. It NEVER returns an error and never panics out: measurement failing must
// not fail a user's save (§6.1). Synchronous rather than spawned so events land in write order and
// tests are deterministic — the work is one small append to a local file.
func (s *analyticsSink) emit(in eventInput) {
	if s == nil || (s.dir == "" && s.docs == nil) {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			log.Printf("journal: analytics emit recovered: %v", r)
		}
	}()

	ev, ok := s.build(in)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.docs != nil {
		if err := s.docs.appendAnalytics(ev); err != nil && !s.warned {
			s.warned = true
			log.Printf("journal: analytics PostgreSQL unwritable: %v (events will not be recorded)", err)
		}
		return
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return
	}
	path := filepath.Join(s.dir, s.now().UTC().Format("2006-01-02")+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		if !s.warned {
			s.warned = true
			log.Printf("journal: analytics file %q unwritable: %v (events will not be recorded)", path, err)
		}
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}
