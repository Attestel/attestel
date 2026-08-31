package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// thesis_monitor_test.go — Wave 5 Lane 5B.
//
// The four assertions that matter are the contract ones, and three of them are NEGATIVE:
// the monitor never calls the llm service (§9.23), it reads at the last stored bar and never at
// `now` (§9.24), queue leasing is durable/idempotent, and it does not fire twice for a condition
// that is still true (doc §12.9 — change, not volume).
//
// NO TEST HERE TOUCHES THE NETWORK: every upstream is an `httptest` server owned by this file.

// ── fakes ────────────────────────────────────────────────────────────────────────────────────────

type monitorFakes struct {
	analysis *httptest.Server
	journal  *httptest.Server
	// hits records every path either fake was asked for, so a NEGATIVE can be asserted as a
	// measurement rather than as the absence of a log line (§9.44).
	hits []string
}

func candles(closePx float64, ts int64, synthetic bool) map[string]any {
	return map[string]any{
		"bars":              []any{map[string]any{"time": float64(ts), "close": closePx}},
		"sourceIsSynthetic": synthetic,
		"source":            "yfinance",
	}
}

func TestLastStoredBarReadsTheAnalysisBarsField(t *testing.T) {
	m, _ := newMonitor(t, candles(123.45, 1_780_000_000, false), nil)
	point, ok := m.lastStoredBar(context.Background(), "NVDA")
	if !ok || point.close != 123.45 || point.ts != 1_780_000_000 {
		t.Fatalf("lastStoredBar = %+v, %v", point, ok)
	}
}

func newMonitor(t *testing.T, bars map[string]any, theses []any) (*ThesisMonitor, *monitorFakes) {
	t.Helper()
	f := &monitorFakes{}

	f.analysis = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits = append(f.hits, r.URL.String())
		writeJSON(w, http.StatusOK, bars)
	}))
	t.Cleanup(f.analysis.Close)

	f.journal = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits = append(f.hits, r.URL.Path)
		writeJSON(w, http.StatusOK, map[string]any{"theses": theses})
	}))
	t.Cleanup(f.journal.Close)

	dir := t.TempDir()
	cfg := loadConfig()
	cfg.RulesDir = dir
	cfg.AnalysisURL = f.analysis.URL
	cfg.JournalURL = f.journal.URL
	cfg.MonitorEnabled = true
	cfg.WebhookURL, cfg.SMTPHost = "", "" // notifications are inert; the in-app event still records

	store, err := openStore(dir)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	return newThesisMonitor(cfg, store, newNotifier(cfg)), f
}

func thesisRow(id, uid, ticker string, lastReview int64, due []int64, level string) map[string]any {
	row := map[string]any{
		"id": id, "userId": uid, "ticker": ticker, "status": "open",
		"createdAt": float64(1_700_000_000), "notificationLevel": level,
	}
	if lastReview > 0 {
		row["lastReview"] = map[string]any{"at": float64(lastReview)}
	}
	if due != nil {
		vals := []any{}
		for _, d := range due {
			vals = append(vals, float64(d))
		}
		row["catalystsDue"] = vals
	}
	return row
}

// ── §9.23: the queue, not the drainer, and never the llm ─────────────────────────────────────────

func TestMonitorNeverNamesTheLLM(t *testing.T) {
	// `alerts` is stdlib-only Go and cannot hold the Python `fcntl.flock` model lease (§9.21), so a
	// generation started here would run OUTSIDE the lease that exists to serialise generation.
	// Source-level, in the same style `eval_test.go` already uses for the evaluator.
	source, err := os.ReadFile("thesis_monitor.go")
	if err != nil {
		t.Fatal(err)
	}
	// COMMENT LINES ARE STRIPPED before the check, so the file is free to name the banned
	// identifiers in order to explain why it does not use them. A source-level gate that its own
	// documentation trips is a gate somebody deletes.
	code := []string{}
	for _, line := range strings.Split(string(source), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		code = append(code, line)
	}
	text := strings.Join(code, "\n")
	for _, banned := range []string{"LLM_URL", "LLMURL", "cfg.LLM", "/chat", "/analyst", "qwen",
		"ollama", "THESIS_RESYNTH_ENABLED"} {
		if strings.Contains(text, banned) {
			t.Fatalf("thesis_monitor.go names %q — Lane 5B ships the QUEUE, not the drainer (§9.23)",
				banned)
		}
	}
}

