package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// analytics_report.go — the internal product-team readout over the self-hosted event log (§6.1).
//
// It is a READ-ONLY report, and deliberately NOT an HTTP route:
//
//   - §6.1 leaves the choice to this step and forbids an Analytics destination in primary navigation;
//   - an internal HTTP surface would need an access-control policy, and D-16 (who may read internal
//     surfaces) is still OPEN. Adding an unauthenticated internal endpoint would reproduce exactly the
//     exposure D-16 exists to close, and choosing an auth policy here would be inventing a decision
//     the register reserves. A local command needs neither.
//
// Run it against the data directory:
//
//	cd journal && go run . -analytics-report                     # excludes synthetic/demo activity
//	cd journal && go run . -analytics-report -include-synthetic   # everything, clearly labelled
//	cd journal && go run . -analytics-report -dir ./data/journal -days 90
//
// Every number states whether synthetic/demo rows were excluded, because a metric that silently
// includes demo activity is worse than no metric.

// readoutOptions configures one run.
type readoutOptions struct {
	IncludeSynthetic bool
	Now              time.Time
	Days             int // 0 = all history
}

type weekCount struct {
	Week   string
	Theses int
}

type retentionBucket struct {
	Week     int // 2, 4, 8, 12
	Eligible int // activated long enough ago for this window to have closed
	Returned int
}

type depthBucket struct {
	Label  string
	Theses int
}

// readout is the computed report. Every field is derived from de-duplicated events; nothing is
// estimated and nothing is combined across the reasoning/P&L boundary (§5.5).
type readout struct {
	GeneratedAt      time.Time
	FromDate         string
	ToDate           string
	IncludeSynthetic bool

	TotalEvents     int
	DuplicateEvents int
	SyntheticEvents int
	CountedEvents   int
	EventCounts     map[string]int

	WeeklyThesesReviewed []weekCount

	StartedActors   int
	ActivatedActors int

	MedianTimeToActivationSec int64
	ActivationSamples         int

	Retention []retentionBucket

	Depth       []depthBucket
	DepthTheses int

	DecisionsRecorded int
	OutcomesReviewed  int

	RatedUseful    int
	RatedNotUseful int
	CitesAccepted  int
	CitesRejected  int
}

// loadAnalyticsEvents reads every {YYYY-MM-DD}.jsonl in the analytics directory. A malformed line is
// skipped rather than failing the report: a partially-written last line must not hide a month of
// measurement.
func loadAnalyticsEvents(dir string) ([]analyticsEvent, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jsonl") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	var out []analyticsEvent
	for _, name := range names {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			var ev analyticsEvent
			if json.Unmarshal([]byte(line), &ev) != nil {
				continue
			}
			if ev.Event == "" || ev.EventID == "" {
				continue
			}
			out = append(out, ev)
		}
		f.Close()
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At < out[j].At })
	return out, nil
}

func isoWeekKey(t time.Time) string {
	y, w := t.UTC().ISOWeek()
	return fmt.Sprintf("%04d-W%02d", y, w)
}

