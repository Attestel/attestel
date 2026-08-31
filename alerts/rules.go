package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// rules.go — the Rule / Event data model and validation.
//
// A Rule is a DESCRIPTIVE condition over the deterministic signals already computed by the
// analysis + gateway services. Rules never carry buy/sell intent and never place orders — an
// alert only ever says "this condition became true" (see notify.go / eval.go for the messages).

// Rule types. Each maps to an evaluator branch in eval.go.
const (
	TypePriceCross         = "price_cross"         // params: level(float), direction(above|below)
	TypePctMove            = "pct_move"            // params: pct(float), window(day)
	TypeRSIThreshold       = "rsi_threshold"       // params: level(float), direction(above|below)
	TypeMACDCross          = "macd_cross"          // params: direction(bullish|bearish)
	TypeTrendFlip          = "trend_flip"          // params: {}
	TypeConfluenceConflict = "confluence_conflict" // params: mode(appears|resolves)
	TypeVWAPCross          = "vwap_cross"          // params: direction(above|below)
	TypeNew8K              = "new_8k"              // params: {}
	TypeCalendarEvent      = "calendar_event"      // params: kind(earnings|macro), lead(int days)

	// TypeThesisReview is the ONE new rule type Step 08 adds (PARALLEL_CONTRACTS.md §4.1). It is a
	// SCHEDULE, not a market condition: it fires when a review comes due, so a free-text assumption
	// that no evaluator can test becomes an honest reminder instead of a price_cross-shaped guess.
	TypeThesisReview = "thesis_review" // params: { everyDays: int } — or a one-shot via reviewAt
)

// Monitoring intents (§4.1). "" keeps every rule that exists today untargeted and unchanged.
const (
	IntentInvalidation     = "invalidation"
	IntentCatalyst         = "catalyst"
	IntentAssumptionReview = "assumption_review"
	IntentThesisReview     = "thesis_review"
)

// Research actions carried on an event (§4.2). Always a RESEARCH verb — never a trade action.
const (
	ActionReviewEvidence = "review_evidence"
	ActionReviewThesis   = "review_thesis"
	ActionAnswerQuestion = "answer_question"
)

// Bearings an event may carry (§4.2). Set ONLY when deterministic; otherwise the field stays null.
const (
	BearingStrengthens = "strengthens"
	BearingWeakens     = "weakens"
	BearingUpdates     = "updates"
)

// Data states (§4.2). Stamped at evaluation so a synthetic-triggered event is unmistakable.
const (
	DataStateLive      = "live"
	DataStateSeed      = "seed"
	DataStateSynthetic = "synthetic"
)

var allowedTimeframes = map[string]bool{"1D": true, "1H": true, "15m": true, "5m": true}

// allowedIntents maps an intent to the thesis-item prefix it may name. An intent that names an item
// of the WRONG kind is rejected: D-10's auto-link permission keys off exactly this pairing, so a
// mislabelled rule would be a way to manufacture a link the user never asserted.
var allowedIntents = map[string]string{
	IntentInvalidation:     "inv_",
	IntentCatalyst:         "cat_",
	IntentAssumptionReview: "asm_",
	IntentThesisReview:     "", // watches the whole thesis, so it names no single item
}

// ResearchLink is the structured direct link to Research context (§4.5). It is a STRUCTURED OBJECT
// rather than a URL because D-19 is still open — the company is not addressable in the URL, so the
// client sets the active company from `ticker` and then applies `hash`.
//
// This is always computed SERVER-SIDE (see researchLinkFor). A client-supplied link is ignored: the
// hash has to be a value routes.js::parseHash resolves today, and a browser that could choose it
// could point a notification anywhere.
type ResearchLink struct {
	View     string  `json:"view"`
	Subview  string  `json:"subview"`
	Tab      *string `json:"tab"`
	Ticker   string  `json:"ticker"`
	ThesisID string  `json:"thesisId"`
	Hash     string  `json:"hash"`
}

