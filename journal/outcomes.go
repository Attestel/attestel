package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// outcomes.go — OUTCOME REVIEWS. Contract: PARALLEL_CONTRACTS.md §5.4, §5.5.
//
// The review asks a different question from "did it make money": was the reasoning sound? Those two
// answers are kept structurally apart, and that separation is the feature:
//
//   - `reasoningQuality` is USER-authored, 1..5, and no code anywhere derives it from `marketOutcome`,
//     ranks one by the other, or combines them into a single score. A profitable decision reviewed at
//     2 and a losing one reviewed at 5 are both normal and both render without contradiction.
//   - `marketOutcome` is DERIVED server-side from the linked trade, or it is null. A client cannot
//     assert P&L, and a decision with no linked trade — every deliberate non-action — simply has no
//     quantitative outcome rather than a fabricated one.
//   - Nothing here produces a verdict, and profit is never presented as evidence of analytical
//     quality.
//
// A review can never modify the decision it reviews: no function in this file opens decisions.json
// for writing (decision_store.go's addOutcome/patchOutcome touch outcomes.json only). Revising a
// review is a PATCH of the four user-authored fields; the observation of what happened is not
// rewritable into a different decision.

const (
	maxObservedOutcomeLen = 2000
	maxRetrospectiveLen   = 4000
	maxProcessLessons     = 20
	maxProcessLessonLen   = 500
	maxOutcomeItems       = 40
	maxOutcomeLabelLen    = 500
	maxOutcomeNoteLen     = 500
)

// assumption verdicts (§5.4). `unknown` is first-class: "I cannot tell yet" is an honest review
// answer and must not be forced into held/failed.
var validAssumptionVerdict = map[string]bool{"held": true, "failed": true, "unknown": true}

// CatalystOutcome records whether an expected catalyst actually occurred.
type CatalystOutcome struct {
	ThesisItemID string `json:"thesisItemId"`
	Label        string `json:"label"`
	Occurred     bool   `json:"occurred"`
}

// AssumptionOutcome records whether one assumption held.
type AssumptionOutcome struct {
	ThesisItemID string `json:"thesisItemId"`
	Verdict      string `json:"verdict"` // held | failed | unknown
	Note         string `json:"note"`
}

// MarketOutcome is the optional quantitative result, derived from the linked trade. `synthetic` and
// `source` ride along because a P&L computed against synthetic prices is not a result — it is a demo,
// and it says so.
type MarketOutcome struct {
	PnlPct    *float64 `json:"pnlPct"`
	TradeID   string   `json:"tradeId"`
	TradeMode string   `json:"tradeMode"` // live | paper — never merged into one number (§5.6)
	PriceThen *float64 `json:"priceThen"`
	PriceNow  *float64 `json:"priceNow"`
	Source    string   `json:"source"`
	Synthetic bool     `json:"synthetic"`
	Realized  bool     `json:"realized"` // false => marked to a live quote, not a closed trade
}

// OutcomeReview is the persisted record. Field order mirrors §5.4.
type OutcomeReview struct {
	ID            string `json:"id"` // out_ + 16 hex
	SchemaVersion int    `json:"schemaVersion"`
	DecisionID    string `json:"decisionId"`
	ThesisID      string `json:"thesisId"`
	Ticker        string `json:"ticker"`

	ReviewedAt     int64 `json:"reviewedAt"`
	HorizonElapsed bool  `json:"horizonElapsed"`

	ObservedOutcome   string              `json:"observedOutcome"`
	CatalystsOccurred []CatalystOutcome   `json:"catalystsOccurred"`
	Assumptions       []AssumptionOutcome `json:"assumptions"`

	// ReasoningQuality is the user's own assessment of their PROCESS. Nullable, because a review that
	// records what happened without yet judging the reasoning is a legitimate half-step.
	ReasoningQuality *int     `json:"reasoningQuality"`
	ProcessLessons   []string `json:"processLessons"`
	Retrospective    string   `json:"retrospective"`

	MarketOutcome *MarketOutcome `json:"marketOutcome"`

	CreatedAt int64 `json:"createdAt"`
	UpdatedAt int64 `json:"updatedAt"`
}

var errOutcomeNotFound = &apiError{
	Status: http.StatusNotFound, Code: "not_found", Msg: "outcome review not found",
}

// ---- create ----

