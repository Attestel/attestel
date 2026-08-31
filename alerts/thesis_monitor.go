package main

import (
	"context"
	cryptorand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// thesis_monitor.go — Wave 5 Lane 5B. Continuous, DETERMINISTIC monitoring of the theses a user has
// written, so a thesis whose world has moved stops looking current.
//
// FOUR CONSTRAINTS DEFINE THIS FILE, AND EVERY ONE OF THEM IS A CONTRACT CLAUSE.
//
//  1. **It owns the deterministic queue, never generation (§9.23).** When a thesis goes stale this
//     service writes a marker and a re-synthesis job. A small internal API may lease and complete
//     that job, but the Python worker in `services/llm` is the only component allowed to hold the
//     cross-process model lease and generate. Alerts remains stdlib-only and model-free.
//
//  2. **It makes ZERO outbound calls to the llm service, full stop (§9.23).** Not for a summary, not
//     for a bearing, not for a headline. `thesis_monitor_test.go` asserts this at the source level,
//     the same way `eval_test.go` already does for the evaluator, because a comment is not a
//     control.
//
//  3. **The tick's cutoff is the LAST STORED BAR, never `now` (§9.24).** `as_of = now` is Wave 1B's
//     LIVE path: it would fan out one live provider fetch per followed ticker per tick, an
//     unbudgeted cost AD-10's budgets do not cover (they live in `services/events`, not in
//     `services/analysis`). Reading at the last stored bar also makes a tick REPRODUCIBLE — the
//     same boundary yields the same answer — which is what lets a marker be diffed instead of
//     merely re-observed.
//
//  4. **Change, not volume (doc §12.9).** A tick that finds nothing writes nothing and notifies
//     nobody. The thresholds are stated constants, and a condition that is still true from last
//     tick does not fire again: `lastFired` edge-triggers exactly as the evaluator's `lastState`
//     does. Alerts driven by volume are how a user learns to ignore them.
//
// OFF BY DEFAULT. `THESIS_MONITOR_ENABLED` ships false, like `EVENT_ENRICH_ENABLED` and
// `INGEST_ENABLED`. The whole stack still runs with an empty `.env` and this loop simply never
// starts, which is invariant #1's requirement and also the correct default for a fan-out.

const (
	// monitorStaleMovePct is the close-to-close move, since the boundary the thesis was last
	// synthesised against, at which the thesis is considered to have gone stale. A THRESHOLD IS
	// REQUIRED: without one this would fire on every tick and "your theses are current" could never
	// be true, which is the same argument `changes.go`'s `materialMovePct` makes for itself. Kept
	// numerically equal to it on purpose — one idea of "a material move" across the product.
	monitorStaleMovePct = 5.0

	// monitorMaxTheses bounds one tick. Truncation is LOGGED and reported on the tick summary,
	// never silent: a quietly shortened sweep reads as "nothing else went stale".
	monitorMaxTheses = 200

	// monitorQueueMax bounds the durable queue, including terminal entries retained for idempotency.
	monitorQueueMax = 5000

	// A lease is deliberately longer than two 120-second model attempts plus the upstream reads.
	// An expired lease is eligible again, so a killed worker cannot strand a thesis forever.
	monitorResynthLease       = 10 * time.Minute
	monitorResynthMaxAttempts = 3
	monitorResynthRetryDelay  = 24 * time.Hour

	monitorHTTPTimeout = 15 * time.Second
)

// Notification levels, per the doc §16.11 matrix. They are the values journal already stores on a
// subscription (`notificationLevel`), read here rather than redefined.
const (
	notifyAll      = "all"      // every stale marker notifies
	notifyMaterial = "material" // only markers whose severity is material (the shipped default)
	notifyNone     = "none"     // in-app only; no webhook, no email
)

// Severity of a stale marker. Two values, because the matrix has two rows and a third would be a
// gradation nobody can act on differently.
const (
	severityMaterial = "material"
	severityMinor    = "minor"
)

// ThesisMarker is the STALE MARKER (§9.23). It is this service's own record — the monitor never
// writes into journal's thesis, which it does not own.
type ThesisMarker struct {
	ThesisID string `json:"thesisId"`
	UserID   string `json:"userId"`
	Ticker   string `json:"ticker"`

	// Stale is the state a UI renders as "this thesis has not been reviewed since the world moved".
	Stale     bool   `json:"stale"`
	Severity  string `json:"severity"`
	Reason    string `json:"reason"`
	Bearing   string `json:"bearing,omitempty"`
	DataState string `json:"dataState"`

	// AsOf is the LAST STORED BAR the tick read at, not the wall clock (§9.24). It is what makes a
	// marker reproducible and therefore diffable.
	AsOf       int64   `json:"asOf"`
	AsOfISO    string  `json:"asOfISO"`
	MarkedAt   int64   `json:"markedAt"`
	BaselineAt int64   `json:"baselineAt"`
	BaselinePx float64 `json:"baselinePx"`
	ObservedPx float64 `json:"observedPx"`
	MovePct    float64 `json:"movePct"`
	// LastFired edge-triggers: a condition still true next tick does not fire again.
	LastFired string `json:"lastFired,omitempty"`
}

const (
	resynthQueued     = "queued"
	resynthProcessing = "processing"
	resynthCompleted  = "completed"
	resynthFailed     = "failed"
)

// ResynthJob is one durable queue entry. It contains identifiers and deterministic trigger facts,
// never thesis prose or model output. The worker resolves the current thesis from journal only
// after it has obtained a lease.
type ResynthJob struct {
	ID          string `json:"id"`
	ThesisID    string `json:"thesisId"`
	UserID      string `json:"userId"`
	Ticker      string `json:"ticker"`
	Reason      string `json:"reason"`
	Severity    string `json:"severity"`
	AsOf        string `json:"asOf"`
	EnqueuedAt  int64  `json:"enqueuedAt"`
	State       string `json:"state"`
	Attempts    int    `json:"attempts"`
	LeasedAt    int64  `json:"leasedAt,omitempty"`
	LeaseUntil  int64  `json:"leaseUntil,omitempty"`
	LeaseToken  string `json:"leaseToken,omitempty"`
	CompletedAt int64  `json:"completedAt,omitempty"`
	FailedAt    int64  `json:"failedAt,omitempty"`
	LastError   string `json:"lastError,omitempty"`
}

// ResynthQueueStats separates actionable work from retained terminal entries. `Depth` is what the
// UI and an operator should treat as backlog; completed jobs do not inflate it.
type ResynthQueueStats struct {
	Depth      int `json:"depth"`
	Queued     int `json:"queued"`
	Processing int `json:"processing"`
	Completed  int `json:"completed"`
	Failed     int `json:"failed"`
	Dropped    int `json:"dropped"`
}

// MonitorTick is one sweep's summary, served by the status route and logged.
type MonitorTick struct {
	At        int64    `json:"at"`
	AsOf      string   `json:"asOf"`
	Theses    int      `json:"theses"`
	Examined  int      `json:"examined"`
	Marked    int      `json:"marked"`
	Enqueued  int      `json:"enqueued"`
	Notified  int      `json:"notified"`
	Truncated bool     `json:"truncated"`
	Degraded  []string `json:"degraded"`
}

// ThesisMonitor is the loop. Its durable marker/queue state uses the same Store backend as alerts;
// it only appends user-visible events through the existing Store API.
type ThesisMonitor struct {
	cfg      Config
	store    *Store
	notifier *Notifier
	http     *http.Client

	mu       sync.Mutex
	markers  map[string]ThesisMarker // keyed by thesisId
	queue    []ResynthJob
	dropped  int
	lastTick MonitorTick
}

func newThesisMonitor(cfg Config, store *Store, notifier *Notifier) *ThesisMonitor {
	m := &ThesisMonitor{
		cfg: cfg, store: store, notifier: notifier,
		http:    &http.Client{Timeout: monitorHTTPTimeout},
		markers: map[string]ThesisMarker{},
	}
	m.load()
	return m
}

// Run drives the sweep until ctx is cancelled. It does NOT evaluate once immediately: the first
// tick of a monitor with no baselines would mark every thesis stale against a baseline it invented
// on the spot. The first tick establishes baselines; the second one can compare.
func (m *ThesisMonitor) Run(ctx context.Context) {
	if !m.cfg.MonitorEnabled {
		log.Printf("thesis monitor: disabled (THESIS_MONITOR_ENABLED=false) — no sweep will run")
		return
	}
	log.Printf("thesis monitor: interval=%s journal=%s (model-free queue producer; worker is "+
		"services/llm/app/thesis_resynth.py)",
		m.cfg.MonitorInterval, m.cfg.JournalURL)
	ticker := time.NewTicker(m.cfg.MonitorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tick := m.Tick(ctx)
			log.Printf("thesis monitor: asOf=%s examined=%d marked=%d enqueued=%d degraded=%v",
				tick.AsOf, tick.Examined, tick.Marked, tick.Enqueued, tick.Degraded)
		}
	}
}

