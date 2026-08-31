package main

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// routes_extra.go — the monitoring surface added by Step 08 (PARALLEL_CONTRACTS.md §4).
//
// This file exists because of the route seam in §7.1: `handlers.go` is a shared file, so this lane
// registers its routes from here and touches the shared file with exactly one line.
//
// Nothing in this file evaluates anything. The scheduler in eval.go remains the only thing that
// decides a rule has fired, and it makes no model call (§4.6) — these are read endpoints over rules
// and events that already exist.

func (a *API) registerExtraRoutes(mux *http.ServeMux) {
	// Due and overdue research reviews for the caller — what Today's attention list renders.
	mux.HandleFunc("GET /monitoring/due", a.optionalAuth(a.handleMonitoringDue))
	// Wave 5 Lane 5B. The thesis monitor's stale markers for the caller, plus the sweep's own
	// status. A READ endpoint over state the sweep already wrote: it triggers nothing, and it makes
	// no model call, because nothing in this service ever does (§9.23).
	mux.HandleFunc("GET /monitoring/theses", a.optionalAuth(a.handleMonitorTheses))
	// Server-to-server queue handoff. These routes are never proxied by the gateway and require the
	// shared internal secret; a browser carrying a session cookie receives 404 before auth is read.
	mux.HandleFunc("POST /_internal/resynth/lease", a.handleResynthLease)
	mux.HandleFunc("POST /_internal/resynth/{id}/complete", a.handleResynthComplete)
	// Phase 1. ONE deterministic sweep, on demand, for the operator-invoked automation runner.
	//
	// This adds no second scheduler. `Run` (gated on THESIS_MONITOR_ENABLED) already drives Tick on
	// an interval when an operator wants that; this route is the other way to reach the SAME Tick —
	// from a cron outside the codebase, through the automation runner, so the sweep gets a durable
	// run record and a lane lease instead of only a log line. It is model-free, exactly as every
	// other path through this service is (§9.23), and it is not proxied by the gateway.
	mux.HandleFunc("POST /_internal/monitor/tick", a.handleMonitorTick)
}

// handleMonitorTick performs exactly one sweep and returns its summary plus the queue depth.
//
// It respects THESIS_MONITOR_ENABLED: a disabled monitor answers 200 with `ran: false` rather than
// sweeping anyway. The flag means "this deployment monitors theses"; a manual trigger is a
// different CADENCE for that decision, not a way around it.
func (a *API) handleMonitorTick(w http.ResponseWriter, r *http.Request) {
	if a.rejectInternalResynth(w, r) {
		return
	}
	if !a.cfg.MonitorEnabled {
		writeJSON(w, http.StatusOK, map[string]any{
			"ran": false, "reason": "disabled", "flag": "THESIS_MONITOR_ENABLED",
		})
		return
	}
	tick := a.monitor.Tick(r.Context())
	queue := a.monitor.QueueStats()
	writeJSON(w, http.StatusOK, map[string]any{
		"ran": true, "tick": tick, "queue": queue,
	})
}

// handleMonitorTheses serves this user's stale markers and the last sweep's summary.
//
// Queue state is reported separately from monitor state: a disabled producer may still have jobs
// left from an earlier run, and a built worker is not evidence that an operator enabled it.
func (a *API) handleMonitorTheses(w http.ResponseWriter, r *http.Request) {
	if a.monitor == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled": false, "markers": []ThesisMarker{},
			"note": "THESIS_MONITOR_ENABLED is not true, so no sweep has run. This is not a " +
				"statement that nothing went stale.",
		})
		return
	}
	stats := a.monitor.QueueStats()
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":  a.cfg.MonitorEnabled,
		"markers":  a.monitor.MarkersFor(userID(r)),
		"lastTick": a.monitor.LastTick(),
		"resynthQueue": map[string]any{
			"depth": stats.Depth, "queued": stats.Queued, "processing": stats.Processing,
			"completed": stats.Completed, "failed": stats.Failed, "dropped": stats.Dropped,
			"drainer": "lease API ready; services/llm/app/thesis_resynth.py drains one bounded " +
				"operator-invoked pass when THESIS_RESYNTH_ENABLED=true",
		},
	})
}

type resynthCompletion struct {
	LeaseToken string `json:"leaseToken"`
	Outcome    string `json:"outcome"` // completed | queued (retry)
	Error      string `json:"error"`
}

func (a *API) internalResynthAuthorized(r *http.Request) bool {
	if _, err := r.Cookie(a.cfg.CookieName); err == nil {
		return false
	}
	want, got := []byte(a.cfg.Secret), []byte(r.Header.Get("X-Internal-Secret"))
	return len(want) == len(got) && subtle.ConstantTimeCompare(want, got) == 1
}

func (a *API) rejectInternalResynth(w http.ResponseWriter, r *http.Request) bool {
	if _, err := r.Cookie(a.cfg.CookieName); err == nil {
		http.NotFound(w, r)
		return true
	}
	if !a.internalResynthAuthorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "internal authentication required"})
		return true
	}
	if a.monitor == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "thesis monitor unavailable"})
		return true
	}
	return false
}

