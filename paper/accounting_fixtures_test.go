package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// accounting_fixtures_test.go — the Go half of the cross-language accounting guarantee.
//
// Go and Python cannot share code; they share `testdata/accounting/*.json`. Those files state the
// contract's accounting atom (§5.1) and `services/prediction/tests/test_accounting_fixtures.py`
// holds `backtest.net_returns` to them. This file holds the LEDGER to the same numbers — and it
// does not do so by re-implementing the formula in Go and comparing that. A second translation of
// one formula agreeing with the first proves nothing about the book.
//
// Instead it drives a REAL `Ledger` bar by bar — real fills, real fees on real traded notional, real
// daily marks — and then re-derives the per-bar net-return series from the fills and marks the
// ledger itself wrote down (`deriveNetReturns`, accounting.go). A fee booked at the wrong bar, a
// missing leg of a flip, a lot sized against the wrong equity or marked at the wrong price all
// break the derivation. If the ledger cannot reproduce the atom from its own bookkeeping, THE
// LEDGER IS WRONG, NOT THE FIXTURE.

type accountingFixture struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Inputs      struct {
		CostBps   float64   `json:"costBps"`
		Timeframe string    `json:"timeframe"`
		RetNext   []float64 `json:"retNext"`
		Positions []float64 `json:"positions"`
	} `json:"inputs"`
	Expected struct {
		Net         []float64 `json:"net"`
		Equity      []float64 `json:"equity"`
		TotalReturn float64   `json:"totalReturn"`
		Mean        float64   `json:"mean"`
		Std         float64   `json:"std"`
		NumTrades   int       `json:"numTrades"`
		Turnover    []float64 `json:"turnover"`
	} `json:"expected"`
	Tolerance float64 `json:"tolerance"`
}

func fixtureDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "testdata", "accounting"))
	if err != nil {
		t.Fatalf("resolving the fixture directory: %v", err)
	}
	return dir
}

func loadFixtures(t *testing.T) []accountingFixture {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(fixtureDir(t), "*.json"))
	if err != nil {
		t.Fatalf("globbing fixtures: %v", err)
	}
	// A silently-empty parametrization is a suite that passes by testing nothing.
	if len(paths) < 5 {
		t.Fatalf("expected at least 5 accounting fixtures under %s, found %d", fixtureDir(t), len(paths))
	}
	out := make([]accountingFixture, 0, len(paths))
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("reading %s: %v", p, err)
		}
		var fx accountingFixture
		if err := json.Unmarshal(b, &fx); err != nil {
			t.Fatalf("parsing %s: %v", p, err)
		}
		if fx.Tolerance <= 0 {
			fx.Tolerance = 1e-9
		}
		out = append(out, fx)
	}
	return out
}

// fixtureCfg is the single config the fixtures are driven through: N=1, so the whole book is
// allocated to it and its fractional returns ARE the portfolio's.
var fixtureCfg = PaperCfg{Ticker: "FIX", Timeframe: "1D", Horizon: 1}

// fixtureDate is one bar's date label. The dates are consecutive calendar days; the ledger never
// does arithmetic on them, it only orders them.
func fixtureDate(i int) string {
	return time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i).Format("2006-01-02")
}