// Tick performs one sweep and returns its summary. Exported-shaped (capitalised) so a test can
// drive exactly one sweep without a timer, which is how every assertion about this file is made.
func (m *ThesisMonitor) Tick(ctx context.Context) MonitorTick {
	tick := MonitorTick{At: time.Now().Unix(), Degraded: []string{}}

	theses, ok := m.fetchTheses(ctx)
	if !ok {
		tick.Degraded = append(tick.Degraded, "journal_unreachable")
		m.recordTick(tick)
		return tick
	}
	if len(theses) > monitorMaxTheses {
		theses = theses[:monitorMaxTheses]
		tick.Truncated = true
	}
	tick.Theses = len(theses)

	// One bar read per distinct ticker, not one per thesis: ten theses on NVDA are one question
	// about NVDA.
	bars := map[string]barPoint{}
	for _, th := range theses {
		sym := strings.ToUpper(th.Ticker)
		if sym == "" {
			continue
		}
		if _, seen := bars[sym]; seen {
			continue
		}
		point, ok := m.lastStoredBar(ctx, sym)
		if !ok {
			tick.Degraded = append(tick.Degraded, "bars:"+sym)
			continue
		}
		bars[sym] = point
	}

	for _, th := range theses {
		point, ok := bars[strings.ToUpper(th.Ticker)]
		if !ok {
			continue
		}
		tick.Examined++
		if tick.AsOf == "" {
			tick.AsOf = point.iso
		}
		marker, changed := m.evaluateThesis(th, point)
		if !changed {
			continue
		}
		tick.Marked++
		m.putMarker(marker)
		if job, enqueued := m.enqueue(marker); enqueued {
			tick.Enqueued++
			_ = job
		}
		if m.emit(marker, th) {
			tick.Notified++
		}
	}

	sort.Strings(tick.Degraded)
	m.persist()
	m.recordTick(tick)
	return tick
}

