package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// The experimental shadow ledger answers one deliberately narrower question than the official
// paper book: "what happened after the Buy/Sell/Hold calls the model actually served?" It never
// opens a journal trade, never changes the official ledger, and never participates in a gate.
const (
	shadowContractVersion = "experimental-shadow-v1"
	shadowEpisodeNotional = 10_000.0
	shadowHistoryLimit    = 16 // enough to settle the fixed H10 outcome after a short outage
)

var shadowHorizons = []int{1, 3, 5, 10}

// ShadowObservation is the first signal and quote the paper engine saw for one completed bar. The
// key is deterministic, so retries and restarts preserve the FIRST observation instead of replacing
// it with a more convenient later quote or model response.
type ShadowObservation struct {
	ContractVersion   string  `json:"contractVersion"`
	Config            string  `json:"config"`
	Ticker            string  `json:"ticker"`
	Timeframe         string  `json:"timeframe"`
	ModelHorizon      int     `json:"modelHorizon"`
	SignalBar         string  `json:"signalBar"`
	SignalBarUnix     int64   `json:"signalBarUnix"`
	ObservedAt        string  `json:"observedAt"`
	Direction         string  `json:"direction"`
	Target            string  `json:"target"`
	ProbUp            float64 `json:"probUp"`
	Confidence        float64 `json:"confidence"`
	ModelVersion      string  `json:"modelVersion"`
	StrategyVersion   string  `json:"strategyVersion"`
	Evaluation        string  `json:"evaluation"`
	EvaluationCurrent bool    `json:"evaluationCurrent"`
	BacktestPassed    bool    `json:"backtestPassed"`
	CostBps           float64 `json:"costBps"`
	EntryPrice        float64 `json:"entryPrice"`
	EntrySource       string  `json:"entrySource"`
	EntryAsOf         string  `json:"entryAsOf"`
	EntryEligible     bool    `json:"entryEligible"`
	EntryReason       string  `json:"entryReason"`
}

func (o ShadowObservation) key() string {
	return fmt.Sprintf("%s:%s:%d", o.ContractVersion, o.Config, o.SignalBarUnix)
}

// ShadowBar is a completed bar retained solely to settle forward outcomes. It is not an execution
// price: the observation's contemporaneous quote is the simulated entry, and this close is a later
// mark. Synthetic or source-less bars stay visible but make the affected outcome ineligible.
type ShadowBar struct {
	Ticker    string  `json:"ticker"`
	Timeframe string  `json:"timeframe"`
	Bar       string  `json:"bar"`
	BarUnix   int64   `json:"barUnix"`
	Close     float64 `json:"close"`
	Source    string  `json:"source"`
	Synthetic bool    `json:"synthetic"`
}

func (b ShadowBar) key() string {
	return fmt.Sprintf("%s:%s:%d", b.Ticker, b.Timeframe, b.BarUnix)
}

// ShadowOutcome is one immutable fixed-horizon result. Returns are decimals. A Buy earns the raw
// return, a Sell earns its negative, and both pay one entry plus one exit cost. Hold is a flat
// simulated position with zero strategy return and deliberately has no "correct" label: without a
// pre-registered neutral band, declaring a Hold right or wrong would be threshold tuning in disguise.
type ShadowOutcome struct {
	ContractVersion   string  `json:"contractVersion"`
	Config            string  `json:"config"`
	Ticker            string  `json:"ticker"`
	Timeframe         string  `json:"timeframe"`
	ModelHorizon      int     `json:"modelHorizon"`
	SignalBar         string  `json:"signalBar"`
	SignalBarUnix     int64   `json:"signalBarUnix"`
	Horizon           int     `json:"horizon"`
	OutcomeBar        string  `json:"outcomeBar"`
	OutcomeBarUnix    int64   `json:"outcomeBarUnix"`
	SettledAt         string  `json:"settledAt"`
	Direction         string  `json:"direction"`
	EntryPrice        float64 `json:"entryPrice"`
	ExitPrice         float64 `json:"exitPrice"`
	RawReturn         float64 `json:"rawReturn"`
	StrategyReturn    float64 `json:"strategyReturn"`
	EpisodePnl        float64 `json:"episodePnl"`
	Correct           *bool   `json:"correct"`
	Eligible          bool    `json:"eligible"`
	EligibilityReason string  `json:"eligibilityReason"`
}

