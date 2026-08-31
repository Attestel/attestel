package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// decisions.go — durable DECISION RECORDS. Contract: PARALLEL_CONTRACTS.md §5.1–§5.3, §5.7.
//
// A decision record answers a question no other object in this tree can answer: WHY did the user act,
// or deliberately not act, and what did they know at the time? Three properties make it worth keeping:
//
//  1. A DELIBERATE NON-ACTION IS FIRST-CLASS. `Trade` requires entry.price > 0 and entry.size > 0
//     (trades.go), so "I looked at this and decided not to buy" is inexpressible there. Here it is an
//     `intent` of do_not_act or wait with `action.kind == "none"` — a real record, countable, and the
//     denominator of the decision-loop metric. That is D-05 option (c): intent and execution are
//     separate objects, so the trade journal's schema is untouched.
//  2. THE SNAPSHOT IS WRITE-ONCE AND SERVER-BUILT. The thesis you decided on will change — that is
//     what a thesis is for. If the record pointed only at the live thesis, every later edit would
//     silently rewrite the past and the review would be worthless. So POST /decisions captures a
//     BOUNDED immutable summary (§5.3) from the caller's OWN thesis + evidence, and PATCH accepts
//     `tradeId` and nothing else. A changed mind is a NEW record — history must show that it changed.
//  3. IT IS A RECORD, NOT A RECOMMENDATION. `intent` and `action` carry the USER'S own decision, the
//     same reason `Trade.side` is allowed today. Nothing in this file produces, validates against, or
//     stores a system verdict, a price target, or a suggestion, and nothing here executes anything.
//
// References out (evidence, reviews, change items, monitoring rules) are stored as IDs. Evidence and
// review IDs are RESOLVED against the caller's own stores — a foreign or unknown id is a 400, so a
// citation always points at something real. Change-item and monitoring-rule ids are opaque: they are
// owned by the monitoring lane (§4.4/§4.1) and resolving them here would mean building a second
// monitoring representation, which the wave rules forbid.

const (
	decisionSchemaVersion = 1

	maxRationaleLen    = 4000
	maxOpenQuestions   = 20
	maxQuestionLen     = 500
	maxConditionsLen   = 500
	maxRefsPerList     = 50
	maxRefIDLen        = 128
	defaultDecisionLim = 100
	maxDecisionLim     = 500

	// Snapshot bounds (§5.3). The snapshot is a summary, never a copy of unlimited documents.
	maxSnapshotClaimLen = 2000
	maxSnapshotItemLen  = 500
	maxSnapshotItems    = 20
	maxSnapshotEvidence = 50
	maxSignalReasonLen  = 200
	maxPriceSourceLen   = 40
)

// intent (D-05 option c). `act` means the user did something; the other two are deliberate.
var validDecisionIntent = map[string]bool{"act": true, "do_not_act": true, "wait": true}

// action.kind describes what the USER did — their own record, never a suggestion.
var validActionKind = map[string]bool{"open": true, "add": true, "trim": true, "exit": true, "none": true}

var validActionSide = map[string]bool{"long": true, "short": true}

// DecisionAction is the user's own action detail. `none` is what makes a non-action expressible.
type DecisionAction struct {
	Kind string  `json:"kind"`
	Side *string `json:"side"`
}

// SnapshotItem is one thesis child item, flattened to id + text. Only assumptions and invalidation
// conditions are captured: they are the two lists a later review actually judges (§5.4 grades
// assumptions held/failed and catalysts occurred; catalysts are captured through `conditionsToMonitor`
// the user writes themselves).
type SnapshotItem struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// SnapshotEvidence is the display metadata of one cited evidence record — deliberately WITHOUT the
// excerpt. Excerpt caps are D-08's, enforced in exactly one place (evidence.go); copying excerpts
// here would duplicate a legally-bounded field into a second store and make the decision record grow
// without limit. The id is the durable link; this is only what a review needs to recognise the item.
type SnapshotEvidence struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Title      string `json:"title"`
	Provider   string `json:"provider"`
	DataState  string `json:"dataState"`
	SourceTime int64  `json:"sourceTime"`
}

