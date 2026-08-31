package main

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// theses.go — the user's structured, versioned investment thesis (Thesis v2).
//
// A thesis is the USER'S OWN falsifiable hypothesis ("buying GOOGL because Waymo expands to N cities
// this year"), broken into the parts that make it testable: the claim, the assumptions it rests on,
// the catalysts that would advance it, the risks and invalidation conditions that would kill it, and
// the questions still open. The llm service compares new facts against the claim (confirming /
// neutral / challenged); that verdict lands in LastCheck and is about the THESIS, never about the
// stock. This service still executes nothing and advises nothing.
//
// Schema: docs/research-os/PARALLEL_CONTRACTS.md §1 (FROZEN). Three rules run through everything here:
//
//  1. USER-OWNED vs SYSTEM-AUTHORED. Claim, horizon, confidence, notes and the five item lists belong
//     to the user. LastCheck and LastReview belong to the system. No route lets one overwrite the
//     other — in particular the model's confidence goes to LastCheck.Confidence and can never reach
//     Thesis.Confidence (§1.6). An AI response does not silently edit a belief.
//  2. LEGACY RECORDS ARE NEVER REWRITTEN BEHIND THE USER'S BACK. A v1 record (bare `text`) is
//     normalized in memory on read (normalizeThesis) and reaches disk in v2 shape only when the user
//     next mutates it. No startup pass, no backfill, so there is nothing to roll back (§1.1). The
//     unattributed `_legacy` bucket stays read-never / write-never (§1.10).
//  3. `text` IS A PERMANENT COMPATIBILITY MIRROR of `claim` (§1.5). It keeps the shipped
//     ThesisTracker.jsx and gateway/research.go (which reads t["text"]) working with zero change.
//
// History (`versions`) records MATERIAL USER-AUTHORED changes only, computed by diffing the stored
// record against the incoming one inside the same mutex hold as the write — so a replayed PATCH or a
// reload appends nothing.

// ---- schema ----

const (
	thesisSchemaVersion = 2

	// maxThesisLen bounds claim/text. v1 TRUNCATED on overflow rather than failing the write, and
	// §1.2 keeps that behaviour ("the current truncate-on-overflow behaviour is preserved for text").
	// Every other limit below is a 400.
	maxThesisLen = 2000

	maxNotesLen     = 4000
	maxItemTextLen  = 500
	maxItemsPerList = 20

	// maxActiveTheses caps `active` theses per (uid, ticker) — D-03 option (c), §1.7. draft, closed
	// and archived are unbounded. Users already over the cap are grandfathered: reads never fail,
	// only new activations are refused.
	maxActiveTheses = 3

	// Version history is bounded so one thesis record cannot grow without limit (§1.4).
	maxVersionEvents    = 200
	maxVersionBeforeLen = 500
	maxVersionReasonLen = 280
)

// Lifecycle (§1.3). `invalidated` is a close REASON, never a status.
const (
	statusDraft    = "draft"
	statusActive   = "active"
	statusClosed   = "closed"
	statusArchived = "archived"
)

// ThesisItem id prefixes. Stable per-item ids are what let Step 06 link evidence to one assumption
// and Step 08 watch one invalidation condition — which is why the five repeatable fields are arrays
// of objects and never comma-separated strings.
const (
	prefixAssumption   = "asm_"
	prefixCatalyst     = "cat_"
	prefixRisk         = "rsk_"
	prefixInvalidation = "inv_"
	prefixQuestion     = "qst_"
)

var validThesisStatus = map[string]bool{statusDraft: true, statusActive: true, statusClosed: true, statusArchived: true}

var validCloseReason = map[string]bool{
	"invalidated": true, "played_out": true, "position_exited": true, "superseded": true, "abandoned": true,
}

var validHorizon = map[string]bool{"days": true, "weeks": true, "months": true, "quarters": true, "years": true}

var validVerdict = map[string]bool{"confirming": true, "neutral": true, "challenged": true}

var validCheckpointSource = map[string]bool{"user": true, "decision": true}

// legalThesisTransition (§1.3). `closed -> active` is deliberately ABSENT: a reopen goes through
// `archived`, so it is always visible in history. `active` and `archived` keep their exact v1
// meaning, which is what lets every legacy record map in untouched.
var legalThesisTransition = map[[2]string]bool{
	{statusDraft, statusActive}:    true,
	{statusDraft, statusArchived}:  true,
	{statusActive, statusClosed}:   true,
	{statusActive, statusArchived}: true,
	{statusClosed, statusArchived}: true,
	{statusArchived, statusActive}: true,
}

// ThesisCheck is the SYSTEM-authored result of comparing collected facts against the user's claim.
// Written only via PATCH by the gateway after an llm /thesis-check run. Unchanged from v1 apart from
// Confidence (§1.5).
type ThesisCheck struct {
	At        int64            `json:"at"`      // unix seconds
	Verdict   string           `json:"verdict"` // confirming | neutral | challenged
	Summary   string           `json:"summary"`
	Evidence  []map[string]any `json:"evidence,omitempty"`
	WatchFor  []string         `json:"watchFor,omitempty"`
	ModelUsed string           `json:"modelUsed,omitempty"`

	// Confidence is the MODEL's confidence in THIS CHECK, 0..100 (llm emits 0..1; the gateway
	// scales it). Distinct from Thesis.Confidence, the user's own conviction — never one shared
	// badge (§1.6). nil means unknown, which every historical check will always be: absence is not
	// zero.
	Confidence *int `json:"confidence"`
}

// ReviewCheckpoint is the "you have reviewed everything up to here" boundary — the single object
// Step 08 computes change sets against (§1.8). Advanced ONLY by an explicit user action through
// POST /theses/{id}/review-checkpoint: never by a scheduler, never by the alert evaluator, never as
// a side effect of a model check. nil means never reviewed, and Step 08 then falls back to createdAt.
type ReviewCheckpoint struct {
	CheckpointID string `json:"checkpointId"`
	At           int64  `json:"at"`        // unix s — the boundary; changes strictly after are "new"
	ItemCount    int    `json:"itemCount"` // change items acknowledged at that moment; informational
	Source       string `json:"source"`    // user | decision
}