// researchLinkFor builds the link for a rule/event. A thesis-linked rule lands on the thesis section;
// an untargeted rule keeps the monitoring list as its destination. Both hashes are literals that
// parseHash resolves today, and neither has a slash in the first segment (auth/google.go
// sanitizeReturnTo would silently drop it).
func researchLinkFor(ticker, thesisID string) *ResearchLink {
	if thesisID != "" {
		return &ResearchLink{
			View: "research", Subview: "thesis", Tab: nil,
			Ticker: ticker, ThesisID: thesisID, Hash: "#research/thesis",
		}
	}
	return &ResearchLink{
		View: "watchlist", Subview: "monitoring", Tab: nil,
		Ticker: ticker, ThesisID: "", Hash: "#watchlist/monitoring",
	}
}

// Rule is one user-defined alert condition. UserID scopes it to its owner: the monitor evaluates
// EVERY user's rules, but each user only sees (and can mutate) their own.
type Rule struct {
	ID            string         `json:"id"`
	UserID        string         `json:"userId"`
	Ticker        string         `json:"ticker"`
	Timeframe     string         `json:"timeframe"`
	Type          string         `json:"type"`
	Params        map[string]any `json:"params"`
	Active        bool           `json:"active"`
	CooldownSec   int            `json:"cooldownSec"`
	LastTriggered int64          `json:"lastTriggered"` // unix seconds, 0 = never
	LastState     any            `json:"lastState"`     // edge-triggering memory (bool/string/nil)
	CreatedAt     int64          `json:"createdAt"`     // unix seconds

	// ---- Step 08 additions (PARALLEL_CONTRACTS.md §4.1). ADDITIVE ONLY ----------------------------
	// Every field below is zero-valued on every rule that exists today, and a zero value means
	// exactly what it meant before Step 08: an untargeted rule with no thesis context. Nothing here
	// changes how the nine existing types are evaluated.

	ThesisID     string `json:"thesisId"`     // "" = untargeted (every pre-Step-08 rule)
	ThesisItemID string `json:"thesisItemId"` // asm_|cat_|inv_|rsk_ item this rule watches, or ""
	Intent       string `json:"intent"`       // "" | invalidation | catalyst | assumption_review | thesis_review
	ReviewAt     int64  `json:"reviewAt"`     // unix s — scheduled review rules only (one-shot)

	// DataState is stamped at EVALUATION time, not at creation: whether the numbers behind a firing
	// were real is a property of the moment it fired.
	DataState string `json:"dataState"`

	// ResearchLink is server-computed on create/update; a client-supplied value is discarded.
	ResearchLink *ResearchLink `json:"researchLink"`

	// UserCreatedFromItem records that a USER REQUEST named the thesis item this rule watches — the
	// precondition D-10 puts on auto-linking evidence ("the user already asserted the causal link").
	//
	// It is derived by the handlers from whether the caller's own create/patch body carried a
	// thesisItemId. It is never accepted from the wire and no system path ever sets it, so it cannot
	// be forged into a link the user did not make.
	UserCreatedFromItem bool `json:"userCreatedFromItem"`
}

// TargetsThesis reports whether the rule carries thesis context.
func (r Rule) TargetsThesis() bool { return r.ThesisID != "" }

// MayAutoLinkEvidence encodes D-10 exactly: an alert may auto-link evidence to a thesis ONLY when
// the rule names an inv_ or cat_ item AND the user themselves created it from that item. Every other
// case produces a SUGGESTION, which is transient UI state and is never written to evidence_links.json.
func (r Rule) MayAutoLinkEvidence() bool {
	if r.ThesisID == "" || !r.UserCreatedFromItem {
		return false
	}
	return strings.HasPrefix(r.ThesisItemID, "inv_") || strings.HasPrefix(r.ThesisItemID, "cat_")
}