// SnapshotModelContext records the MODEL and DATA state at decision time.
//
// `signalPresent` says whether a WALK-FORWARD-VALIDATED signal existed — never a bare direction.
// Recording "the model said up" would turn an audit record into a stored recommendation; recording
// "a validated signal existed / did not, and why" is what a reviewer actually needs to judge whether
// the decision leaned on it.
type SnapshotModelContext struct {
	SignalPresent    bool   `json:"signalPresent"`
	SignalReason     string `json:"signalReason"`
	PriceSource      string `json:"priceSource"`
	PriceIsSynthetic bool   `json:"priceIsSynthetic"`
}

// DecisionSnapshot is the bounded immutable summary (§5.3). Written once, at create, by the server.
type DecisionSnapshot struct {
	CapturedAt             int64                `json:"capturedAt"`
	ThesisVersionID        string               `json:"thesisVersionId"`
	Claim                  string               `json:"claim"`
	Status                 string               `json:"status"`
	Horizon                *string              `json:"horizon"`
	Confidence             *int                 `json:"confidence"`
	Assumptions            []SnapshotItem       `json:"assumptions"`
	InvalidationConditions []SnapshotItem       `json:"invalidationConditions"`
	Evidence               []SnapshotEvidence   `json:"evidence"`
	ModelContext           SnapshotModelContext `json:"modelContext"`
	DataState              string               `json:"dataState"`
}

// DecisionRecord is the persisted record. Field order mirrors §5.2.
type DecisionRecord struct {
	ID            string `json:"id"` // dec_ + 16 hex
	SchemaVersion int    `json:"schemaVersion"`
	Ticker        string `json:"ticker"`

	ThesisID        string `json:"thesisId"`
	ThesisVersionID string `json:"thesisVersionId"` // REQUIRED — the exact version decided on
	CheckpointID    string `json:"checkpointId"`    // optional, when made from a change review

	Intent string         `json:"intent"`
	Action DecisionAction `json:"action"`

	Horizon   *string `json:"horizon"`
	Rationale string  `json:"rationale"` // user-authored, REQUIRED

	UserConfidence *int `json:"userConfidence"` // 0..100 | null — the USER'S, never a model's

	EvidenceIds       []string `json:"evidenceIds"`
	ReviewIds         []string `json:"reviewIds"`
	ChangeItemIds     []string `json:"changeItemIds"`     // chg_ — opaque, owned by the monitoring lane
	MonitoringRuleIds []string `json:"monitoringRuleIds"` // alerts rule ids — opaque, same reason

	OpenQuestions       []string `json:"openQuestions"`
	ConditionsToMonitor []string `json:"conditionsToMonitor"`

	Snapshot DecisionSnapshot `json:"snapshot"`

	TradeID   *string `json:"tradeId"`   // optional link to an existing Trade
	TradeMode string  `json:"tradeMode"` // "live" | "paper", copied at link time (§5.6)

	DecidedAt int64 `json:"decidedAt"` // user-settable
	CreatedAt int64 `json:"createdAt"`
}

// ---- errors ----

func unknownRefError(field string, ids []string) *apiError {
	return &apiError{
		Status: http.StatusBadRequest, Code: "unknown_reference",
		Msg:   "one or more referenced records do not exist in your account",
		Field: field,
		Extra: map[string]any{"unknown": ids},
	}
}

var errDecisionNotFound = &apiError{
	Status: http.StatusNotFound, Code: "not_found", Msg: "decision not found",
}

var errDecisionStoreUnreadable = &apiError{
	Status: http.StatusServiceUnavailable, Code: "store_unreadable",
	Msg: "stored decisions could not be read; refusing to overwrite them",
}

// immutableFieldError is what makes the write-once rule visible to a client instead of silently
// ignoring the field (§5.3).
func immutableFieldError(field string) *apiError {
	return &apiError{
		Status: http.StatusBadRequest, Code: "immutable_field",
		Msg:   "a decision record is write-once apart from tradeId; record a new decision instead",
		Field: field,
	}
}

// ---- API ----

type decisionAPI struct {
	srv   *Server
	store *decisionStore
}