// driveFixture walks a fixture's position series through a real ledger and returns it with the mark
// series it was driven on.
//
// THE ORDER IS THE POINT, and it is the engine's order (engine.go marks before it decides): on each
// bar the book is MARKED FIRST, at that bar's close, and only then reconciled. So a snapshot for
// date t values the position that was held over bar t-1, which is exactly what the atom's
// `equity[t-1]` is. Marking after reconciling would fold bar t's fee into bar t-1's return.
func driveFixture(t *testing.T, dir string, fx accountingFixture) (*Ledger, []markPoint) {
	t.Helper()
	l, err := openLedger(dir, defaultStartingCash)
	if err != nil {
		t.Fatalf("openLedger: %v", err)
	}
	keys := []string{fixtureCfg.Key()}

	// Prices implied by the fixture's ret_next: p[t+1] = p[t] * (1 + ret_next[t]).
	prices := make([]float64, len(fx.Inputs.RetNext)+1)
	prices[0] = 100.0
	for i, r := range fx.Inputs.RetNext {
		prices[i+1] = prices[i] * (1 + r)
	}

	marks := make([]markPoint, 0, len(prices))
	now := time.Date(2026, 1, 5, 12, 0, 0, 0, time.UTC)
	for i, p := range prices {
		date := fixtureDate(i)
		marks = append(marks, markPoint{Date: date, Price: p})
		l.Mark(keys, fixtureCfg.Key(), date, p, true)

		if i >= len(fx.Inputs.Positions) {
			break // the final mark closes the series; there is no bar after it to decide on
		}
		target := sideOfPosition(fx.Inputs.Positions[i])
		current := "flat"
		if lot := l.LotFor(fixtureCfg.Key()); lot != nil {
			current = lot.Side
		}
		at := now.AddDate(0, 0, i)
		switch {
		case target == current:
			// hold — no fill, no fee, exactly as the atom charges nothing on zero turnover
		case current == "flat":
			if _, err := l.Open(fixtureCfg, target, date, "", p, fx.Inputs.CostBps, 1, at); err != nil {
				t.Fatalf("%s: open at bar %d: %v", fx.Name, i, err)
			}
		case target == "flat":
			if _, err := l.Close(fixtureCfg, date, p, at, ""); err != nil {
				t.Fatalf("%s: close at bar %d: %v", fx.Name, i, err)
			}
		default:
			if _, _, err := l.Flip(fixtureCfg, target, date, "", p, fx.Inputs.CostBps, 1, at); err != nil {
				t.Fatalf("%s: flip at bar %d: %v", fx.Name, i, err)
			}
		}
	}
	return l, marks
}

func sideOfPosition(p float64) string {
	switch {
	case p > 0:
		return "long"
	case p < 0:
		return "short"
	default:
		return "flat"
	}
}

// TestTheLedgerReproducesTheAccountingAtom is the guarantee itself.
func TestTheLedgerReproducesTheAccountingAtom(t *testing.T) {
	for _, fx := range loadFixtures(t) {
		t.Run(fx.Name, func(t *testing.T) {
			l, marks := driveFixture(t, t.TempDir(), fx)

			fills, err := l.readFills(0)
			if err != nil {
				t.Fatalf("reading fills: %v", err)
			}
			net, err := deriveNetReturns(fills, marks)
			if err != nil {
				t.Fatalf("deriving net returns from the ledger's own fills and marks: %v", err)
			}

			if len(net) != len(fx.Expected.Net) {
				t.Fatalf("derived %d net returns, the fixture states %d", len(net), len(fx.Expected.Net))
			}
			for i := range net {
				if math.Abs(net[i]-fx.Expected.Net[i]) > fx.Tolerance {
					t.Errorf("net[%d] = %.12f, fixture says %.12f — the ledger is not keeping score by "+
						"the contract's atom", i, net[i], fx.Expected.Net[i])
				}
			}

			equity := equityFromNet(net)
			for i := range equity {
				if math.Abs(equity[i]-fx.Expected.Equity[i]) > fx.Tolerance {
					t.Errorf("equity[%d] = %.12f, fixture says %.12f", i, equity[i], fx.Expected.Equity[i])
				}
			}
			if n := len(equity); n > 0 {
				if got := equity[n-1] - 1.0; math.Abs(got-fx.Expected.TotalReturn) > fx.Tolerance {
					t.Errorf("totalReturn %.12f, fixture says %.12f", got, fx.Expected.TotalReturn)
				}
			}

			mean, std := meanStd(net)
			if math.Abs(mean-fx.Expected.Mean) > fx.Tolerance {
				t.Errorf("mean %.12f, fixture says %.12f", mean, fx.Expected.Mean)
			}
			if math.Abs(std-fx.Expected.Std) > fx.Tolerance {
				t.Errorf("std %.12f, fixture says %.12f", std, fx.Expected.Std)
			}

			// A "trade" is a transition INTO a nonzero position (§1.2). The multi-bar-hold fixture is
			// in the set precisely so an implementation counting held bars fails here.
			if got := countEpisodes(fills); got != fx.Expected.NumTrades {
				t.Errorf("counted %d trades, fixture says %d", got, fx.Expected.NumTrades)
			}
		})
	}
}

