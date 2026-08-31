package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func calendarServer(t *testing.T, handler http.HandlerFunc) (*Server, *deniedTransport) {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	parsed, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := &deniedTransport{
		allow: map[string]bool{parsed.Host: true}, base: http.DefaultTransport,
	}
	cfg := loadConfig()
	cfg.EventsURL = upstream.URL
	cfg.CalendarTTL = time.Hour
	// A configured provider key must not change the read path or enable a second host.
	cfg.AlphaVantageKey = "must-not-fetch"
	return &Server{
		cfg: cfg, cache: newCache(), http: &http.Client{Transport: transport, Timeout: time.Second},
	}, transport
}

func TestCalendarReadsTheCanonicalStoreAndKeepsCompatibilityAliases(t *testing.T) {
	var gotQuery url.Values
	srv, transport := calendarServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/calendar" {
			http.NotFound(w, r)
			return
		}
		gotQuery = r.URL.Query()
		writeJSON(w, http.StatusOK, map[string]any{
			"asOf": "2026-08-22T10:00:00Z", "degraded": []string{},
			"scheduled": []map[string]any{{
				"id": "sch_0123456789abcdef", "occurrenceKey": "macro|CPI|2026-08",
				"kind": "macro_release", "series": "CPI",
				"title":       "Consumer Price Index (CPI)",
				"description": "Inflation updates the policy backdrop.",
				"scheduledAt": "2026-09-10T12:30:00Z", "timezone": "America/New_York",
				"localTime": "08:30", "status": "confirmed", "confirmed": true,
				"importance": "high", "source": "fred", "sourceTier": "official",
				"sourceUrl": "https://fred.test/cpi", "expected": 3.1, "unit": "%",
				"revisions": []map[string]any{{
					"id": "seh_0123456789abcdef", "observedAt": "2026-08-20T00:00:00Z",
					"changeType": "rescheduled", "priorScheduledAt": "2026-09-09T12:30:00Z",
					"scheduledAt": "2026-09-10T12:30:00Z", "priorStatus": "confirmed",
					"status": "confirmed", "source": "fred", "sourceTier": "official",
				}},
			}},
		})
	})

	calendar := srv.calendarFor(context.Background(), "2026-09-01", "2026-09-30")
	if calendar.Source != "store" || len(calendar.Events) != 1 || len(calendar.Degraded) != 0 {
		t.Fatalf("unexpected calendar envelope: %#v", calendar)
	}
	event := calendar.Events[0]
	if event.Title != "Consumer Price Index (CPI)" || event.Name != event.Title {
		t.Fatalf("canonical/compatibility title mismatch: %#v", event)
	}
	if event.OccurrenceKey != "macro|CPI|2026-08" || len(event.Revisions) != 1 ||
		event.Revisions[0].ChangeType != "rescheduled" {
		t.Fatalf("occurrence history was not forwarded: %#v", event)
	}
	if event.Date != "2026-09-10" || event.TimeET != "08:30" || event.Estimate == nil || *event.Estimate != 3.1 {
		t.Fatalf("legacy aliases were not mapped from the store: %#v", event)
	}
	if gotQuery.Get("from") != "2026-09-01T00:00:00Z" || gotQuery.Get("to") != "2026-09-30T23:59:59Z" {
		t.Fatalf("wrong store window: %v", gotQuery)
	}
	attempts := transport.attempted()
	if len(attempts) != 1 {
		t.Fatalf("calendar read attempted hosts %v; want only the canonical store", attempts)
	}
}

func TestCalendarUnavailableIsEmptyAndNeverFallsBackToSeed(t *testing.T) {
	cfg := loadConfig()
	cfg.EventsURL = ""
	cfg.CalendarTTL = time.Hour
	srv := &Server{cfg: cfg, cache: newCache(), http: &http.Client{Timeout: time.Second}}

	calendar := srv.calendarFor(context.Background(), "2026-09-01", "2026-09-30")
	if calendar.Source != "store:unavailable" || len(calendar.Events) != 0 {
		t.Fatalf("unavailable store produced calendar facts: %#v", calendar)
	}
	if len(calendar.Degraded) != 1 || calendar.Degraded[0] != "events:unavailable" {
		t.Fatalf("missing honest degradation: %#v", calendar.Degraded)
	}
}

