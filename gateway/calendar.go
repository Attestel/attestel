package main

// calendar.go — the read side of Catalyst Calendar Intelligence.
//
// Providers are ingestion concerns owned by services/events. A browser read reaches only the
// Attestel-owned canonical store through eventsGet; it never calls FRED, FMP or Alpha Vantage and it
// never manufactures a seeded date. An unavailable store produces an honest empty/degraded
// envelope so the rest of the cockpit remains usable without turning approximation into fact.

import (
	"context"
	"encoding/json"
	"net/url"
	"sort"
	"strings"
	"time"
)

// CalEvent is additive over the legacy economic-calendar shape. date/name/timeET/estimate remain
// for older panels while scheduledAt/title/timezone/status/sourceTier are the canonical contract.
type CalEvent struct {
	ID            string             `json:"id"`
	OccurrenceKey string             `json:"occurrenceKey,omitempty"`
	Date          string             `json:"date"`
	TimeET        string             `json:"timeET,omitempty"`
	ScheduledAt   string             `json:"scheduledAt"`
	Timezone      string             `json:"timezone,omitempty"`
	LocalTime     string             `json:"localTime,omitempty"`
	Name          string             `json:"name"`
	Title         string             `json:"title"`
	Description   string             `json:"description,omitempty"`
	Importance    string             `json:"importance"`
	Status        string             `json:"status"`
	Confirmed     bool               `json:"confirmed"`
	Source        string             `json:"source"`
	SourceTier    string             `json:"sourceTier"`
	SourceURL     string             `json:"sourceUrl,omitempty"`
	FirstSeenAt   string             `json:"firstSeenAt,omitempty"`
	UpdatedAt     string             `json:"updatedAt,omitempty"`
	Kind          string             `json:"kind"`
	Ticker        string             `json:"ticker,omitempty"`
	Series        string             `json:"series,omitempty"`
	Previous      *float64           `json:"previous,omitempty"`
	Estimate      *float64           `json:"estimate,omitempty"`
	Actual        *float64           `json:"actual,omitempty"`
	Surprise      *float64           `json:"surprise,omitempty"`
	Unit          string             `json:"unit,omitempty"`
	Revisions     []CalendarRevision `json:"revisions"`
}

type CalendarRevision struct {
	ID               string `json:"id"`
	ObservedAt       string `json:"observedAt"`
	ChangeType       string `json:"changeType"`
	PriorScheduledAt string `json:"priorScheduledAt,omitempty"`
	ScheduledAt      string `json:"scheduledAt"`
	PriorStatus      string `json:"priorStatus,omitempty"`
	Status           string `json:"status"`
	Source           string `json:"source"`
	SourceTier       string `json:"sourceTier"`
}

type storedCalendarEvent struct {
	ID            string             `json:"id"`
	OccurrenceKey string             `json:"occurrenceKey"`
	Kind          string             `json:"kind"`
	Ticker        string             `json:"ticker"`
	Series        string             `json:"series"`
	Title         string             `json:"title"`
	Description   string             `json:"description"`
	ScheduledAt   string             `json:"scheduledAt"`
	Timezone      string             `json:"timezone"`
	LocalTime     string             `json:"localTime"`
	Status        string             `json:"status"`
	Confirmed     bool               `json:"confirmed"`
	Importance    string             `json:"importance"`
	Source        string             `json:"source"`
	SourceTier    string             `json:"sourceTier"`
	SourceURL     string             `json:"sourceUrl"`
	FirstSeenAt   string             `json:"firstSeenAt"`
	UpdatedAt     string             `json:"updatedAt"`
	Previous      *float64           `json:"previous"`
	Expected      *float64           `json:"expected"`
	Actual        *float64           `json:"actual"`
	Surprise      *float64           `json:"surprise"`
	Unit          string             `json:"unit"`
	Revisions     []CalendarRevision `json:"revisions"`
}

type calendarEnvelope struct {
	Events   []CalEvent `json:"events"`
	Source   string     `json:"source"`
	AsOf     string     `json:"asOf,omitempty"`
	Degraded []string   `json:"degraded"`
}

// normalizeCalWindow defaults an absent/invalid window to today..+30d and prevents an inverted
// range from reaching the store. Dates remain the public gateway input; the events service gets
// fixed-width UTC timestamps for its point-in-time comparisons.
func normalizeCalWindow(from, to string) (string, string) {
	today := time.Now().UTC()
	fromTime, fromErr := time.Parse("2006-01-02", from)
	if fromErr != nil || len(from) != 10 {
		fromTime = today
		from = fromTime.Format("2006-01-02")
	}
	toTime, toErr := time.Parse("2006-01-02", to)
	if toErr != nil || len(to) != 10 || toTime.Before(fromTime) {
		to = fromTime.AddDate(0, 0, 30).Format("2006-01-02")
	}
	return from, to
}

