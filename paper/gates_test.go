package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// gates_test.go — the four hard gates (docs/PAPER_EXECUTION_CONTRACT.md §4).
//
// Each case takes the ONE clean /predict response that passes everything, breaks exactly one thing,
// and asserts that (a) nothing is written to the journal and (b) the named gate is the one that
// refused, with a reason a human can act on. Testing them one at a time is the point: a gate that
// only ever fires because another gate fired first is not a gate.

// refuseCase drives one config decision against a mutated fake and returns the recorded decision.
func refuseCase(t *testing.T, mutate func(*fakes), now time.Time) (*Decision, []string) {
	t.Helper()
	f := newFakes(t, now)
	e, store := harness(t, f, t.TempDir())
	f.set(mutate)
	e.evalConfig(context.Background(), testCfg, now)
	return lastDecision(t, store), f.journalCalls()
}

var gateNow = time.Date(2026, 8, 20, 15, 0, 0, 0, time.UTC)

func TestGatesRefuseOneAtATime(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(*fakes)
		gate      string
		wantIn    string
		retryable bool
	}{
		{
			name:      "synthetic quote",
			mutate:    func(f *fakes) { f.quote = map[string]any{"symbol": "NVDA", "price": 100.0, "source": "synthetic"} },
			gate:      "no-synthetic-data",
			wantIn:    "execution quote is synthetic",
			retryable: true,
		},
		{
			name:   "synthetically trained model",
			mutate: func(f *fakes) { f.pred["trainedOnSynthetic"] = true },
			gate:   "no-synthetic-data",
			wantIn: "trained on synthetic data",
		},
		{
			name: "synthetic feature frame",
			mutate: func(f *fakes) {
				f.pred["currentData"] = map[string]any{"source": "synthetic-seed", "synthetic": true}
			},
			gate:   "no-synthetic-data",
			wantIn: "current feature frame is synthetic",
		},
		{
			name:   "unknown feature-frame provenance",
			mutate: func(f *fakes) { f.pred["currentData"] = nil },
			gate:   "no-synthetic-data",
			wantIn: "provenance is unknown",
		},
		{
			name:      "synthetic bar",
			mutate:    func(f *fakes) { f.bar["sourceIsSynthetic"] = true },
			gate:      "no-synthetic-data",
			wantIn:    "latest bar is synthetic",
			retryable: true,
		},
		{
			name:   "stale bar",
			mutate: func(f *fakes) { f.bar = cleanBar(gateNow.AddDate(0, 0, -14)) },
			gate:   "fresh-data",
			wantIn: "sessions behind today",
		},
		{
			name:   "stale model",
			mutate: func(f *fakes) { f.pred["dataThrough"] = gateNow.AddDate(0, 0, -60).Format("2006-01-02") },
			gate:   "fresh-data",
			wantIn: "sessions behind the latest bar",
		},
		{
			name:   "no dataThrough at all",
			mutate: func(f *fakes) { f.pred["dataThrough"] = "" },
			gate:   "fresh-data",
			wantIn: "no dataThrough",
		},
		{
			name:   "backtest not passing",
			mutate: func(f *fakes) { f.pred["backtest"].(map[string]any)["passed"] = false },
			gate:   "backtest-passed",
			wantIn: "has not passed",
		},
		{
			name:   "no backtest report",
			mutate: func(f *fakes) { f.pred["backtest"] = nil },
			gate:   "backtest-passed",
			wantIn: "not trained",
		},
		{
			name:   "missing evaluator verdict",
			mutate: func(f *fakes) { f.pred["evaluation"] = nil },
			gate:   "evaluator-verdict",
			wantIn: "no persisted evaluator verdict",
		},
		{
			name: "non-EDGE verdict",
			mutate: func(f *fakes) {
				f.pred["evaluation"].(map[string]any)["verdict"] = "NO EDGE"
			},
			gate:   "evaluator-verdict",
			wantIn: `is "NO EDGE", not "EDGE"`,
		},
		{
			name: "INCONCLUSIVE is not permission",
			mutate: func(f *fakes) {
				f.pred["evaluation"].(map[string]any)["verdict"] = "INCONCLUSIVE"
			},
			gate:   "evaluator-verdict",
			wantIn: `not "EDGE"`,
		},
		{
			name: "stale strategy version",
			mutate: func(f *fakes) {
				f.pred["evaluation"].(map[string]any)["current"] = false
			},
			gate:   "evaluator-verdict",
			wantIn: "no longer the one this service runs",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, calls := refuseCase(t, tc.mutate, gateNow)
			if len(calls) != 0 {
				t.Fatalf("a refused config must write nothing to the journal, calls = %v", calls)
			}
			if d.Action != "none" {
				t.Errorf("action = %q, want none", d.Action)
			}
			if d.Gate != tc.gate {
				t.Errorf("gate = %q, want %q (reason: %s)", d.Gate, tc.gate, d.Reason)
			}
			if !strings.Contains(d.Reason, tc.wantIn) {
				t.Errorf("reason = %q, want it to contain %q", d.Reason, tc.wantIn)
			}
			// Every gate's verdict rides along, not just the failing one — the status payload shows
			// the whole picture.
			if len(d.Gates) != 4 {
				t.Errorf("expected all four gate results, got %d", len(d.Gates))
			}
		})
	}
}