// outcomeCreateReq is the accepted create surface. `marketOutcome` is absent by design — it is
// derived, not asserted.
type outcomeCreateReq struct {
	DecisionID string `json:"decisionId"`

	ReviewedAt     int64 `json:"reviewedAt"`
	HorizonElapsed bool  `json:"horizonElapsed"`

	ObservedOutcome   string              `json:"observedOutcome"`
	CatalystsOccurred []CatalystOutcome   `json:"catalystsOccurred"`
	Assumptions       []AssumptionOutcome `json:"assumptions"`

	ReasoningQuality *int     `json:"reasoningQuality"`
	ProcessLessons   []string `json:"processLessons"`
	Retrospective    string   `json:"retrospective"`
}

func (a *decisionAPI) handleCreateOutcome(w http.ResponseWriter, r *http.Request) {
	uid := userID(r)

	var req outcomeCreateReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeAPIError(w, badField("", "invalid JSON: "+err.Error()))
		return
	}

	decisionID := strings.TrimSpace(req.DecisionID)
	if decisionID == "" {
		writeAPIError(w, badField("decisionId", "decisionId is required"))
		return
	}
	decision, ok := a.store.getDecision(uid, decisionID)
	if !ok {
		writeAPIError(w, unknownRefError("decisionId", []string{decisionID}))
		return
	}

	rec, apiErr := a.buildOutcome(r, uid, decision, req)
	if apiErr != nil {
		writeAPIError(w, apiErr)
		return
	}

	saved, conflict, err := a.store.addOutcome(uid, rec)
	if err != nil {
		writeAPIError(w, errDecisionStoreUnreadable)
		return
	}
	if conflict {
		writeAPIError(w, &apiError{
			Status: http.StatusConflict, Code: "outcome_exists",
			Msg:   "this decision already has an outcome review — revise it instead of adding a second",
			Field: "decisionId",
			Extra: map[string]any{"outcomeId": saved.ID},
		})
		return
	}
	// §6.2. reasoningQuality is the USER'S own 1..5 process score and rides along because the readout
	// must never derive it from P&L (§5.5) — it is reported on its own, next to nothing. The
	// observation, the retrospective, and the process lessons are user-authored prose and stay here.
	in := eventInput{
		Event:            evOutcomeReviewed,
		UID:              uid,
		TransitionKey:    "created",
		Ticker:           saved.Ticker,
		ObjectID:         saved.ID,
		ThesisID:         saved.ThesisID,
		ReasoningQuality: saved.ReasoningQuality,
	}
	if saved.MarketOutcome != nil {
		in.Synthetic = saved.MarketOutcome.Synthetic
	}
	analytics.emit(in)
	writeJSON(w, http.StatusCreated, map[string]any{"outcome": saved})
}