func (a *API) handleResynthLease(w http.ResponseWriter, r *http.Request) {
	if a.rejectInternalResynth(w, r) {
		return
	}
	job, ok := a.monitor.LeaseResynth(time.Now().UTC())
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": job})
}

func (a *API) handleResynthComplete(w http.ResponseWriter, r *http.Request) {
	if a.rejectInternalResynth(w, r) {
		return
	}
	var body resynthCompletion
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON: " + err.Error()})
		return
	}
	if body.Outcome != resynthCompleted && body.Outcome != resynthQueued {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "outcome must be completed or queued"})
		return
	}
	if !a.monitor.CompleteResynth(r.PathValue("id"), body.LeaseToken, body.Outcome, body.Error, time.Now().UTC()) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": "re-synthesis lease is missing, expired, or no longer active"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "queue": a.monitor.QueueStats()})
}

// filterEvents keeps the events satisfying keep, always returning a non-nil slice so the JSON is
// `[]` rather than `null` when nothing matches — an honest empty list, not a missing field.
func filterEvents(in []Event, keep func(Event) bool) []Event {
	out := make([]Event, 0, len(in))
	for _, ev := range in {
		if keep(ev) {
			out = append(out, ev)
		}
	}
	return out
}

// dueReview is one scheduled review that has come due (or is about to), computed deterministically
// from the rule's own fields. No market data is consulted and no model is called.
type dueReview struct {
	RuleID       string        `json:"ruleId"`
	Ticker       string        `json:"ticker"`
	ThesisID     string        `json:"thesisId"`
	ThesisItemID string        `json:"thesisItemId,omitempty"`
	Intent       string        `json:"intent"`
	DueAt        int64         `json:"dueAt"`
	OverdueDays  int           `json:"overdueDays"` // 0 = due today or in the future
	Label        string        `json:"label"`       // composed in code from typed fields
	ResearchLink *ResearchLink `json:"researchLink"`
}

// handleMonitoringDue lists the caller's scheduled reviews that are due within `withinDays`
// (default 0 = due now or overdue). Scoped to the caller by RulesForUser: a guest sees an empty list,
// and one user's reviews are never visible to another.
func (a *API) handleMonitoringDue(w http.ResponseWriter, r *http.Request) {
	within := 0
	if q := r.URL.Query().Get("withinDays"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n >= 0 && n <= 3650 {
			within = n
		}
	}
	thesisFilter := strings.TrimSpace(r.URL.Query().Get("thesisId"))

	now := time.Now().Unix()
	horizon := now + int64(within)*86400

	out := []dueReview{}
	for _, rule := range a.store.RulesForUser(userID(r)) {
		if rule.Type != TypeThesisReview || !rule.Active {
			continue
		}
		if thesisFilter != "" && rule.ThesisID != thesisFilter {
			continue
		}
		due, ok := nextReviewDue(rule)
		if !ok || due > horizon {
			continue
		}
		overdue := 0
		if now > due {
			overdue = int((now - due) / 86400)
		}
		out = append(out, dueReview{
			RuleID: rule.ID, Ticker: rule.Ticker, ThesisID: rule.ThesisID,
			ThesisItemID: rule.ThesisItemID, Intent: rule.Intent,
			DueAt: due, OverdueDays: overdue,
			Label:        reviewLabel(rule, due, overdue),
			ResearchLink: researchLinkFor(rule.Ticker, rule.ThesisID),
		})
	}
	// Most overdue first — the thing that has waited longest is the thing to look at first.
	sort.SliceStable(out, func(i, j int) bool { return out[i].DueAt < out[j].DueAt })
	writeJSON(w, http.StatusOK, map[string]any{"due": out, "asOf": now})
}

// nextReviewDue returns the unix second at which this review rule is next due. ok=false when the
// rule carries neither a one-shot date nor a usable cadence — an unanswerable question is reported
// as "no due date", never as a guessed one.
func nextReviewDue(r Rule) (int64, bool) {
	if r.ReviewAt > 0 {
		return r.ReviewAt, true
	}
	days, err := paramFloat(r.Params, "everyDays")
	if err != nil || days <= 0 {
		return 0, false
	}
	since := r.LastTriggered
	if since == 0 {
		since = r.CreatedAt
	}
	if since == 0 {
		return 0, false
	}
	return since + int64(days*86400), true
}

// reviewLabel composes the display string IN CODE from typed fields, for the same reason §4.4
// requires it of change items: a label a model wrote could drift from what the rule actually says.
func reviewLabel(r Rule, due int64, overdueDays int) string {
	what := "Thesis review"
	if r.Intent == IntentAssumptionReview {
		what = "Assumption review"
	}
	switch {
	case overdueDays == 1:
		return what + " for " + r.Ticker + " — 1 day overdue (due " + isoDay(due) + ")"
	case overdueDays > 1:
		return what + " for " + r.Ticker + " — " + strconv.Itoa(overdueDays) + " days overdue (due " + isoDay(due) + ")"
	default:
		return what + " for " + r.Ticker + " — due " + isoDay(due)
	}
}
