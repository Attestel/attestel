package main

import (
	"testing"
	"time"
)

// §9.32 as the INTEGRATOR verified it: Explore renders ITEMS with enrichment off.
//
// TestExploreSectionsAreAlwaysAllSix builds its events fake with an EMPTY list, so it asserts the
// six section keys against emptiness. That is a real check of the response shape, but it passes
// whether or not anything renders — and "all six sections empty" is the exact outcome §9.32 exists
// to prevent. This populates the fake and asserts the items.
//
// It matters because §9.32's LITERAL mechanism cannot work. The clause says possibleReadThrough
// falls back to eventType plus the leading affectedDimensions entry — but affectedDimensions is
// itself enrichment-gated in services/events/app/events.py: _link_tickers writes '[]' at ingest,
// _ticker_json emits the field only inside `if enriched:`, and the enrichment UPDATE is its only
// writer. With EVENT_ENRICH_ENABLED=false — the shipped default — both of the clause's inputs are
// absent. Lane 2C's three-stage composer supplies a third stage derived from the closed §3.4 event
// type enum; this test is what proves that stage is load-bearing.
func TestExploreRendersItemsWithEnrichmentOff(t *testing.T) {
	now := time.Now().UTC()
	at := func(hoursAgo int) string {
		return now.Add(-time.Duration(hoursAgo) * time.Hour).Format(time.RFC3339)
	}

	// Every ticker link is rawTicker: relevance and isPrimary only. No transmission, no
	// affectedDimensions — precisely what :8004 emits with enrichment off.
	raw := []map[string]any{
		fixtureEvent("evt_x1", "earnings_result", at(3), 0.9, 0.8, "official", 5,
			[]map[string]any{rawTicker("TSM", 1.0, true), rawTicker("NVDA", 0.5, false)}),
		fixtureEvent("evt_x2", "macro_release", at(6), 0.8, 0.4, "official", 3,
			[]map[string]any{}),
		fixtureEvent("evt_x3", "product_launch", at(20), 0.6, 0.9, "professional", 2,
			[]map[string]any{rawTicker("AMD", 1.0, true)}),
		fixtureEvent("evt_x4", "supply_chain", at(30), 0.7, 0.7, "professional", 4,
			[]map[string]any{rawTicker("TSM", 1.0, true)}),
	}

	journal := journalFake(t, map[string][]string{"u1": {"NVDA"}})
	ev := newEventsFake(t, raw)
	f := newFeedsFixture(t, ev.URL(), journal.URL)

	_, body := f.get(t, "/api/explore", "u1")
	sections, ok := body["sections"].(map[string]any)
	if !ok {
		t.Fatalf("no sections object: %v", body)
	}

	total := 0
	for key, v := range sections {
		arr, _ := v.([]any)
		total += len(arr)
		for _, entry := range arr {
			it, _ := entry.(map[string]any)
			id, _ := it["id"].(string)
			for _, field := range []string{"whyYouAreSeeingThis", "possibleReadThrough"} {
				if s, _ := it[field].(string); s == "" {
					t.Fatalf("section %s item %s has an empty %s with enrichment off — §9.32 forbids exactly this",
						key, id, field)
				}
			}
			// Nothing is enriched, so every item must have landed on the third stage.
			if basis, _ := it["readThroughBasis"].(string); basis != "eventType" {
				t.Fatalf("section %s item %s readThroughBasis = %q, want %q",
					key, id, basis, "eventType")
			}
		}
	}

	if total == 0 {
		t.Fatal("every section is empty with enrichment off — the §9.32 failure, and a " +
			"six-keys-present check passes right here")
	}
}