// Event is an append-only record of a rule firing. UserID is copied from the rule so the events
// feed can be scoped to its owner.
type Event struct {
	ID        string `json:"id"`
	RuleID    string `json:"ruleId"`
	UserID    string `json:"userId"`
	Ticker    string `json:"ticker"`
	Timeframe string `json:"timeframe"`
	Type      string `json:"type"`
	Message   string `json:"message"` // neutral, descriptive — never advice
	TS        int64  `json:"ts"`      // unix seconds
	Read      bool   `json:"read"`    // derived from the read watermark on list

	// ---- Step 08 additions (§4.2). ADDITIVE ONLY --------------------------------------------------
	// Events already on disk decode with these zero-valued, and an old client ignores them.

	ThesisID     string `json:"thesisId,omitempty"`
	ThesisItemID string `json:"thesisItemId,omitempty"`
	Intent       string `json:"intent,omitempty"`

	// Bearing is set ONLY when it is deterministic — a named invalidation condition crossing its
	// threshold weakens the thesis; a named catalyst occurring as expected strengthens it. It is
	// nil in every other case and is NEVER inferred by a model in the evaluation path.
	Bearing *string `json:"bearing"`

	// DataState makes a synthetic-triggered event unmistakable wherever it is rendered (invariant #1).
	DataState string `json:"dataState,omitempty"`

	// DedupeKey exists so the change set (§4.4) never shows one firing twice. Duplicate SUPPRESSION
	// remains the existing LastTriggered/CooldownSec/LastState mechanism — this key does not replace it.
	DedupeKey string `json:"dedupeKey,omitempty"`

	ResearchLink *ResearchLink `json:"researchLink,omitempty"`

	// ResearchAction is always a research verb. There is no code path that can put a trade action here.
	ResearchAction string `json:"researchAction,omitempty"`

	// EvidenceSuggested marks the D-10 case where the system may offer to link this event as evidence
	// but has NOT done so. A suggestion is not a link: nothing is written until the user accepts.
	EvidenceSuggested bool `json:"evidenceSuggested,omitempty"`
}

// dedupeKeyFor returns sha256(ruleID + NUL + triggerBucket) where triggerBucket is the cooldown
// window index (§4.2). Two firings of the same rule inside one cooldown window share a key.
func dedupeKeyFor(ruleID string, cooldownSec int, ts int64) string {
	bucket := ts
	if cooldownSec > 0 {
		bucket = ts / int64(cooldownSec)
	}
	sum := sha256.Sum256([]byte(ruleID + "\x00" + strconv.FormatInt(bucket, 10)))
	return hex.EncodeToString(sum[:])
}

// researchActionFor picks the research verb for an event. Deliberately a small, total function over
// the intent enum: there is no input that yields anything but one of the three research verbs.
func researchActionFor(intent string) string {
	switch intent {
	case IntentInvalidation, IntentCatalyst:
		return ActionReviewEvidence
	case IntentAssumptionReview, IntentThesisReview:
		return ActionReviewThesis
	default:
		return ActionReviewEvidence
	}
}