// A transient refusal (synthetic quote/bar) leaves the bar un-consumed so the engine retries; a
// settled one (synthetic model, stale data, no verdict) consumes it — re-asking gets the same answer.
func TestRefusalRetryabilityDecidesWhetherTheBarIsConsumed(t *testing.T) {
	transient := func(f *fakes) {
		f.quote = map[string]any{"symbol": "NVDA", "price": 100.0, "source": "synthetic"}
	}
	settled := func(f *fakes) { f.pred["evaluation"] = nil }

	for _, tc := range []struct {
		name     string
		mutate   func(*fakes)
		consumed bool
	}{
		{"synthetic quote is retried within the bar", transient, false},
		{"a missing verdict settles the bar", settled, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakes(t, gateNow)
			e, store := harness(t, f, t.TempDir())
			f.set(tc.mutate)
			e.evalConfig(context.Background(), testCfg, gateNow)

			got := store.StateFor(testCfg).LastBarActedOn != ""
			if got != tc.consumed {
				t.Errorf("bar consumed = %v, want %v", got, tc.consumed)
			}
		})
	}
}

// Closing to flat is NOT gated: a gate must never be able to trap an open position.
func TestClosingToFlatIsNeverGated(t *testing.T) {
	f := newFakes(t, gateNow)
	e, store := harness(t, f, t.TempDir())
	ctx := context.Background()

	e.evalConfig(ctx, testCfg, gateNow) // open long through clean gates
	next := gateNow.AddDate(0, 0, 1)
	f.set(func(f *fakes) {
		f.bar = cleanBar(next)
		p := cleanPredict(next, "Hold")
		// Everything a gate could object to, all at once — and none of it may block a close.
		p["trainedOnSynthetic"] = true
		p["evaluation"] = nil
		p["backtest"].(map[string]any)["passed"] = false
		f.pred = p
	})
	e.evalConfig(ctx, testCfg, next)

	if got := f.journalCalls(); len(got) != 2 || got[1] != "PATCH /trades/t1" {
		t.Fatalf("an explicit flat target must always be executable, calls = %v", got)
	}
	if st := store.StateFor(testCfg); st.Side != "" {
		t.Errorf("expected flat, got %q", st.Side)
	}
}

// --------------------------------------------------------------------------- the shipped configs
//
// The three CONFIGS this service ships with, against the model records that are actually on disk
// (services/prediction/data/models/*/record.json, read 2026-08-23). Every one of them is refused.
// That is the intended state, and this test pins the exact reason each one is refused with so a
// future change that starts trading them cannot do so silently.