func TestTheMonitorEnqueuesLeasesAndCompletesIdempotently(t *testing.T) {
	m, _ := newMonitor(t, candles(100, 1_780_000_000, false), nil)
	marker := ThesisMarker{ThesisID: "th_1", UserID: "u1", Ticker: "NVDA", Stale: true,
		Severity: severityMaterial, LastFired: "abc", AsOf: 1_780_000_000,
		AsOfISO: "2026-06-01T00:00:00Z", ObservedPx: 107, BaselinePx: 100}
	m.putMarker(marker)
	if _, ok := m.enqueue(marker); !ok {
		t.Fatal("a stale marker must enqueue a re-synthesis job")
	}
	depth, dropped := m.QueueDepth()
	if depth != 1 || dropped != 0 {
		t.Fatalf("queue depth=%d dropped=%d, want 1/0", depth, dropped)
	}
	now := time.Unix(1_800_000_000, 0).UTC()
	job, ok := m.LeaseResynth(now)
	if !ok || job.State != resynthProcessing || job.Attempts != 1 || job.LeaseUntil <= now.Unix() {
		t.Fatalf("leased job = %+v, ok=%v", job, ok)
	}
	if _, ok := m.LeaseResynth(now); ok {
		t.Fatal("a live processing lease must not be handed to a second worker")
	}
	if !m.CompleteResynth(job.ID, job.LeaseToken, resynthCompleted, "", now.Add(time.Minute)) {
		t.Fatal("completion was rejected")
	}
	if !m.CompleteResynth(job.ID, job.LeaseToken, resynthCompleted, "", now.Add(2*time.Minute)) {
		t.Fatal("a replayed completion must be idempotent")
	}
	stats := m.QueueStats()
	if stats.Depth != 0 || stats.Completed != 1 {
		t.Fatalf("queue stats = %+v, want no active work and one completed row", stats)
	}
	resolved, _ := m.marker("th_1")
	if resolved.Stale || resolved.BaselinePx != marker.ObservedPx || resolved.LastFired != "" {
		t.Fatalf("completed marker = %+v, want a reset baseline", resolved)
	}
	// Enqueuing the same condition twice is a no-op: the id is derived from (thesis, condition).
	if _, ok := m.enqueue(marker); ok {
		t.Fatal("the same stale condition must not enqueue twice")
	}
}

func TestAnExpiredLeaseRetriesThenFailsAtTheAttemptCap(t *testing.T) {
	m, _ := newMonitor(t, candles(100, 1_780_000_000, false), nil)
	marker := ThesisMarker{ThesisID: "th_1", UserID: "u1", Ticker: "NVDA", Stale: true,
		Severity: severityMaterial, LastFired: "abc", AsOfISO: "2026-06-01T00:00:00Z"}
	m.enqueue(marker)
	now := time.Unix(1_800_000_000, 0).UTC()
	firstToken := ""
	for attempt := 1; attempt <= monitorResynthMaxAttempts; attempt++ {
		job, ok := m.LeaseResynth(now)
		if !ok || job.Attempts != attempt {
			t.Fatalf("attempt %d lease = %+v, ok=%v", attempt, job, ok)
		}
		if attempt == 1 {
			firstToken = job.LeaseToken
		}
		if attempt == 2 && m.CompleteResynth(job.ID, firstToken, resynthQueued, "late worker", now) {
			t.Fatal("an expired lease token changed a newer worker's job")
		}
		now = time.Unix(job.LeaseUntil+1, 0).UTC()
	}
	if _, ok := m.LeaseResynth(now); ok {
		t.Fatal("a job past the attempt cap must become terminal")
	}
	if stats := m.QueueStats(); stats.Failed != 1 || stats.Depth != 0 {
		t.Fatalf("queue stats = %+v, want one terminal failure", stats)
	}
	job, ok := m.LeaseResynth(now.Add(monitorResynthRetryDelay + time.Second))
	if !ok || job.State != resynthProcessing || job.Attempts != 1 {
		t.Fatalf("cooled-down failure did not start a fresh bounded cycle: %+v, ok=%v", job, ok)
	}
}

