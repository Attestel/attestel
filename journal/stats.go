package main

// stats.go — descriptive aggregate statistics over the user's OWN past trades. These summarize
// decisions already made; they are NOT recommendations and contain no buy/sell language.

type ByTicker struct {
	Ticker    string  `json:"ticker"`
	Count     int     `json:"count"`
	WinRate   float64 `json:"winRate"`
	TotalPnl  float64 `json:"totalPnl"`
	AvgPnlPct float64 `json:"avgPnlPct"`
}

type Stats struct {
	// realized (closed trades)
	Count            int        `json:"count"`
	WinRate          float64    `json:"winRate"`
	AvgWinPct        float64    `json:"avgWinPct"`  // mean % of winners
	AvgLossPct       float64    `json:"avgLossPct"` // mean % of losers, as a positive magnitude
	Expectancy       float64    `json:"expectancy"` // winRate*avgWin - lossRate*avgLoss (% per trade)
	TotalRealizedPnl float64    `json:"totalRealizedPnl"`
	AvgRMultiple     *float64   `json:"avgRMultiple"`
	ByTicker         []ByTicker `json:"byTicker"`
	// live
	OpenCount          int     `json:"openCount"`
	TotalUnrealizedPnl float64 `json:"totalUnrealizedPnl"`
	// checklistSplit compares the user's OWN checklist-complete (all-pass) trades vs the rest —
	// descriptive coaching from their record, never a recommendation. Only present once a few
	// checklisted trades exist, so an n=1 doesn't over-claim (docs/BEGINNER.md Piece 3).
	ChecklistSplit *ChecklistSplit `json:"checklistSplit,omitempty"`
}

// minChecklistedForSplit — don't surface the split until at least this many closed trades carry a
// checklist, to avoid a misleading tiny-sample comparison.
const minChecklistedForSplit = 4

type ChecklistBucket struct {
	Count      int     `json:"count"`
	WinRate    float64 `json:"winRate"`
	Expectancy float64 `json:"expectancy"` // % per trade, same definition as Stats.Expectancy
}

type ChecklistSplit struct {
	WithAllPass ChecklistBucket `json:"withAllPass"`
	Without     ChecklistBucket `json:"without"`
}

// ckAcc accumulates one checklist bucket's win/loss stats; bucket() collapses it to the wire shape.
type ckAcc struct {
	count, wins, losses   int
	winPctSum, lossPctSum float64
}

func (a ckAcc) bucket() ChecklistBucket {
	b := ChecklistBucket{Count: a.count}
	if a.count == 0 {
		return b
	}
	b.WinRate = round(float64(a.wins)/float64(a.count), 4)
	avgWin, avgLoss := 0.0, 0.0
	if a.wins > 0 {
		avgWin = a.winPctSum / float64(a.wins)
	}
	if a.losses > 0 {
		avgLoss = -a.lossPctSum / float64(a.losses) // positive magnitude
	}
	lossRate := float64(a.losses) / float64(a.count)
	b.Expectancy = round(b.WinRate*avgWin-lossRate*avgLoss, 2)
	return b
}

// computeStats aggregates a set of computed trade views.
func computeStats(views []TradeView) Stats {
	st := Stats{ByTicker: []ByTicker{}}

	var wins, losses int
	var winPctSum, lossPctSum float64
	var rSum float64
	var rCount int

	// per-ticker accumulators (closed trades only)
	type acc struct {
		count  int
		wins   int
		totPnl float64
		pctSum float64
	}
	byT := map[string]*acc{}
	order := []string{}

	// checklist split accumulators (closed, checklisted trades only)
	var ckWith, ckWithout ckAcc
	ckTotal := 0

	for _, v := range views {
		if v.Exit == nil {
			// open — contributes to the live totals only
			st.OpenCount++
			if v.PnlAbs != nil {
				st.TotalUnrealizedPnl += *v.PnlAbs
			}
			continue
		}
		// closed — realized
		st.Count++
		if v.PnlAbs != nil {
			st.TotalRealizedPnl += *v.PnlAbs
		}
		pct := 0.0
		if v.PnlPct != nil {
			pct = *v.PnlPct
		}
		abs := 0.0
		if v.PnlAbs != nil {
			abs = *v.PnlAbs
		}
		if abs > 0 {
			wins++
			winPctSum += pct
		} else if abs < 0 {
			losses++
			lossPctSum += pct // negative
		}
		if v.RMultiple != nil {
			rSum += *v.RMultiple
			rCount++
		}
		a, ok := byT[v.Ticker]
		if !ok {
			a = &acc{}
			byT[v.Ticker] = a
			order = append(order, v.Ticker)
		}
		a.count++
		a.totPnl += abs
		a.pctSum += pct
		if abs > 0 {
			a.wins++
		}

		// checklist split — bucket by whether the trade's recorded checklist was all-pass
		if v.Checklist != nil {
			ckTotal++
			b := &ckWithout
			if v.Checklist.AllPass {
				b = &ckWith
			}
			b.count++
			if abs > 0 {
				b.wins++
				b.winPctSum += pct
			} else if abs < 0 {
				b.losses++
				b.lossPctSum += pct // negative
			}
		}
	}

	if st.Count > 0 {
		st.WinRate = round(float64(wins)/float64(st.Count), 4)
		st.TotalRealizedPnl = round(st.TotalRealizedPnl, 2)
	}
	if wins > 0 {
		st.AvgWinPct = round(winPctSum/float64(wins), 2)
	}
	if losses > 0 {
		st.AvgLossPct = round(-lossPctSum/float64(losses), 2) // positive magnitude
	}
	// expectancy in % per trade: winRate*avgWin - lossRate*avgLoss
	if st.Count > 0 {
		lossRate := float64(losses) / float64(st.Count)
		st.Expectancy = round(st.WinRate*st.AvgWinPct-lossRate*st.AvgLossPct, 2)
	}
	if rCount > 0 {
		st.AvgRMultiple = f64ptr(round(rSum/float64(rCount), 2))
	}
	st.TotalUnrealizedPnl = round(st.TotalUnrealizedPnl, 2)

	for _, tk := range order {
		a := byT[tk]
		bt := ByTicker{Ticker: tk, Count: a.count, TotalPnl: round(a.totPnl, 2)}
		if a.count > 0 {
			bt.WinRate = round(float64(a.wins)/float64(a.count), 4)
			bt.AvgPnlPct = round(a.pctSum/float64(a.count), 2)
		}
		st.ByTicker = append(st.ByTicker, bt)
	}

	// Only expose the split once enough checklisted trades exist (avoid a misleading n=1).
	if ckTotal >= minChecklistedForSplit {
		st.ChecklistSplit = &ChecklistSplit{WithAllPass: ckWith.bucket(), Without: ckWithout.bucket()}
	}
	return st
}