func (o ShadowOutcome) key() string {
	return fmt.Sprintf("%s:%s:%d:%d", o.ContractVersion, o.Config, o.SignalBarUnix, o.Horizon)
}

type shadowDataset struct {
	Observations []ShadowObservation
	Bars         []ShadowBar
	Outcomes     []ShadowOutcome
}

type shadowMetricRow struct {
	Config              string   `json:"config"`
	Ticker              string   `json:"ticker"`
	Timeframe           string   `json:"timeframe"`
	ModelHorizon        int      `json:"modelHorizon"`
	OutcomeHorizon      int      `json:"outcomeHorizon"`
	NSettled            int      `json:"nSettled"`
	NDirectional        int      `json:"nDirectional"`
	NCorrect            int      `json:"nCorrect"`
	DirectionalAccuracy *float64 `json:"directionalAccuracy"`
	MeanEpisodeReturn   *float64 `json:"meanEpisodeReturn"`
	EpisodePnl          float64  `json:"episodePnl"`
	Buy                 int      `json:"buy"`
	Sell                int      `json:"sell"`
	Hold                int      `json:"hold"`
}

type shadowRecent struct {
	Observation ShadowObservation `json:"observation"`
	Outcomes    []ShadowOutcome   `json:"outcomes"`
}

type shadowReport struct {
	ContractVersion  string            `json:"contractVersion"`
	Recording        bool              `json:"recording"`
	Error            string            `json:"error,omitempty"`
	EpisodeNotional  float64           `json:"episodeNotional"`
	Horizons         []int             `json:"horizons"`
	Observations     int               `json:"observations"`
	Executable       int               `json:"executable"`
	Directions       map[string]int    `json:"directions"`
	SettledOutcomes  int               `json:"settledOutcomes"`
	EligibleOutcomes int               `json:"eligibleOutcomes"`
	Rows             []shadowMetricRow `json:"rows"`
	Recent           []shadowRecent    `json:"recent"`
	Note             string            `json:"note"`
}

func newShadowObservation(cfg PaperCfg, bar *latestBar, pred *predictResp, q *quoteResp,
	quoteErr error, now time.Time) (ShadowObservation, bool) {
	target, ok := targetFor(pred)
	if !ok || bar == nil || pred == nil || pred.Signal == nil {
		return ShadowObservation{}, false
	}
	obs := ShadowObservation{
		ContractVersion: shadowContractVersion,
		Config:          cfg.Key(), Ticker: cfg.Ticker, Timeframe: cfg.Timeframe, ModelHorizon: cfg.Horizon,
		SignalBar: bar.Time.Label, SignalBarUnix: bar.Time.Unix,
		ObservedAt: now.UTC().Format(time.RFC3339), Direction: pred.Signal.Direction, Target: target,
		ProbUp: pred.Signal.ProbUp, Confidence: pred.Signal.Confidence,
		ModelVersion: pred.ModelVersion, StrategyVersion: pred.StrategyVersion,
		CostBps: pred.costBpsFloat(),
	}
	if pred.Backtest != nil {
		obs.BacktestPassed, _ = pred.Backtest["passed"].(bool)
	}
	if pred.Evaluation != nil {
		obs.Evaluation = pred.Evaluation.Verdict
		obs.EvaluationCurrent = pred.Evaluation.Current && pred.Evaluation.EvidenceCurrent
	}
	if quoteErr != nil {
		obs.EntryReason = "quote unavailable when the signal was first observed: " + quoteErr.Error()
		return obs, true
	}
	if q == nil || q.Price == nil || *q.Price <= 0 {
		obs.EntryReason = "quote unavailable when the signal was first observed"
		return obs, true
	}
	obs.EntryPrice, obs.EntrySource, obs.EntryAsOf = *q.Price, q.Source, q.AsOf
	if issue := executionQuoteIssue(q, bar, cfg.Timeframe); issue != "" {
		obs.EntryReason = issue
		return obs, true
	}
	obs.EntryEligible = true
	obs.EntryReason = "real, source-labelled quote observed at or after the completed signal bar"
	return obs, true
}