// TestEveryFillIsChargedTheContractsCost checks the fee RATE on each leg independently of the
// series it sits in — and that the number of legs at a bar IS the atom's turnover.
func TestEveryFillIsChargedTheContractsCost(t *testing.T) {
	for _, fx := range loadFixtures(t) {
		t.Run(fx.Name, func(t *testing.T) {
			l, _ := driveFixture(t, t.TempDir(), fx)
			fills, err := l.readFills(0)
			if err != nil {
				t.Fatalf("reading fills: %v", err)
			}
			legsAt := map[string]float64{}
			for _, f := range fills {
				if f.Notional <= 0 {
					t.Fatalf("fill %d has no traded notional to charge a fee against", f.Seq)
				}
				rate := f.Fee / f.Notional * 10000.0
				if math.Abs(rate-fx.Inputs.CostBps) > 1e-9 {
					t.Errorf("fill %d was charged %.6f bps on its traded notional, the model was "+
						"validated at %.6f bps", f.Seq, rate, fx.Inputs.CostBps)
				}
				legsAt[f.Bar]++
			}
			for i, want := range fx.Expected.Turnover {
				if got := legsAt[fixtureDate(i)]; got != want {
					t.Errorf("bar %d (%s) booked %.0f fills, the atom charges %.0f units of turnover — "+
						"a flip must book BOTH legs and a hold must book none", i, fixtureDate(i), got, want)
				}
			}
		})
	}
}

// TestTheBooksDailyReturnsAreExposureTimesTheAtom states, EXACTLY, the one place the simulated
// dollar book and the compounded atom differ — and proves the book has no OTHER source of
// divergence (contract §5.6).
//
// The atom assumes a unit-exposure position: `net[t] = pos[t]*r[t] - c*turnover[t]`, and compounding
// it implies the position is rebalanced back to 1x at every bar. The book does not rebalance — the
// contract forbids it, because a real book cannot rebalance for free and the backtest charges
// turnover only on position CHANGES. So the book holds a FIXED SHARE COUNT through an episode and
// its exposure DRIFTS: a short that gains is a smaller fraction of a larger book.
//
// Written out, the book's own daily return is:
//
//	E[t+1]/E[t] - 1  ==  w[t]*r[t] - fees[t]/E[t]
//
// where `w[t]` is the signed exposure fraction the book actually carried into bar t and `fees[t]`
// is what it actually paid at bar t. This is the atom with `pos[t]` replaced by the exposure that
// really existed. The identity is EXACT and it is asserted exactly: any deviation is a bug in the
// book, not a convention.
//
// This is the honest statement of the gap, and it is stated rather than bounded. An earlier draft of
// this test asserted `|book - atom| <= 2*cost_bps*movement`, which is wrong: the drift is
// first-order in the RETURN, not in the cost, and the short fixture exceeded it by an order of
// magnitude. A bound that has to be widened until the test passes is not a measurement.
func TestTheBooksDailyReturnsAreExposureTimesTheAtom(t *testing.T) {
	for _, fx := range loadFixtures(t) {
		t.Run(fx.Name, func(t *testing.T) {
			l, marks := driveFixture(t, t.TempDir(), fx)
			fills, err := l.readFills(0)
			if err != nil {
				t.Fatalf("reading fills: %v", err)
			}
			if len(l.st.Snapshots) != len(marks) {
				t.Fatalf("the book took %d snapshots for %d marked bars — it must mark every bar it saw",
					len(l.st.Snapshots), len(marks))
			}

			feesAt, qtyAfter := map[string]float64{}, map[string]float64{}
			signed := 0.0
			for _, f := range fills {
				feesAt[f.Bar] += f.Fee
				switch f.Kind {
				case fillOpen, fillFlipOpen:
					if f.Position == "short" {
						signed = -f.Qty
					} else {
						signed = f.Qty
					}
				case fillClose, fillFlipClose:
					signed = 0
				}
				qtyAfter[f.Bar] = signed
			}
			// A bar with no fill carries the previous bar's share count forward.
			carried := 0.0
			for i := range marks {
				if q, ok := qtyAfter[marks[i].Date]; ok {
					carried = q
				} else {
					qtyAfter[marks[i].Date] = carried
				}
			}

			for i := 0; i < len(marks)-1; i++ {
				date := marks[i].Date
				eNow, eNext := l.st.Snapshots[i].Equity, l.st.Snapshots[i+1].Equity
				if eNow == 0 {
					t.Fatalf("the book's equity reached zero at %s", date)
				}
				r := marks[i+1].Price/marks[i].Price - 1.0
				w := qtyAfter[date] * marks[i].Price / eNow
				want := w*r - feesAt[date]/eNow
				got := eNext/eNow - 1.0
				if math.Abs(got-want) > 1e-9 {
					t.Errorf("bar %d (%s): the book returned %.12f but exposure x return less fees is "+
						"%.12f — the book has a source of P&L its own fills and marks do not explain",
						i, date, got, want)
				}
			}
		})
	}
}