// registerDecisionRoutes mounts the decision + outcome surface (§5.7). Reads are optionalAuth (a
// guest simply has none); every write is requireAuth.
//
// Constructed here rather than hung off Server so this lane's routes, types, store, and snapshot
// composition stay in its own files — one registration line in handlers.go, exactly as the evidence
// and review lanes did.
func (s *Server) registerDecisionRoutes(mux *http.ServeMux) {
	store, err := openDecisionStoreWithRepository(s.cfg.TradesDir, s.documents)
	if err != nil {
		// Additive: if the store cannot open, trades / theses / evidence / reviews must still serve.
		log.Printf("journal: decision store unavailable in %q: %v (decision routes not mounted)", s.cfg.TradesDir, err)
		return
	}
	api := &decisionAPI{srv: s, store: store}

	mux.HandleFunc("GET /decisions", s.optionalAuth(api.handleList))
	mux.HandleFunc("POST /decisions", s.requireAuth(api.handleCreate))
	mux.HandleFunc("GET /decisions/{id}", s.optionalAuth(api.handleGet))
	mux.HandleFunc("PATCH /decisions/{id}", s.requireAuth(api.handlePatch))
	mux.HandleFunc("DELETE /decisions/{id}", s.requireAuth(api.handleDelete))

	mux.HandleFunc("GET /outcomes", s.optionalAuth(api.handleListOutcomes))
	mux.HandleFunc("POST /outcomes", s.requireAuth(api.handleCreateOutcome))
	mux.HandleFunc("GET /outcomes/{id}", s.optionalAuth(api.handleGetOutcome))
	mux.HandleFunc("PATCH /outcomes/{id}", s.requireAuth(api.handlePatchOutcome))
}

func newDecisionID() string { return "dec_" + newID() }
func newOutcomeID() string  { return "out_" + newID() }

// ---- validation helpers ----

// normalizeRefs trims, drops blanks, de-duplicates (preserving order) and bounds a reference list.
// It never reorders: the order a user cited things in is information.
func normalizeRefs(field string, in []string) ([]string, *apiError) {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, raw := range in {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if len(id) > maxRefIDLen {
			return nil, badField(field, field+" contains an over-long id")
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	if len(out) > maxRefsPerList {
		return nil, badField(field, field+" is limited to "+strconv.Itoa(maxRefsPerList)+" references")
	}
	return out, nil
}

// normalizeTextList bounds a list of user-authored lines (open questions, conditions to monitor).
func normalizeTextList(field string, in []string, maxLen int) ([]string, *apiError) {
	out := make([]string, 0, len(in))
	for _, raw := range in {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		if len([]rune(s)) > maxLen {
			return nil, badField(field, field+" entries are limited to "+strconv.Itoa(maxLen)+" characters")
		}
		out = append(out, s)
	}
	if len(out) > maxOpenQuestions {
		return nil, badField(field, field+" is limited to "+strconv.Itoa(maxOpenQuestions)+" entries")
	}
	return out, nil
}

// ---- create ----

// decisionCreateReq is the ENTIRE accepted create surface. `snapshot` is absent by design: §5.7 says
// the server assembles it, so a client has no way to claim it decided on a state it did not have.
//
// modelSignalPresent / modelSignalReason are the one part of the snapshot this service cannot
// compute: the validated-signal state lives in the prediction service, which the journal has no
// client for (and should not grow one — the gateway is the aggregator). The gateway fills them in on
// the caller's behalf; both are bounded and the fallback is the honest "not recorded", never a guess.
type decisionCreateReq struct {
	Ticker          string `json:"ticker"`
	ThesisID        string `json:"thesisId"`
	ThesisVersionID string `json:"thesisVersionId"`
	CheckpointID    string `json:"checkpointId"`

	Intent string          `json:"intent"`
	Action *DecisionAction `json:"action"`

	Horizon        *string `json:"horizon"`
	Rationale      string  `json:"rationale"`
	UserConfidence *int    `json:"userConfidence"`

	EvidenceIds       []string `json:"evidenceIds"`
	ReviewIds         []string `json:"reviewIds"`
	ChangeItemIds     []string `json:"changeItemIds"`
	MonitoringRuleIds []string `json:"monitoringRuleIds"`

	OpenQuestions       []string `json:"openQuestions"`
	ConditionsToMonitor []string `json:"conditionsToMonitor"`

	TradeID   *string `json:"tradeId"`
	DecidedAt int64   `json:"decidedAt"`

	ModelSignalPresent bool   `json:"modelSignalPresent"`
	ModelSignalReason  string `json:"modelSignalReason"`
}

func (a *decisionAPI) handleCreate(w http.ResponseWriter, r *http.Request) {
	uid := userID(r)

	var req decisionCreateReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeAPIError(w, badField("", "invalid JSON: "+err.Error()))
		return
	}

	rec, apiErr := a.buildDecision(r.Context(), uid, req)
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}

	saved, err := a.store.addDecision(uid, rec)
	if err != nil {
		writeAPIError(w, errDecisionStoreUnreadable)
		return
	}
	// §6.2. `intent` is the one thing the decision-loop metric needs and the one thing that is not
	// personal content: the rationale, the open questions, and the conditions to monitor are all
	// user-authored prose and stay here.
	analytics.emit(eventInput{
		Event:         evDecisionRecorded,
		UID:           uid,
		TransitionKey: "created",
		Ticker:        saved.Ticker,
		ObjectID:      saved.ID,
		ThesisID:      saved.ThesisID,
		Intent:        saved.Intent,
		DataState:     saved.Snapshot.DataState,
		Synthetic:     saved.Snapshot.ModelContext.PriceIsSynthetic,
	})
	writeJSON(w, http.StatusCreated, map[string]any{"decision": saved})
}