// settleShadowOutcomes returns only missing outcomes. It never revises an existing result.
func settleShadowOutcomes(data shadowDataset, now time.Time) []ShadowOutcome {
	existing := make(map[string]bool, len(data.Outcomes))
	for _, outcome := range data.Outcomes {
		existing[outcome.key()] = true
	}
	barsByStream := map[string][]ShadowBar{}
	for _, bar := range data.Bars {
		key := bar.Ticker + ":" + bar.Timeframe
		barsByStream[key] = append(barsByStream[key], bar)
	}
	for key := range barsByStream {
		sort.SliceStable(barsByStream[key], func(i, j int) bool {
			return barsByStream[key][i].BarUnix < barsByStream[key][j].BarUnix
		})
	}

	created := []ShadowOutcome{}
	for _, obs := range data.Observations {
		bars := barsByStream[obs.Ticker+":"+obs.Timeframe]
		start := sort.Search(len(bars), func(i int) bool { return bars[i].BarUnix > obs.SignalBarUnix })
		for _, horizon := range shadowHorizons {
			key := fmt.Sprintf("%s:%s:%d:%d", obs.ContractVersion, obs.Config, obs.SignalBarUnix, horizon)
			if existing[key] || len(bars)-start < horizon {
				continue
			}
			window := bars[start : start+horizon]
			exit := window[len(window)-1]
			outcome := ShadowOutcome{
				ContractVersion: obs.ContractVersion, Config: obs.Config, Ticker: obs.Ticker,
				Timeframe: obs.Timeframe, ModelHorizon: obs.ModelHorizon,
				SignalBar: obs.SignalBar, SignalBarUnix: obs.SignalBarUnix,
				Horizon: horizon, OutcomeBar: exit.Bar, OutcomeBarUnix: exit.BarUnix,
				SettledAt: now.UTC().Format(time.RFC3339), Direction: obs.Direction,
				EntryPrice: obs.EntryPrice, ExitPrice: exit.Close,
				Eligible: obs.EntryEligible,
			}
			if !outcome.Eligible {
				outcome.EligibilityReason = obs.EntryReason
			}
			for _, bar := range window {
				if bar.Synthetic || strings.TrimSpace(bar.Source) == "" || bar.Close <= 0 {
					outcome.Eligible = false
					outcome.EligibilityReason = "one or more completed horizon bars were synthetic, source-less, or missing a close"
					break
				}
			}
			if obs.EntryPrice > 0 && exit.Close > 0 {
				outcome.RawReturn = exit.Close/obs.EntryPrice - 1
				switch obs.Direction {
				case "Buy":
					correct := outcome.RawReturn > 0
					outcome.Correct = &correct
					outcome.StrategyReturn = outcome.RawReturn - 2*obs.CostBps/10_000
				case "Sell":
					correct := outcome.RawReturn < 0
					outcome.Correct = &correct
					outcome.StrategyReturn = -outcome.RawReturn - 2*obs.CostBps/10_000
				case "Hold":
					outcome.StrategyReturn = 0
				}
				outcome.EpisodePnl = shadowEpisodeNotional * outcome.StrategyReturn
			}
			created = append(created, outcome)
			existing[key] = true
		}
	}
	return created
}

