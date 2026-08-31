package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// rules_test.go — validation and the D-10 auto-link precondition (PARALLEL_CONTRACTS.md §4.1/§4.2).

// TestLegacyRulesValidateUnchanged is the compatibility guarantee in test form: every rule shape that
// existed before Step 08 still validates, and none of them acquires thesis context by accident.
func TestLegacyRulesValidateUnchanged(t *testing.T) {
	legacy := []Rule{
		{Ticker: "NVDA", Type: TypePriceCross, Params: map[string]any{"level": 130.0, "direction": "above"}},
		{Ticker: "NVDA", Type: TypeRSIThreshold, Params: map[string]any{"level": 70.0, "direction": "above"}},
		{Ticker: "NVDA", Type: TypePctMove, Params: map[string]any{"pct": 5.0}},
		{Ticker: "NVDA", Type: TypeMACDCross, Params: map[string]any{"direction": "bullish"}},
		{Ticker: "NVDA", Type: TypeTrendFlip, Params: map[string]any{}},
		{Ticker: "NVDA", Type: TypeConfluenceConflict, Params: map[string]any{"mode": "appears"}},
		{Ticker: "NVDA", Type: TypeVWAPCross, Params: map[string]any{"direction": "above"}},
		{Ticker: "NVDA", Type: TypeNew8K, Params: map[string]any{}},
		{Ticker: "NVDA", Type: TypeCalendarEvent, Params: map[string]any{"kind": "earnings", "lead": 3.0}},
	}
	for _, r := range legacy {
		rule := r
		if err := validateAndNormalize(&rule); err != nil {
			t.Fatalf("%s: legacy rule must still validate, got %v", r.Type, err)
		}
		if rule.ThesisID != "" || rule.Intent != "" || rule.ThesisItemID != "" {
			t.Errorf("%s: a legacy rule must stay untargeted, got thesisId=%q intent=%q item=%q",
				r.Type, rule.ThesisID, rule.Intent, rule.ThesisItemID)
		}
		if rule.MayAutoLinkEvidence() {
			t.Errorf("%s: a legacy rule must never be allowed to auto-link evidence", r.Type)
		}
	}
}

// TestLegacyRuleJSONRoundTrips proves a rules.json written before Step 08 still loads: the new
// fields decode to their zero values and the rule keeps working.
func TestLegacyRuleJSONRoundTrips(t *testing.T) {
	const onDisk = `{"id":"abc123","userId":"u1","ticker":"NVDA","timeframe":"1D",
		"type":"price_cross","params":{"level":130,"direction":"above"},
		"active":true,"cooldownSec":3600,"lastTriggered":0,"lastState":null,"createdAt":1700000000}`
	var r Rule
	if err := json.Unmarshal([]byte(onDisk), &r); err != nil {
		t.Fatalf("a pre-Step-08 rule must still decode: %v", err)
	}
	if r.ID != "abc123" || r.Type != TypePriceCross || !r.Active {
		t.Fatalf("legacy fields lost on decode: %+v", r)
	}
	if r.ThesisID != "" || r.Intent != "" || r.ReviewAt != 0 || r.ResearchLink != nil {
		t.Errorf("new fields must be zero-valued on a legacy rule, got %+v", r)
	}
	if r.UserCreatedFromItem {
		t.Error("a legacy rule must not read as user-created-from-item")
	}
}

// TestLegacyEventJSONRoundTrips is the same guarantee for events already in events.jsonl.
func TestLegacyEventJSONRoundTrips(t *testing.T) {
	const onDisk = `{"id":"e1","ruleId":"r1","userId":"u1","ticker":"NVDA","timeframe":"1D",
		"type":"price_cross","message":"NVDA price crossed above 130.00","ts":1700000000,"read":false}`
	var ev Event
	if err := json.Unmarshal([]byte(onDisk), &ev); err != nil {
		t.Fatalf("a pre-Step-08 event must still decode: %v", err)
	}
	if ev.Message == "" || ev.TS == 0 {
		t.Fatal("legacy event fields lost on decode")
	}
	if ev.Bearing != nil || ev.ThesisID != "" || ev.ResearchAction != "" {
		t.Errorf("new event fields must be absent on a legacy event, got %+v", ev)
	}
}