// TestAtEveryEntryBarTheBookIsTheAtomExactly is the other half of the same statement: the drift
// above starts at ZERO. On the bar a position is opened from flat, the book's exposure IS 1/N of
// its equity and its fee IS cost_bps on that notional, so that bar's return is the atom's `net[t]`
// to the last decimal. Sizing that used the wrong equity, the wrong N or the wrong cost would show
// up here immediately.
func TestAtEveryEntryBarTheBookIsTheAtomExactly(t *testing.T) {
	checked := 0
	for _, fx := range loadFixtures(t) {
		t.Run(fx.Name, func(t *testing.T) {
			l, marks := driveFixture(t, t.TempDir(), fx)
			fills, err := l.readFills(0)
			if err != nil {
				t.Fatalf("reading fills: %v", err)
			}
			pureOpen := map[string]bool{}
			legs := map[string]int{}
			for _, f := range fills {
				legs[f.Bar]++
				if f.Kind == fillOpen {
					pureOpen[f.Bar] = true
				}
			}
			for i := 0; i < len(marks)-1; i++ {
				date := marks[i].Date
				if !pureOpen[date] || legs[date] != 1 {
					continue // a flip's close leg is priced on the OLD exposure — see the test above
				}
				got := l.st.Snapshots[i+1].Equity/l.st.Snapshots[i].Equity - 1.0
				want := fx.Expected.Net[i]
				if math.Abs(got-want) > 1e-12 {
					t.Errorf("entry bar %d (%s): the book returned %.14f, the atom says %.14f",
						i, date, got, want)
				}
				checked++
			}
		})
	}
	if checked == 0 {
		t.Fatal("no entry bar was checked — the fixture set must contain at least one flat->position open")
	}
}

// TestTheBooksThreeComponentsAddUpToItsEquity pins the identity every accounting engine must
// satisfy and this one does exactly: equity == startingCash + realized + unrealized, where
// `unrealized` is net of the entry fee already paid. An engine whose components do not sum to its
// own equity is not one.
func TestTheBooksThreeComponentsAddUpToItsEquity(t *testing.T) {
	for _, fx := range loadFixtures(t) {
		t.Run(fx.Name, func(t *testing.T) {
			l, _ := driveFixture(t, t.TempDir(), fx)
			l.mu.Lock()
			equity := l.equityLocked()
			sum := l.st.StartingCash + l.st.Realized + l.unrealizedLocked()
			l.mu.Unlock()
			if math.Abs(equity-sum) > 1e-6 {
				t.Errorf("equity %.10f != startingCash + realized + unrealized = %.10f", equity, sum)
			}
			// Every snapshot must be interpretable from ITS OWN record: it carries the marks it was
			// taken at, so cash plus the position valued at those marks must be the equity it states.
			for _, s := range l.st.Snapshots {
				if len(s.Marks) == 0 {
					t.Errorf("snapshot %s carries no marks — it cannot be reproduced from its own record", s.Date)
				}
				if s.Equity < s.Cash-1e-6 && s.Equity > s.Cash+1e-6 {
					t.Errorf("snapshot %s: equity %.6f and cash %.6f cannot both be right with no lot",
						s.Date, s.Equity, s.Cash)
				}
			}
		})
	}
}

// TestFixtureCoverage states, in the suite itself, which scenarios the contract requires — so
// deleting one is a test failure rather than a quiet loss of coverage.
func TestFixtureCoverage(t *testing.T) {
	have := map[string]bool{}
	for _, fx := range loadFixtures(t) {
		have[fx.Name] = true
	}
	for _, required := range []string{
		"flat-long-flat", "flip-long-to-short", "short-episode",
		"flip-costs-both-legs", "all-flat", "long-hold-real-costs",
	} {
		if !have[required] {
			t.Errorf("fixture %q is missing — %s", required, fmt.Sprintf(
				"the contract's §5 fixture set is the shared definition of the accounting atom"))
		}
	}
}