// ThesisItem is the one shape used by all five repeatable lists.
type ThesisItem struct {
	ID        string `json:"id"`   // asm_ | cat_ | rsk_ | inv_ | qst_ + 16 hex
	Text      string `json:"text"` // required, 1..500
	CreatedAt int64  `json:"createdAt"`

	// ResolvedAt applies to nextQuestions only (unix s when the user marks it answered); DueAt to
	// catalysts only (the expected date). Both are dropped on the lists they do not belong to.
	ResolvedAt *int64 `json:"resolvedAt"`
	DueAt      *int64 `json:"dueAt"`
}

// ItemDelta records WHICH items changed, by id only — never their text (§1.4: history stays bounded).
type ItemDelta struct {
	Added   []string `json:"added,omitempty"`
	Removed []string `json:"removed,omitempty"`
	Edited  []string `json:"edited,omitempty"`
}

// VersionEvent is one append-only entry of material user-authored change. No endpoint edits or
// deletes one.
type VersionEvent struct {
	ID            string   `json:"id"` // ver_ + 16 hex
	At            int64    `json:"at"`
	Actor         string   `json:"actor"`       // "user" | "system"
	ActorDetail   string   `json:"actorDetail"` // "" for user
	ChangedFields []string `json:"changedFields"`
	Reason        string   `json:"reason"` // optional user note, <=280

	// Before holds prior values of changed SCALAR fields only, each truncated — not a full prior
	// copy of the record.
	Before    map[string]any       `json:"before,omitempty"`
	ItemDelta map[string]ItemDelta `json:"itemDelta,omitempty"`
}

// Thesis is the v2 aggregate. Field order mirrors §1.2.
type Thesis struct {
	ID            string `json:"id"`            // bare 16 hex — PRESERVED VERBATIM, never regenerated
	SchemaVersion int    `json:"schemaVersion"` // absent/0 on disk => legacy record, normalized on read
	Ticker        string `json:"ticker"`
	Claim         string `json:"claim"` // user-authored, required, 1..2000
	Text          string `json:"text"`  // compatibility mirror of Claim (§1.5)
	Status        string `json:"status"`

	CloseReason         *string `json:"closeReason"`                   // non-nil iff Status == closed
	ClosedAt            *int64  `json:"closedAt"`                      // set with Status == closed
	ClosedByConditionID string  `json:"closedByConditionId,omitempty"` // an inv_ item on THIS thesis

	Horizon    *string `json:"horizon"`    // days|weeks|months|quarters|years|null
	Confidence *int    `json:"confidence"` // 0..100 | null — USER-owned only

	Assumptions            []ThesisItem `json:"assumptions"`
	Catalysts              []ThesisItem `json:"catalysts"`
	Risks                  []ThesisItem `json:"risks"`
	InvalidationConditions []ThesisItem `json:"invalidationConditions"`
	NextQuestions          []ThesisItem `json:"nextQuestions"`

	Notes string `json:"notes"` // user-authored freeform, <=4000

	LastCheck  *ThesisCheck      `json:"lastCheck,omitempty"` // system-authored (omitempty: v1 shape)
	LastReview *ReviewCheckpoint `json:"lastReview"`

	Versions          []VersionEvent `json:"versions"`
	VersionsTruncated bool           `json:"versionsTruncated,omitempty"`

	CreatedAt int64 `json:"createdAt"`
	UpdatedAt int64 `json:"updatedAt"`
}

// itemList pairs a repeatable field's name and id prefix with a pointer to it.
type itemList struct {
	Field  string
	Prefix string
	Items  *[]ThesisItem
}

// itemLists exposes the five repeatable lists uniformly (diffing, validation, patching) so a sixth
// cannot be added in one place and forgotten in another.
func (t *Thesis) itemLists() []itemList {
	return []itemList{
		{"assumptions", prefixAssumption, &t.Assumptions},
		{"catalysts", prefixCatalyst, &t.Catalysts},
		{"risks", prefixRisk, &t.Risks},
		{"invalidationConditions", prefixInvalidation, &t.InvalidationConditions},
		{"nextQuestions", prefixQuestion, &t.NextQuestions},
	}
}

// ---- errors ----

// apiError carries the HTTP status, a stable machine code, and (where it helps a form) the offending
// field, so the frontend can associate a server error with an input instead of showing a toast.
type apiError struct {
	Status int
	Code   string
	Msg    string
	Field  string
	Extra  map[string]any
}

func (e *apiError) Error() string { return e.Msg }

func writeAPIError(w http.ResponseWriter, e *apiError) {
	body := map[string]any{"error": e.Msg}
	if e.Code != "" {
		body["code"] = e.Code
	}
	if e.Field != "" {
		body["field"] = e.Field
	}
	for k, v := range e.Extra {
		body[k] = v
	}
	writeJSON(w, e.Status, body)
}

func badField(field, msg string) *apiError {
	return &apiError{Status: http.StatusBadRequest, Code: "validation_failed", Msg: msg, Field: field}
}

// errThesisNotFound is returned for an id that is not in the CALLER'S partition — a foreign id is a
// 404, never a 403, so existence is not confirmed across users (§1.9).
var errThesisNotFound = &apiError{Status: http.StatusNotFound, Code: "not_found", Msg: "thesis not found"}

// errStoreUnreadable means the user's theses.json exists but did not parse. We refuse to write
// rather than silently replace an unparseable file with a fresh one (see loadUser).
var errStoreUnreadable = &apiError{
	Status: http.StatusServiceUnavailable, Code: "store_unreadable",
	Msg: "stored theses could not be read; refusing to overwrite them",
}