func TestThesisReviewRuleValidation(t *testing.T) {
	const tid = "9f2c1b7a4e0d5a83"

	t.Run("recurring cadence", func(t *testing.T) {
		r := Rule{Ticker: "NVDA", Type: TypeThesisReview, ThesisID: tid,
			Params: map[string]any{"everyDays": 30.0}}
		if err := validateAndNormalize(&r); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if r.Intent != IntentThesisReview {
			t.Errorf("a review rule with no stated intent must default to thesis_review, got %q", r.Intent)
		}
	})

	t.Run("one-shot date", func(t *testing.T) {
		r := Rule{Ticker: "NVDA", Type: TypeThesisReview, ThesisID: tid, ReviewAt: 1800000000,
			Params: map[string]any{}}
		if err := validateAndNormalize(&r); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("needs a thesis", func(t *testing.T) {
		r := Rule{Ticker: "NVDA", Type: TypeThesisReview, Params: map[string]any{"everyDays": 30.0}}
		if err := validateAndNormalize(&r); err == nil {
			t.Fatal("a review rule with no thesis must be refused")
		}
	})

	t.Run("needs a schedule", func(t *testing.T) {
		r := Rule{Ticker: "NVDA", Type: TypeThesisReview, ThesisID: tid, Params: map[string]any{}}
		if err := validateAndNormalize(&r); err == nil {
			t.Fatal("a review rule with neither everyDays nor reviewAt must be refused")
		}
	})

	t.Run("reviewAt is only for review rules", func(t *testing.T) {
		r := Rule{Ticker: "NVDA", Type: TypePriceCross, ThesisID: tid, ReviewAt: 1800000000,
			Params: map[string]any{"level": 130.0, "direction": "above"}}
		if err := validateAndNormalize(&r); err == nil {
			t.Fatal("reviewAt on a price rule must be refused")
		}
	})
}

// TestIntentMustMatchItemKind is the guard that keeps D-10 honest: the auto-link permission is
// derived from (intent, item prefix), so a mismatched pair must never be storable.
func TestIntentMustMatchItemKind(t *testing.T) {
	const tid = "9f2c1b7a4e0d5a83"
	cases := []struct {
		name, intent, item string
		wantErr            bool
	}{
		{"invalidation with inv_", IntentInvalidation, "inv_1111111111111111", false},
		{"invalidation with asm_", IntentInvalidation, "asm_1111111111111111", true},
		{"catalyst with cat_", IntentCatalyst, "cat_1111111111111111", false},
		{"catalyst with inv_", IntentCatalyst, "inv_1111111111111111", true},
		{"assumption_review with asm_", IntentAssumptionReview, "asm_1111111111111111", false},
		{"thesis_review takes no item", IntentThesisReview, "asm_1111111111111111", true},
		{"unknown intent", "please_link_this", "inv_1111111111111111", true},
		{"bad item prefix", IntentInvalidation, "xxx_1111111111111111", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Rule{Ticker: "NVDA", Type: TypePriceCross, ThesisID: tid,
				Intent: c.intent, ThesisItemID: c.item,
				Params: map[string]any{"level": 130.0, "direction": "above"}}
			err := validateAndNormalize(&r)
			if c.wantErr && err == nil {
				t.Fatalf("expected %s/%s to be refused", c.intent, c.item)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("expected %s/%s to be accepted, got %v", c.intent, c.item, err)
			}
		})
	}
}

func TestThesisContextRequiresThesisID(t *testing.T) {
	for _, c := range []struct{ name, item, intent string }{
		{"item without thesis", "inv_1111111111111111", ""},
		{"intent without thesis", "", IntentInvalidation},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := Rule{Ticker: "NVDA", Type: TypePriceCross, ThesisItemID: c.item, Intent: c.intent,
				Params: map[string]any{"level": 130.0, "direction": "above"}}
			if err := validateAndNormalize(&r); err == nil {
				t.Fatal("thesis context without a thesisId must be refused")
			}
		})
	}
}

func TestMalformedThesisIDRefused(t *testing.T) {
	for _, bad := range []string{"nope", "9F2C1B7A4E0D5A83", "9f2c1b7a4e0d5a8", "../../etc/passwd"} {
		r := Rule{Ticker: "NVDA", Type: TypePriceCross, ThesisID: bad,
			Params: map[string]any{"level": 130.0, "direction": "above"}}
		if err := validateAndNormalize(&r); err == nil {
			t.Errorf("thesisId %q must be refused", bad)
		}
	}
}

// TestAutoLinkPermission is D-10 stated as a table. Only an inv_/cat_ item the USER named may ever
// auto-link; everything else is a suggestion the user has to accept.
func TestAutoLinkPermission(t *testing.T) {
	const tid = "9f2c1b7a4e0d5a83"
	cases := []struct {
		name     string
		rule     Rule
		wantLink bool
	}{
		{"user-named invalidation condition",
			Rule{ThesisID: tid, ThesisItemID: "inv_1111111111111111", UserCreatedFromItem: true}, true},
		{"user-named catalyst",
			Rule{ThesisID: tid, ThesisItemID: "cat_1111111111111111", UserCreatedFromItem: true}, true},
		{"user-named assumption is NOT an auto-link case",
			Rule{ThesisID: tid, ThesisItemID: "asm_1111111111111111", UserCreatedFromItem: true}, false},
		{"user-named risk is NOT an auto-link case",
			Rule{ThesisID: tid, ThesisItemID: "rsk_1111111111111111", UserCreatedFromItem: true}, false},
		{"invalidation the user did not create from",
			Rule{ThesisID: tid, ThesisItemID: "inv_1111111111111111", UserCreatedFromItem: false}, false},
		{"thesis-linked but no item", Rule{ThesisID: tid, UserCreatedFromItem: true}, false},
		{"untargeted rule", Rule{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.rule.MayAutoLinkEvidence(); got != c.wantLink {
				t.Errorf("MayAutoLinkEvidence() = %v, want %v", got, c.wantLink)
			}
		})
	}
}