func TestShippedConfigsAreAllRefusedToday(t *testing.T) {
	// dataThrough and passed as persisted in each record.json.
	records := []struct {
		ticker       string
		passed       bool
		wantGate     string
		wantReasonIn string
	}{
		// NVDA's backtest does not pass, so /predict serves NO SIGNAL at all — the refusal comes
		// from the prediction service before a gate is ever consulted.
		{"NVDA", false, "signal", "no validated signal from /predict"},
		// GOOGL and TSLA pass their own backtests but were fitted on synthetic prices.
		{"GOOGL", true, "no-synthetic-data", "trained on synthetic data"},
		{"TSLA", true, "no-synthetic-data", "trained on synthetic data"},
	}
	for _, rec := range records {
		t.Run(rec.ticker, func(t *testing.T) {
			cfg := PaperCfg{Ticker: rec.ticker, Timeframe: "1D", Horizon: 5}
			f := newFakes(t, gateNow)
			f.set(func(f *fakes) {
				p := cleanPredict(gateNow, "Buy")
				p["ticker"] = rec.ticker
				p["trainedOnSynthetic"] = true  // every record on disk
				p["dataThrough"] = "2026-07-07" // every record on disk
				p["backtest"].(map[string]any)["passed"] = rec.passed
				p["evaluation"] = nil // no verdict has ever been produced
				if !rec.passed {
					p["signal"] = nil
					p["reason"] = "insufficient validation"
				}
				f.pred = p
			})
			e, store := harness(t, f, t.TempDir())
			e.cfg.Configs = []PaperCfg{cfg}
			e.evalConfig(context.Background(), cfg, gateNow)

			if got := f.journalCalls(); len(got) != 0 {
				t.Fatalf("%s must trade nothing today, calls = %v", rec.ticker, got)
			}
			// AND THE BOOK MUST HOLD NOTHING. The ledger (contract §5) keeps score; it must not be
			// able to create anything to score. If a refused config ever showed up as a fill, the
			// accounting layer would be manufacturing the very evidence the gates exist to withhold.
			if fills, err := e.ledger.readFills(0); err != nil || len(fills) != 0 {
				t.Fatalf("%s was refused but the ledger booked %d fills (err %v) — the book must not "+
					"create anything to score", rec.ticker, len(fills), err)
			}
			if eq := e.ledger.Equity(); eq != defaultStartingCash {
				t.Errorf("%s was refused but the book's equity moved to %.2f", rec.ticker, eq)
			}
			if lot := e.ledger.LotFor(cfg.Key()); lot != nil {
				t.Errorf("%s was refused but the book holds a lot: %+v", rec.ticker, lot)
			}
			st := store.StateFor(cfg)
			if st.LastDecision == nil {
				t.Fatal("no decision recorded")
			}
			if st.LastDecision.Gate != rec.wantGate {
				t.Errorf("gate = %q, want %q (reason %q)", st.LastDecision.Gate, rec.wantGate, st.LastDecision.Reason)
			}
			if !strings.Contains(st.LastDecision.Reason, rec.wantReasonIn) {
				t.Errorf("reason = %q, want it to contain %q", st.LastDecision.Reason, rec.wantReasonIn)
			}
		})
	}
}

// --------------------------------------------------------------------------- session arithmetic

func TestWeekdaysBetweenCountsSessionsConservatively(t *testing.T) {
	d := func(s string) time.Time {
		v, err := time.Parse("2006-01-02", s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	cases := []struct {
		from, to string
		want     int
	}{
		{"2026-08-20", "2026-08-20", 0},  // same day
		{"2026-08-21", "2026-08-20", 0},  // never negative
		{"2026-08-20", "2026-08-21", 1},  // Thu -> Fri
		{"2026-08-21", "2026-08-24", 1},  // Fri -> Mon: the weekend is not two sessions
		{"2026-08-20", "2026-08-27", 5},  // one full week
		{"2026-08-20", "2026-09-03", 10}, // two full weeks
	}
	for _, c := range cases {
		if got := weekdaysBetween(d(c.from), d(c.to)); got != c.want {
			t.Errorf("weekdaysBetween(%s, %s) = %d, want %d", c.from, c.to, got, c.want)
		}
	}
}

func TestParseBarTimeAcceptsBothAnalysisShapes(t *testing.T) {
	daily, err := parseBarTime([]byte(`"2026-08-20"`))
	if err != nil || daily.Label != "2026-08-20" {
		t.Fatalf("daily = %+v, err = %v", daily, err)
	}
	intraday, err := parseBarTime([]byte(`1755702000`))
	if err != nil || intraday.Unix != 1755702000 {
		t.Fatalf("intraday = %+v, err = %v", intraday, err)
	}
	if _, err := parseBarTime([]byte(`"not-a-date"`)); err == nil {
		t.Error("an unreadable bar time must be an error, not a zero timestamp that reads as 1970")
	}
}

// --------------------------------------------------------------------------- empty provenance
//
// Gate 1 used to refuse only the LITERAL string "synthetic". Anything else passed — including an
// EMPTY source, which is not a clean provenance but the absence of one. A gate whose whole claim is
// that it fails closed must not have "unknown" as its default-pass case.

func TestEmptyProvenanceFailsClosed(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(*fakes)
		gate      string
		wantIn    string
		retryable bool
	}{
		{
			name:      "quote with an empty source",
			mutate:    func(f *fakes) { f.quote = map[string]any{"symbol": "NVDA", "price": 100.0, "source": ""} },
			gate:      "no-synthetic-data",
			wantIn:    "execution quote carries no source",
			retryable: true,
		},
		{
			name:      "quote with no source field at all",
			mutate:    func(f *fakes) { f.quote = map[string]any{"symbol": "NVDA", "price": 100.0} },
			gate:      "no-synthetic-data",
			wantIn:    "execution quote carries no source",
			retryable: true,
		},
		{
			name:      "bar with an empty source",
			mutate:    func(f *fakes) { f.bar["source"] = "" },
			gate:      "no-synthetic-data",
			wantIn:    "latest bar carries no source",
			retryable: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, calls := refuseCase(t, tc.mutate, gateNow)
			if len(calls) != 0 {
				t.Fatalf("unknown provenance must write nothing, calls = %v", calls)
			}
			if d.Gate != tc.gate || !strings.Contains(d.Reason, tc.wantIn) {
				t.Fatalf("decision = %+v", d)
			}
		})
	}
}

