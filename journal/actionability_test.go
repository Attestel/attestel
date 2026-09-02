package main

import "testing"

// actionability_test.go — the served block, which is the only part of this vocabulary the patch
// consumes.
//
// The enumeration tests that used to live here belonged to `deriveAgencyAction`, which was removed
// as unwired (see actionability.go's scope note). They should come back with the change that
// actually derives an action; what is left is the property this patch has to hold today.

func TestTheResearchLaneAlwaysReportsNoSignalWithNoGatesPassed(t *testing.T) {
	// The whole safety claim of this lane, in one assertion: a research run — including a completed
	// one that raised a veto — reports NO_SIGNAL, no target, and NO_ACTION, with every quantitative
	// gate honestly marked un-evaluated.
	run := AgencyRun{Status: agencyCompleted, Artifact: &AgencyArtifact{
		Veto: AgencyVeto{Raised: true, Scope: agencyVetoScope, Reasons: []string{"thin evidence"}},
	}}
	view := agencyActionability(run)

	if view.EvidenceState != evidenceNoSignal {
		t.Fatalf("evidenceState = %q, want %q", view.EvidenceState, evidenceNoSignal)
	}
	if view.Action != actionNoAction {
		t.Fatalf("action = %q, want %q", view.Action, actionNoAction)
	}
	if view.Target != "" {
		t.Fatalf("a research run produced a directional target: %q", view.Target)
	}
	if !view.VetoRaised || view.VetoScope != agencyVetoScope {
		t.Fatalf("the veto was not reported honestly: %+v", view)
	}
	for _, g := range view.Gates {
		if g.Passed {
			t.Fatalf("gate %q reported as passed by the research lane", g.Name)
		}
		if g.Detail == "" {
			t.Fatalf("gate %q gives no reason for not being evaluated", g.Name)
		}
	}
}

func TestNoSignalIsReportedInEveryRunState(t *testing.T) {
	// Not only on the states where it is obvious. A `queued` run has nothing to act on and a
	// `completed` one has research; both must report the same absence of a signal, so a reader
	// never learns to treat "completed" as meaning something changed.
	for _, status := range []string{
		agencyQueued, agencyClaimed, agencyRunning,
		agencyCompleted, agencyFailed, agencyCancelled, agencyExpired,
	} {
		view := agencyActionability(AgencyRun{Status: status})
		if view.EvidenceState != evidenceNoSignal || view.Action != actionNoAction {
			t.Fatalf("run state %q reported %s/%s, want %s/%s",
				status, view.EvidenceState, view.Action, evidenceNoSignal, actionNoAction)
		}
	}
}

func TestTheRequiredGateListIsTheOneTheRestOfTheRepositoryEnforces(t *testing.T) {
	// The contract with the quantitative side: these six names are the gates a future actionable
	// result must clear, and each one exists in the codebase today.
	want := []string{
		"real-data", "freshness", "backtest-passed", "pooled-edge",
		"strategy-version", "portfolio-policy",
	}
	if len(agencyRequiredGates) != len(want) {
		t.Fatalf("gates = %d, want %d", len(agencyRequiredGates), len(want))
	}
	for i := range want {
		if agencyRequiredGates[i] != want[i] {
			t.Fatalf("gate %d = %q, want %q", i, agencyRequiredGates[i], want[i])
		}
	}
}

func TestNoSignalAndValidatedAreDistinctStates(t *testing.T) {
	// The defect this vocabulary exists to prevent is two different things sharing one word. If
	// these ever became equal, "we have no evidence" and "the evidence cleared every gate" would be
	// the same value — which is precisely how `Hold` came to mean both "no view" and "flat".
	if evidenceNoSignal == evidenceValidated {
		t.Fatal("NO_SIGNAL and VALIDATED collapsed to one value")
	}
}