// TestBearingIsOnlySetWhenDeterministic — §4.2. A bearing is an assertion about the user's thesis,
// so it exists only in the two cases the user themselves defined.
func TestBearingIsOnlySetWhenDeterministic(t *testing.T) {
	const tid = "9f2c1b7a4e0d5a83"

	weakens := Rule{ThesisID: tid, Intent: IntentInvalidation,
		ThesisItemID: "inv_1111111111111111", UserCreatedFromItem: true}
	if b := bearingFor(weakens); b == nil || *b != BearingWeakens {
		t.Errorf("a fired invalidation condition must weaken the thesis, got %v", b)
	}

	strengthens := Rule{ThesisID: tid, Intent: IntentCatalyst,
		ThesisItemID: "cat_1111111111111111", UserCreatedFromItem: true}
	if b := bearingFor(strengthens); b == nil || *b != BearingStrengthens {
		t.Errorf("a catalyst occurring as expected must strengthen the thesis, got %v", b)
	}

	for _, c := range []struct {
		name string
		rule Rule
	}{
		{"untargeted rule", Rule{}},
		{"no intent", Rule{ThesisID: tid, ThesisItemID: "inv_1111111111111111", UserCreatedFromItem: true}},
		{"assumption review", Rule{ThesisID: tid, Intent: IntentAssumptionReview,
			ThesisItemID: "asm_1111111111111111", UserCreatedFromItem: true}},
		{"scheduled thesis review", Rule{ThesisID: tid, Intent: IntentThesisReview, UserCreatedFromItem: true}},
		{"not created from the item", Rule{ThesisID: tid, Intent: IntentInvalidation,
			ThesisItemID: "inv_1111111111111111", UserCreatedFromItem: false}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if b := bearingFor(c.rule); b != nil {
				t.Errorf("bearing must stay null here, got %q", *b)
			}
		})
	}
}

// TestResearchActionIsAlwaysAResearchVerb — §4.2. There is no input that yields a trade action.
func TestResearchActionIsAlwaysAResearchVerb(t *testing.T) {
	allowed := map[string]bool{
		ActionReviewEvidence: true, ActionReviewThesis: true, ActionAnswerQuestion: true,
	}
	for _, intent := range []string{
		"", IntentInvalidation, IntentCatalyst, IntentAssumptionReview, IntentThesisReview,
		"buy", "sell", "garbage",
	} {
		got := researchActionFor(intent)
		if !allowed[got] {
			t.Errorf("researchActionFor(%q) = %q, which is not a research verb", intent, got)
		}
		for _, banned := range []string{"buy", "sell", "hold", "trade", "order", "exit"} {
			if strings.Contains(got, banned) {
				t.Errorf("researchActionFor(%q) = %q contains the trade verb %q", intent, got, banned)
			}
		}
	}
}

// TestResearchLinkIsParseable — §4.5. The hash must be a value routes.js::parseHash resolves today:
// no query string, and no slash in the first segment (auth/google.go sanitizeReturnTo would drop it).
func TestResearchLinkIsParseable(t *testing.T) {
	cases := []struct{ ticker, thesisID, wantHash string }{
		{"NVDA", "9f2c1b7a4e0d5a83", "#research/thesis"},
		{"NVDA", "", "#watchlist/monitoring"},
	}
	for _, c := range cases {
		link := researchLinkFor(c.ticker, c.thesisID)
		if link.Hash != c.wantHash {
			t.Errorf("hash = %q, want %q", link.Hash, c.wantHash)
		}
		if link.Ticker != c.ticker {
			t.Errorf("the link must carry the company so the client can set it: %+v", link)
		}
		if strings.ContainsAny(link.Hash, "?&=") {
			t.Errorf("hash %q must not carry a query string — parseHash would not resolve it", link.Hash)
		}
		first := strings.TrimPrefix(link.Hash, "#")
		if i := strings.Index(first, "/"); i >= 0 {
			first = first[:i]
		}
		for _, ch := range first {
			if (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') && ch != '-' {
				t.Errorf("first hash segment %q breaks sanitizeReturnTo's ^[a-z][a-z0-9-]{0,30}$", first)
			}
		}
	}
}

// TestDedupeKeyBuckets — §4.2. Two firings inside one cooldown window share a key; a later window
// does not. This is what stops the change set showing one firing twice.
func TestDedupeKeyBuckets(t *testing.T) {
	const rule, cooldown = "r1", 3600
	a := dedupeKeyFor(rule, cooldown, 1_000_000_000)
	b := dedupeKeyFor(rule, cooldown, 1_000_000_000+60) // same hour
	c := dedupeKeyFor(rule, cooldown, 1_000_000_000+7200)
	if a != b {
		t.Error("two firings inside one cooldown window must share a dedupe key")
	}
	if a == c {
		t.Error("firings in different cooldown windows must have different dedupe keys")
	}
	if dedupeKeyFor("r2", cooldown, 1_000_000_000) == a {
		t.Error("different rules must never share a dedupe key")
	}
}