// ── the boundary (§9.24) ─────────────────────────────────────────────────────────────────────────

type barPoint struct {
	ts    int64
	iso   string
	close float64
}

// lastStoredBar reads the LAST STORED BAR — `limit=1`, no `as_of` — and that timestamp becomes the
// tick's cutoff. It is deliberately not `/quote/{ticker}`: a quote is a live fetch, which is the
// unbudgeted per-ticker provider call §9.24 exists to forbid.
func (m *ThesisMonitor) lastStoredBar(ctx context.Context, ticker string) (barPoint, bool) {
	u := fmt.Sprintf("%s/candles/%s?timeframe=1D&limit=1",
		m.cfg.AnalysisURL, url.PathEscape(ticker))
	body, ok := m.getJSON(ctx, u)
	if !ok {
		return barPoint{}, false
	}
	// A SYNTHETIC bar is not history (§9.46). Marking a thesis stale against one would attribute a
	// change to a price nobody traded at.
	if synthetic, _ := body["sourceIsSynthetic"].(bool); synthetic {
		return barPoint{}, false
	}
	// `/candles` serves `bars`. Accept the pre-contract `candles` spelling as a compatibility
	// fallback for older deployments, but never require it: doing so left the production monitor
	// with zero observations while its unit-test fixture remained green.
	raw, _ := body["bars"].([]any)
	if len(raw) == 0 {
		raw, _ = body["candles"].([]any)
	}
	if len(raw) == 0 {
		return barPoint{}, false
	}
	last, _ := raw[len(raw)-1].(map[string]any)
	if last == nil {
		return barPoint{}, false
	}
	closePx, ok := floatOf(last["close"])
	if !ok {
		return barPoint{}, false
	}
	ts := monitorUnix(last["time"])
	if ts == 0 {
		ts = monitorUnix(last["t"])
	}
	if ts == 0 {
		return barPoint{}, false
	}
	return barPoint{ts: ts, iso: time.Unix(ts, 0).UTC().Format(time.RFC3339), close: closePx}, true
}