// buildDecision validates the request and composes the record, INCLUDING the server-built snapshot.
// Split out from the handler so the create path is testable without HTTP and so the ordering —
// validate everything, then capture, then write — stays obvious.
func (a *decisionAPI) buildDecision(ctx context.Context, uid string, req decisionCreateReq) (DecisionRecord, *apiError) {
	now := time.Now().Unix()

	// --- thesis: required, must be the caller's own, and pins the exact version ---
	thesisID := strings.TrimSpace(req.ThesisID)
	if thesisID == "" {
		return DecisionRecord{}, badField("thesisId", "thesisId is required — a decision is always about a thesis")
	}
	thesis, ok := a.srv.theses.Get(uid, thesisID)
	if !ok {
		return DecisionRecord{}, unknownRefError("thesisId", []string{thesisID})
	}

	// The version the user decided on. A client may pin one explicitly (it decided from a specific
	// point in history); absent, the CURRENT head version is used. A thesis with no recorded history
	// yet gets the sentinel below rather than a fabricated version id — §1.5 forbids inventing history.
	versionID := strings.TrimSpace(req.ThesisVersionID)
	if versionID == "" {
		versionID = currentThesisVersionID(thesis)
	} else if !thesisHasVersion(thesis, versionID) {
		return DecisionRecord{}, unknownRefError("thesisVersionId", []string{versionID})
	}

	// --- intent + action (§5.1) ---
	intent := strings.TrimSpace(req.Intent)
	if !validDecisionIntent[intent] {
		return DecisionRecord{}, badField("intent", "intent must be act, do_not_act, or wait")
	}
	action := DecisionAction{Kind: "none"}
	if req.Action != nil {
		action = *req.Action
	}
	action.Kind = strings.TrimSpace(action.Kind)
	if action.Kind == "" {
		action.Kind = "none"
	}
	if !validActionKind[action.Kind] {
		return DecisionRecord{}, badField("action.kind", "action.kind must be open, add, trim, exit, or none")
	}
	// This pair of rules is what keeps a deliberate non-action honest in both directions: a
	// non-action cannot secretly carry an action, and "act" cannot be recorded with nothing done.
	if intent != "act" && action.Kind != "none" {
		return DecisionRecord{}, badField("action.kind",
			"a deliberate non-action records action.kind \"none\" — record intent \"act\" if you acted")
	}
	if intent == "act" && action.Kind == "none" {
		return DecisionRecord{}, badField("action.kind",
			"intent \"act\" requires an action.kind of open, add, trim, or exit")
	}
	if action.Side != nil {
		side := strings.TrimSpace(*action.Side)
		if side == "" {
			action.Side = nil
		} else if !validActionSide[side] {
			return DecisionRecord{}, badField("action.side", "action.side must be long, short, or null")
		} else {
			action.Side = &side
		}
	}
	if action.Kind == "none" {
		action.Side = nil // a non-action has no side
	}

	// --- rationale: the whole point of the record ---
	rationale := strings.TrimSpace(req.Rationale)
	if rationale == "" {
		return DecisionRecord{}, badField("rationale", "rationale is required — the reasoning is the record")
	}
	if len([]rune(rationale)) > maxRationaleLen {
		return DecisionRecord{}, badField("rationale",
			"rationale is limited to "+strconv.Itoa(maxRationaleLen)+" characters")
	}

	// --- horizon + confidence ---
	horizon := req.Horizon
	if horizon != nil {
		h := strings.TrimSpace(*horizon)
		if h == "" {
			horizon = nil
		} else if !validHorizon[h] {
			return DecisionRecord{}, badField("horizon", "horizon must be days, weeks, months, quarters, years, or null")
		} else {
			horizon = &h
		}
	}
	if req.UserConfidence != nil && (*req.UserConfidence < 0 || *req.UserConfidence > 100) {
		return DecisionRecord{}, badField("userConfidence", "userConfidence must be between 0 and 100")
	}

	// --- references ---
	evidenceIds, apiErr := normalizeRefs("evidenceIds", req.EvidenceIds)
	if apiErr != nil {
		return DecisionRecord{}, apiErr
	}
	reviewIds, apiErr := normalizeRefs("reviewIds", req.ReviewIds)
	if apiErr != nil {
		return DecisionRecord{}, apiErr
	}
	changeItemIds, apiErr := normalizeRefs("changeItemIds", req.ChangeItemIds)
	if apiErr != nil {
		return DecisionRecord{}, apiErr
	}
	monitoringRuleIds, apiErr := normalizeRefs("monitoringRuleIds", req.MonitoringRuleIds)
	if apiErr != nil {
		return DecisionRecord{}, apiErr
	}

	// Evidence and reviews are resolved against the CALLER'S own stores, read fresh from disk. A
	// foreign id is indistinguishable from a nonexistent one here, which is the point: this never
	// confirms that another user's record exists.
	records, err := readUserEvidence(a.srv.cfg.TradesDir, uid, a.srv.documents)
	if err != nil {
		return DecisionRecord{}, errStoreUnreadable
	}
	byEvidenceID := map[string]EvidenceRecord{}
	for _, rec := range records {
		byEvidenceID[rec.ID] = rec
	}
	if missing := missingIDs(evidenceIds, func(id string) bool { _, ok := byEvidenceID[id]; return ok }); len(missing) > 0 {
		return DecisionRecord{}, unknownRefError("evidenceIds", missing)
	}

	knownReviews, err := readUserReviewIDs(a.srv.cfg.TradesDir, uid, a.srv.documents)
	if err != nil {
		return DecisionRecord{}, errStoreUnreadable
	}
	if missing := missingIDs(reviewIds, func(id string) bool { return knownReviews[id] }); len(missing) > 0 {
		return DecisionRecord{}, unknownRefError("reviewIds", missing)
	}

	openQuestions, apiErr := normalizeTextList("openQuestions", req.OpenQuestions, maxQuestionLen)
	if apiErr != nil {
		return DecisionRecord{}, apiErr
	}
	conditions, apiErr := normalizeTextList("conditionsToMonitor", req.ConditionsToMonitor, maxConditionsLen)
	if apiErr != nil {
		return DecisionRecord{}, apiErr
	}

	// --- optional trade link ---
	tradeID, tradeMode, apiErr := a.resolveTradeLink(uid, req.TradeID)
	if apiErr != nil {
		return DecisionRecord{}, apiErr
	}

	decidedAt := req.DecidedAt
	if decidedAt <= 0 {
		decidedAt = now
	}

	ticker := strings.ToUpper(strings.TrimSpace(req.Ticker))
	if ticker == "" {
		ticker = thesis.Ticker // the thesis is the authority; a mismatch is not worth a 400
	}

	snapshot := a.captureSnapshot(ctx, thesis, versionID, evidenceIds, byEvidenceID, req, now)

	return DecisionRecord{
		ID:                  newDecisionID(),
		SchemaVersion:       decisionSchemaVersion,
		Ticker:              ticker,
		ThesisID:            thesis.ID,
		ThesisVersionID:     versionID,
		CheckpointID:        strings.TrimSpace(req.CheckpointID),
		Intent:              intent,
		Action:              action,
		Horizon:             horizon,
		Rationale:           rationale,
		UserConfidence:      req.UserConfidence,
		EvidenceIds:         evidenceIds,
		ReviewIds:           reviewIds,
		ChangeItemIds:       changeItemIds,
		MonitoringRuleIds:   monitoringRuleIds,
		OpenQuestions:       openQuestions,
		ConditionsToMonitor: conditions,
		Snapshot:            snapshot,
		TradeID:             tradeID,
		TradeMode:           tradeMode,
		DecidedAt:           decidedAt,
		CreatedAt:           now,
	}, nil
}