// A NON-EMPTY, non-synthetic source still passes: this narrows nothing except the unknown case.
func TestAnUnfamiliarButNamedSourceStillPasses(t *testing.T) {
	f := newFakes(t, gateNow)
	e, store := harness(t, f, t.TempDir())
	f.set(func(f *fakes) {
		q := cleanQuote(gateNow)
		q["source"] = "some-provider-nobody-has-heard-of"
		f.quote = q
		f.bar["source"] = "another-one"
	})
	e.evalConfig(context.Background(), testCfg, gateNow)

	if got := f.journalCalls(); len(got) != 1 || got[0] != "POST /trades" {
		t.Fatalf("a named, non-synthetic source is clean; calls = %v", got)
	}
	if d := lastDecision(t, store); d.Action != "open" {
		t.Errorf("decision = %+v", d)
	}
}

// --------------------------------------------------------------------------- exits are not exempt
//
// Closing to flat is exempt from the four GATES — a gate must never trap an open position — but not
// from execution-price integrity. A close at an invented or stale price records a P&L that never
// happened, which is worse for the book than a close that has not happened yet.

func TestACloseIsDeferredRatherThanFilledAtASyntheticPrice(t *testing.T) {
	f := newFakes(t, gateNow)
	e, store := harness(t, f, t.TempDir())
	ctx := context.Background()

	e.evalConfig(ctx, testCfg, gateNow) // open long through clean gates
	next := gateNow.AddDate(0, 0, 1)
	f.set(func(f *fakes) {
		f.bar = cleanBar(next)
		f.pred = cleanPredict(next, "Hold") // the target goes flat
		f.quote = map[string]any{"symbol": "NVDA", "price": 100.0, "source": "synthetic"}
	})
	e.evalConfig(ctx, testCfg, next)

	if got := f.journalCalls(); len(got) != 1 {
		t.Fatalf("no exit may be recorded at an invented price, calls = %v", got)
	}
	st := store.StateFor(testCfg)
	if st.Side != "long" {
		t.Errorf("the position is KEPT while the close is deferred, got %q", st.Side)
	}
	if st.LastBarActedOn == barLabel(next) {
		t.Error("a deferred close must leave the bar un-consumed so it is retried")
	}
	d := lastDecision(t, store)
	if !strings.HasPrefix(d.Reason, "close deferred:") || d.Gate != "quote" {
		t.Fatalf("the deferral must be visible in /paper/status, got %+v", d)
	}

	// A real quote later in the same bar completes the close.
	f.set(func(f *fakes) { f.quote = cleanQuote(next) })
	e.evalConfig(ctx, testCfg, next.Add(5*time.Minute))
	if st := store.StateFor(testCfg); st.Side != "" {
		t.Errorf("the deferred close should have completed, got %+v", st)
	}
}

func TestACloseIsDeferredOnAStaleQuote(t *testing.T) {
	f := newFakes(t, gateNow)
	e, store := harness(t, f, t.TempDir())
	ctx := context.Background()

	e.evalConfig(ctx, testCfg, gateNow)
	next := gateNow.AddDate(0, 0, 1)
	f.set(func(f *fakes) {
		f.bar = cleanBar(next)
		f.pred = cleanPredict(next, "Hold")
		q := cleanQuote(next)
		q["asOf"] = "2026-07-01T00:00:00Z"
		f.quote = q
	})
	e.evalConfig(ctx, testCfg, next)

	if st := store.StateFor(testCfg); st.Side != "long" {
		t.Errorf("a stale exit price is not an exit, got %q", st.Side)
	}
	if d := lastDecision(t, store); !strings.Contains(d.Reason, "close deferred") {
		t.Errorf("reason = %q", d.Reason)
	}
}