func activeCapError(count int) *apiError {
	return &apiError{
		Status: http.StatusConflict, Code: "active_thesis_cap",
		Msg:   "at most " + strconv.Itoa(maxActiveTheses) + " active theses per ticker",
		Field: "status",
		Extra: map[string]any{"cap": maxActiveTheses, "activeCount": count},
	}
}

// ---- compatibility reader ----

// normalizeThesis is THE compatibility reader (MIGRATION_CONTRACT §3.3). One function, used by every
// consumer, that accepts a v1 record (`text`, no `claim`), a dual-shape record (both), and a v2
// record (`claim` only), and yields the v2 shape IN MEMORY. Deterministic and idempotent — no model,
// no network, nothing fabricated:
//
//   - `claim` wins when present and non-empty, else `text`; the two are then mirrored
//   - absent `status` -> "active" (v1's own default)
//   - absent lists    -> [] — a claim-only thesis is a VALID thesis; no step may require more
//   - absent scalars  -> nil, meaning "unknown": never a guess, never derived from lastCheck
//   - createdAt/updatedAt are left EXACTLY as stored: 0 stays 0 and the UI renders "unknown",
//     because a fabricated timestamp is worse than a missing one (§1.5)
//   - `versions` stays [] — NO synthetic "created" event; inventing history for a record whose
//     history was never recorded would make the version list a lie (§1.5)
//
// It never writes. The record reaches disk in v2 shape only when the user next mutates it (§1.1).
func normalizeThesis(t *Thesis) {
	if strings.TrimSpace(t.Claim) == "" && t.Text != "" {
		t.Claim = t.Text // byte-for-byte: no trimming, re-wrapping, or re-wording
	}
	if t.Claim != "" {
		// `text` is DEFINED as a mirror of `claim`, so it is always re-derived, never merely
		// defaulted. A dual-shape record whose `text` lagged behind (an old binary wrote it after a
		// v2 write) must not serve the stale wording to ThesisTracker or to the thesis check, which
		// reads t["text"] as the claim to evaluate.
		t.Text = t.Claim
	}
	if t.Status == "" {
		t.Status = statusActive
	}
	for _, l := range t.itemLists() {
		if *l.Items == nil {
			*l.Items = []ThesisItem{}
		}
	}
	if t.Versions == nil {
		t.Versions = []VersionEvent{}
	}
	// Close metadata only exists on a closed record. This can only fire on a hand-edited file; it
	// keeps the in-memory record self-consistent so the next PATCH is not blocked forever.
	if t.Status != statusClosed {
		t.CloseReason, t.ClosedAt, t.ClosedByConditionID = nil, nil, ""
	}
	t.SchemaVersion = thesisSchemaVersion
}

// ---- validation ----

// validateThesisInvariants enforces the cross-field rules shared by create and patch. Field-level
// enum/range checks live where the value is decoded, so the error can name the field.
func validateThesisInvariants(t *Thesis) *apiError {
	t.Ticker = strings.ToUpper(strings.TrimSpace(t.Ticker))
	if t.Ticker == "" {
		return badField("ticker", "ticker is required")
	}

	// claim resolves from claim-or-text and is then mirrored back into text, forever-until-deprecated.
	claim := strings.TrimSpace(t.Claim)
	if claim == "" {
		claim = strings.TrimSpace(t.Text)
	}
	if claim == "" {
		return badField("claim", "thesis claim is required")
	}
	if len(claim) > maxThesisLen {
		claim = claim[:maxThesisLen] // truncate, as v1 did — an over-long claim must not fail the save
	}
	t.Claim, t.Text = claim, claim

	if !validThesisStatus[t.Status] {
		return badField("status", "status must be draft, active, closed, or archived")
	}
	if t.Status == statusClosed {
		if t.CloseReason == nil || *t.CloseReason == "" {
			return badField("closeReason", "closeReason is required when status is closed")
		}
	} else if t.CloseReason != nil {
		return badField("closeReason", "closeReason is only valid when status is closed")
	}
	if t.CloseReason != nil && !validCloseReason[*t.CloseReason] {
		return badField("closeReason", "closeReason must be invalidated, played_out, position_exited, superseded, or abandoned")
	}
	if t.Horizon != nil && !validHorizon[*t.Horizon] {
		return badField("horizon", "horizon must be days, weeks, months, quarters, years, or null")
	}
	if t.Confidence != nil && (*t.Confidence < 0 || *t.Confidence > 100) {
		return badField("confidence", "confidence must be between 0 and 100")
	}
	if len(t.Notes) > maxNotesLen {
		return badField("notes", "notes must be at most "+strconv.Itoa(maxNotesLen)+" characters")
	}
	for _, l := range t.itemLists() {
		if e := validateItems(l.Field, *l.Items); e != nil {
			return e
		}
	}
	// closedByConditionId, when set, must name a real invalidation condition on THIS thesis (§1.3).
	if t.ClosedByConditionID != "" {
		found := false
		for _, it := range t.InvalidationConditions {
			if it.ID == t.ClosedByConditionID {
				found = true
				break
			}
		}
		if !found {
			return badField("closedByConditionId", "closedByConditionId must reference an invalidation condition on this thesis")
		}
	}
	return nil
}

func validateItems(field string, items []ThesisItem) *apiError {
	if len(items) > maxItemsPerList {
		return badField(field, field+" is limited to "+strconv.Itoa(maxItemsPerList)+" items")
	}
	for _, it := range items {
		if strings.TrimSpace(it.Text) == "" {
			return badField(field, "every "+field+" item needs text")
		}
		if len(it.Text) > maxItemTextLen {
			return badField(field, field+" item text must be at most "+strconv.Itoa(maxItemTextLen)+" characters")
		}
	}
	return nil
}

