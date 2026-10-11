package main

import "sort"

// agency_workflows.go — the CLOSED REGISTRY of workflows this deployment will dispatch.
//
// ─────────────────────────────────────────────────────────────────────────────────────────────
// WHY A REGISTRY AND NOT A SECOND CONSTANT
// ─────────────────────────────────────────────────────────────────────────────────────────────
// Before this file there was one workflow, and its profile chain was a package-level slice that
// `validateAgencyArtifact` indexed into directly. Adding a second workflow by adding a second
// package-level slice would have left every validator having to remember which one applied —
// exactly the shape in which one workflow eventually gets validated against another's rules.
//
// So a workflow is a VALUE, looked up by the name the run was created with, and every rule that
// depends on "which chain ran" reads it from that value. There is no default and no fallback: a
// name not in this map is not a workflow, and every entry point refuses it rather than guessing.
//
// ─────────────────────────────────────────────────────────────────────────────────────────────
// `company_research_v1` IS UNCHANGED, AND THAT IS A REQUIREMENT
// ─────────────────────────────────────────────────────────────────────────────────────────────
// Its four profiles, in this order, and its two schema version strings are byte-identical to what
// they were before this file existed. A worker built against the old server must keep working
// against this one, and `agency_test.go` pins the chain independently. Adding a workflow must not
// change the meaning of an existing one — if `experiment_review_v1` ever needs a rule
// `company_research_v1` does not have, the rule belongs on the workflow value, never on the shared
// path.

// Workflow kinds. The kind decides which JOB ENVELOPE and which ARTIFACT a run uses, and the two
// kinds share no struct — a review cannot be completed with a research artifact or the reverse,
// because the decoders are different types.
const (
	workflowKindResearch = "research"
	workflowKindReview   = "review"
)

const (
	// agencyWorkflowExperimentReview reviews a STORED EVIDENCE SNAPSHOT of this deployment's own
	// paper experiment. It takes no ticker, no question and no URL — its entire input is a snapshot
	// id, and the snapshot was assembled by this server from its own services.
	agencyWorkflowExperimentReview = "experiment_review_v1"

	// The review lane's own versioned envelopes. SEPARATE FROM the research ones on purpose: a
	// shared `attestel.agency.job/1` that meant two different shapes depending on a sibling field
	// is a schema version that no longer versions anything.
	agencyReviewJobSchemaVersion      = "attestel.agency.review-job/1"
	agencyReviewArtifactSchemaVersion = "attestel.agency.review-artifact/1"
)

// agencyWorkflow is one dispatchable workflow.
type agencyWorkflow struct {
	Name string
	Kind string
	// Profiles is the FIXED, ORDERED chain the name means. Recorded here so the hosted side can
	// verify what ran; it is never sent to a worker as an instruction, and the worker takes its
	// chain from its own copy (bridge/hermes.go). The two must agree, and both sides' tests pin the
	// same strings.
	Profiles []string
	// JobSchemaVersion / ArtifactSchemaVersion are the envelopes this workflow uses.
	JobSchemaVersion      string
	ArtifactSchemaVersion string
}

// agencyWorkflows is the whole vocabulary. Two entries, and the set is closed.
var agencyWorkflows = map[string]agencyWorkflow{
	agencyWorkflowCompanyResearch: {
		Name: agencyWorkflowCompanyResearch,
		Kind: workflowKindResearch,
		// UNCHANGED. See the header.
		Profiles: []string{
			"stock-scout",
			"stock-fundamentals",
			"stock-risk",
			"stock-chair",
		},
		JobSchemaVersion:      agencyJobSchemaVersion,
		ArtifactSchemaVersion: agencyArtifactSchemaVersion,
	},
	agencyWorkflowExperimentReview: {
		Name: agencyWorkflowExperimentReview,
		Kind: workflowKindReview,
		// Three stages, and none of them reads the open web.
		//
		//   experiment-auditor — operational, integrity and readiness blockers
		//   evidence-skeptic   — separates missing evidence, insufficient sample and genuinely
		//                        negative evidence, which is the distinction this whole lane exists
		//                        to keep straight
		//   experiment-chair   — the concise final review and the prioritized next checks
		Profiles: []string{
			"experiment-auditor",
			"evidence-skeptic",
			"experiment-chair",
		},
		JobSchemaVersion:      agencyReviewJobSchemaVersion,
		ArtifactSchemaVersion: agencyReviewArtifactSchemaVersion,
	},
}

// lookupAgencyWorkflow resolves a name. `ok == false` for anything not in the registry, and every
// caller treats that as a refusal rather than as a default.
func lookupAgencyWorkflow(name string) (agencyWorkflow, bool) {
	wf, ok := agencyWorkflows[name]
	return wf, ok
}

// agencyWorkflowNames is the offered vocabulary, sorted so the worker preflight's answer is stable.
func agencyWorkflowNames() []string {
	names := make([]string, 0, len(agencyWorkflows))
	for name := range agencyWorkflows {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// agencyChainFor returns the profile chain a workflow means, or nil for an unknown name. Callers
// that hold a stored run use it instead of a package-level slice, so the rules a run is validated
// against are always the rules of the workflow that run was created with.
func agencyChainFor(workflow string) []string {
	wf, ok := lookupAgencyWorkflow(workflow)
	if !ok {
		return nil
	}
	return wf.Profiles
}

// agencyProfileChain is `company_research_v1`'s chain, kept as a named value because it reads
// better at the call sites that are only ever about that workflow — and because agency_test.go
// pins it directly. It is the registry's copy, not a second source of truth.
var agencyProfileChain = agencyWorkflows[agencyWorkflowCompanyResearch].Profiles

// agencyKnownStage reports whether `stage` is a profile in ANY registered chain.
//
// It is the ROUTE-level vocabulary check: enough to keep free text out of a stored record without
// the route needing to load the run first. The per-run check — is this stage part of THIS run's
// workflow — lives in `AgencyStore.Heartbeat`, which is the only place that knows which run it is.
func agencyKnownStage(stage string) bool {
	for _, wf := range agencyWorkflows {
		if containsString(wf.Profiles, stage) {
			return true
		}
	}
	return false
}
