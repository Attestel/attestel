package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
	"time"
)

// trades.go — the Trade data model, validation, and the P&L / R-multiple / holding-period math.
//
// This is a RECORD of trades the user made themselves. It computes descriptive statistics about
// past decisions. It executes nothing, connects to no broker, and moves no money — ever.

const dateLayout = "2006-01-02"

var allowedTimeframes = map[string]bool{"1D": true, "1H": true, "15m": true, "5m": true}

// Entry / Exit / AttachedRead are the nested records of a trade.
type Entry struct {
	Date      string  `json:"date"`
	Price     float64 `json:"price"`
	Size      float64 `json:"size"`
	Timeframe string  `json:"timeframe"`
}

type Exit struct {
	Date  string  `json:"date"`
	Price float64 `json:"price"`
}

// AttachedRead is the analytical read that was live at entry (optional snapshot). Stored verbatim
// so the review moment — thesis-at-entry vs. actual outcome — is always available.
type AttachedRead struct {
	Structured map[string]any `json:"structured"`
	Regime     map[string]any `json:"regime"`
	CapturedAt string         `json:"capturedAt"`
}

// RiskPlan is the position-sizing decision recorded at entry (docs/BEGINNER.md Piece 2). It is a
// RECORD of the plan the user sized to — never a live number and never used to compute P&L. Optional
// and backward-compatible: trades created before this field (and manual trades) simply omit it.
type RiskPlan struct {
	AccountEquity float64  `json:"accountEquity"`
	RiskPct       float64  `json:"riskPct"`
	DollarRisk    float64  `json:"dollarRisk"`
	RR            *float64 `json:"rr,omitempty"`
}

// ChecklistItem / Checklist record the pre-trade checklist result at entry (docs/BEGINNER.md Piece 3).
// Like Thesis/Tags it is the user's OWN record — the client's self-attest answers and auto-eval are
// trusted and kept as recorded. It scores plan completeness; it is never a verdict and never feeds
// P&L. Optional + backward-compatible: old and manual trades simply omit it.
type ChecklistItem struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Passed bool   `json:"passed"`
	Auto   bool   `json:"auto"`
	Source string `json:"source"`
}

type Checklist struct {
	Items   []ChecklistItem `json:"items"`
	AllPass bool            `json:"allPass"`
	Mode    string          `json:"mode"` // beginner | standard
}

// AttachedSignal is the quant signal that was live at entry (for signal-originated trades — e.g.
// the paper-trading engine). Snapshotted so a trade's outcome can be judged against the exact call
// (and its backtest) that opened it.
type AttachedSignal struct {
	Direction  string         `json:"direction"`
	ProbUp     float64        `json:"probUp"`
	Confidence float64        `json:"confidence"`
	Horizon    int            `json:"horizon"`
	Backtest   map[string]any `json:"backtest"` // hitRate/sharpe/expectancy/numTrades/suspect/passed
	CapturedAt string         `json:"capturedAt"`
}

// Trade is the persisted record. Computed fields (pnl, rMultiple, holdingDays) are NOT stored here;
// they're derived on read in TradeView so they never go stale.
type Trade struct {
	ID           string        `json:"id"`
	Ticker       string        `json:"ticker"`
	Side         string        `json:"side"`   // long | short
	Status       string        `json:"status"` // open | closed (derived from exit)
	Entry        Entry         `json:"entry"`
	Stop         *float64      `json:"stop,omitempty"`
	Target       *float64      `json:"target,omitempty"`
	Exit         *Exit         `json:"exit,omitempty"`
	Thesis       string        `json:"thesis"`
	Tags         []string      `json:"tags"`
	AttachedRead *AttachedRead `json:"attachedRead,omitempty"`
	// Risk is the optional position-sizing plan the trade was sized to (docs/BEGINNER.md Piece 2).
	// Backward-compatible: absent on old records and manual trades, and never fed into P&L.
	Risk *RiskPlan `json:"risk,omitempty"`
	// Checklist is the optional pre-trade checklist result (docs/BEGINNER.md Piece 3), same contract:
	// backward-compatible, trusted-as-recorded, never fed into P&L.
	Checklist *Checklist `json:"checklist,omitempty"`
	// mode keeps SIMULATED paper trades strictly separate from real/live trades — paper P&L must
	// never contaminate live stats. origin says whether a human or a signal opened it.
	Mode           string          `json:"mode"`   // paper | live (default live)
	Origin         string          `json:"origin"` // signal | manual (default manual)
	AttachedSignal *AttachedSignal `json:"attachedSignal,omitempty"`
	CreatedAt      int64           `json:"createdAt"`
	UpdatedAt      int64           `json:"updatedAt"`
}