// bearingFor returns the DETERMINISTIC bearing for a firing, or nil when none can be justified.
//
// Only two cases qualify, and both are ones the user themselves asserted by creating the rule from a
// named item: an invalidation condition that crossed its threshold weakens the thesis, and a catalyst
// that occurred as expected strengthens it. A scheduled review changes nothing about the world, so it
// carries no bearing at all — and neither does any rule the user did not tie to a specific item.
func bearingFor(r Rule) *string {
	if r.ThesisID == "" || !r.UserCreatedFromItem {
		return nil
	}
	switch r.Intent {
	case IntentInvalidation:
		if strings.HasPrefix(r.ThesisItemID, "inv_") {
			b := BearingWeakens
			return &b
		}
	case IntentCatalyst:
		if strings.HasPrefix(r.ThesisItemID, "cat_") {
			b := BearingStrengthens
			return &b
		}
	}
	return nil
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// normalizeTimeframe validates a timeframe, defaulting to 1D (backward compatible with analysis).
func normalizeTimeframe(tf string) string {
	if allowedTimeframes[tf] {
		return tf
	}
	// tolerate a couple of aliases the analysis service also accepts
	switch strings.ToLower(strings.TrimSpace(tf)) {
	case "1d", "day", "daily", "":
		return "1D"
	case "1h", "hour":
		return "1H"
	case "15min":
		return "15m"
	case "5min":
		return "5m"
	}
	return "1D"
}

// validateAndNormalize checks a submitted rule, fills defaults, and normalizes fields. It mutates
// the rule in place and returns an error describing the first problem found.
func validateAndNormalize(r *Rule) error {
	r.Ticker = strings.ToUpper(strings.TrimSpace(r.Ticker))
	if r.Ticker == "" {
		return fmt.Errorf("ticker is required")
	}
	r.Timeframe = normalizeTimeframe(r.Timeframe)
	if r.Params == nil {
		r.Params = map[string]any{}
	}
	if r.CooldownSec < 0 {
		return fmt.Errorf("cooldownSec must be >= 0")
	}
	if r.CooldownSec == 0 {
		r.CooldownSec = 3600 // sensible default: at most once/hour per rule
	}

	switch r.Type {
	case TypePriceCross, TypeRSIThreshold:
		if _, err := paramFloat(r.Params, "level"); err != nil {
			return err
		}
		if err := requireDirection(r.Params, "above", "below"); err != nil {
			return err
		}
	case TypePctMove:
		pct, err := paramFloat(r.Params, "pct")
		if err != nil {
			return err
		}
		if pct <= 0 {
			return fmt.Errorf("pct must be > 0")
		}
		window, _ := r.Params["window"].(string)
		if window == "" {
			r.Params["window"] = "day"
		} else if window != "day" {
			return fmt.Errorf("pct_move: only window=\"day\" is supported")
		}
	case TypeMACDCross:
		if err := requireDirection(r.Params, "bullish", "bearish"); err != nil {
			return err
		}
	case TypeVWAPCross:
		if err := requireDirection(r.Params, "above", "below"); err != nil {
			return err
		}
	case TypeConfluenceConflict:
		mode, _ := r.Params["mode"].(string)
		if mode != "appears" && mode != "resolves" {
			return fmt.Errorf("confluence_conflict: mode must be \"appears\" or \"resolves\"")
		}
	case TypeCalendarEvent:
		kind, _ := r.Params["kind"].(string)
		if kind != "earnings" && kind != "macro" {
			return fmt.Errorf("calendar_event: kind must be \"earnings\" or \"macro\"")
		}
		lead, err := paramFloat(r.Params, "lead")
		if err != nil {
			return err
		}
		if lead < 0 {
			return fmt.Errorf("lead must be >= 0")
		}
		r.Params["lead"] = lead // normalize to a number
	case TypeThesisReview:
		// A scheduled review: either a recurring cadence or a one-shot date. It reads NO market data,
		// which is the point — it is the honest representation of a condition nothing can evaluate.
		if r.ThesisID == "" {
			return fmt.Errorf("thesis_review: thesisId is required")
		}
		every, hasEvery := r.Params["everyDays"]
		if hasEvery {
			days, err := paramFloat(r.Params, "everyDays")
			if err != nil {
				return err
			}
			if days < 1 || days > 3650 {
				return fmt.Errorf("everyDays must be between 1 and 3650")
			}
			r.Params["everyDays"] = days // normalize to a number
		} else if r.ReviewAt <= 0 {
			return fmt.Errorf("thesis_review: either params.everyDays or reviewAt is required")
		}
		_ = every
	case TypeTrendFlip, TypeNew8K:
		// no params
	default:
		return fmt.Errorf("unknown rule type %q", r.Type)
	}

	return validateThesisLinkage(r)
}

// validateThesisLinkage checks the Step 08 additions (§4.1). Every rule that exists today has all of
// these empty and passes unchanged — that is the compatibility guarantee, enforced by test.
func validateThesisLinkage(r *Rule) error {
	r.ThesisID = strings.TrimSpace(r.ThesisID)
	r.ThesisItemID = strings.TrimSpace(r.ThesisItemID)
	r.Intent = strings.TrimSpace(r.Intent)

	if r.ThesisID != "" && !isThesisID(r.ThesisID) {
		return fmt.Errorf("thesisId must be 16 lowercase hex characters")
	}
	// Thesis context is the only thing that gives an item id or an intent meaning.
	if r.ThesisID == "" {
		if r.ThesisItemID != "" {
			return fmt.Errorf("thesisItemId requires thesisId")
		}
		if r.Intent != "" {
			return fmt.Errorf("intent requires thesisId")
		}
	}
	if r.ThesisItemID != "" && !isThesisItemID(r.ThesisItemID) {
		return fmt.Errorf("thesisItemId must start with asm_, cat_, inv_ or rsk_")
	}

	if r.Intent != "" {
		wantPrefix, ok := allowedIntents[r.Intent]
		if !ok {
			return fmt.Errorf("intent must be one of invalidation|catalyst|assumption_review|thesis_review")
		}
		// An intent that names an item of the wrong kind is refused: D-10's auto-link permission is
		// derived from this pairing, so allowing a mismatch would let a rule claim a causal link the
		// user never made.
		if wantPrefix != "" && r.ThesisItemID != "" && !strings.HasPrefix(r.ThesisItemID, wantPrefix) {
			return fmt.Errorf("intent %q requires a %s thesisItemId", r.Intent, strings.TrimSuffix(wantPrefix, "_"))
		}
		if r.Intent == IntentThesisReview && r.ThesisItemID != "" {
			return fmt.Errorf("intent thesis_review watches the whole thesis and takes no thesisItemId")
		}
	}

	// A scheduled review reminder is the ONLY thing that may carry reviewAt.
	if r.ReviewAt != 0 && r.Type != TypeThesisReview {
		return fmt.Errorf("reviewAt is only valid on a thesis_review rule")
	}
	if r.Type == TypeThesisReview && r.Intent == "" {
		r.Intent = IntentThesisReview // a review rule with no stated intent reviews the thesis
	}

	// The link is always ours to compute (§4.5) — never the client's to choose.
	r.ResearchLink = researchLinkFor(r.Ticker, r.ThesisID)
	return nil
}

// isThesisID matches the journal's bare-16-hex thesis id (PARALLEL_CONTRACTS.md §1.2). This is a
// SHAPE check, not an ownership check: alerts deliberately gains no journal client (§4.1). Ownership
// holds because a rule is always stored under the caller's own uid and every reader resolves the
// thesis with the caller's own cookie, so a foreign id resolves to nothing for everyone.
func isThesisID(s string) bool {
	if len(s) != 16 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func isThesisItemID(s string) bool {
	for _, p := range []string{"asm_", "cat_", "inv_", "rsk_"} {
		if strings.HasPrefix(s, p) && len(s) > len(p) {
			return true
		}
	}
	return false
}

// paramFloat reads a numeric param (JSON numbers decode as float64; also tolerates numeric strings).
func paramFloat(p map[string]any, key string) (float64, error) {
	v, ok := p[key]
	if !ok {
		return 0, fmt.Errorf("%s is required", key)
	}
	switch n := v.(type) {
	case float64:
		return n, nil
	case int:
		return float64(n), nil
	case string:
		var f float64
		if _, err := fmt.Sscanf(strings.TrimSpace(n), "%g", &f); err == nil {
			return f, nil
		}
	}
	return 0, fmt.Errorf("%s must be a number", key)
}

func requireDirection(p map[string]any, allowed ...string) error {
	d, _ := p["direction"].(string)
	for _, a := range allowed {
		if d == a {
			return nil
		}
	}
	return fmt.Errorf("direction must be one of %s", strings.Join(allowed, "|"))
}