// ── the comparison ───────────────────────────────────────────────────────────────────────────────

// monitorThesis is the slice of a journal thesis this file reads. Deliberately narrow: the monitor
// has no business copying a thesis's prose, and a narrow struct is a narrow blast radius when
// journal's shape moves.
type monitorThesis struct {
	ID           string
	UserID       string
	Ticker       string
	CreatedAt    int64
	LastReviewAt int64
	// CatalystsDue are the `dueAt` timestamps the user put on this thesis's catalysts. A catalyst
	// that has come and gone while the thesis was not reviewed is the strongest DETERMINISTIC
	// staleness signal there is — the user named the thing they were waiting for, and it happened.
	CatalystsDue      []int64
	NotificationLevel string
}

// evaluateThesis compares the thesis's baseline against the boundary reading and decides whether it
// has gone stale. Everything here is arithmetic over stored numbers — no model, no heuristic prose.
//
// `changed` is false when nothing crossed a threshold OR when the same condition already fired:
// `lastFired` edge-triggers, exactly as the evaluator's `lastState` does, so a thesis that went
// stale on Tuesday does not re-notify every hour until Friday.
func (m *ThesisMonitor) evaluateThesis(th monitorThesis, point barPoint) (ThesisMarker, bool) {
	baselineAt, baselinePx, ok := m.baselineFor(th)
	if !ok {
		// FIRST SIGHTING. Record the baseline and mark nothing: a monitor that marked every thesis
		// stale on its first tick would be comparing against a number it invented that instant.
		m.putMarker(ThesisMarker{
			ThesisID: th.ID, UserID: th.UserID, Ticker: strings.ToUpper(th.Ticker),
			Stale: false, Severity: "", Reason: "baseline established",
			DataState: "live", AsOf: point.ts, AsOfISO: point.iso,
			BaselineAt: point.ts, BaselinePx: point.close, ObservedPx: point.close,
		})
		return ThesisMarker{}, false
	}

	movePct := 0.0
	if baselinePx != 0 {
		movePct = (point.close - baselinePx) / baselinePx * 100.0
	}

	// TWO SIGNALS, BOTH ARITHMETIC OVER STORED NUMBERS. There is deliberately no third one that
	// reads the thesis's PROSE. The invalidation conditions a user writes are free text
	// ("daily close below 173.80 on above-average volume"), and anything clever enough to parse
	// that into a threshold is a false-positive generator — §9.56 learned this the expensive way,
	// and a monitor that cried wolf off a misparsed sentence would be worse than one that stayed
	// quiet. When invalidation conditions become structured, this is where the third case goes.
	reason, severity, bearing := "", "", ""
	switch {
	case absPct(movePct) >= monitorStaleMovePct:
		direction := "higher"
		if movePct < 0 {
			direction = "lower"
		}
		reason = fmt.Sprintf("close moved %.1f%% %s (%.2f → %.2f) since this thesis was last reviewed",
			absPct(movePct), direction, baselinePx, point.close)
		severity = severityMaterial
		// NO BEARING. A move is not a direction on somebody's thesis: the same 6% strengthens a long
		// and weakens a short, and this service cannot know which one the user wrote. `bearing` is
		// set ONLY where it is deterministic (`rules.go`'s own rule), so here it stays empty.
	case dueSince(th.CatalystsDue, baselineAt, point.ts) > 0:
		n := dueSince(th.CatalystsDue, baselineAt, point.ts)
		reason = fmt.Sprintf("%d catalyst(s) you named came due since this thesis was last reviewed",
			n)
		severity = severityMaterial
	default:
		return ThesisMarker{}, false
	}

	fired := firedKey(severity, reason)
	if previous, seen := m.marker(th.ID); seen && previous.LastFired == fired {
		// Still true, already said. Change, not volume.
		return ThesisMarker{}, false
	}

	return ThesisMarker{
		ThesisID: th.ID, UserID: th.UserID, Ticker: strings.ToUpper(th.Ticker),
		Stale: true, Severity: severity, Reason: reason, Bearing: bearing,
		DataState: "live",
		AsOf:      point.ts, AsOfISO: point.iso, MarkedAt: time.Now().Unix(),
		BaselineAt: baselineAt, BaselinePx: baselinePx, ObservedPx: point.close,
		MovePct: movePct, LastFired: fired,
	}, true
}