// currentThesisVersionID returns the head version id, or the explicit sentinel when a thesis has no
// recorded history. §1.5 keeps `versions` empty for legacy records rather than fabricating a "created"
// event — so a decision on such a thesis says so plainly instead of citing a version that never
// existed. The snapshot below still captures the full state, so the record stays reviewable either way.
func currentThesisVersionID(t Thesis) string {
	if len(t.Versions) == 0 {
		return "unversioned"
	}
	head := t.Versions[0]
	for _, v := range t.Versions {
		if v.At >= head.At {
			head = v
		}
	}
	return head.ID
}

func thesisHasVersion(t Thesis, id string) bool {
	if id == "unversioned" {
		return len(t.Versions) == 0
	}
	for _, v := range t.Versions {
		if v.ID == id {
			return true
		}
	}
	return false
}

func missingIDs(ids []string, known func(string) bool) []string {
	var missing []string
	for _, id := range ids {
		if !known(id) {
			missing = append(missing, id)
		}
	}
	return missing
}

// resolveTradeLink validates an optional trade link and copies the trade's mode. Both modes are
// linkable (§5.6) — what matters is that the mode is RECORDED, so a decision-level metric can group
// by it and paper can never be silently counted as live.
func (a *decisionAPI) resolveTradeLink(uid string, tradeID *string) (*string, string, *apiError) {
	if tradeID == nil {
		return nil, "", nil
	}
	id := strings.TrimSpace(*tradeID)
	if id == "" {
		return nil, "", nil
	}
	trade, ok, err := a.srv.store.Get(uid, id)
	if err != nil {
		return nil, "", errTradeStoreUnavailable
	}
	if !ok {
		return nil, "", unknownRefError("tradeId", []string{id})
	}
	return &id, effectiveMode(trade.Mode), nil
}

