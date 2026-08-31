package main

import "testing"

func TestNormalizedInvestigationAddOnsDefaultsDedupesAndRejectsUnknown(t *testing.T) {
	defaults, ok := normalizedInvestigationAddOns(nil)
	if !ok || len(defaults) != len(defaultInvestigationAddOns) {
		t.Fatalf("default add-ons = %#v, ok=%v", defaults, ok)
	}
	explicitEmpty, ok := normalizedInvestigationAddOns([]string{})
	if !ok || len(explicitEmpty) != 0 {
		t.Fatalf("explicit empty add-ons = %#v, ok=%v", explicitEmpty, ok)
	}

	got, ok := normalizedInvestigationAddOns([]string{" news_events ", "news_events", "my_thesis"})
	if !ok || len(got) != 2 || got[0] != "news_events" || got[1] != "my_thesis" {
		t.Fatalf("normalized add-ons = %#v, ok=%v", got, ok)
	}
	if _, ok := normalizedInvestigationAddOns([]string{"browse_everything"}); ok {
		t.Fatal("unknown add-on was accepted")
	}
}

func TestInvestigationSourceManifestKeepsLinksButNotPrivateBundle(t *testing.T) {
	bundle := map[string]any{
		"news_events": []map[string]any{{
			"headline": "A sourced headline", "url": "https://example.com/story",
			"source": "Example Wire", "publishedAt": "2026-08-14",
		}},
		"my_thesis": []map[string]any{{"text": "private claim"}},
	}

	manifest := investigationSourceManifest(bundle)
	if len(manifest) != 1 || manifest[0]["key"] != "news_events" {
		t.Fatalf("manifest = %#v", manifest)
	}
	items, ok := manifest[0]["items"].([]map[string]any)
	if !ok || len(items) != 1 || items[0]["url"] != "https://example.com/story" {
		t.Fatalf("manifest items = %#v", manifest[0]["items"])
	}
	if _, leaked := manifest[0]["my_thesis"]; leaked {
		t.Fatal("private thesis leaked into the source manifest")
	}
}

func TestTrimInvestigationThesesDropsStoredReviews(t *testing.T) {
	trimmed := trimInvestigationTheses([]map[string]any{{
		"id": "thesis-1", "text": "my claim", "lastCheck": map[string]any{"summary": "model output"},
	}})
	if len(trimmed) != 1 || trimmed[0]["text"] != "my claim" {
		t.Fatalf("trimmed thesis = %#v", trimmed)
	}
	if _, exists := trimmed[0]["lastCheck"]; exists {
		t.Fatal("prior model review was included in a fresh investigation source bundle")
	}
}