// dueSince counts the catalyst due-dates that fell strictly inside `(after, upTo]`.
//
// Half-open on purpose: a catalyst due exactly at the baseline was already visible when the user
// last looked, and re-reporting it would be the "still true, already said" firing this file's
// fourth constraint forbids.
func dueSince(due []int64, after, upTo int64) int {
	n := 0
	for _, at := range due {
		if at > after && at <= upTo {
			n++
		}
	}
	return n
}

func absPct(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// firedKey identifies WHICH condition fired, so a different condition on the same thesis is a new
// event while the same one re-observed is not.
func firedKey(severity, reason string) string {
	sum := sha256.Sum256([]byte(severity + "\x00" + reason))
	return hex.EncodeToString(sum[:])[:16]
}

// baselineFor is the point the comparison runs from: the reading at the user's last review when we
// have one, else the reading when we first saw the thesis.
func (m *ThesisMonitor) baselineFor(th monitorThesis) (int64, float64, bool) {
	previous, ok := m.marker(th.ID)
	if !ok || previous.BaselinePx == 0 {
		return 0, 0, false
	}
	// A REVIEW RESETS THE BASELINE. The user looked; the boundary moves to what they looked at.
	// Without this a thesis reviewed after a 6% move would immediately go stale again on the same
	// 6%, which teaches the user that the marker means nothing.
	if th.LastReviewAt > previous.BaselineAt {
		return 0, 0, false
	}
	return previous.BaselineAt, previous.BaselinePx, true
}

// ── the queue (§9.23) ────────────────────────────────────────────────────────────────────────────

// enqueue writes the re-synthesis JOB. It does not perform it, and there is no code path in this
// service that could: nothing here holds the model lease, so nothing here may generate.
func (m *ThesisMonitor) enqueue(marker ThesisMarker) (ResynthJob, bool) {
	if !marker.Stale {
		return ResynthJob{}, false
	}
	job := ResynthJob{
		ID:       "rsy_" + firedKey(marker.ThesisID, marker.LastFired+"\x00"+marker.AsOfISO),
		ThesisID: marker.ThesisID, UserID: marker.UserID, Ticker: marker.Ticker,
		Reason: marker.Reason, Severity: marker.Severity,
		AsOf: marker.AsOfISO, EnqueuedAt: time.Now().Unix(), State: resynthQueued,
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.queue {
		if existing.ID == job.ID {
			return ResynthJob{}, false
		}
	}
	m.queue = append(m.queue, job)
	if len(m.queue) > monitorQueueMax {
		// Oldest first. The drop is COUNTED, not silent. Terminal entries normally occupy the oldest
		// end; a persistently dead worker still cannot grow the volume without bound.
		drop := len(m.queue) - monitorQueueMax
		m.queue = m.queue[drop:]
		m.dropped += drop
	}
	return job, true
}

// LeaseResynth atomically claims the oldest eligible job. Processing leases whose worker died are
// returned to the pool after their deadline; after three starts they become terminal failures so a
// permanently malformed thesis cannot consume every operator pass forever.
func (m *ThesisMonitor) LeaseResynth(now time.Time) (ResynthJob, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	nowUnix := now.Unix()
	for i := range m.queue {
		job := &m.queue[i]
		if job.State == resynthFailed && job.FailedAt > 0 &&
			job.FailedAt+int64(monitorResynthRetryDelay/time.Second) <= nowUnix {
			// A terminal-looking row is a COOLDOWN, not permanent abandonment. A provider or model
			// outage can last longer than three operator passes; retry one fresh bounded cycle after a
			// day without turning the worker into a hot loop.
			job.State, job.Attempts, job.FailedAt = resynthQueued, 0, 0
		}
		eligible := job.State == resynthQueued ||
			(job.State == resynthProcessing && job.LeaseUntil > 0 && job.LeaseUntil <= nowUnix)
		if !eligible {
			continue
		}
		if job.Attempts >= monitorResynthMaxAttempts {
			job.State = resynthFailed
			job.FailedAt = nowUnix
			job.LeasedAt, job.LeaseUntil, job.LeaseToken = 0, 0, ""
			if job.LastError == "" {
				job.LastError = "worker lease expired after maximum attempts"
			}
			continue
		}
		job.State = resynthProcessing
		job.Attempts++
		job.LeasedAt = nowUnix
		job.LeaseUntil = now.Add(monitorResynthLease).Unix()
		job.LeaseToken = resynthLeaseToken(job.ID, now)
		job.LastError = ""
		m.persistLocked()
		return *job, true
	}
	m.persistLocked()
	return ResynthJob{}, false
}

// CompleteResynth acknowledges a lease. Successful jobs are retained as terminal rows so a replay
// of the same completion is idempotent. Retryable failures return to queued until the attempt cap.
// A successful re-synthesis also advances the marker baseline to the observation it just reviewed,
// allowing a future material move to create a new edge-triggered job.
func (m *ThesisMonitor) CompleteResynth(id, leaseToken, outcome, detail string, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.queue {
		job := &m.queue[i]
		if job.ID != id {
			continue
		}
		if leaseToken == "" || job.LeaseToken != leaseToken {
			return false
		}
		if job.State == resynthCompleted {
			return true
		}
		if job.State != resynthProcessing {
			return false
		}
		nowUnix := now.Unix()
		detail = strings.TrimSpace(detail)
		if len(detail) > 500 {
			detail = detail[:500]
		}
		switch outcome {
		case resynthCompleted:
			job.State, job.CompletedAt = resynthCompleted, nowUnix
			job.LeasedAt, job.LeaseUntil, job.LastError = 0, 0, ""
			if marker, ok := m.markers[job.ThesisID]; ok && marker.Stale && marker.AsOfISO == job.AsOf {
				marker.Stale = false
				marker.Severity = ""
				marker.Reason = "re-synthesis completed"
				marker.BaselineAt = marker.AsOf
				marker.BaselinePx = marker.ObservedPx
				marker.LastFired = ""
				m.markers[job.ThesisID] = marker
			}
		case resynthQueued:
			job.LastError = detail
			job.LeasedAt, job.LeaseUntil, job.LeaseToken = 0, 0, ""
			if job.Attempts >= monitorResynthMaxAttempts {
				job.State, job.FailedAt = resynthFailed, nowUnix
			} else {
				job.State = resynthQueued
			}
		default:
			return false
		}
		m.persistLocked()
		return true
	}
	return false
}

func resynthLeaseToken(jobID string, now time.Time) string {
	raw := make([]byte, 16)
	if _, err := cryptorand.Read(raw); err == nil {
		return hex.EncodeToString(raw)
	}
	return firedKey(jobID, fmt.Sprint(now.UnixNano()))
}

// ── notification (doc §16.11's matrix) ───────────────────────────────────────────────────────────

// shouldNotify is the matrix, in one function. The in-app event is ALWAYS recorded — the feed is
// the record and suppressing it would lose the fact, not just the ping — and this decides only
// whether the webhook/email channels fire.
func shouldNotify(level, severity string) bool {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case notifyAll:
		return true
	case notifyNone:
		return false
	case notifyMaterial, "":
		// Empty means the shipped default, which is `material`. Defaulting to `all` would make a
		// user who never opened settings the loudest-notified user on the platform.
		return severity == severityMaterial
	default:
		// An unknown level is treated as the default rather than as "all": an unrecognised setting
		// must never escalate what a user receives.
		return severity == severityMaterial
	}
}

// emit records the event and, if the matrix allows, notifies. Returns whether a channel fired.
func (m *ThesisMonitor) emit(marker ThesisMarker, th monitorThesis) bool {
	ev := Event{
		ID:        "evt_" + firedKey(marker.ThesisID, marker.LastFired),
		RuleID:    "",
		UserID:    marker.UserID,
		Ticker:    marker.Ticker,
		Timeframe: "1D",
		Type:      "thesis_stale",
		// Neutral and descriptive. It states what became true; it does not say what to do about it.
		Message:   marker.Reason,
		TS:        marker.MarkedAt,
		ThesisID:  marker.ThesisID,
		Intent:    "monitor",
		DataState: marker.DataState,
		DedupeKey: marker.LastFired,
	}
	if marker.Bearing != "" {
		bearing := marker.Bearing
		ev.Bearing = &bearing
	}
	if err := m.store.AppendEvent(ev); err != nil {
		log.Printf("thesis monitor: could not record event for %s: %v", marker.ThesisID, err)
	}
	if !shouldNotify(th.NotificationLevel, marker.Severity) {
		return false
	}
	m.notifier.Notify(ev)
	return true
}

// ── journal ──────────────────────────────────────────────────────────────────────────────────────

// fetchTheses reads every user's theses through journal's cross-user internal route. That route is
// deliberately NOT proxied by the gateway (§9.4) — a browser must never reach it — but a
// server-side sweep is exactly what it exists for.
func (m *ThesisMonitor) fetchTheses(ctx context.Context) ([]monitorThesis, bool) {
	body, ok := m.getJSON(ctx, m.cfg.JournalURL+"/_internal/theses?limit="+
		fmt.Sprint(monitorMaxTheses))
	if !ok {
		return nil, false
	}
	raw, _ := body["theses"].([]any)
	out := make([]monitorThesis, 0, len(raw))
	for _, r := range raw {
		t, _ := r.(map[string]any)
		if t == nil {
			continue
		}
		// Closed, archived and draft theses are not live beliefs. Re-synthesising one would revive a
		// record the user explicitly stopped monitoring.
		status := stringOf(t["status"])
		if status != "active" && status != "open" { // `open` is the pre-v2 active spelling
			continue
		}
		th := monitorThesis{
			ID:                stringOf(t["id"]),
			UserID:            stringOf(t["userId"]),
			Ticker:            strings.ToUpper(stringOf(t["ticker"])),
			CreatedAt:         monitorUnix(t["createdAt"]),
			NotificationLevel: stringOf(t["notificationLevel"]),
		}
		if lr, _ := t["lastReview"].(map[string]any); lr != nil {
			th.LastReviewAt = monitorUnix(lr["at"])
		}
		if raw, _ := t["catalystsDue"].([]any); raw != nil {
			for _, d := range raw {
				if at := monitorUnix(d); at > 0 {
					th.CatalystsDue = append(th.CatalystsDue, at)
				}
			}
		}
		if th.ID != "" {
			out = append(out, th)
		}
	}
	return out, true
}

func (m *ThesisMonitor) getJSON(ctx context.Context, rawURL string) (map[string]any, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, false
	}
	resp, err := m.http.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, false
	}
	return body, true
}