// captureSnapshot builds the write-once summary (§5.3).
//
// It copies TEXT for the claim, assumptions and invalidation conditions — bounded — because those are
// the user's own words and are exactly what a later review re-reads. It copies only METADATA for
// evidence, never excerpts: the excerpt lives once, in the evidence record, under D-08's caps.
//
// Every field is either read from the caller's own stores or computed here. Nothing is taken from the
// request except the two model-signal fields the gateway supplies, which are bounded and default to
// "not recorded".
func (a *decisionAPI) captureSnapshot(
	ctx context.Context,
	thesis Thesis,
	versionID string,
	evidenceIds []string,
	byEvidenceID map[string]EvidenceRecord,
	req decisionCreateReq,
	now int64,
) DecisionSnapshot {
	snap := DecisionSnapshot{
		CapturedAt:             now,
		ThesisVersionID:        versionID,
		Claim:                  boundSnapshotText(thesis.Claim, maxSnapshotClaimLen),
		Status:                 thesis.Status,
		Horizon:                thesis.Horizon,
		Confidence:             thesis.Confidence,
		Assumptions:            snapshotItems(thesis.Assumptions),
		InvalidationConditions: snapshotItems(thesis.InvalidationConditions),
		Evidence:               []SnapshotEvidence{},
	}

	for _, id := range evidenceIds {
		if len(snap.Evidence) >= maxSnapshotEvidence {
			break
		}
		rec, ok := byEvidenceID[id]
		if !ok {
			continue // unreachable: create validated these first
		}
		snap.Evidence = append(snap.Evidence, SnapshotEvidence{
			ID:         rec.ID,
			Kind:       rec.Kind,
			Title:      boundSnapshotText(rec.Title, maxSnapshotItemLen),
			Provider:   rec.Provider,
			DataState:  rec.DataState,
			SourceTime: rec.SourceTime,
		})
	}

	// Price provenance is computed here from the journal's OWN quote client — the same one that marks
	// open trades — so the recorded data state is observed, not asserted by the caller. A failed fetch
	// leaves it empty and the UI says "unknown": invariant #1's visible half is never guessed at.
	mc := SnapshotModelContext{
		SignalPresent: req.ModelSignalPresent,
		SignalReason:  boundSnapshotText(strings.TrimSpace(req.ModelSignalReason), maxSignalReasonLen),
	}
	if mc.SignalReason == "" {
		mc.SignalReason = "not recorded"
	}
	qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if q, err := a.srv.fetchQuote(qctx, thesis.Ticker); err == nil && q != nil {
		mc.PriceSource = boundSnapshotText(q.Source, maxPriceSourceLen)
		mc.PriceIsSynthetic = q.Source == "synthetic"
	}
	snap.ModelContext = mc

	// The snapshot's own dataState follows the price feed, because that is the one provenance signal
	// that covers the whole decision moment. Unknown stays unknown.
	switch {
	case mc.PriceSource == "":
		snap.DataState = "unknown"
	case mc.PriceIsSynthetic:
		snap.DataState = "synthetic"
	case mc.PriceSource == "seed":
		snap.DataState = "seed"
	default:
		snap.DataState = "live"
	}
	return snap
}