func TestInternalQueueRoutesRequireSecretAndRefuseBrowserCookies(t *testing.T) {
	m, _ := newMonitor(t, candles(100, 1_780_000_000, false), nil)
	marker := ThesisMarker{ThesisID: "th_1", UserID: "u1", Ticker: "NVDA", Stale: true,
		Severity: severityMaterial, LastFired: "abc", AsOfISO: "2026-06-01T00:00:00Z"}
	m.enqueue(marker)
	api := newAPI(m.store, m.cfg)
	api.monitor = m

	unauthorized := httptest.NewRequest(http.MethodPost, "/_internal/resynth/lease", nil)
	w := httptest.NewRecorder()
	api.routes().ServeHTTP(w, unauthorized)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("without secret status=%d, want 401", w.Code)
	}

	browser := httptest.NewRequest(http.MethodPost, "/_internal/resynth/lease", nil)
	browser.Header.Set("X-Internal-Secret", m.cfg.Secret)
	browser.AddCookie(&http.Cookie{Name: m.cfg.CookieName, Value: "anything"})
	w = httptest.NewRecorder()
	api.routes().ServeHTTP(w, browser)
	if w.Code != http.StatusNotFound {
		t.Fatalf("browser-shaped request status=%d, want 404", w.Code)
	}

	leaseReq := httptest.NewRequest(http.MethodPost, "/_internal/resynth/lease", nil)
	leaseReq.Header.Set("X-Internal-Secret", m.cfg.Secret)
	w = httptest.NewRecorder()
	api.routes().ServeHTTP(w, leaseReq)
	if w.Code != http.StatusOK {
		t.Fatalf("lease status=%d body=%s", w.Code, w.Body.String())
	}
	var leased map[string]any
	json.Unmarshal(w.Body.Bytes(), &leased)
	job := leased["job"].(map[string]any)

	completeReq := httptest.NewRequest(http.MethodPost,
		"/_internal/resynth/"+job["id"].(string)+"/complete",
		bytes.NewBufferString(`{"outcome":"completed","leaseToken":"`+job["leaseToken"].(string)+`"}`))
	completeReq.Header.Set("X-Internal-Secret", m.cfg.Secret)
	w = httptest.NewRecorder()
	api.routes().ServeHTTP(w, completeReq)
	if w.Code != http.StatusOK {
		t.Fatalf("complete status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestAnUnstaleMarkerEnqueuesNothing(t *testing.T) {
	m, _ := newMonitor(t, candles(100, 1_780_000_000, false), nil)
	if _, ok := m.enqueue(ThesisMarker{ThesisID: "th_1", Stale: false}); ok {
		t.Fatal("a baseline marker is not a re-synthesis job")
	}
}

// ── §9.24: the boundary is the last stored bar ───────────────────────────────────────────────────

func TestTheTickReadsTheLastStoredBarAndNeverAQuote(t *testing.T) {
	// `as_of = now` is Wave 1B's LIVE path: it would fan out one provider fetch per followed ticker
	// per tick, an unbudgeted cost AD-10's budgets do not cover.
	m, f := newMonitor(t, candles(100, 1_780_000_000, false),
		[]any{thesisRow("th_1", "u1", "NVDA", 0, nil, "material")})
	m.Tick(context.Background())

	for _, hit := range f.hits {
		if strings.Contains(hit, "/quote/") {
			t.Fatalf("the sweep called %s — a quote is a live fetch (§9.24)", hit)
		}
		if strings.Contains(hit, "as_of=") || strings.Contains(hit, "asOf=") {
			t.Fatalf("the sweep passed a cutoff of its own (%s); the boundary IS the last stored "+
				"bar, and asking for one would make the read no longer reproducible", hit)
		}
	}
	if !hasHit(f.hits, "/candles/NVDA") || !hasHit(f.hits, "limit=1") {
		t.Fatalf("expected a single-bar candles read, got %v", f.hits)
	}
}

func TestASyntheticBarIsNeverAComparisonBaseline(t *testing.T) {
	// §9.46: a synthetic bar is not history. Marking a thesis stale against one would attribute a
	// change to a price nobody traded at.
	m, _ := newMonitor(t, candles(100, 1_780_000_000, true),
		[]any{thesisRow("th_1", "u1", "NVDA", 0, nil, "material")})
	tick := m.Tick(context.Background())
	if tick.Examined != 0 {
		t.Fatalf("examined = %d, want 0 on synthetic bars", tick.Examined)
	}
	if !hasHit(tick.Degraded, "bars:NVDA") {
		t.Fatalf("degraded = %v, want the ticker named — a skipped read that says nothing is "+
			"indistinguishable from a read that found nothing", tick.Degraded)
	}
}

func TestOneBarReadPerDistinctTickerNotPerThesis(t *testing.T) {
	m, f := newMonitor(t, candles(100, 1_780_000_000, false), []any{
		thesisRow("th_1", "u1", "NVDA", 0, nil, "material"),
		thesisRow("th_2", "u1", "NVDA", 0, nil, "material"),
		thesisRow("th_3", "u2", "NVDA", 0, nil, "material"),
	})
	m.Tick(context.Background())
	candleReads := 0
	for _, hit := range f.hits {
		if strings.Contains(hit, "/candles/") {
			candleReads++
		}
	}
	if candleReads != 1 {
		t.Fatalf("candle reads = %d, want 1 — three theses on NVDA are one question about NVDA",
			candleReads)
	}
}

func TestInactiveThesesAreNeverMonitoredOrQueued(t *testing.T) {
	archived := thesisRow("th_1", "u1", "NVDA", 0, nil, "material")
	archived["status"] = "archived"
	m, f := newMonitor(t, candles(100, 1_780_000_000, false), []any{archived})
	tick := m.Tick(context.Background())
	if tick.Theses != 0 || tick.Examined != 0 || tick.Enqueued != 0 {
		t.Fatalf("inactive thesis tick = %+v", tick)
	}
	if hasHit(f.hits, "/candles/") {
		t.Fatalf("inactive thesis caused a market-data read: %v", f.hits)
	}
}

// ── the first tick establishes; the second compares ──────────────────────────────────────────────

func TestTheFirstTickMarksNothing(t *testing.T) {
	// A monitor that marked every thesis stale on its first tick would be comparing against a
	// number it invented that instant.
	m, _ := newMonitor(t, candles(100, 1_780_000_000, false),
		[]any{thesisRow("th_1", "u1", "NVDA", 0, nil, "material")})
	tick := m.Tick(context.Background())
	if tick.Marked != 0 || tick.Enqueued != 0 {
		t.Fatalf("first tick marked=%d enqueued=%d, want 0/0", tick.Marked, tick.Enqueued)
	}
	marker, ok := m.marker("th_1")
	if !ok || marker.Stale || marker.BaselinePx != 100 {
		t.Fatalf("first tick must record a baseline, got %+v", marker)
	}
}

func TestAMaterialMoveMarksStaleOnceAndOnlyOnce(t *testing.T) {
	m, _ := newMonitor(t, candles(100, 1_780_000_000, false),
		[]any{thesisRow("th_1", "u1", "NVDA", 0, nil, "material")})
	m.Tick(context.Background()) // baseline at 100

	m.cfg.AnalysisURL = swap(t, m, 107) // +7% — above the stated threshold
	tick := m.Tick(context.Background())
	if tick.Marked != 1 || tick.Enqueued != 1 {
		t.Fatalf("marked=%d enqueued=%d, want 1/1", tick.Marked, tick.Enqueued)
	}
	marker, _ := m.marker("th_1")
	if !marker.Stale || marker.Severity != severityMaterial {
		t.Fatalf("marker = %+v, want a material stale marker", marker)
	}
	// NO BEARING on a price move: the same 7% strengthens a long and weakens a short, and this
	// service cannot know which the user wrote.
	if marker.Bearing != "" {
		t.Fatalf("bearing = %q — a move is not a direction on somebody's thesis", marker.Bearing)
	}

	// STILL TRUE NEXT TICK, AND IT MUST NOT FIRE AGAIN. Change, not volume (doc §12.9).
	tick = m.Tick(context.Background())
	if tick.Marked != 0 {
		t.Fatalf("the same condition fired twice (marked=%d) — that is volume, not change", tick.Marked)
	}
}

func TestASubThresholdMoveIsNotAChange(t *testing.T) {
	m, _ := newMonitor(t, candles(100, 1_780_000_000, false),
		[]any{thesisRow("th_1", "u1", "NVDA", 0, nil, "material")})
	m.Tick(context.Background())
	m.cfg.AnalysisURL = swap(t, m, 102) // +2%
	if tick := m.Tick(context.Background()); tick.Marked != 0 {
		t.Fatalf("marked=%d on a 2%% move — without a stated threshold this would fire every tick "+
			"and 'your theses are current' could never be true", tick.Marked)
	}
}

func TestACatalystComingDueIsAMaterialChange(t *testing.T) {
	due := int64(1_780_000_500)
	m, _ := newMonitor(t, candles(100, 1_780_000_000, false),
		[]any{thesisRow("th_1", "u1", "NVDA", 0, []int64{due}, "material")})
	m.Tick(context.Background()) // baseline at ts 1_780_000_000, before the catalyst is due

	m.cfg.AnalysisURL = swapAt(t, m, 100, 1_780_001_000) // same price, later bar
	tick := m.Tick(context.Background())
	if tick.Marked != 1 {
		t.Fatalf("marked=%d — a catalyst the user NAMED came and went while they were not looking, "+
			"which is the strongest deterministic staleness signal there is", tick.Marked)
	}
	marker, _ := m.marker("th_1")
	if !strings.Contains(marker.Reason, "catalyst") {
		t.Fatalf("reason = %q, want the catalyst named", marker.Reason)
	}
}

func TestAReviewResetsTheBaseline(t *testing.T) {
	// Without this a thesis reviewed after a 7% move would immediately go stale again on the same
	// 7%, which teaches the user that the marker means nothing.
	m, _ := newMonitor(t, candles(100, 1_780_000_000, false),
		[]any{thesisRow("th_1", "u1", "NVDA", 0, nil, "material")})
	m.Tick(context.Background())
	m.cfg.AnalysisURL = swap(t, m, 107)
	m.Tick(context.Background()) // marked stale

	// The user reviews, at a time after the baseline was taken.
	m.cfg.JournalURL = swapTheses(t, []any{
		thesisRow("th_1", "u1", "NVDA", 1_780_000_900, nil, "material")})
	m.cfg.AnalysisURL = swapAt(t, m, 107, 1_780_001_000)
	tick := m.Tick(context.Background())
	if tick.Marked != 0 {
		t.Fatalf("marked=%d right after a review — the boundary must move to what the user looked at",
			tick.Marked)
	}
	marker, _ := m.marker("th_1")
	if marker.Stale || marker.BaselinePx != 107 {
		t.Fatalf("marker = %+v, want a fresh baseline at the reviewed price", marker)
	}
}

// ── doc §16.11's notification matrix ─────────────────────────────────────────────────────────────

func TestTheNotificationMatrix(t *testing.T) {
	cases := []struct {
		level, severity string
		want            bool
	}{
		{notifyAll, severityMinor, true},
		{notifyAll, severityMaterial, true},
		{notifyMaterial, severityMinor, false},
		{notifyMaterial, severityMaterial, true},
		{notifyNone, severityMaterial, false},
		{notifyNone, severityMinor, false},
		// Empty is the SHIPPED DEFAULT, which is `material`. Defaulting to `all` would make a user
		// who never opened settings the loudest-notified user on the platform.
		{"", severityMaterial, true},
		{"", severityMinor, false},
		// An unrecognised setting must never ESCALATE what a user receives.
		{"weekly-digest-please", severityMinor, false},
		{"ALL", severityMinor, true}, // case-insensitive, because journal stores it verbatim
	}
	for _, c := range cases {
		if got := shouldNotify(c.level, c.severity); got != c.want {
			t.Fatalf("shouldNotify(%q, %q) = %v, want %v", c.level, c.severity, got, c.want)
		}
	}
}

func TestTheInAppEventIsRecordedEvenWhenTheChannelsAreSilent(t *testing.T) {
	// The feed is the RECORD. Suppressing it would lose the fact, not just the ping.
	m, _ := newMonitor(t, candles(100, 1_780_000_000, false),
		[]any{thesisRow("th_1", "u1", "NVDA", 0, nil, notifyNone)})
	m.Tick(context.Background())
	m.cfg.AnalysisURL = swap(t, m, 107)
	tick := m.Tick(context.Background())

	if tick.Notified != 0 {
		t.Fatalf("notified=%d with level=none, want 0", tick.Notified)
	}
	events, _ := m.store.ListEvents("u1", 10)
	if len(events) != 1 || events[0].Type != "thesis_stale" {
		t.Fatalf("events = %+v, want one thesis_stale event recorded regardless", events)
	}
	if events[0].RuleID != "" {
		t.Fatalf("a monitor event must not claim a rule fired: ruleId=%q", events[0].RuleID)
	}
}

// ── degradation ──────────────────────────────────────────────────────────────────────────────────

func TestAnUnreachableJournalDegradesRatherThanCrashing(t *testing.T) {
	m, _ := newMonitor(t, candles(100, 1_780_000_000, false), nil)
	m.cfg.JournalURL = "http://127.0.0.1:1"
	tick := m.Tick(context.Background())
	if !hasHit(tick.Degraded, "journal_unreachable") {
		t.Fatalf("degraded = %v, want journal_unreachable", tick.Degraded)
	}
	if tick.Marked != 0 {
		t.Fatalf("a sweep that could not read the theses must mark nothing, got %d", tick.Marked)
	}
}

func TestADisabledMonitorRunsNoSweep(t *testing.T) {
	m, f := newMonitor(t, candles(100, 1_780_000_000, false),
		[]any{thesisRow("th_1", "u1", "NVDA", 0, nil, "material")})
	m.cfg.MonitorEnabled = false
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	m.Run(ctx) // returns immediately rather than blocking on a ticker
	if len(f.hits) != 0 {
		t.Fatalf("a disabled monitor made %d upstream call(s): %v", len(f.hits), f.hits)
	}
}

func TestMarkersAreScopedToTheirOwner(t *testing.T) {
	m, _ := newMonitor(t, candles(100, 1_780_000_000, false), nil)
	m.putMarker(ThesisMarker{ThesisID: "a", UserID: "u1", Stale: true, MarkedAt: 2})
	m.putMarker(ThesisMarker{ThesisID: "b", UserID: "u2", Stale: true, MarkedAt: 1})
	got := m.MarkersFor("u1")
	if len(got) != 1 || got[0].ThesisID != "a" {
		t.Fatalf("MarkersFor(u1) = %+v — a marker must never cross a user boundary", got)
	}
}

func TestStateSurvivesARestart(t *testing.T) {
	m, _ := newMonitor(t, candles(100, 1_780_000_000, false),
		[]any{thesisRow("th_1", "u1", "NVDA", 0, nil, "material")})
	m.Tick(context.Background())

	// A fresh monitor over the same directory must see the baseline, or every restart would
	// re-establish baselines and the first post-restart move would go unreported.
	again := newThesisMonitor(m.cfg, m.store, newNotifier(m.cfg))
	marker, ok := again.marker("th_1")
	if !ok || marker.BaselinePx != 100 {
		t.Fatalf("baseline did not survive a restart: %+v", marker)
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────────────────────────

func hasHit(list []string, want string) bool {
	for _, h := range list {
		if strings.Contains(h, want) {
			return true
		}
	}
	return false
}

func swap(t *testing.T, m *ThesisMonitor, closePx float64) string {
	return swapAt(t, m, closePx, 1_780_000_000)
}

func swapAt(t *testing.T, m *ThesisMonitor, closePx float64, ts int64) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, candles(closePx, ts, false))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func swapTheses(t *testing.T, theses []any) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"theses": theses})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

var _ = json.Marshal