func (a *decisionAPI) buildOutcome(r *http.Request, uid string, decision DecisionRecord, req outcomeCreateReq) (OutcomeReview, *apiError) {
	now := time.Now().Unix()

	observed := strings.TrimSpace(req.ObservedOutcome)
	if observed == "" {
		return OutcomeReview{}, badField("observedOutcome", "observedOutcome is required — record what actually happened")
	}
	if len([]rune(observed)) > maxObservedOutcomeLen {
		return OutcomeReview{}, badField("observedOutcome",
			"observedOutcome is limited to "+strconv.Itoa(maxObservedOutcomeLen)+" characters")
	}

	retro := strings.TrimSpace(req.Retrospective)
	if len([]rune(retro)) > maxRetrospectiveLen {
		return OutcomeReview{}, badField("retrospective",
			"retrospective is limited to "+strconv.Itoa(maxRetrospectiveLen)+" characters")
	}

	if req.ReasoningQuality != nil && (*req.ReasoningQuality < 1 || *req.ReasoningQuality > 5) {
		return OutcomeReview{}, badField("reasoningQuality", "reasoningQuality must be between 1 and 5")
	}

	lessons, apiErr := normalizeTextList("processLessons", req.ProcessLessons, maxProcessLessonLen)
	if apiErr != nil {
		return OutcomeReview{}, apiErr
	}
	if len(lessons) > maxProcessLessons {
		return OutcomeReview{}, badField("processLessons",
			"processLessons is limited to "+strconv.Itoa(maxProcessLessons)+" entries")
	}

	// Catalyst and assumption rows may only name items that were on the thesis AT DECISION TIME. That
	// is what makes the review a review of THIS decision: grading an assumption the user added later
	// would be judging the decision by information it never had.
	knownAssumption := map[string]bool{}
	for _, it := range decision.Snapshot.Assumptions {
		knownAssumption[it.ID] = true
	}
	knownCondition := map[string]bool{}
	for _, it := range decision.Snapshot.InvalidationConditions {
		knownCondition[it.ID] = true
	}

	catalysts, apiErr := normalizeCatalysts(req.CatalystsOccurred)
	if apiErr != nil {
		return OutcomeReview{}, apiErr
	}
	assumptions, apiErr := normalizeAssumptions(req.Assumptions, knownAssumption, knownCondition)
	if apiErr != nil {
		return OutcomeReview{}, apiErr
	}

	reviewedAt := req.ReviewedAt
	if reviewedAt <= 0 {
		reviewedAt = now
	}

	marketOutcome, apiErr := a.deriveMarketOutcome(r, uid, decision)
	if apiErr != nil {
		return OutcomeReview{}, apiErr
	}

	return OutcomeReview{
		ID:                newOutcomeID(),
		SchemaVersion:     decisionSchemaVersion,
		DecisionID:        decision.ID,
		ThesisID:          decision.ThesisID,
		Ticker:            decision.Ticker,
		ReviewedAt:        reviewedAt,
		HorizonElapsed:    req.HorizonElapsed,
		ObservedOutcome:   observed,
		CatalystsOccurred: catalysts,
		Assumptions:       assumptions,
		ReasoningQuality:  req.ReasoningQuality,
		ProcessLessons:    lessons,
		Retrospective:     retro,
		MarketOutcome:     marketOutcome,
		CreatedAt:         now,
		UpdatedAt:         now,
	}, nil
}

func normalizeCatalysts(in []CatalystOutcome) ([]CatalystOutcome, *apiError) {
	out := make([]CatalystOutcome, 0, len(in))
	for _, c := range in {
		c.ThesisItemID = strings.TrimSpace(c.ThesisItemID)
		c.Label = strings.TrimSpace(c.Label)
		if c.Label == "" && c.ThesisItemID == "" {
			continue
		}
		if len([]rune(c.Label)) > maxOutcomeLabelLen {
			return nil, badField("catalystsOccurred", "catalyst labels are limited to "+strconv.Itoa(maxOutcomeLabelLen)+" characters")
		}
		out = append(out, c)
	}
	if len(out) > maxOutcomeItems {
		return nil, badField("catalystsOccurred", "catalystsOccurred is limited to "+strconv.Itoa(maxOutcomeItems)+" entries")
	}
	return out, nil
}

// normalizeAssumptions validates each verdict and each referenced item. An item id that was not on
// the decision's snapshot is a 400 rather than a silent drop — a review that appears to grade an
// assumption but quietly discarded it is worse than one that refuses.
func normalizeAssumptions(in []AssumptionOutcome, knownAssumption, knownCondition map[string]bool) ([]AssumptionOutcome, *apiError) {
	out := make([]AssumptionOutcome, 0, len(in))
	var unknown []string
	for _, as := range in {
		as.ThesisItemID = strings.TrimSpace(as.ThesisItemID)
		as.Verdict = strings.TrimSpace(as.Verdict)
		as.Note = strings.TrimSpace(as.Note)
		if as.ThesisItemID == "" {
			continue
		}
		if !validAssumptionVerdict[as.Verdict] {
			return nil, badField("assumptions", "assumption verdict must be held, failed, or unknown")
		}
		if len([]rune(as.Note)) > maxOutcomeNoteLen {
			return nil, badField("assumptions", "assumption notes are limited to "+strconv.Itoa(maxOutcomeNoteLen)+" characters")
		}
		if !knownAssumption[as.ThesisItemID] && !knownCondition[as.ThesisItemID] {
			unknown = append(unknown, as.ThesisItemID)
			continue
		}
		out = append(out, as)
	}
	if len(unknown) > 0 {
		return nil, unknownRefError("assumptions", unknown)
	}
	if len(out) > maxOutcomeItems {
		return nil, badField("assumptions", "assumptions is limited to "+strconv.Itoa(maxOutcomeItems)+" entries")
	}
	return out, nil
}