func calendarDate(instant string) string {
	if len(instant) >= 10 {
		return instant[:10]
	}
	return instant
}

func normalizeImportance(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "high":
		return "high"
	case "low":
		return "low"
	default:
		return "medium"
	}
}

func toCalEvent(item storedCalendarEvent) CalEvent {
	title := strings.TrimSpace(item.Title)
	if title == "" {
		if item.Ticker != "" && item.Kind == "earnings" {
			title = item.Ticker + " earnings"
		} else if item.Series != "" {
			title = item.Series
		} else {
			title = "Scheduled event"
		}
	}
	timeET := ""
	if item.Timezone == "America/New_York" {
		timeET = item.LocalTime
	}
	revisions := item.Revisions
	if revisions == nil {
		revisions = []CalendarRevision{}
	}
	return CalEvent{
		ID: item.ID, OccurrenceKey: item.OccurrenceKey,
		Date: calendarDate(item.ScheduledAt), TimeET: timeET,
		ScheduledAt: item.ScheduledAt, Timezone: item.Timezone, LocalTime: item.LocalTime,
		Name: title, Title: title, Description: item.Description,
		Importance: normalizeImportance(item.Importance), Status: item.Status,
		Confirmed: item.Confirmed, Source: item.Source, SourceTier: item.SourceTier,
		SourceURL: item.SourceURL, FirstSeenAt: item.FirstSeenAt, UpdatedAt: item.UpdatedAt,
		Kind: item.Kind, Ticker: item.Ticker, Series: item.Series,
		Previous: item.Previous, Estimate: item.Expected, Actual: item.Actual,
		Surprise: item.Surprise, Unit: item.Unit, Revisions: revisions,
	}
}

func sortCalEvents(events []CalEvent) {
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].ScheduledAt != events[j].ScheduledAt {
			return events[i].ScheduledAt < events[j].ScheduledAt
		}
		return events[i].Title < events[j].Title
	})
}

// calendarFor reads the canonical store and caches both success and degradation. There is no
// provider or seed fallback in this function by design.
func (s *Server) calendarFor(ctx context.Context, from, to string) calendarEnvelope {
	key := "calendar-store:" + from + ":" + to
	if b, ok := s.cache.get(key); ok {
		var cached calendarEnvelope
		if json.Unmarshal(b, &cached) == nil {
			if cached.Events == nil {
				cached.Events = []CalEvent{}
			}
			if cached.Degraded == nil {
				cached.Degraded = []string{}
			}
			return cached
		}
	}

	query := url.Values{}
	query.Set("from", from+"T00:00:00Z")
	query.Set("to", to+"T23:59:59Z")
	body, err := s.eventsGet(ctx, "/calendar?"+query.Encode())
	if err != nil {
		result := calendarEnvelope{
			Events: []CalEvent{}, Source: "store:unavailable",
			Degraded: []string{"events:unavailable"},
		}
		if encoded, marshalErr := json.Marshal(result); marshalErr == nil {
			s.cache.set(key, encoded, s.cfg.CalendarTTL)
		}
		return result
	}

	var upstream struct {
		Scheduled []storedCalendarEvent `json:"scheduled"`
		AsOf      string                `json:"asOf"`
		Degraded  []string              `json:"degraded"`
	}
	if err := json.Unmarshal(body, &upstream); err != nil {
		return calendarEnvelope{
			Events: []CalEvent{}, Source: "store:invalid",
			Degraded: []string{"events:invalid-response"},
		}
	}

	events := make([]CalEvent, 0, len(upstream.Scheduled))
	for _, item := range upstream.Scheduled {
		events = append(events, toCalEvent(item))
	}
	sortCalEvents(events)
	if upstream.Degraded == nil {
		upstream.Degraded = []string{}
	}
	result := calendarEnvelope{
		Events: events, Source: "store", AsOf: upstream.AsOf, Degraded: upstream.Degraded,
	}
	if encoded, err := json.Marshal(result); err == nil {
		s.cache.set(key, encoded, s.cfg.CalendarTTL)
	}
	return result
}