// ── state ────────────────────────────────────────────────────────────────────────────────────────

func (m *ThesisMonitor) markersPath() string {
	return filepath.Join(m.cfg.RulesDir, "thesis_markers.json")
}

func (m *ThesisMonitor) queuePath() string {
	return filepath.Join(m.cfg.RulesDir, "resynth_queue.json")
}

type monitorState struct {
	Markers map[string]ThesisMarker `json:"markers"`
	Queue   []ResynthJob            `json:"queue"`
	Dropped int                     `json:"dropped"`
}

func (m *ThesisMonitor) load() {
	if m.store.db != nil {
		loaded, found, err := m.store.loadMonitorPostgres()
		if err != nil {
			log.Printf("thesis monitor: cannot load PostgreSQL state: %v", err)
			return
		}
		if found {
			if loaded.Markers != nil {
				m.markers = loaded.Markers
			}
			m.queue, m.dropped = loaded.Queue, loaded.Dropped
		}
		return
	}
	if b, err := os.ReadFile(m.markersPath()); err == nil {
		var loaded map[string]ThesisMarker
		if json.Unmarshal(b, &loaded) == nil && loaded != nil {
			m.markers = loaded
		}
	}
	if b, err := os.ReadFile(m.queuePath()); err == nil {
		var loaded monitorState
		if json.Unmarshal(b, &loaded) == nil {
			m.queue, m.dropped = loaded.Queue, loaded.Dropped
		}
	}
}