func snapshotItems(items []ThesisItem) []SnapshotItem {
	out := make([]SnapshotItem, 0, len(items))
	for i, it := range items {
		if i >= maxSnapshotItems {
			break
		}
		out = append(out, SnapshotItem{ID: it.ID, Text: boundSnapshotText(it.Text, maxSnapshotItemLen)})
	}
	return out
}

// truncateRunes bounds a string on a rune boundary so a snapshot can never split a multi-byte
// character mid-way.
func boundSnapshotText(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// ---- read ----

func (a *decisionAPI) handleList(w http.ResponseWriter, r *http.Request) {
	uid := userID(r)
	q := r.URL.Query()
	ticker := strings.ToUpper(strings.TrimSpace(q.Get("ticker")))
	thesisID := strings.TrimSpace(q.Get("thesisId"))
	intent := strings.TrimSpace(q.Get("intent"))

	all := a.store.listDecisions(uid)
	out := make([]DecisionRecord, 0, len(all))
	for _, d := range all {
		if ticker != "" && d.Ticker != ticker {
			continue
		}
		if thesisID != "" && d.ThesisID != thesisID {
			continue
		}
		if intent != "" && d.Intent != intent {
			continue
		}
		out = append(out, d)
	}

	limit := parseDecisionLimit(q.Get("limit"))
	cursor := strings.TrimSpace(q.Get("cursor"))
	if cursor != "" {
		for i, d := range out {
			if d.ID == cursor {
				out = out[i+1:]
				break
			}
		}
	}
	next := ""
	if len(out) > limit {
		next = out[limit-1].ID
		out = out[:limit]
	}

	// reviewedIds lets a list render "reviewed / awaiting review" without N+1 calls — and it is the
	// numerator of this step's metric (share of decisions that later receive an outcome review).
	reviewed := map[string]bool{}
	for _, o := range a.store.listOutcomes(uid) {
		reviewed[o.DecisionID] = true
	}
	ids := make([]string, 0, len(reviewed))
	for id := range reviewed {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	writeJSON(w, http.StatusOK, map[string]any{
		"decisions": out, "nextCursor": next, "reviewedDecisionIds": ids,
	})
}

func parseDecisionLimit(raw string) int {
	n := defaultDecisionLim
	if raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			n = v
		}
	}
	if n > maxDecisionLim {
		n = maxDecisionLim
	}
	return n
}

func (a *decisionAPI) handleGet(w http.ResponseWriter, r *http.Request) {
	uid := userID(r)
	rec, ok := a.store.getDecision(uid, r.PathValue("id"))
	if !ok {
		// A foreign id is 404, never 403 — existence is not confirmed across users.
		writeAPIError(w, errDecisionNotFound)
		return
	}
	body := map[string]any{"decision": rec}
	if out, ok := a.store.outcomeFor(uid, rec.ID); ok {
		body["outcome"] = out
	}
	writeJSON(w, http.StatusOK, body)
}

// ---- patch (tradeId only) ----