// resolveItems applies an incoming replacement list. Items carrying an id KNOWN ON THIS LIST keep it
// (and their original createdAt); everything else — no id, an unknown id, or a duplicate within the
// payload — gets a freshly minted one. Refusing to honour an unfamiliar id is deliberate: item ids
// are link targets for evidence (Step 06) and alert rules (Step 08), so a client cannot mint or
// borrow one.
func resolveItems(field, prefix string, incoming, current []ThesisItem, now int64) ([]ThesisItem, *apiError) {
	if len(incoming) > maxItemsPerList {
		return nil, badField(field, field+" is limited to "+strconv.Itoa(maxItemsPerList)+" items")
	}
	known := make(map[string]ThesisItem, len(current))
	for _, it := range current {
		known[it.ID] = it
	}
	seen := make(map[string]bool, len(incoming))
	out := make([]ThesisItem, 0, len(incoming))
	for _, in := range incoming {
		text := strings.TrimSpace(in.Text)
		if text == "" {
			return nil, badField(field, "every "+field+" item needs text")
		}
		if len(text) > maxItemTextLen {
			return nil, badField(field, field+" item text must be at most "+strconv.Itoa(maxItemTextLen)+" characters")
		}
		item := ThesisItem{Text: text}
		if prev, ok := known[in.ID]; ok && in.ID != "" && !seen[in.ID] {
			item.ID, item.CreatedAt = prev.ID, prev.CreatedAt
		} else {
			item.ID, item.CreatedAt = prefix+newID(), now
		}
		seen[item.ID] = true
		// Optional per-list fields; ignored (not rejected) on the lists they do not belong to, so a
		// client can echo a whole item back without special-casing.
		if prefix == prefixQuestion {
			item.ResolvedAt = in.ResolvedAt
		}
		if prefix == prefixCatalyst {
			item.DueAt = in.DueAt
		}
		out = append(out, item)
	}
	return out, nil
}

// ---- version diffing ----

// diffThesis returns the VersionEvent for a material user-authored change, or nil when nothing
// material changed (§1.4). Material = claim, status, closeReason, horizon, confidence, notes, or any
// of the five item lists. Deliberately NOT material: lastCheck and lastReview (system-authored
// review output, not a belief change), and normalization (not a change at all) — which is exactly
// what makes a replayed PATCH and a reload both append nothing.
//
// Reordering a list without editing any item is not recorded: ItemDelta is defined in terms of
// added/removed/edited ids, so an order-only change has no representation and an empty event would
// be noise.
func diffThesis(before, after Thesis, reason string, now int64) *VersionEvent {
	var changed []string
	prev := map[string]any{}

	if before.Claim != after.Claim {
		changed = append(changed, "claim")
		prev["claim"] = truncateBytesOnRuneBoundary(before.Claim, maxVersionBeforeLen)
	}
	if before.Status != after.Status {
		changed = append(changed, "status")
		prev["status"] = before.Status
	}
	if !sameOptString(before.CloseReason, after.CloseReason) {
		changed = append(changed, "closeReason")
		prev["closeReason"] = optStringValue(before.CloseReason)
	}
	if !sameOptString(before.Horizon, after.Horizon) {
		changed = append(changed, "horizon")
		prev["horizon"] = optStringValue(before.Horizon)
	}
	if !sameOptInt(before.Confidence, after.Confidence) {
		changed = append(changed, "confidence")
		prev["confidence"] = optIntValue(before.Confidence)
	}
	if before.Notes != after.Notes {
		changed = append(changed, "notes")
		prev["notes"] = truncateBytesOnRuneBoundary(before.Notes, maxVersionBeforeLen)
	}

	deltas := map[string]ItemDelta{}
	beforeLists, afterLists := before.itemLists(), after.itemLists()
	for i := range afterLists {
		if d, ok := itemDelta(*beforeLists[i].Items, *afterLists[i].Items); ok {
			changed = append(changed, afterLists[i].Field)
			deltas[afterLists[i].Field] = d
		}
	}

	if len(changed) == 0 {
		return nil
	}
	sort.Strings(changed)
	ev := &VersionEvent{
		ID:            "ver_" + newID(),
		At:            now,
		Actor:         "user", // system writes (lastCheck/lastReview) produce no version event at all
		ChangedFields: changed,
		Reason:        truncateBytesOnRuneBoundary(strings.TrimSpace(reason), maxVersionReasonLen),
	}
	if len(prev) > 0 {
		ev.Before = prev
	}
	if len(deltas) > 0 {
		ev.ItemDelta = deltas
	}
	return ev
}

func itemDelta(before, after []ThesisItem) (ItemDelta, bool) {
	beforeByID := make(map[string]ThesisItem, len(before))
	for _, it := range before {
		beforeByID[it.ID] = it
	}
	afterByID := make(map[string]ThesisItem, len(after))
	for _, it := range after {
		afterByID[it.ID] = it
	}
	var d ItemDelta
	for _, it := range after {
		prev, ok := beforeByID[it.ID]
		if !ok {
			d.Added = append(d.Added, it.ID)
			continue
		}
		if prev.Text != it.Text || !sameOptInt64(prev.DueAt, it.DueAt) || !sameOptInt64(prev.ResolvedAt, it.ResolvedAt) {
			d.Edited = append(d.Edited, it.ID)
		}
	}
	for _, it := range before {
		if _, ok := afterByID[it.ID]; !ok {
			d.Removed = append(d.Removed, it.ID)
		}
	}
	return d, len(d.Added)+len(d.Removed)+len(d.Edited) > 0
}

