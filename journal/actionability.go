package main

// actionability.go — the block every research run serves, and the reason it always says NO_SIGNAL.
//
// SCOPE NOTE. An earlier draft of this file also carried a full LONG/FLAT/SHORT decision function
// (`deriveAgencyAction`) mapping a validated target and the user's current position onto an action.
// Nothing in this patch consumed it — the research lane cannot produce a target, so the function
// had no caller and no route — so it has been removed and left for the follow-up that actually
// wires the quantitative gates. What remains here is what this patch genuinely uses: the two
// evidence states, the no-action outcome, and the gate list served on every run.
//
// WHY THE BLOCK EXISTS AT ALL, given that it is a constant.
//
// A completed research run is the single most likely thing in this application to be misread as a
// verdict. Four agents, citations, a thesis, an anti-thesis, a risk section and a chair's
// conclusion look exactly like something you would act on. Serving `NO_SIGNAL` alongside it, in
// every state including `completed`, is what stops the absence of a signal from being expressed as
// the absence of a field — which is how a reader supplies their own.
//
// THE VOCABULARY THIS ESTABLISHES, AND THE DEFECT IT IS AIMED AT.
//
// `services/prediction/app/model.py::derive_direction` serves Buy / Hold / Sell, and
// `docs/PAPER_EXECUTION_CONTRACT.md` §1.1 maps them to targets +1 / 0 / -1. Two different things
// are collapsed into "Hold": "no directional view" (p between the thresholds) and "actively
// bearish, but this record never backtested shorting" (`derive_direction`: *"otherwise low p(up)
// means 'don't be long' = Hold"*). A third is collapsed at the boundary — `Hold` means target FLAT,
// which against an open long is an EXIT, while the English word means "keep".
//
// Separately, a MISSING signal is not a flat one. `services/prediction/app/main.py` is careful
// about that (`signal: null` plus a `reason` on every un-validated branch) and
// `paper/engine.go::targetFor` restates it: *"an absence, which is not a flat target"*. `NO_SIGNAL`
// gives that absence a NAME so it can stop being expressed as the lack of one.
//
// `NO_SIGNAL` is any missing or failed validation: no model, a failed backtest, a stale data
// policy, no current pooled EDGE verdict, a strategy-version mismatch, an unreachable upstream, or
// a failed research run. It is never `HOLD`, and there is no code path here that can make it one —
// there is no branch at all.

// Evidence states. Two, and the set is closed. `VALIDATED` is not reachable from this lane; it is
// declared because `NO_SIGNAL` only means something next to the state it is not.
const (
	evidenceValidated = "VALIDATED"
	evidenceNoSignal  = "NO_SIGNAL"
)

// actionNoAction is the only outcome this lane can produce. The full action vocabulary — opening,
// exiting, and holding what you already hold — belongs with the change that can actually derive
// one, because each of those words is only meaningful relative to a position this lane never reads.
const actionNoAction = "NO_ACTION"

// agencyRequiredGates names every gate a future actionable result must clear. The names are the
// ones the code already uses, so a reader can go and find each one:
//
//	real-data          paper/gates.go::noSyntheticData
//	freshness          paper/gates.go::freshData
//	backtest-passed    paper/gates.go::backtestPassed  (services/prediction backtest.is_passing)
//	pooled-edge        paper/gates.go::evaluatorVerdict (verdicts.TRADEABLE_VERDICT == "EDGE")
//	strategy-version   services/prediction/app/verdicts.py::evaluation_block -> current
//	portfolio-policy   journal/portfolio_intelligence.go::portfolioPolicyVersion
var agencyRequiredGates = []string{
	"real-data",
	"freshness",
	"backtest-passed",
	"pooled-edge",
	"strategy-version",
	"portfolio-policy",
}

type agencyGateView struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

// agencyActionabilityView is served on every run, in every state.
type agencyActionabilityView struct {
	EvidenceState string           `json:"evidenceState"`
	Target        string           `json:"target,omitempty"`
	Action        string           `json:"action"`
	Gates         []agencyGateView `json:"gates"`
	VetoRaised    bool             `json:"vetoRaised"`
	VetoScope     string           `json:"vetoScope,omitempty"`
	Note          string           `json:"note"`
}

const agencyActionabilityNote = "Research does not produce a signal. This run was not evaluated " +
	"against any quantitative gate, so the evidence state is NO_SIGNAL — which is not HOLD and is " +
	"not a reason to do anything. A directional target can only come from the backtest-gated quant " +
	"model, and only once every gate below has actually passed."

// agencyActionability is the block attached to a run view. It is a CONSTANT answer and says so:
// NO_SIGNAL, no target, NO_ACTION, and all six gates un-evaluated.
//
// A veto, if the artifact raised one, is REPORTED — and note that it changes nothing here. That is
// the point rather than an oversight: a veto's only power is to withhold NEW exposure, and there is
// no exposure to withhold when there is no signal. When the gates are eventually wired, the veto
// must keep exactly that asymmetry — it may subtract an opening action and may never create one,
// never flip one, and never stand in the way of closing or reducing a position. `paper/gates.go`
// states the same rule for the quantitative gates: *"a gate must not be able to trap a position
// that is already open."*
func agencyActionability(run AgencyRun) agencyActionabilityView {
	gates := make([]agencyGateView, 0, len(agencyRequiredGates))
	for _, name := range agencyRequiredGates {
		gates = append(gates, agencyGateView{
			Name:   name,
			Passed: false,
			Detail: "not evaluated: the research lane does not read quantitative evidence",
		})
	}
	view := agencyActionabilityView{
		EvidenceState: evidenceNoSignal,
		Action:        actionNoAction,
		Gates:         gates,
		Note:          agencyActionabilityNote,
	}
	if run.Artifact != nil {
		view.VetoRaised = run.Artifact.Veto.Raised
		view.VetoScope = run.Artifact.Veto.Scope
	}
	return view
}