// --------------------------------------------------------------------------- intraday quote timing
//
// `executionQuoteIssue` compared CALENDAR DAYS for every timeframe. That is right for 1D and wrong
// for everything else: on a 15m frame a quote stamped 09:35 is the same calendar day as a bar that
// closed at 15:45, so a fill from six hours BEFORE the decision passed a check whose entire claim is
// that a fill older than the decision it executes is not a fill (contract §3.5). Intraday now
// compares INSTANTS against the decided bar's END (start + barDuration).

// intradayBar is a bar identified by its START instant, the way the analysis service serves an
// intraday frame (`_fmt_time` emits UNIX seconds).
func intradayBar(start time.Time) *latestBar {
	return &latestBar{
		Time:   barTime{Label: start.UTC().Format(time.RFC3339), Unix: start.UTC().Unix()},
		Source: "tiingo", Close: 100,
	}
}

func quoteAt(at time.Time) *quoteResp {
	p := 100.0
	return &quoteResp{Symbol: "NVDA", Price: &p, Source: "tiingo", AsOf: at.UTC().Format(time.RFC3339)}
}

func TestIntradayQuoteMustBeAtOrAfterTheBarEnd(t *testing.T) {
	start := time.Date(2026, 8, 20, 15, 30, 0, 0, time.UTC) // a 15m bar covering 15:30–15:45
	bar := intradayBar(start)

	cases := []struct {
		name      string
		timeframe string
		quoteAt   time.Time
		wantRefus bool
	}{
		// The bug, exactly: same calendar day, hours before the bar even started.
		{"same-day quote from before the bar", "15m", start.Add(-6 * time.Hour), true},
		{"a quote from INSIDE the bar", "15m", start.Add(7 * time.Minute), true},
		{"a quote one second before the bar ends", "15m", start.Add(15*time.Minute - time.Second), true},
		{"a quote exactly at the bar's end", "15m", start.Add(15 * time.Minute), false},
		{"a quote after the bar's end", "15m", start.Add(30 * time.Minute), false},
		// The same instants under the other intraday frames.
		{"1H: inside the hour", "1H", start.Add(30 * time.Minute), true},
		{"1H: at the hour's end", "1H", start.Add(time.Hour), false},
		{"5m: inside the bar", "5m", start.Add(2 * time.Minute), true},
		{"5m: at the bar's end", "5m", start.Add(5 * time.Minute), false},
		// 1D keeps the DATE-level rule: a daily session has no wall-clock end this service computes,
		// so a quote from later the same day is the right price to reconcile it at.
		{"1D: earlier the same day is fine", "1D", start.Add(-6 * time.Hour), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			issue := executionQuoteIssue(quoteAt(tc.quoteAt), bar, tc.timeframe)
			if tc.wantRefus && issue == "" {
				t.Fatalf("a %s quote at %s must be refused against a bar starting %s",
					tc.timeframe, tc.quoteAt.Format(time.RFC3339), start.Format(time.RFC3339))
			}
			if !tc.wantRefus && issue != "" {
				t.Fatalf("a %s quote at %s must be accepted, got %q",
					tc.timeframe, tc.quoteAt.Format(time.RFC3339), issue)
			}
		})
	}
}

// Fail-closed stays fail-closed: an unparseable asOf, an absent one, and a bar with no instant to
// compare against are all refusals, on every timeframe.
func TestIntradayQuoteTimingStillFailsClosed(t *testing.T) {
	start := time.Date(2026, 8, 20, 15, 30, 0, 0, time.UTC)
	price := 100.0
	for _, tf := range []string{"1D", "1H", "15m", "5m"} {
		if issue := executionQuoteIssue(&quoteResp{Price: &price, Source: "tiingo", AsOf: "not a time"},
			intradayBar(start), tf); issue == "" {
			t.Errorf("%s: an unparseable asOf must be refused", tf)
		}
		if issue := executionQuoteIssue(&quoteResp{Price: &price, Source: "tiingo", AsOf: ""},
			intradayBar(start), tf); issue == "" {
			t.Errorf("%s: a missing asOf must be refused", tf)
		}
	}
	// An intraday bar with no instant cannot be compared against, so it refuses rather than passing.
	noInstant := &latestBar{Time: barTime{Label: "2026-08-20"}, Source: "tiingo", Close: 100}
	if issue := executionQuoteIssue(quoteAt(start.Add(time.Hour)), noInstant, "15m"); issue == "" {
		t.Error("an intraday bar with no instant must refuse, not pass")
	}
}