// buildReadout computes the report. De-duplication on eventId happens here — §6.3 makes the READER
// responsible for it, which is what lets the writer stay append-only and coordination-free.
func buildReadout(events []analyticsEvent, opt readoutOptions) readout {
	now := opt.Now
	if now.IsZero() {
		now = time.Now()
	}
	r := readout{
		GeneratedAt:      now.UTC(),
		IncludeSynthetic: opt.IncludeSynthetic,
		EventCounts:      map[string]int{},
	}

	var cutoff int64
	if opt.Days > 0 {
		cutoff = now.UTC().AddDate(0, 0, -opt.Days).Unix()
	}

	seen := map[string]bool{}
	counted := make([]analyticsEvent, 0, len(events))
	for _, ev := range events {
		r.TotalEvents++
		if seen[ev.EventID] {
			r.DuplicateEvents++
			continue
		}
		seen[ev.EventID] = true
		if ev.Synthetic {
			r.SyntheticEvents++
			if !opt.IncludeSynthetic {
				continue
			}
		}
		if cutoff > 0 && ev.At < cutoff {
			continue
		}
		counted = append(counted, ev)
	}
	r.CountedEvents = len(counted)
	if len(counted) > 0 {
		r.FromDate = time.Unix(counted[0].At, 0).UTC().Format("2006-01-02")
		r.ToDate = time.Unix(counted[len(counted)-1].At, 0).UTC().Format("2006-01-02")
	}
	for _, ev := range counted {
		r.EventCounts[ev.Event]++
	}

	// ---- north star: distinct theses reviewed per week ----
	// "Reviewed" = a challenge review completed on it, or the user explicitly marking it reviewed.
	// Both are durable transitions; neither is a page view.
	perWeek := map[string]map[string]bool{}
	for _, ev := range counted {
		if ev.Event != evChallengeReviewCompleted && ev.Event != evThesisReviewed {
			continue
		}
		if ev.ThesisID == "" {
			continue
		}
		wk := isoWeekKey(time.Unix(ev.At, 0))
		if perWeek[wk] == nil {
			perWeek[wk] = map[string]bool{}
		}
		perWeek[wk][ev.ThesisID] = true
	}
	weeks := make([]string, 0, len(perWeek))
	for w := range perWeek {
		weeks = append(weeks, w)
	}
	sort.Strings(weeks)
	for _, w := range weeks {
		r.WeeklyThesesReviewed = append(r.WeeklyThesesReviewed, weekCount{Week: w, Theses: len(perWeek[w])})
	}

	// ---- activation funnel ----
	startedAt := map[string]int64{}   // actor -> first onboarding_started
	activatedAt := map[string]int64{} // actor -> first onboarding_completed
	actorEvents := map[string][]int64{}
	for _, ev := range counted {
		id := ev.Actor.IDHash
		if id == "" {
			continue
		}
		actorEvents[id] = append(actorEvents[id], ev.At)
		switch ev.Event {
		case evOnboardingStarted:
			if prev, ok := startedAt[id]; !ok || ev.At < prev {
				startedAt[id] = ev.At
			}
		case evOnboardingCompleted:
			if prev, ok := activatedAt[id]; !ok || ev.At < prev {
				activatedAt[id] = ev.At
			}
		}
	}
	r.StartedActors = len(startedAt)
	r.ActivatedActors = len(activatedAt)

	// ---- median time to value ----
	var deltas []int64
	for id, done := range activatedAt {
		if start, ok := startedAt[id]; ok && done >= start {
			deltas = append(deltas, done-start)
		}
	}
	sort.Slice(deltas, func(i, j int) bool { return deltas[i] < deltas[j] })
	r.ActivationSamples = len(deltas)
	if n := len(deltas); n > 0 {
		if n%2 == 1 {
			r.MedianTimeToActivationSec = deltas[n/2]
		} else {
			r.MedianTimeToActivationSec = (deltas[n/2-1] + deltas[n/2]) / 2
		}
	}

	// ---- retention of ACTIVATED users (never of all signups) ----
	// Week N means the seven-day window starting (N-1)*7 days after activation. A user is only counted
	// in the denominator once that window has fully closed — otherwise "week 12 retention" would be
	// dragged down by users who activated yesterday.
	const day = int64(86400)
	for _, wk := range []int{2, 4, 8, 12} {
		b := retentionBucket{Week: wk}
		startOff := int64(wk-1) * 7 * day
		endOff := startOff + 7*day
		for id, act := range activatedAt {
			if now.UTC().Unix() < act+endOff {
				continue // window has not closed yet
			}
			b.Eligible++
			for _, at := range actorEvents[id] {
				if at >= act+startOff && at < act+endOff {
					b.Returned++
					break
				}
			}
		}
		r.Retention = append(r.Retention, b)
	}

	// ---- depth: evidence links + challenge reviews per thesis ----
	depth := map[string]int{}
	for _, ev := range counted {
		if ev.ThesisID == "" {
			continue
		}
		switch ev.Event {
		case evEvidenceLinked, evChallengeReviewCompleted:
			depth[ev.ThesisID]++
		}
	}
	buckets := []struct {
		label  string
		lo, hi int
	}{{"1 item", 1, 1}, {"2-3 items", 2, 3}, {"4-6 items", 4, 6}, {"7+ items", 7, 1 << 30}}
	counts := make([]depthBucket, len(buckets))
	for i, b := range buckets {
		counts[i] = depthBucket{Label: b.label}
	}
	for _, n := range depth {
		for i, b := range buckets {
			if n >= b.lo && n <= b.hi {
				counts[i].Theses++
				break
			}
		}
	}
	r.Depth = counts
	r.DepthTheses = len(depth)

	// ---- decision loop, quality ----
	// One outcome review per decision is enforced by the store (§5.4), so this ratio IS the share of
	// decision records later reviewed against outcomes.
	r.DecisionsRecorded = r.EventCounts[evDecisionRecorded]
	r.OutcomesReviewed = r.EventCounts[evOutcomeReviewed]
	for _, ev := range counted {
		if ev.Rating == nil {
			continue
		}
		switch ev.Event {
		case evReviewRated:
			switch *ev.Rating {
			case "useful":
				r.RatedUseful++
			case "not_useful":
				r.RatedNotUseful++
			}
		case evCitationFeedback:
			switch *ev.Rating {
			case "accepted":
				r.CitesAccepted++
			case "rejected":
				r.CitesRejected++
			}
		}
	}
	return r
}

func pct(num, den int) string {
	if den == 0 {
		return "n/a"
	}
	return strconv.FormatFloat(float64(num)*100/float64(den), 'f', 1, 64) + "%"
}

func humanDuration(sec int64) string {
	if sec <= 0 {
		return "n/a"
	}
	d := time.Duration(sec) * time.Second
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%.1fh", d.Hours())
	default:
		return fmt.Sprintf("%.1fd", d.Hours()/24)
	}
}