// decisionPatch is the ENTIRE mutable surface. Every other field is decoded only so an attempt to
// change it can be REFUSED by name (§5.3) — silently ignoring it would let a client believe the
// record had been rewritten.
type decisionPatch struct {
	TradeID json.RawMessage `json:"tradeId"`

	Rationale           *string          `json:"rationale"`
	Intent              *string          `json:"intent"`
	Action              *json.RawMessage `json:"action"`
	Snapshot            *json.RawMessage `json:"snapshot"`
	UserConfidence      *int             `json:"userConfidence"`
	EvidenceIds         *[]string        `json:"evidenceIds"`
	ReviewIds           *[]string        `json:"reviewIds"`
	ChangeItemIds       *[]string        `json:"changeItemIds"`
	MonitoringRuleIds   *[]string        `json:"monitoringRuleIds"`
	OpenQuestions       *[]string        `json:"openQuestions"`
	ConditionsToMonitor *[]string        `json:"conditionsToMonitor"`
	Horizon             *string          `json:"horizon"`
	ThesisID            *string          `json:"thesisId"`
	ThesisVersionID     *string          `json:"thesisVersionId"`
	Ticker              *string          `json:"ticker"`
	DecidedAt           *int64           `json:"decidedAt"`
}

// immutableAttempt names the first write-once field a patch tried to change, or "".
func (p decisionPatch) immutableAttempt() string {
	switch {
	case p.Rationale != nil:
		return "rationale"
	case p.Intent != nil:
		return "intent"
	case p.Action != nil:
		return "action"
	case p.Snapshot != nil:
		return "snapshot"
	case p.UserConfidence != nil:
		return "userConfidence"
	case p.EvidenceIds != nil:
		return "evidenceIds"
	case p.ReviewIds != nil:
		return "reviewIds"
	case p.ChangeItemIds != nil:
		return "changeItemIds"
	case p.MonitoringRuleIds != nil:
		return "monitoringRuleIds"
	case p.OpenQuestions != nil:
		return "openQuestions"
	case p.ConditionsToMonitor != nil:
		return "conditionsToMonitor"
	case p.Horizon != nil:
		return "horizon"
	case p.ThesisID != nil:
		return "thesisId"
	case p.ThesisVersionID != nil:
		return "thesisVersionId"
	case p.Ticker != nil:
		return "ticker"
	case p.DecidedAt != nil:
		return "decidedAt"
	}
	return ""
}

func (a *decisionAPI) handlePatch(w http.ResponseWriter, r *http.Request) {
	uid := userID(r)
	id := r.PathValue("id")

	var p decisionPatch
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<18)).Decode(&p); err != nil {
		writeAPIError(w, badField("", "invalid JSON: "+err.Error()))
		return
	}
	if field := p.immutableAttempt(); field != "" {
		writeAPIError(w, immutableFieldError(field))
		return
	}
	if _, ok := a.store.getDecision(uid, id); !ok {
		writeAPIError(w, errDecisionNotFound)
		return
	}

	present, isNull := rawPresent(p.TradeID)
	if !present {
		writeAPIError(w, badField("tradeId", "nothing to update — a decision record accepts tradeId only"))
		return
	}

	var tradeID *string
	tradeMode := ""
	if !isNull {
		var raw string
		if err := json.Unmarshal(p.TradeID, &raw); err != nil {
			writeAPIError(w, badField("tradeId", "tradeId must be a string or null"))
			return
		}
		var apiErr *apiError
		tradeID, tradeMode, apiErr = a.resolveTradeLink(uid, &raw)
		if apiErr != nil {
			writeAPIError(w, apiErr)
			return
		}
	}

	updated, ok, err := a.store.linkTrade(uid, id, tradeID, tradeMode)
	if err != nil {
		writeAPIError(w, errDecisionStoreUnreadable)
		return
	}
	if !ok {
		writeAPIError(w, errDecisionNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"decision": updated})
}

func (a *decisionAPI) handleDelete(w http.ResponseWriter, r *http.Request) {
	uid := userID(r)
	found, cascaded, err := a.store.deleteDecision(uid, r.PathValue("id"))
	if err != nil {
		writeAPIError(w, errDecisionStoreUnreadable)
		return
	}
	if !found {
		writeAPIError(w, errDecisionNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": r.PathValue("id"), "outcomesRemoved": cascaded})
}
