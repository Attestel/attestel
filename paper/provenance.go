package main

import (
	"context"
	"net/http"
	"sort"
	"time"
)

// provenance.go — `GET /paper/provenance`: WHICH MODEL IS SERVING, and WHAT THE EVALUATOR SAID
// ABOUT IT, as structured fields rather than as prose.
//
// WHY THIS ENDPOINT HAS TO EXIST AT ALL. Every one of these facts is already fetched on this
// service's own hot paths — `gates.go` spends them on every decision and `readiness.go` re-evaluates
// them for the launch checklist — but neither surface serves them as DATA:
//
//   - `/paper/status` carries `modelVersion` and `strategyVersion` ONLY inside an open position's
//     block (`handlers.go`, `if st.Side != ""`). No EDGE verdict exists, so no position is open, so
//     today those fields are absent everywhere. "Which model is live" is unanswerable from it.
//   - `/paper/readiness` reaches the verdict, but flattens it into a human sentence in a check's
//     `detail` string. A consumer wanting the verdict would have to parse English.
//
// A reader that has to parse prose to learn whether the served model was trained on synthetic data
// is a reader that will eventually parse it wrong. So this route serves the same values the gates
// judge, in the same shape `predictResp` already decodes them.
//
// IT IS A READ, AND ONLY A READ. It takes no lock on the engine, mutates nothing, books nothing and
// cannot cause a decision. It calls `/predict` exactly as `readiness.go` does.
//
// ─────────────────────────────────────────────────────────────────────────────────────────────
// UNAVAILABLE IS NOT ZERO, AND IT IS NOT CLEAN
// ─────────────────────────────────────────────────────────────────────────────────────────────
// Every per-config row states its own `availability`. An upstream that could not be reached yields
// `unavailable` WITH THE REASON — never a row of nulls, and never a `trainedOnSynthetic: false`
// that a reader would take for "verified not synthetic". This is the same distinction
// `gates.go::noSyntheticData` already draws when it refuses on `currentData == nil`: unknown
// provenance is unknown, and unknown refuses.
//
// Numeric and boolean fields that can be absent are POINTERS for the same reason. `false` and "the
// service never told us" are different answers and an ordinary `bool` collapses them.

// provenanceAvailability is the state of ONE config's row.
const (
	provenanceLive        = "live"
	provenanceUnavailable = "unavailable"
)

// evaluationProvenance is the offline evaluator's persisted verdict for one config, restated as
// data. It mirrors `predictResp.Evaluation` field for field; nothing is summarised, and the raw
// `verdict` string is passed through VERBATIM rather than being mapped onto a local vocabulary —
// a consumer that needs to know the difference between "NO EDGE" and "SUSPECT" must be able to see
// which one was actually recorded.
type evaluationProvenance struct {
	Verdict         string   `json:"verdict"`
	EvaluatedAt     string   `json:"evaluatedAt"`
	StrategyVersion string   `json:"strategyVersion"`
	Current         *bool    `json:"current"`
	EvidenceCurrent *bool    `json:"evidenceCurrent"`
	EvidenceIssues  []string `json:"evidenceIssues"`
	Method          string   `json:"method"`
	Report          string   `json:"report"`
}

// frameProvenance is the provenance of the feature frame `/predict` actually scored. A NIL
// `frameProvenance` on a `live` row means the prediction service served `currentData: null`, which
// is UNKNOWN provenance — see the gate's comment. It is not a clean frame.
type frameProvenance struct {
	Source    string `json:"source"`
	Synthetic bool   `json:"synthetic"`
}

// configProvenance is one enabled config's model identity and evaluator verdict.
type configProvenance struct {
	Config    string `json:"config"`
	Ticker    string `json:"ticker"`
	Timeframe string `json:"timeframe"`
	Horizon   int    `json:"horizon"`

	// Availability is the row's own state. On `unavailable` every field below is absent and
	// `unavailableReason` says why — a consumer must never read an absent field as a measured one.
	Availability      string `json:"availability"`
	UnavailableReason string `json:"unavailableReason,omitempty"`

	ModelVersion    string `json:"modelVersion,omitempty"`
	StrategyVersion string `json:"strategyVersion,omitempty"`
	// TrainedOnSynthetic is a POINTER. `false` means the record states it was not; `null` means the
	// question was never answered. Gate 1 treats those differently and so must every reader.
	TrainedOnSynthetic *bool `json:"trainedOnSynthetic"`
	// DataThrough is the last bar the model was trained through — the input to the model-age half
	// of gate 2.
	DataThrough string `json:"dataThrough,omitempty"`
	// HasSignal reports whether `/predict` returned a target-bearing signal at all. Pointer, for
	// the same reason as above.
	HasSignal *bool  `json:"hasSignal"`
	Reason    string `json:"reason,omitempty"`

	CurrentData *frameProvenance      `json:"currentData"`
	Evaluation  *evaluationProvenance `json:"evaluation"`
}