// renderReadout formats the report. It answers exactly the eight questions §6.1/the Step 11 prompt
// require and stops there — an internal report, not a dashboard product.
func renderReadout(r readout) string {
	var b strings.Builder
	scope := "EXCLUDING synthetic/demo activity"
	if r.IncludeSynthetic {
		scope = "INCLUDING synthetic/demo activity"
	}
	fmt.Fprintf(&b, "Attestel — internal research-loop readout\n")
	fmt.Fprintf(&b, "generated %s · %s\n", r.GeneratedAt.Format(time.RFC3339), scope)
	if r.FromDate != "" {
		fmt.Fprintf(&b, "events %s..%s\n", r.FromDate, r.ToDate)
	}
	fmt.Fprintf(&b, "\nEVENT LOG\n")
	fmt.Fprintf(&b, "  read %d · duplicates dropped %d · synthetic/demo %d · counted %d\n",
		r.TotalEvents, r.DuplicateEvents, r.SyntheticEvents, r.CountedEvents)
	names := make([]string, 0, len(r.EventCounts))
	for k := range r.EventCounts {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(&b, "    %-28s %d\n", n, r.EventCounts[n])
	}

	fmt.Fprintf(&b, "\nNORTH STAR — distinct theses reviewed per week\n")
	if len(r.WeeklyThesesReviewed) == 0 {
		fmt.Fprintf(&b, "  (no thesis reviews recorded)\n")
	}
	for _, w := range r.WeeklyThesesReviewed {
		fmt.Fprintf(&b, "  %s  %d\n", w.Week, w.Theses)
	}

	fmt.Fprintf(&b, "\nACTIVATION (thesis + source-backed evidence + challenge review, all durable)\n")
	fmt.Fprintf(&b, "  started %d · activated %d · conversion %s\n",
		r.StartedActors, r.ActivatedActors, pct(r.ActivatedActors, r.StartedActors))
	fmt.Fprintf(&b, "  median time to activation %s (n=%d)\n",
		humanDuration(r.MedianTimeToActivationSec), r.ActivationSamples)

	fmt.Fprintf(&b, "\nRETENTION of ACTIVATED users (closed windows only)\n")
	for _, bk := range r.Retention {
		fmt.Fprintf(&b, "  week %-2d  %d/%d  %s\n", bk.Week, bk.Returned, bk.Eligible, pct(bk.Returned, bk.Eligible))
	}

	fmt.Fprintf(&b, "\nDEPTH — evidence links + challenge reviews per thesis (%d theses)\n", r.DepthTheses)
	for _, d := range r.Depth {
		fmt.Fprintf(&b, "  %-10s %d\n", d.Label, d.Theses)
	}

	fmt.Fprintf(&b, "\nDECISION LOOP\n")
	fmt.Fprintf(&b, "  decisions %d · outcomes reviewed %d · completion %s\n",
		r.DecisionsRecorded, r.OutcomesReviewed, pct(r.OutcomesReviewed, r.DecisionsRecorded))

	fmt.Fprintf(&b, "\nQUALITY\n")
	fmt.Fprintf(&b, "  lens usefulness   useful %d · not useful %d · %s useful\n",
		r.RatedUseful, r.RatedNotUseful, pct(r.RatedUseful, r.RatedUseful+r.RatedNotUseful))
	fmt.Fprintf(&b, "  citations         accepted %d · rejected %d · %s accepted\n",
		r.CitesAccepted, r.CitesRejected, pct(r.CitesAccepted, r.CitesAccepted+r.CitesRejected))

	if !r.IncludeSynthetic && r.SyntheticEvents > 0 {
		fmt.Fprintf(&b, "\n%d synthetic/demo events were excluded. Re-run with -include-synthetic to see them.\n",
			r.SyntheticEvents)
	}
	return b.String()
}

// runAnalyticsReport is the CLI entry point, called from main() before the server starts. It prints
// and returns; it never serves.
func runAnalyticsReport(args []string, defaultDir, databaseURL, databaseSchema string) int {
	dir, days, includeSynthetic := defaultDir, 0, false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-include-synthetic", "--include-synthetic":
			includeSynthetic = true
		case "-dir", "--dir":
			if i+1 < len(args) {
				i++
				dir = args[i]
			}
		case "-days", "--days":
			if i+1 < len(args) {
				i++
				if n, err := strconv.Atoi(args[i]); err == nil {
					days = n
				}
			}
		}
	}
	var events []analyticsEvent
	var err error
	if databaseURL != "" {
		store, openErr := openPostgresTradeStore(defaultDir, databaseURL, databaseSchema)
		if openErr != nil {
			fmt.Fprintf(os.Stderr, "cannot open PostgreSQL analytics store: %v\n", openErr)
			return 1
		}
		defer store.db.Close()
		docs := newDocumentRepository(store.db, store.schema, defaultDir)
		if err = docs.importAnalyticsFiles(); err == nil {
			events, err = docs.analyticsEvents()
		}
	} else {
		events, err = loadAnalyticsEvents(filepath.Join(dir, analyticsDirName))
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot read analytics events in %s: %v\n", dir, err)
		return 1
	}
	fmt.Print(renderReadout(buildReadout(events, readoutOptions{
		IncludeSynthetic: includeSynthetic,
		Now:              time.Now(),
		Days:             days,
	})))
	return 0
}