func TestNextEarningsIsAStoreProjection(t *testing.T) {
	srv, transport := calendarServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"asOf": "2026-08-22T10:00:00Z", "degraded": []string{},
			"scheduled": []map[string]any{{
				"id": "sch_abcdef0123456789", "kind": "earnings", "ticker": "NVDA",
				"title": "NVIDIA earnings", "scheduledAt": time.Now().UTC().AddDate(0, 1, 0).Format(time.RFC3339),
				"status": "tentative", "confirmed": false, "importance": "high",
				"source": "alphavantage", "sourceTier": "professional", "timezone": "UTC",
			}},
		})
	})

	date, source := srv.nextEarningsFor(context.Background(), "nvda")
	wantDate := time.Now().UTC().AddDate(0, 1, 0).Format("2006-01-02")
	if date != wantDate || source != "store:alphavantage" {
		t.Fatalf("next earnings = (%q, %q), want (%q, store:alphavantage)", date, source, wantDate)
	}
	if len(transport.attempted()) != 1 {
		t.Fatalf("next earnings reached more than the event store: %v", transport.attempted())
	}
}

func TestCalendarHandlerIncludesAsOfAndDegradation(t *testing.T) {
	srv, _ := calendarServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"scheduled": []any{}, "asOf": "2026-08-22T10:00:00Z",
			"degraded": []string{"fred:no-key"},
		})
	})
	req := httptest.NewRequest(http.MethodGet, "/api/calendar?from=2026-09-01&to=2026-09-30", nil)
	rec := httptest.NewRecorder()
	srv.handleCalendar(rec, req)

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["asOf"] != "2026-08-22T10:00:00Z" || body["source"] != "store" {
		t.Fatalf("handler dropped canonical metadata: %#v", body)
	}
	degraded, _ := body["degraded"].([]any)
	if len(degraded) != 1 || degraded[0] != "fred:no-key" {
		t.Fatalf("handler dropped degradation: %#v", body)
	}
}

// ── Phase 2C — coverage on the calendar read ────────────────────────────────────────────────────

func TestCalendarCarriesIRCoverageAndStillCallsNoProvider(t *testing.T) {
	var seen []string
	srv, transport := calendarServer(t, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		switch r.URL.Path {
		case "/calendar":
			writeJSON(w, http.StatusOK, map[string]any{
				"asOf": "2026-08-23T10:00:00Z", "degraded": []string{}, "scheduled": []any{},
			})
		case "/ir/coverage":
			writeJSON(w, http.StatusOK, map[string]any{
				"covered": []map[string]any{{
					"ticker": "NVDA", "company": "NVIDIA Corporation",
					"sourceLabel": "NVIDIA Investor Relations", "feedKind": "rss",
					"homeUrl":    "https://investor.nvidia.com/events-and-presentations/events/",
					"eventKinds": []string{"earnings"},
				}},
				"missing":        []string{"GOOGL", "TSLA"},
				"registrySource": "builtin",
			})
		default:
			http.NotFound(w, r)
		}
	})

	req := httptest.NewRequest(http.MethodGet, "/api/calendar", nil)
	rec := httptest.NewRecorder()
	srv.handleCalendar(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}

	var body struct {
		IRCoverage IRCoverage `json:"irCoverage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.IRCoverage.Covered) != 1 || body.IRCoverage.Covered[0].Ticker != "NVDA" {
		t.Fatalf("covered = %+v", body.IRCoverage.Covered)
	}
	// The point of the block: the gap is NAMED, so the UI can say "no official confirmation" for
	// these companies instead of showing an aggregator estimate that looks the same as a
	// confirmation.
	if len(body.IRCoverage.Missing) != 2 {
		t.Fatalf("missing = %+v, want the two uncovered companies", body.IRCoverage.Missing)
	}
	if len(body.IRCoverage.Degraded) != 0 {
		t.Fatalf("a healthy coverage read must not be degraded: %+v", body.IRCoverage.Degraded)
	}

	for _, host := range transport.attempted() {
		if host != "" && !transport.allow[host] {
			t.Fatalf("the calendar read reached a provider host: %s", host)
		}
	}
	if len(seen) == 0 {
		t.Fatalf("the store was never read")
	}
}

func TestUnknownIRCoverageIsNeverReportedAsFullyCovered(t *testing.T) {
	srv, _ := calendarServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/calendar" {
			writeJSON(w, http.StatusOK, map[string]any{
				"asOf": "2026-08-23T10:00:00Z", "degraded": []string{}, "scheduled": []any{},
			})
			return
		}
		http.Error(w, "down", http.StatusServiceUnavailable)
	})

	req := httptest.NewRequest(http.MethodGet, "/api/calendar", nil)
	rec := httptest.NewRecorder()
	srv.handleCalendar(rec, req)

	var body struct {
		IRCoverage IRCoverage `json:"irCoverage"`
	}
	json.Unmarshal(rec.Body.Bytes(), &body)
	if len(body.IRCoverage.Covered) != 0 {
		t.Fatalf("an unreadable registry must claim no coverage: %+v", body.IRCoverage.Covered)
	}
	if len(body.IRCoverage.Degraded) == 0 {
		t.Fatalf("the unknown must be stated, not implied")
	}
}