func (m *ThesisMonitor) persist() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.persistLocked()
}

// persistLocked keeps an externally visible lease and its durable record in the same mutex hold.
func (m *ThesisMonitor) persistLocked() {
	state := monitorState{Markers: m.markers, Queue: m.queue, Dropped: m.dropped}
	if m.store.db != nil {
		if err := m.store.saveMonitorPostgres(state); err != nil {
			log.Printf("thesis monitor: cannot persist PostgreSQL state: %v", err)
		}
		return
	}
	markers, _ := json.MarshalIndent(m.markers, "", "  ")
	queue, _ := json.MarshalIndent(state, "", "  ")
	if err := writeFileAtomic(m.markersPath(), markers); err != nil {
		log.Printf("thesis monitor: cannot persist markers: %v", err)
	}
	if err := writeFileAtomic(m.queuePath(), queue); err != nil {
		log.Printf("thesis monitor: cannot persist queue: %v", err)
	}
}

func (m *ThesisMonitor) marker(thesisID string) (ThesisMarker, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.markers[thesisID]
	return v, ok
}

func (m *ThesisMonitor) putMarker(marker ThesisMarker) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.markers[marker.ThesisID] = marker
}

func (m *ThesisMonitor) recordTick(tick MonitorTick) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastTick = tick
}