// effectiveMode treats a trade with no mode (pre-dating this field) as "live", so old records and
// manual UI trades default to real/live and are never counted as paper.
func effectiveMode(m string) string {
	if m == "" {
		return "live"
	}
	return m
}

// TradeView is a Trade plus the fields computed at read time.
type TradeView struct {
	Trade
	PnlAbs        *float64 `json:"pnlAbs"`
	PnlPct        *float64 `json:"pnlPct"`
	RMultiple     *float64 `json:"rMultiple"`
	HoldingDays   *int     `json:"holdingDays"`
	Unrealized    bool     `json:"unrealized"`                // true when P&L is marked to a live quote
	MarkPrice     *float64 `json:"markPrice,omitempty"`       // current price used for an open trade
	MarkSource    string   `json:"markSource,omitempty"`      // quote source (e.g. synthetic, alpaca:iex)
	MarkSynthetic bool     `json:"markIsSynthetic,omitempty"` // true when the mark is synthetic seed data
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func normalizeTimeframe(tf string) string {
	if allowedTimeframes[tf] {
		return tf
	}
	switch strings.ToLower(strings.TrimSpace(tf)) {
	case "1h", "hour":
		return "1H"
	case "15min":
		return "15m"
	case "5min":
		return "5m"
	default:
		return "1D"
	}
}

// validateTrade checks required fields and normalizes the trade in place. Status is derived from
// the presence of a valid exit.
func validateTrade(t *Trade) error {
	t.Ticker = strings.ToUpper(strings.TrimSpace(t.Ticker))
	if t.Ticker == "" {
		return fmt.Errorf("ticker is required")
	}
	if t.Side != "long" && t.Side != "short" {
		return fmt.Errorf("side must be \"long\" or \"short\"")
	}
	if t.Entry.Price <= 0 {
		return fmt.Errorf("entry.price must be > 0")
	}
	if t.Entry.Size <= 0 {
		return fmt.Errorf("entry.size must be > 0")
	}
	if _, err := time.Parse(dateLayout, t.Entry.Date); err != nil {
		return fmt.Errorf("entry.date must be YYYY-MM-DD")
	}
	t.Entry.Timeframe = normalizeTimeframe(t.Entry.Timeframe)
	if t.Stop != nil && *t.Stop <= 0 {
		return fmt.Errorf("stop must be > 0")
	}
	if t.Target != nil && *t.Target <= 0 {
		return fmt.Errorf("target must be > 0")
	}
	// Risk is optional plan metadata (accepted-if-present). Sanity-check it but never derive P&L from
	// it; a nil Risk (old + manual trades) is left untouched.
	if t.Risk != nil {
		if t.Risk.RiskPct <= 0 || t.Risk.RiskPct > 2 {
			return fmt.Errorf("risk.riskPct must be in (0, 2]")
		}
		if t.Risk.AccountEquity < 0 {
			return fmt.Errorf("risk.accountEquity must be >= 0")
		}
		if t.Risk.DollarRisk < 0 {
			return fmt.Errorf("risk.dollarRisk must be >= 0")
		}
	}
	if t.Tags == nil {
		t.Tags = []string{}
	}
	t.Mode = effectiveMode(strings.ToLower(strings.TrimSpace(t.Mode)))
	if t.Mode != "paper" && t.Mode != "live" {
		return fmt.Errorf("mode must be \"paper\" or \"live\"")
	}
	t.Origin = strings.ToLower(strings.TrimSpace(t.Origin))
	if t.Origin == "" {
		t.Origin = "manual"
	}
	if t.Origin != "signal" && t.Origin != "manual" {
		return fmt.Errorf("origin must be \"signal\" or \"manual\"")
	}
	if t.Exit != nil {
		if t.Exit.Price <= 0 {
			return fmt.Errorf("exit.price must be > 0")
		}
		if _, err := time.Parse(dateLayout, t.Exit.Date); err != nil {
			return fmt.Errorf("exit.date must be YYYY-MM-DD")
		}
		t.Status = "closed"
	} else {
		t.Status = "open"
	}
	return nil
}

func f64ptr(v float64) *float64 { return &v }
func intptr(v int) *int         { return &v }

// pnl returns (absolute, percent) for a trade given an exit/mark price. Percent is relative to the
// capital at risk (entry price × size), so it's consistent for long and short.
//
//	long:  pnlAbs = (mark - entry) * size
//	short: pnlAbs = (entry - mark) * size
//	pnlPct = pnlAbs / (entry * size) * 100
func pnl(side string, entry, size, mark float64) (float64, float64) {
	var abs float64
	if side == "short" {
		abs = (entry - mark) * size
	} else {
		abs = (mark - entry) * size
	}
	capital := entry * size
	pct := 0.0
	if capital != 0 {
		pct = abs / capital * 100
	}
	return abs, pct
}

// rMultiple returns realized reward in units of initial risk, or nil if no stop / zero risk.
//
//	long:  (exit - entry) / (entry - stop)
//	short: (entry - exit) / (stop - entry)
func rMultiple(side string, entry, stop, exit float64) *float64 {
	var risk, reward float64
	if side == "short" {
		risk = stop - entry
		reward = entry - exit
	} else {
		risk = entry - stop
		reward = exit - entry
	}
	if risk == 0 {
		return nil
	}
	r := reward / risk
	if math.IsNaN(r) || math.IsInf(r, 0) {
		return nil
	}
	return f64ptr(round(r, 2))
}

// holdingDays returns whole days from entry to exit (closed) or entry to `now` (open).
func holdingDays(entryDate, endDate string, now time.Time) *int {
	start, err := time.Parse(dateLayout, entryDate)
	if err != nil {
		return nil
	}
	var end time.Time
	if endDate != "" {
		end, err = time.Parse(dateLayout, endDate)
		if err != nil {
			return nil
		}
	} else {
		end = now
	}
	days := int(end.Sub(start).Hours() / 24)
	if days < 0 {
		days = 0
	}
	return intptr(days)
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}

// computeView derives the read-time fields. For a closed trade P&L uses the recorded exit; for an
// open trade it marks to `mark` (a live quote) when available, flagging the source.
func computeView(t Trade, mark *quoteResp, now time.Time) TradeView {
	v := TradeView{Trade: t}

	if t.Exit != nil {
		// realized
		abs, pct := pnl(t.Side, t.Entry.Price, t.Entry.Size, t.Exit.Price)
		v.PnlAbs = f64ptr(round(abs, 2))
		v.PnlPct = f64ptr(round(pct, 2))
		if t.Stop != nil {
			v.RMultiple = rMultiple(t.Side, t.Entry.Price, *t.Stop, t.Exit.Price)
		}
		v.HoldingDays = holdingDays(t.Entry.Date, t.Exit.Date, now)
		v.Unrealized = false
		return v
	}

	// open — mark to the live quote if we have one
	v.HoldingDays = holdingDays(t.Entry.Date, "", now)
	v.Unrealized = true
	if mark != nil && mark.Price != nil {
		abs, pct := pnl(t.Side, t.Entry.Price, t.Entry.Size, *mark.Price)
		v.PnlAbs = f64ptr(round(abs, 2))
		v.PnlPct = f64ptr(round(pct, 2))
		v.MarkPrice = f64ptr(round(*mark.Price, 2))
		v.MarkSource = mark.Source
		v.MarkSynthetic = mark.Source == "synthetic"
		if t.Stop != nil {
			v.RMultiple = rMultiple(t.Side, t.Entry.Price, *t.Stop, *mark.Price)
		}
	}
	return v
}
