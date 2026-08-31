package main

import (
	"fmt"
	"math"
	"sort"
)

// accounting.go — re-deriving the contract's accounting ATOM from the ledger's own bookkeeping.
//
// docs/PAPER_EXECUTION_CONTRACT.md §5.1 states one formula, and two programs claim to implement it:
//
//	prev[t]     = positions[t-1]                     (0 before the first bar)
//	turnover[t] = |positions[t] - prev[t]|
//	net[t]      = positions[t]*ret_next[t] - (cost_bps/10000)*turnover[t]
//
// Python implements it directly (`backtest.net_returns`). Go does NOT get a second copy of it —
// a second copy would only prove that two translations of one formula agree, which is not the
// question. The question is whether the LEDGER — real fills, real fees on real traded notional,
// real daily marks — is keeping score by that formula. So this file reconstructs the atom out of
// what the ledger actually wrote down:
//
//	net[t] = (position P&L over bar t) / (position market value at the START of bar t)
//	         - SUM over the bar's fills of (fee / traded notional)
//
// Both terms are exact, and that is the whole design:
//
//   - the first term is `signedQty*(p[t+1]-p[t]) / |signedQty*p[t]|` = `pos[t] * ret_next[t]`,
//     for a held lot as well as a fresh one, because the denominator is the CURRENT market value
//     rather than the entry notional;
//   - the second term is `cost_bps/10000` per LEG, and the leg count is the turnover: 1 to open,
//     1 to close, 2 for a flip, 0 for a hold.
//
// If the ledger books a fee at the wrong bar, misses a leg of a flip, sizes against the wrong
// equity, or marks a lot at the wrong price, the derived series stops matching the fixtures. If it
// cannot reproduce the atom from its own bookkeeping, THE LEDGER IS WRONG, NOT THE FIXTURE.
//
// WHERE THE DOLLAR BOOK AND THE ATOM LEGITIMATELY DIFFER (§5.6). The derived RETURN series is the
// atom exactly. The ledger's dollar equity curve is not a rescaling of it, and must not be claimed
// to be: the book charges its exit fee on the notional actually traded (`qty*exit`), where the atom
// charges a flat unit, and it holds a fixed share count through a hold, where compounding the atom
// implies continuous rebalancing. Both differences are second-order — bounded by
// `cost_bps x |return|` per bar — and both are on the side of the REAL book being more literal.
// They are measured, not assumed: `TestTheDollarBookTracksTheAtom` pins the size of the gap.

// markPoint is one date's close for one config — the marking input the atom needs to turn a
// position into a return.
type markPoint struct {
	Date  string
	Price float64
}

// deriveNetReturns reconstructs the per-bar net-return series for ONE config from its fills and the
// marks it was valued at. `marks` must be ascending by date and hold one point per bar; the series
// returned is one shorter, because the last bar has no next close to earn.
//
// Fills whose `Bar` is not one of the mark dates are an error rather than a silent skip: a fill the
// derivation cannot place is a fill whose cost would go uncounted, which is exactly the failure this
// function exists to catch.
func deriveNetReturns(fills []Fill, marks []markPoint) ([]float64, error) {
	if len(marks) < 2 {
		return []float64{}, nil
	}
	idx := make(map[string]int, len(marks))
	for i, m := range marks {
		if m.Price <= 0 {
			return nil, fmt.Errorf("mark for %s is not a price (%v)", m.Date, m.Price)
		}
		idx[m.Date] = i
	}

	byBar := make(map[int][]Fill, len(fills))
	for _, f := range fills {
		i, ok := idx[f.Bar]
		if !ok {
			return nil, fmt.Errorf("fill %d is dated %q, which is not one of the marked bars", f.Seq, f.Bar)
		}
		byBar[i] = append(byBar[i], f)
	}
	for i := range byBar {
		sort.SliceStable(byBar[i], func(a, b int) bool { return byBar[i][a].Seq < byBar[i][b].Seq })
	}

	out := make([]float64, 0, len(marks)-1)
	signedQty := 0.0
	for t := 0; t < len(marks)-1; t++ {
		costTerm := 0.0
		for _, f := range byBar[t] {
			if f.Notional <= 0 {
				return nil, fmt.Errorf("fill %d has no traded notional to charge its fee against", f.Seq)
			}
			// Each leg's fee, expressed as a fraction of its OWN traded notional, is cost_bps/10000
			// by construction — so this sum is cost_bps/10000 x (number of legs) = the turnover term.
			costTerm += f.Fee / f.Notional
			switch f.Kind {
			case fillOpen, fillFlipOpen:
				if f.Position == "short" {
					signedQty = -f.Qty
				} else {
					signedQty = f.Qty
				}
			case fillClose, fillFlipClose:
				signedQty = 0
			default:
				return nil, fmt.Errorf("fill %d has an unknown kind %q", f.Seq, f.Kind)
			}
		}
		ret := 0.0
		if signedQty != 0 {
			base := math.Abs(signedQty * marks[t].Price)
			ret = signedQty * (marks[t+1].Price - marks[t].Price) / base
		}
		out = append(out, ret-costTerm)
	}
	return out, nil
}

// equityFromNet compounds a net-return series exactly as the contract's atom does:
// `equity = cumprod(1 + net)`, starting from 1.
func equityFromNet(net []float64) []float64 {
	out := make([]float64, 0, len(net))
	acc := 1.0
	for _, n := range net {
		acc *= 1.0 + n
		out = append(out, acc)
	}
	return out
}

// countEpisodes counts TRADES the way the contract defines them (§1.2): transitions INTO a nonzero
// position. Nine bars of one held position is one trade; a flip is one close and one open.
func countEpisodes(fills []Fill) int {
	n := 0
	for _, f := range fills {
		if f.Kind == fillOpen || f.Kind == fillFlipOpen {
			n++
		}
	}
	return n
}