// deriveMarketOutcome computes the optional quantitative result from the decision's LINKED TRADE, or
// returns nil.
//
// nil is the common and correct answer: every deliberate non-action has no trade, and inventing a
// number for it would imply the app tracked a position it never had. When a trade IS linked, the P&L
// comes from the journal's own TradeView math — the same numbers the trade ledger shows — so a
// decision review can never disagree with the trade record about what happened.
func (a *decisionAPI) deriveMarketOutcome(r *http.Request, uid string, decision DecisionRecord) (*MarketOutcome, *apiError) {
	if decision.TradeID == nil || *decision.TradeID == "" {
		return nil, nil
	}
	trade, ok, err := a.srv.store.Get(uid, *decision.TradeID)
	if err != nil {
		return nil, errTradeStoreUnavailable
	}
	if !ok {
		return nil, nil // the trade was deleted after linking; the decision record still stands
	}
	view := a.srv.buildView(r.Context(), trade)

	mo := &MarketOutcome{
		TradeID:   trade.ID,
		TradeMode: effectiveMode(trade.Mode),
		PnlPct:    view.PnlPct,
		PriceThen: f64ptr(trade.Entry.Price),
		Realized:  trade.Exit != nil,
	}
	if trade.Exit != nil {
		mo.PriceNow = f64ptr(trade.Exit.Price)
		mo.Source = "recorded exit"
	} else {
		mo.PriceNow = view.MarkPrice
		mo.Source = view.MarkSource
		mo.Synthetic = view.MarkSynthetic
	}
	return mo, nil
}

// ---- read ----

func (a *decisionAPI) handleListOutcomes(w http.ResponseWriter, r *http.Request) {
	uid := userID(r)
	q := r.URL.Query()
	decisionID := strings.TrimSpace(q.Get("decisionId"))
	thesisID := strings.TrimSpace(q.Get("thesisId"))
	ticker := strings.ToUpper(strings.TrimSpace(q.Get("ticker")))

	all := a.store.listOutcomes(uid)
	out := make([]OutcomeReview, 0, len(all))
	for _, o := range all {
		if decisionID != "" && o.DecisionID != decisionID {
			continue
		}
		if thesisID != "" && o.ThesisID != thesisID {
			continue
		}
		if ticker != "" && o.Ticker != ticker {
			continue
		}
		out = append(out, o)
	}
	writeJSON(w, http.StatusOK, map[string]any{"outcomes": out})
}

func (a *decisionAPI) handleGetOutcome(w http.ResponseWriter, r *http.Request) {
	rec, ok := a.store.getOutcome(userID(r), r.PathValue("id"))
	if !ok {
		writeAPIError(w, errOutcomeNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"outcome": rec})
}

// ---- patch (the four user-authored fields) ----

// outcomePatchValues is the store-level shape of an accepted revision.
type outcomePatchValues struct {
	ObservedOutcome  *string
	ReasoningQuality *int
	ProcessLessons   *[]string
	Retrospective    *string
	Now              int64
}

// outcomePatch mirrors decisionPatch: the four mutable fields, plus the immutable ones decoded only
// so an attempt to change them is refused by name.
type outcomePatch struct {
	ObservedOutcome  *string   `json:"observedOutcome"`
	ReasoningQuality *int      `json:"reasoningQuality"`
	ProcessLessons   *[]string `json:"processLessons"`
	Retrospective    *string   `json:"retrospective"`

	DecisionID        *string          `json:"decisionId"`
	ThesisID          *string          `json:"thesisId"`
	Ticker            *string          `json:"ticker"`
	MarketOutcome     *json.RawMessage `json:"marketOutcome"`
	CatalystsOccurred *json.RawMessage `json:"catalystsOccurred"`
	Assumptions       *json.RawMessage `json:"assumptions"`
	ReviewedAt        *int64           `json:"reviewedAt"`
	HorizonElapsed    *bool            `json:"horizonElapsed"`
}

func (p outcomePatch) immutableAttempt() string {
	switch {
	case p.DecisionID != nil:
		return "decisionId"
	case p.ThesisID != nil:
		return "thesisId"
	case p.Ticker != nil:
		return "ticker"
	case p.MarketOutcome != nil:
		return "marketOutcome"
	case p.CatalystsOccurred != nil:
		return "catalystsOccurred"
	case p.Assumptions != nil:
		return "assumptions"
	case p.ReviewedAt != nil:
		return "reviewedAt"
	case p.HorizonElapsed != nil:
		return "horizonElapsed"
	}
	return ""
}

