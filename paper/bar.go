package main

import (
	"encoding/json"
	"fmt"
	"time"
)

// bar.go — what a "bar" IS, and how old it is.
//
// A bar is a TRADING SESSION as served by the analysis service, never a wall-clock interval
// (docs/PAPER_EXECUTION_CONTRACT.md §2). Weekends and holidays simply produce no bar. Everything the
// engine does about time is a comparison between two BAR TIMESTAMPS; the only place `now` appears is
// the freshness gate, which asks how far behind today the newest available bar is.
//
// This file replaces `Config.barSeconds`, which counted CALENDAR days for 1D (86400s) and so treated
// a Saturday as a bar. Nothing derives a position lifetime from a duration any more.

// barTime is one bar's identity: the label the analysis service serves (daily "YYYY-MM-DD",
// intraday UNIX seconds) plus a comparable instant. Comparison is on Unix; Label is what gets
// persisted and displayed, so the journal and the status payload name the bar the way the chart does.
type barTime struct {
	Label string `json:"label"`
	Unix  int64  `json:"unix"`
}

// parseBarTime accepts BOTH shapes the analysis service emits from `_fmt_time`: a "YYYY-MM-DD"
// string for daily frames and a UNIX-seconds number for intraday ones.
func parseBarTime(raw json.RawMessage) (barTime, error) {
	if len(raw) == 0 {
		return barTime{}, fmt.Errorf("bar has no time")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		t, err := time.Parse("2006-01-02", s)
		if err != nil {
			return barTime{}, fmt.Errorf("unrecognised bar time %q", s)
		}
		return barTime{Label: s, Unix: t.UTC().Unix()}, nil
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err == nil {
		t := time.Unix(int64(n), 0).UTC()
		return barTime{Label: t.Format(time.RFC3339), Unix: int64(n)}, nil
	}
	return barTime{}, fmt.Errorf("unrecognised bar time %s", string(raw))
}

// day truncates an instant to its UTC date.
func day(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

// weekdaysBetween counts weekday dates in (from, to] — 0 when `to` is not after `from`.
//
// This is a deliberately CONSERVATIVE approximation of trading sessions: it counts market holidays
// as sessions, so it declares data stale slightly EARLY. Failing closed a day sooner is the right
// direction to be wrong in; a real session calendar is not worth a dependency here (and this service
// is standard-library-only Go).
func weekdaysBetween(from, to time.Time) int {
	from, to = day(from), day(to)
	if !to.After(from) {
		return 0
	}
	days := int(to.Sub(from).Hours() / 24)
	n := (days / 7) * 5 // any 7 consecutive days contain exactly 5 weekdays
	d := from.AddDate(0, 0, (days/7)*7)
	for i := 0; i < days%7; i++ {
		d = d.AddDate(0, 0, 1)
		if wd := d.Weekday(); wd != time.Saturday && wd != time.Sunday {
			n++
		}
	}
	return n
}

// sessionsBehind is weekdaysBetween expressed over two bar/record dates.
func sessionsBehind(older, newer time.Time) int { return weekdaysBetween(older, newer) }

// parseDate reads the "YYYY-MM-DD" dates the prediction service serves in `dataThrough`. It also
// accepts a full RFC3339 timestamp, because `dataThrough` is `str(df.index[-1])` and an intraday
// frame's index is not a bare date.
func parseDate(s string) (time.Time, bool) {
	for _, layout := range []string{"2006-01-02", time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// --- bar COMPLETENESS (contract §2) -------------------------------------------------------------
//
// "Strictly newer than the last bar acted on" is necessary but not sufficient. `/candles?limit=1`
// serves the newest bar the provider has, and for the CURRENT session that bar is still FORMING:
// its close is not the close, and its timestamp is already strictly newer than yesterday's. Acting
// on it decides on a price that has not happened yet and then never re-decides, because the cursor
// has moved past it.
//
// A bar is complete when the session it names is over:
//   * daily  — the current UTC date is strictly after the bar's date;
//   * intraday — now is at or past barStart + the timeframe's duration.
//
// This is the ONLY legitimate per-timeframe seconds arithmetic in the service. It judges whether a
// bar has FINISHED; it never derives a position lifetime, a hold, or an exit. `barSeconds` as a
// position lifetime is what the contract removed and it is not coming back.

// barDuration is the wall-clock length of one intraday bar. 1D returns 0: a daily session is not a
// fixed number of hours, so completeness is judged by date, not by duration.
func barDuration(timeframe string) time.Duration {
	switch timeframe {
	case "1H":
		return time.Hour
	case "15m":
		return 15 * time.Minute
	case "5m":
		return 5 * time.Minute
	default:
		return 0
	}
}

// barComplete reports whether `bar` names a session that has finished as of `now`.
//
// Fail-closed: an unreadable bar timestamp is NOT complete. "We cannot tell whether this session is
// over" must never resolve to "act on it".
func barComplete(bar *latestBar, timeframe string, now time.Time) bool {
	if bar == nil {
		return false
	}
	if d := barDuration(timeframe); d > 0 {
		start := time.Unix(bar.Time.Unix, 0).UTC()
		return !now.UTC().Before(start.Add(d))
	}
	barDate, ok := barDateOf(bar)
	if !ok {
		return false
	}
	return day(now).After(day(barDate))
}

// barDateOf is the bar's own date, from its label when it is a date and from its instant otherwise.
func barDateOf(bar *latestBar) (time.Time, bool) {
	if bar == nil {
		return time.Time{}, false
	}
	if t, ok := parseDate(bar.Time.Label); ok {
		return t, true
	}
	if bar.Time.Unix > 0 {
		return time.Unix(bar.Time.Unix, 0).UTC(), true
	}
	return time.Time{}, false
}