func buildShadowReport(data shadowDataset, recentLimit int, storageErr error) shadowReport {
	report := shadowReport{
		ContractVersion: shadowContractVersion, Recording: storageErr == nil,
		EpisodeNotional: shadowEpisodeNotional, Horizons: append([]int{}, shadowHorizons...),
		Directions: map[string]int{"Buy": 0, "Sell": 0, "Hold": 0},
		Rows:       []shadowMetricRow{}, Recent: []shadowRecent{},
		Note: "Experimental forward evidence only. Every first-seen model call is stored even when the official evaluator gate refuses it. Independent episodes overlap and are not a withdrawable portfolio.",
	}
	if storageErr != nil {
		report.Error = storageErr.Error()
	}
	observations := make([]ShadowObservation, 0, len(data.Observations))
	for _, observation := range data.Observations {
		if observation.ContractVersion == shadowContractVersion {
			observations = append(observations, observation)
		}
	}
	sort.SliceStable(observations, func(i, j int) bool {
		if observations[i].SignalBarUnix == observations[j].SignalBarUnix {
			return observations[i].ObservedAt > observations[j].ObservedAt
		}
		return observations[i].SignalBarUnix > observations[j].SignalBarUnix
	})
	report.Observations = len(observations)
	for _, obs := range observations {
		report.Directions[obs.Direction]++
		if obs.EntryEligible {
			report.Executable++
		}
	}
	currentOutcomes := make([]ShadowOutcome, 0, len(data.Outcomes))
	for _, outcome := range data.Outcomes {
		if outcome.ContractVersion != shadowContractVersion {
			continue
		}
		currentOutcomes = append(currentOutcomes, outcome)
		if outcome.Eligible {
			report.EligibleOutcomes++
		}
	}
	report.SettledOutcomes = len(currentOutcomes)

	type metricKey struct {
		config  string
		horizon int
	}
	groups := map[metricKey][]ShadowOutcome{}
	for _, outcome := range currentOutcomes {
		if outcome.Eligible {
			groups[metricKey{outcome.Config, outcome.Horizon}] = append(groups[metricKey{outcome.Config, outcome.Horizon}], outcome)
		}
	}
	keys := make([]metricKey, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.SliceStable(keys, func(i, j int) bool {
		if keys[i].config == keys[j].config {
			return keys[i].horizon < keys[j].horizon
		}
		return keys[i].config < keys[j].config
	})
	for _, key := range keys {
		outcomes := groups[key]
		row := shadowMetricRow{Config: key.config, OutcomeHorizon: key.horizon, NSettled: len(outcomes)}
		var sum float64
		for _, outcome := range outcomes {
			row.Ticker, row.Timeframe, row.ModelHorizon = outcome.Ticker, outcome.Timeframe, outcome.ModelHorizon
			sum += outcome.StrategyReturn
			row.EpisodePnl += outcome.EpisodePnl
			switch outcome.Direction {
			case "Buy":
				row.Buy++
			case "Sell":
				row.Sell++
			case "Hold":
				row.Hold++
			}
			if outcome.Correct != nil {
				row.NDirectional++
				if *outcome.Correct {
					row.NCorrect++
				}
			}
		}
		if row.NSettled > 0 {
			mean := sum / float64(row.NSettled)
			row.MeanEpisodeReturn = &mean
		}
		if row.NDirectional > 0 {
			accuracy := float64(row.NCorrect) / float64(row.NDirectional)
			row.DirectionalAccuracy = &accuracy
		}
		report.Rows = append(report.Rows, row)
	}

	outcomesByObservation := map[string][]ShadowOutcome{}
	for _, outcome := range currentOutcomes {
		key := fmt.Sprintf("%s:%s:%d", outcome.ContractVersion, outcome.Config, outcome.SignalBarUnix)
		outcomesByObservation[key] = append(outcomesByObservation[key], outcome)
	}
	if recentLimit <= 0 || recentLimit > len(observations) {
		recentLimit = len(observations)
	}
	for _, obs := range observations[:recentLimit] {
		outcomes := append([]ShadowOutcome{}, outcomesByObservation[obs.key()]...)
		sort.SliceStable(outcomes, func(i, j int) bool { return outcomes[i].Horizon < outcomes[j].Horizon })
		report.Recent = append(report.Recent, shadowRecent{Observation: obs, Outcomes: outcomes})
	}
	return report
}

func joinShadowErrors(errs ...error) error {
	parts := []string{}
	for _, err := range errs {
		if err != nil {
			parts = append(parts, err.Error())
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return errors.New(strings.Join(parts, "; "))
}