// handleProvenance serves the model identity and evaluator verdict for every enabled config.
//
// ONE UPSTREAM CALL PER CONFIG, sequentially, bounded by the request's own deadline. Sequential
// rather than concurrent on purpose: the config list is small, the prediction service is a single
// local process, and `readiness.go` already fans out against it — a second concurrent fan-out from
// the same service buys nothing and competes with the one that gates real decisions.
func (a *API) handleProvenance(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()

	configs := a.store.Configs()
	rows := make([]configProvenance, 0, len(configs))
	for _, cfg := range configs {
		rows = append(rows, a.configProvenance(ctx, cfg))
	}
	// Stable ordering, so two reads of an unchanged deployment produce identical bytes. A snapshot
	// that hashes this payload depends on it (journal/experiment_snapshot.go).
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Config < rows[j].Config })

	writeJSON(w, http.StatusOK, map[string]any{
		"paper": true, "simulation": true,
		"asOf":             a.currentTime().Format(time.RFC3339),
		"generation":       a.generation(),
		"revision":         a.cfg.Revision,
		"configs":          rows,
		"tradeableVerdict": verdictEdge,
		"note": "The model identity and the offline evaluator's verdict for each enabled config, " +
			"as the gates read them. A config whose upstream could not be reached is `unavailable` " +
			"with a reason — never a row of zeros, and never a clean one.",
	})
}

func (a *API) configProvenance(ctx context.Context, cfg PaperCfg) configProvenance {
	row := configProvenance{
		Config: cfg.Key(), Ticker: cfg.Ticker, Timeframe: cfg.Timeframe, Horizon: cfg.Horizon,
	}
	pred, err := a.clients.predict(ctx, cfg)
	if err != nil {
		row.Availability = provenanceUnavailable
		row.UnavailableReason = err.Error()
		return row
	}
	if pred == nil {
		// A 200 with no body is not a model record. Refusing it here keeps the caller from having
		// to distinguish "served nothing" from "served a record with empty fields".
		row.Availability = provenanceUnavailable
		row.UnavailableReason = "the prediction service returned no record"
		return row
	}

	row.Availability = provenanceLive
	row.ModelVersion = pred.ModelVersion
	row.StrategyVersion = pred.StrategyVersion
	// Copied THROUGH, never defaulted. A record that did not state its training provenance leaves
	// this nil, and the snapshot's derived rules treat nil as unknown rather than as clean.
	row.TrainedOnSynthetic = pred.TrainedOnSynthetic
	row.DataThrough = pred.DataThrough
	row.HasSignal = boolptr(pred.Signal != nil)
	row.Reason = pred.Reason
	if pred.CurrentData != nil {
		row.CurrentData = &frameProvenance{
			Source: pred.CurrentData.Source, Synthetic: pred.CurrentData.Synthetic,
		}
	}
	if ev := pred.Evaluation; ev != nil {
		issues := ev.EvidenceIssues
		if issues == nil {
			issues = []string{}
		}
		row.Evaluation = &evaluationProvenance{
			Verdict:         ev.Verdict,
			EvaluatedAt:     ev.EvaluatedAt,
			StrategyVersion: ev.StrategyVersion,
			Current:         boolptr(ev.Current),
			EvidenceCurrent: boolptr(ev.EvidenceCurrent),
			EvidenceIssues:  issues,
			Method:          ev.Method,
			Report:          ev.Report,
		}
	}
	return row
}

// generation is the current experiment generation, or 0 when no ledger is running. It is served on
// every payload a snapshot composes so a consumer can prove all four came from ONE generation.
func (a *API) generation() int64 {
	if l := a.ledger(); l != nil {
		return l.Generation()
	}
	return 0
}

func boolptr(v bool) *bool { return &v }