func sameOptString(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func sameOptInt(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func sameOptInt64(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// optStringValue / optIntValue keep a nil prior value as JSON null in `before` rather than "" or 0.
func optStringValue(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func optIntValue(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// truncateBytesOnRuneBoundary caps a string at a BYTE budget, cutting only on a rune boundary so a
// bounded `before` value can never contain half a multi-byte character.
//
// WAVE 2 INTEGRATION NOTE: Step 04 (this file) and Step 06 (evidence.go) each independently added a
// rune-safe truncator called `truncateRunes` to this package, and the two are NOT interchangeable —
// this one takes a byte budget and returns just the string, while evidence.go's takes a rune COUNT
// and also reports whether it cut anything. Git merged the two files cleanly because they never
// touched the same lines, but the package then failed to compile. Each lane's callers depend on its
// own semantics, so the integrator renamed this one to say what it actually does rather than
// collapsing two different functions into one.
func truncateBytesOnRuneBoundary(s string, max int) string {
	if len(s) <= max {
		return s
	}
	out := make([]rune, 0, max)
	n := 0
	for _, r := range s {
		w := len(string(r))
		if n+w > max {
			break
		}
		out = append(out, r)
		n += w
	}
	return string(out)
}

// ---- store (mutex-guarded theses.json, same atomic-write + per-user partition as trades) ----
//
// Theses are PARTITIONED PER USER at {base}/{userId}/theses.json, so ownership on read/patch/delete
// is enforced by partition: an id that is not in the caller's file simply 404s. A pre-auth
// {base}/theses.json is migrated ONCE into {base}/_legacy/theses.json (preserved, unattributed) and
// is then read-never / write-never — no v2 normalization, no version back-fill, no claim (§1.10).

type ThesisStore struct {
	base  string
	mu    sync.Mutex
	cache map[string][]Thesis // userId -> theses (lazy-loaded per user, normalized on load)

	// broken marks users whose theses.json exists but did not parse. We serve empty and refuse to
	// write, so a malformed file is never silently replaced by a fresh one.
	broken map[string]bool
	docs   *documentRepository
}

func openThesisStore(base string) (*ThesisStore, error) {
	return openThesisStoreWithRepository(base, nil)
}

func openThesisStoreWithRepository(base string, docs *documentRepository) (*ThesisStore, error) {
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, err
	}
	s := &ThesisStore{base: base, cache: map[string][]Thesis{}, broken: map[string]bool{}, docs: docs}
	if docs == nil {
		s.migrateLegacy()
	}
	return s, nil
}

func (s *ThesisStore) path(uid string) string { return filepath.Join(s.base, uid, "theses.json") }

// migrateLegacy moves a pre-auth {base}/theses.json into the legacy bucket once. Idempotent. This is
// the ONLY migration in this file: the v1 -> v2 schema change is lazy (see normalizeThesis), so
// there is no eager rewrite to back up or roll back.
func (s *ThesisStore) migrateLegacy() {
	old := filepath.Join(s.base, "theses.json")
	if _, err := os.Stat(old); err != nil {
		return
	}
	if _, err := os.Stat(s.path(legacyBucket)); err == nil {
		return
	}
	if err := os.MkdirAll(filepath.Join(s.base, legacyBucket), 0o755); err != nil {
		return
	}
	if err := os.Rename(old, s.path(legacyBucket)); err == nil {
		log.Printf("journal: migrated pre-auth theses.json -> %s (legacy bucket, unattributed)", s.path(legacyBucket))
	}
}

// loadUser returns the user's theses in v2 shape, reading from disk on first access. Caller must hold
// s.mu. A parse failure is logged and quarantined rather than treated as "no theses", because the
// next write would otherwise destroy the file.
func (s *ThesisStore) loadUser(uid string) []Thesis {
	if t, ok := s.cache[uid]; ok {
		return t
	}
	theses := []Thesis{}
	var b []byte
	var found bool
	var err error
	if s.docs != nil {
		b, found, err = s.docs.load(uid, "theses", s.path(uid))
	} else {
		b, err = os.ReadFile(s.path(uid))
		found = err == nil
	}
	if err != nil && !os.IsNotExist(err) {
		log.Printf("journal: could not read theses for %s (%v) — serving empty and refusing writes", uid, err)
		s.broken[uid] = true
	} else if found {
		if err := json.Unmarshal(b, &theses); err != nil {
			log.Printf("journal: %s did not parse (%v) — serving empty and refusing to overwrite it", s.path(uid), err)
			s.broken[uid] = true
			theses = []Thesis{}
		}
	}
	for i := range theses {
		normalizeThesis(&theses[i])
	}
	s.cache[uid] = theses
	return theses
}

// persistLocked writes {uid}/theses.json via temp file + rename. Caller must hold s.mu.
func (s *ThesisStore) persistLocked(uid string) error {
	if s.broken[uid] {
		return errors.New("thesis store is unreadable")
	}
	if s.docs != nil {
		if err := s.docs.save(uid, "theses", s.cache[uid]); err != nil {
			s.broken[uid] = true
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Join(s.base, uid), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s.cache[uid], "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path(uid) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(uid))
}

func (s *ThesisStore) List(uid string) []Thesis {
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.loadUser(uid)
	out := make([]Thesis, len(src))
	copy(out, src)
	return out
}

func (s *ThesisStore) UserIDs() []string {
	if s.docs != nil {
		ids, err := s.docs.userIDs("theses", "theses.json")
		if err == nil {
			return ids
		}
		return nil
	}
	entries, err := os.ReadDir(s.base)
	if err != nil {
		return nil
	}
	var ids []string
	for _, entry := range entries {
		if entry.IsDir() && !reservedUID(entry.Name()) {
			if _, err := os.Stat(s.path(entry.Name())); err == nil {
				ids = append(ids, entry.Name())
			}
		}
	}
	sort.Strings(ids)
	return ids
}

func (s *ThesisStore) Get(uid, id string) (Thesis, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.loadUser(uid) {
		if t.ID == id {
			return t, true
		}
	}
	return Thesis{}, false
}

// countActive counts `active` theses for one ticker, optionally excluding one id (the record being
// patched). Grandfathering falls out of checking the cap only when ACTIVATING.
func countActive(all []Thesis, ticker, excludeID string) int {
	n := 0
	for _, t := range all {
		if t.ID != excludeID && t.Status == statusActive && t.Ticker == ticker {
			n++
		}
	}
	return n
}

// Create validates the cap and appends. Server-owned fields are set here, never taken from the
// client.
func (s *ThesisStore) Create(uid string, t Thesis, now int64) (Thesis, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.broken[uid] {
		return Thesis{}, errStoreUnreadable
	}
	all := s.loadUser(uid)
	if t.Status == statusActive {
		if n := countActive(all, t.Ticker, ""); n >= maxActiveTheses {
			return Thesis{}, activeCapError(n)
		}
	}
	t.ID = newID()
	t.SchemaVersion = thesisSchemaVersion
	t.CreatedAt, t.UpdatedAt = now, now
	t.LastCheck = nil             // system-owned; set only through the check PATCH
	t.LastReview = nil            // advanced only through the review-checkpoint route
	t.Versions = []VersionEvent{} // no synthetic "created" event (§1.5)
	t.VersionsTruncated = false
	before := append([]Thesis(nil), all...)
	s.cache[uid] = append(all, t)
	if err := s.persistLocked(uid); err != nil {
		s.cache[uid] = before
		return Thesis{}, errStoreUnreadable
	}
	return t, nil
}

// thesisMutation is what a mutation callback returns: the next record plus how to record it.
type thesisMutation struct {
	next Thesis

	// version asks for a VersionEvent if (and only if) the diff finds a material change.
	version bool
	reason  string

	// bumpUpdated is false for writes that are not edits to the thesis itself — advancing a review
	// checkpoint marks that the user looked, not that the belief changed.
	bumpUpdated bool
}

// Mutate applies fn to the CALLER'S thesis and persists the result. fn runs INSIDE the mutex, over
// the currently stored record and the caller's full list, which is what makes two contract rules
// hold at once: the active cap is evaluated against live state, and the version event is diffed from
// the stored record in the same hold as the write — so a replayed identical PATCH appends nothing.
//
// fn must treat `all` as read-only.
func (s *ThesisStore) Mutate(uid, id string, now int64, fn func(cur Thesis, all []Thesis) (thesisMutation, *apiError)) (Thesis, *apiError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.broken[uid] {
		return Thesis{}, errStoreUnreadable
	}
	all := s.loadUser(uid)
	idx := -1
	for i := range all {
		if all[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return Thesis{}, errThesisNotFound
	}
	cur := all[idx]

	m, apiErr := fn(cur, all)
	if apiErr != nil {
		return Thesis{}, apiErr
	}

	next := m.next
	next.ID = cur.ID               // never regenerated
	next.CreatedAt = cur.CreatedAt // preserved verbatim, 0 included
	next.SchemaVersion = thesisSchemaVersion
	next.Versions = append([]VersionEvent{}, cur.Versions...)
	next.VersionsTruncated = cur.VersionsTruncated

	if m.version {
		if ev := diffThesis(cur, next, m.reason, now); ev != nil {
			next.Versions = append(next.Versions, *ev)
			if len(next.Versions) > maxVersionEvents {
				next.Versions = next.Versions[len(next.Versions)-maxVersionEvents:]
				next.VersionsTruncated = true
			}
		}
	}
	if m.bumpUpdated {
		next.UpdatedAt = now
	}

	prior := all[idx]
	all[idx] = next
	s.cache[uid] = all
	if err := s.persistLocked(uid); err != nil {
		all[idx] = prior
		s.cache[uid] = all
		return Thesis{}, errStoreUnreadable
	}
	return next, nil
}

func (s *ThesisStore) Delete(uid, id string) *apiError {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.broken[uid] {
		return errStoreUnreadable
	}
	theses := s.loadUser(uid)
	for i := range theses {
		if theses[i].ID == id {
			before := append([]Thesis(nil), theses...)
			s.cache[uid] = append(theses[:i], theses[i+1:]...)
			if err := s.persistLocked(uid); err != nil {
				s.cache[uid] = before
				return errStoreUnreadable
			}
			return nil
		}
	}
	return errThesisNotFound
}

// ---- handlers ----

func (s *Server) handleListTheses(w http.ResponseWriter, r *http.Request) {
	ticker := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("ticker")))
	status := r.URL.Query().Get("status") // draft | active | closed | archived | "" (all)
	out := make([]Thesis, 0)
	for _, t := range s.theses.List(userID(r)) {
		if ticker != "" && t.Ticker != ticker {
			continue
		}
		if status != "" && t.Status != status {
			continue
		}
		out = append(out, t)
	}
	writeJSON(w, http.StatusOK, map[string]any{"theses": out})
}

func (s *Server) handleGetThesis(w http.ResponseWriter, r *http.Request) {
	t, ok := s.theses.Get(userID(r), r.PathValue("id"))
	if !ok {
		writeAPIError(w, errThesisNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"thesis": t})
}

func (s *Server) handleCreateThesis(w http.ResponseWriter, r *http.Request) {
	var t Thesis
	if err := json.NewDecoder(r.Body).Decode(&t); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON: " + err.Error()})
		return
	}
	now := time.Now().Unix()
	if t.Status == "" {
		t.Status = statusActive // v1's default: a client sending {ticker, text} still gets an active thesis
	}
	// Items arrive without ids on create; mint them all.
	for _, l := range t.itemLists() {
		items, e := resolveItems(l.Field, l.Prefix, *l.Items, nil, now)
		if e != nil {
			writeAPIError(w, e)
			return
		}
		*l.Items = items
	}
	if e := validateThesisInvariants(&t); e != nil {
		writeAPIError(w, e)
		return
	}
	if t.Status == statusClosed && t.ClosedAt == nil {
		at := now
		t.ClosedAt = &at
	}
	created, e := s.theses.Create(userID(r), t, now)
	if e != nil {
		writeAPIError(w, e)
		return
	}
	// Product events (§6.2). The durable write above is their sole authority: nothing is emitted on a
	// rejected create, and the append can never fail this response.
	emitThesisEvent(evThesisCreated, userID(r), created, "created")
	if created.Status == statusActive {
		emitThesisEvent(evThesisActivated, userID(r), created, statusActive)
	}
	writeJSON(w, http.StatusCreated, map[string]any{"thesis": created})
}

// emitThesisEvent is the one place a thesis transition becomes telemetry. It passes IDs, the ticker,
// and the lifecycle enum only — the claim, notes, and every item's text stay where they belong (§6.4).
func emitThesisEvent(event, uid string, t Thesis, transitionKey string) {
	in := eventInput{
		Event:         event,
		UID:           uid,
		TransitionKey: transitionKey,
		Ticker:        t.Ticker,
		ObjectID:      t.ID,
		ThesisID:      t.ID,
	}
	if t.CloseReason != nil {
		in.CloseReason = *t.CloseReason
	}
	analytics.emit(in)
}

// thesisPatch uses pointers so an ABSENT field stays untouched, and json.RawMessage for the three
// nullable scalars so `absent` and `"horizon": null` (clear it) stay distinguishable (§1.9).
// json.RawMessage implements Unmarshaler, so unlike a plain pointer it is still populated for an
// explicit JSON null.
type thesisPatch struct {
	Text   *string `json:"text"`  // accepted forever: writes BOTH text and claim (§1.5)
	Claim  *string `json:"claim"` // wins when both are sent
	Status *string `json:"status"`
	Notes  *string `json:"notes"`

	CloseReason         json.RawMessage `json:"closeReason"`
	ClosedByConditionID *string         `json:"closedByConditionId"`
	Horizon             json.RawMessage `json:"horizon"`
	Confidence          json.RawMessage `json:"confidence"`

	Assumptions            *[]ThesisItem `json:"assumptions"`
	Catalysts              *[]ThesisItem `json:"catalysts"`
	Risks                  *[]ThesisItem `json:"risks"`
	InvalidationConditions *[]ThesisItem `json:"invalidationConditions"`
	NextQuestions          *[]ThesisItem `json:"nextQuestions"`

	// Reason annotates the version event; it is not stored on the thesis.
	Reason *string `json:"reason"`

	// LastCheck is how the gateway persists a completed evidence check. SYSTEM-authored: it produces
	// no version event and can never reach a user-owned field. lastReview is deliberately absent —
	// it moves only through POST /theses/{id}/review-checkpoint (§1.8).
	LastCheck *ThesisCheck `json:"lastCheck"`
}

// touched reports whether the body carried any recognized field, so a completely empty PATCH does
// not bump updatedAt.
func (p *thesisPatch) touched() bool {
	return p.Text != nil || p.Claim != nil || p.Status != nil || p.Notes != nil ||
		len(p.CloseReason) > 0 || p.ClosedByConditionID != nil || len(p.Horizon) > 0 || len(p.Confidence) > 0 ||
		p.Assumptions != nil || p.Catalysts != nil || p.Risks != nil ||
		p.InvalidationConditions != nil || p.NextQuestions != nil || p.LastCheck != nil
}

func (p *thesisPatch) list(field string) *[]ThesisItem {
	switch field {
	case "assumptions":
		return p.Assumptions
	case "catalysts":
		return p.Catalysts
	case "risks":
		return p.Risks
	case "invalidationConditions":
		return p.InvalidationConditions
	case "nextQuestions":
		return p.NextQuestions
	}
	return nil
}

// rawPresent distinguishes the three PATCH states for a nullable scalar.
func rawPresent(raw json.RawMessage) (present, isNull bool) {
	if len(raw) == 0 {
		return false, false
	}
	return true, strings.TrimSpace(string(raw)) == "null"
}

// applyThesisPatch builds the next record from the STORED one. It runs inside the store mutex, so
// `all` is live state and the cap check below cannot race.
func applyThesisPatch(cur Thesis, p thesisPatch, all []Thesis, now int64) (Thesis, *apiError) {
	next := cur

	// claim/text are mirrored: whichever the client sends, both are written. claim wins if both.
	if p.Text != nil {
		next.Claim, next.Text = *p.Text, *p.Text
	}
	if p.Claim != nil {
		next.Claim, next.Text = *p.Claim, *p.Claim
	}
	if p.Notes != nil {
		next.Notes = *p.Notes
	}

	if present, isNull := rawPresent(p.Horizon); present {
		if isNull {
			next.Horizon = nil
		} else {
			var h string
			if err := json.Unmarshal(p.Horizon, &h); err != nil {
				return next, badField("horizon", "horizon must be a string or null")
			}
			if !validHorizon[h] {
				return next, badField("horizon", "horizon must be days, weeks, months, quarters, years, or null")
			}
			next.Horizon = &h
		}
	}

	if present, isNull := rawPresent(p.Confidence); present {
		if isNull {
			next.Confidence = nil
		} else {
			var c int
			if err := json.Unmarshal(p.Confidence, &c); err != nil {
				return next, badField("confidence", "confidence must be a whole number 0-100 or null")
			}
			if c < 0 || c > 100 {
				return next, badField("confidence", "confidence must be between 0 and 100")
			}
			next.Confidence = &c
		}
	}

	if present, isNull := rawPresent(p.CloseReason); present {
		if isNull {
			next.CloseReason = nil
		} else {
			var cr string
			if err := json.Unmarshal(p.CloseReason, &cr); err != nil {
				return next, badField("closeReason", "closeReason must be a string or null")
			}
			next.CloseReason = &cr
		}
	}

	// Item lists REPLACE wholesale; `[]` clears. Ids known on the list survive, so evidence and
	// alert links to a specific item are not silently re-pointed by an ordinary save.
	nextLists, curLists := next.itemLists(), cur.itemLists()
	for i := range nextLists {
		incoming := p.list(nextLists[i].Field)
		if incoming == nil {
			continue
		}
		items, e := resolveItems(nextLists[i].Field, nextLists[i].Prefix, *incoming, *curLists[i].Items, now)
		if e != nil {
			return next, e
		}
		*nextLists[i].Items = items
	}

	if p.ClosedByConditionID != nil {
		next.ClosedByConditionID = *p.ClosedByConditionID
	}

	// Lifecycle last, so a single PATCH may set status and closeReason together.
	if p.Status != nil && *p.Status != cur.Status {
		to := *p.Status
		if !validThesisStatus[to] {
			return next, badField("status", "status must be draft, active, closed, or archived")
		}
		if !legalThesisTransition[[2]string{cur.Status, to}] {
			return next, &apiError{
				Status: http.StatusBadRequest, Code: "illegal_transition", Field: "status",
				Msg: "cannot move a thesis from " + cur.Status + " to " + to,
			}
		}
		if to == statusActive {
			if n := countActive(all, next.Ticker, cur.ID); n >= maxActiveTheses {
				return next, activeCapError(n)
			}
		}
		next.Status = to
		switch {
		case to == statusClosed:
			if next.ClosedAt == nil {
				at := now
				next.ClosedAt = &at
			}
		case cur.Status == statusClosed:
			// Leaving closed: the close record no longer describes the thesis. The prior closeReason
			// survives in the version event's `before` and the status change itself is in history, so
			// nothing is lost by clearing it — and §1.2 requires closeReason to be null whenever
			// status is not "closed".
			next.CloseReason, next.ClosedAt, next.ClosedByConditionID = nil, nil, ""
		}
	}

	// LastCheck is system-authored. Validated, but never diffed and never allowed near a user field:
	// in particular the model's confidence lands on the check, not on Thesis.Confidence (§1.6).
	if p.LastCheck != nil {
		if !validVerdict[p.LastCheck.Verdict] {
			return next, badField("lastCheck.verdict", "lastCheck.verdict must be confirming, neutral, or challenged")
		}
		if p.LastCheck.Confidence != nil && (*p.LastCheck.Confidence < 0 || *p.LastCheck.Confidence > 100) {
			return next, badField("lastCheck.confidence", "lastCheck.confidence must be between 0 and 100")
		}
		if p.LastCheck.At == 0 {
			p.LastCheck.At = now
		}
		check := *p.LastCheck
		next.LastCheck = &check
	}

	if e := validateThesisInvariants(&next); e != nil {
		return next, e
	}
	return next, nil
}

func (s *Server) handleUpdateThesis(w http.ResponseWriter, r *http.Request) {
	var p thesisPatch
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON: " + err.Error()})
		return
	}
	now := time.Now().Unix()
	// prevStatus is captured inside the mutation so the lifecycle event is decided from the STORED
	// record, not from what the client thought the status was.
	prevStatus := ""
	updated, e := s.theses.Mutate(userID(r), r.PathValue("id"), now, func(cur Thesis, all []Thesis) (thesisMutation, *apiError) {
		next, apiErr := applyThesisPatch(cur, p, all, now)
		if apiErr != nil {
			return thesisMutation{}, apiErr
		}
		prevStatus = cur.Status
		reason := ""
		if p.Reason != nil {
			reason = *p.Reason
		}
		return thesisMutation{next: next, version: true, reason: reason, bumpUpdated: p.touched()}, nil
	})
	if e != nil {
		writeAPIError(w, e)
		return
	}
	if updated.Status != prevStatus {
		switch updated.Status {
		case statusActive:
			emitThesisEvent(evThesisActivated, userID(r), updated, statusActive)
		case statusClosed:
			emitThesisEvent(evThesisClosed, userID(r), updated, statusClosed)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"thesis": updated})
}

// handleThesisVersions returns the append-only history, newest first. `truncated` means the response
// is not the whole history — either the limit cut it, or the record itself dropped its oldest
// entries at the cap.
func (s *Server) handleThesisVersions(w http.ResponseWriter, r *http.Request) {
	t, ok := s.theses.Get(userID(r), r.PathValue("id"))
	if !ok {
		writeAPIError(w, errThesisNotFound)
		return
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > maxVersionEvents {
		limit = maxVersionEvents
	}
	out := make([]VersionEvent, 0, limit)
	for i := len(t.Versions) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, t.Versions[i])
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"versions":  out,
		"truncated": t.VersionsTruncated || len(out) < len(t.Versions),
	})
}

type reviewCheckpointReq struct {
	ItemCount int    `json:"itemCount"`
	Source    string `json:"source"`
}

// handleThesisReviewCheckpoint advances the "reviewed up to here" boundary. Explicit user action
// only — this route is the ONLY way lastReview moves. It writes no other field, creates no version
// event (looking is not a belief change), and does not bump updatedAt.
func (s *Server) handleThesisReviewCheckpoint(w http.ResponseWriter, r *http.Request) {
	var req reviewCheckpointReq
	// An empty body is fine: it means "reviewed, nothing else to say".
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON: " + err.Error()})
		return
	}
	if req.Source == "" {
		req.Source = "user"
	}
	if !validCheckpointSource[req.Source] {
		writeAPIError(w, badField("source", "source must be user or decision"))
		return
	}
	if req.ItemCount < 0 {
		writeAPIError(w, badField("itemCount", "itemCount cannot be negative"))
		return
	}
	now := time.Now().Unix()
	updated, e := s.theses.Mutate(userID(r), r.PathValue("id"), now, func(cur Thesis, _ []Thesis) (thesisMutation, *apiError) {
		next := cur
		next.LastReview = &ReviewCheckpoint{
			CheckpointID: "chk_" + newID(),
			At:           now,
			ItemCount:    req.ItemCount,
			Source:       req.Source,
		}
		return thesisMutation{next: next, version: false, bumpUpdated: false}, nil
	})
	if e != nil {
		writeAPIError(w, e)
		return
	}
	// The checkpoint id is the transition key, so a replayed request de-duplicates while a genuine
	// second review (a new checkpoint) is a second event — which is what the north-star metric counts.
	if updated.LastReview != nil {
		emitThesisEvent(evThesisReviewed, userID(r), updated, updated.LastReview.CheckpointID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"thesis": updated})
}

func (s *Server) handleDeleteThesis(w http.ResponseWriter, r *http.Request) {
	if e := s.theses.Delete(userID(r), r.PathValue("id")); e != nil {
		writeAPIError(w, e)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