// MarkersFor returns one user's stale markers, newest first. Scoped by user at the source: the
// route above it forwards a verified uid and this function has no way to return another user's.
func (m *ThesisMonitor) MarkersFor(uid string) []ThesisMarker {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []ThesisMarker{}
	for _, marker := range m.markers {
		if marker.UserID == uid && marker.Stale {
			out = append(out, marker)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MarkedAt > out[j].MarkedAt })
	return out
}

// QueueStats reports active backlog separately from terminal history.
func (m *ThesisMonitor) QueueStats() ResynthQueueStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	stats := ResynthQueueStats{Dropped: m.dropped}
	for _, job := range m.queue {
		switch job.State {
		case resynthQueued:
			stats.Queued++
		case resynthProcessing:
			stats.Processing++
		case resynthCompleted:
			stats.Completed++
		case resynthFailed:
			stats.Failed++
		}
	}
	stats.Depth = stats.Queued + stats.Processing
	return stats
}

// QueueDepth preserves the old test/helper surface while making its meaning the active backlog.
func (m *ThesisMonitor) QueueDepth() (int, int) {
	stats := m.QueueStats()
	return stats.Depth, stats.Dropped
}

func (m *ThesisMonitor) LastTick() MonitorTick {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastTick
}

// ── small helpers (this service has no shared util file) ────────────────────────────────────────

func stringOf(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func floatOf(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	}
	return 0, false
}

// monitorUnix accepts the two spellings this platform actually produces: a unix second and an
// RFC3339 string. It does NOT guess at anything else — an unparseable value is 0, and every caller
// treats 0 as "no timestamp" rather than as the epoch.
func monitorUnix(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int64:
		return t
	case string:
		if parsed, err := time.Parse(time.RFC3339, t); err == nil {
			return parsed.Unix()
		}
	}
	return 0
}