// nextEarningsFor is a projection over the same canonical store. It cannot make an Alpha Vantage
// call on a dashboard or alert read, and an absent row remains absent rather than becoming an
// extrapolated seed date.
func (s *Server) nextEarningsFor(ctx context.Context, ticker string) (string, string) {
	upper := strings.ToUpper(strings.TrimSpace(ticker))
	key := "nextearn-store:" + upper
	if b, ok := s.cache.get(key); ok {
		var cached struct{ Date, Source string }
		if json.Unmarshal(b, &cached) == nil {
			return cached.Date, cached.Source
		}
	}

	today := time.Now().UTC()
	from := today.Format("2006-01-02")
	to := today.AddDate(0, 0, 120).Format("2006-01-02")
	calendar := s.calendarFor(ctx, from, to)
	date, source := "", "none"
	for _, event := range calendar.Events {
		if event.Kind == "earnings" && strings.EqualFold(event.Ticker, upper) && event.Date >= from {
			date = event.Date
			source = "store"
			if event.Source != "" {
				source += ":" + event.Source
			}
			break
		}
	}
	if encoded, err := json.Marshal(struct{ Date, Source string }{date, source}); err == nil {
		s.cache.set(key, encoded, s.cfg.CalendarTTL)
	}
	return date, source
}

// ── Phase 2C: coverage is a fact the Calendar must be able to state ──────────────────────────────
//
// "This company confirmed the date" and "an aggregator estimated it" look identical on a screen
// unless the screen can say which. So the Calendar carries the coverage answer alongside the
// events: which of the configured companies have an official investor-relations source at all.
//
// It is a CONFIGURATION read (`services/events` `GET /ir/coverage` returns its registry), not a
// provider call and not a model call — the same store-only rule the rest of this file obeys — and
// it is cached beside the calendar window it accompanies.

// IRCoverage is the additive coverage block on the calendar envelope.
type IRCoverage struct {
	Covered  []IRCoveredCompany `json:"covered"`
	Missing  []string           `json:"missing"`
	Source   string             `json:"registrySource,omitempty"`
	Degraded []string           `json:"degraded"`
}

type IRCoveredCompany struct {
	Ticker      string   `json:"ticker"`
	Company     string   `json:"company"`
	SourceLabel string   `json:"sourceLabel"`
	FeedKind    string   `json:"feedKind"`
	HomeURL     string   `json:"homeUrl"`
	EventKinds  []string `json:"eventKinds"`
}

func emptyIRCoverage(reason string) IRCoverage {
	return IRCoverage{
		Covered: []IRCoveredCompany{}, Missing: []string{}, Degraded: []string{reason},
	}
}

// irCoverageFor answers for the configured ticker universe. An unreachable store degrades to
// "unknown", NOT to "everything is covered" — a coverage claim we cannot verify is the one answer
// that would be actively misleading here.
func (s *Server) irCoverageFor(ctx context.Context) IRCoverage {
	universe := make([]string, 0, len(s.cfg.Tickers))
	for _, t := range s.cfg.Tickers {
		if t.Symbol != "" {
			universe = append(universe, strings.ToUpper(t.Symbol))
		}
	}
	if len(universe) == 0 {
		return emptyIRCoverage("tickers:unconfigured")
	}
	key := "ir-coverage:" + strings.Join(universe, ",")
	if b, ok := s.cache.get(key); ok {
		var cached IRCoverage
		if json.Unmarshal(b, &cached) == nil {
			if cached.Covered == nil {
				cached.Covered = []IRCoveredCompany{}
			}
			if cached.Missing == nil {
				cached.Missing = []string{}
			}
			if cached.Degraded == nil {
				cached.Degraded = []string{}
			}
			return cached
		}
	}

	query := url.Values{}
	query.Set("tickers", strings.Join(universe, ","))
	body, err := s.eventsGet(ctx, "/ir/coverage?"+query.Encode())
	if err != nil {
		return emptyIRCoverage("events:unavailable")
	}

	var upstream struct {
		Covered []IRCoveredCompany `json:"covered"`
		Missing []string           `json:"missing"`
		Source  string             `json:"registrySource"`
	}
	if json.Unmarshal(body, &upstream) != nil {
		return emptyIRCoverage("events:invalid-response")
	}
	result := IRCoverage{
		Covered: upstream.Covered, Missing: upstream.Missing,
		Source: upstream.Source, Degraded: []string{},
	}
	if result.Covered == nil {
		result.Covered = []IRCoveredCompany{}
	}
	if result.Missing == nil {
		result.Missing = []string{}
	}
	if encoded, marshalErr := json.Marshal(result); marshalErr == nil {
		s.cache.set(key, encoded, s.cfg.CalendarTTL)
	}
	return result
}