func (a *decisionAPI) handlePatchOutcome(w http.ResponseWriter, r *http.Request) {
	uid := userID(r)
	id := r.PathValue("id")

	var p outcomePatch
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p); err != nil {
		writeAPIError(w, badField("", "invalid JSON: "+err.Error()))
		return
	}
	if field := p.immutableAttempt(); field != "" {
		writeAPIError(w, &apiError{
			Status: http.StatusBadRequest, Code: "immutable_field",
			Msg:   "an outcome review accepts observedOutcome, reasoningQuality, processLessons, and retrospective only",
			Field: field,
		})
		return
	}

	values := outcomePatchValues{Now: time.Now().Unix()}
	if p.ObservedOutcome != nil {
		s := strings.TrimSpace(*p.ObservedOutcome)
		if s == "" {
			writeAPIError(w, badField("observedOutcome", "observedOutcome cannot be cleared"))
			return
		}
		if len([]rune(s)) > maxObservedOutcomeLen {
			writeAPIError(w, badField("observedOutcome",
				"observedOutcome is limited to "+strconv.Itoa(maxObservedOutcomeLen)+" characters"))
			return
		}
		values.ObservedOutcome = &s
	}
	if p.ReasoningQuality != nil {
		if *p.ReasoningQuality < 1 || *p.ReasoningQuality > 5 {
			writeAPIError(w, badField("reasoningQuality", "reasoningQuality must be between 1 and 5"))
			return
		}
		values.ReasoningQuality = p.ReasoningQuality
	}
	if p.ProcessLessons != nil {
		lessons, apiErr := normalizeTextList("processLessons", *p.ProcessLessons, maxProcessLessonLen)
		if apiErr != nil {
			writeAPIError(w, apiErr)
			return
		}
		values.ProcessLessons = &lessons
	}
	if p.Retrospective != nil {
		s := strings.TrimSpace(*p.Retrospective)
		if len([]rune(s)) > maxRetrospectiveLen {
			writeAPIError(w, badField("retrospective",
				"retrospective is limited to "+strconv.Itoa(maxRetrospectiveLen)+" characters"))
			return
		}
		values.Retrospective = &s
	}

	updated, ok, err := a.store.patchOutcome(uid, id, values)
	if err != nil {
		writeAPIError(w, errDecisionStoreUnreadable)
		return
	}
	if !ok {
		writeAPIError(w, errOutcomeNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"outcome": updated})
}

// ---- cross-store readers -------------------------------------------------------------------------
//
// Evidence and saved reviews are owned by other lanes' files (evidence_store.go, reviews.go), each
// with its own cached store instance created inside its own route registration. Opening a SECOND
// cached instance here would give this lane a snapshot of those files that goes stale the moment the
// user saves new evidence — and a citation would then be rejected as unknown seconds after it was
// created.
//
// So these read the files fresh, read-only, at the one moment it matters (composing a decision). They
// reuse the owning lanes' types, so the schema still has exactly one definition, and they never write.

func readUserEvidence(base, uid string, docs *documentRepository) ([]EvidenceRecord, error) {
	if uid == "" || reservedUID(uid) {
		return nil, nil
	}
	var b []byte
	var err error
	var found bool
	if docs != nil {
		b, found, err = docs.load(uid, "evidence", filepath.Join(base, uid, evidenceFile))
	} else {
		b, err = os.ReadFile(filepath.Join(base, uid, evidenceFile))
		found = err == nil
	}
	if !found && err == nil {
		return nil, nil
	}
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []EvidenceRecord
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func readUserReviewIDs(base, uid string, docs *documentRepository) (map[string]bool, error) {
	if uid == "" || reservedUID(uid) {
		return map[string]bool{}, nil
	}
	var b []byte
	var err error
	var found bool
	if docs != nil {
		b, found, err = docs.load(uid, "reviews", filepath.Join(base, uid, "reviews.json"))
	} else {
		b, err = os.ReadFile(filepath.Join(base, uid, "reviews.json"))
		found = err == nil
	}
	if !found && err == nil {
		return map[string]bool{}, nil
	}
	if os.IsNotExist(err) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	var list []Review
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(list))
	for _, rev := range list {
		if id := reviewStr(rev, "id"); id != "" {
			out[id] = true
		}
	}
	return out, nil
}
