package main

import "time"

// schema.go — the versioned wire contract, restated on the worker side.
//
// WHY IT IS RESTATED RATHER THAN IMPORTED. `bridge` is an independent Go module with zero
// dependencies, exactly as `gateway` is, and it must build and run on a machine that has no copy of
// the server. This repository already handles that situation the same way in five places: the
// session-token verifier is copy-pasted byte-identical across auth/token.go, gateway/auth.go,
// journal/auth.go, alerts/auth.go, paper/auth.go and feedback/auth.go, with a header on each copy
// saying "if you change it, change every copy". This file is that pattern for the job and artifact
// schemas.
//
// THE VERSIONS ARE PINNED, AND A MISMATCH IS A REFUSAL. `jobSchemaVersion` and
// `artifactSchemaVersion` must equal `agencyJobSchemaVersion` / `agencyArtifactSchemaVersion` in
// journal/agency.go. A server that hands this worker a job of a different version gets nothing:
// `Job.validate` refuses it and the run is failed with a stated reason rather than guessed at. That
// is the fail-closed direction — an old worker silently doing its best with a new envelope is how a
// schema change becomes a data-quality incident nobody notices for a month.
//
// schema_test.go pins every string in this file against the values the server uses.

const (
	jobSchemaVersion      = "attestel.agency.job/1"
	artifactSchemaVersion = "attestel.agency.artifact/1"

	// workflowCompanyResearch is the ONLY workflow this bridge will claim. It is the worker's own
	// allowlist, sent on every claim, and checked again on every job that comes back: a server that
	// dispatched something else — by mistake, by a version skew, or because it was compromised —
	// gets a refusal, not an execution. This is the line that makes "claims only allowlisted,
	// versioned job types" true rather than aspirational.
	workflowCompanyResearch = "company_research_v1"

	// bridgeVersion travels in the artifact's identity block so a range of artifacts can later be
	// identified and, if it ever needs to be, revoked. It is a version string and nothing else — it
	// names no machine, no user and no path.
	bridgeVersion = "attestel-hermes-bridge/1.0.0"
)

// Provenance labels. Mirrors journal/agency.go.
const (
	provenanceSourced    = "sourced"
	provenanceCalculated = "calculated"
	provenanceInferred   = "inferred"
	provenanceUnknown    = "unknown"
)

// Research-priority labels. Mirrors journal/agency.go. These are RESEARCH instructions — what to do
// next with the question — and not positions.
var researchPriorities = []string{"investigate", "watch", "reject", "unknown"}

// vetoScope is fixed to the one value the server accepts. A veto may caution against NEW exposure
// and may do nothing else.
const vetoScope = "new_exposure_only"

// ────────────────────────────────────────────────────────────────────────────────────── the job

// Job is what the hosted side hands this worker.
//
// READ THE FIELD LIST AS A SECURITY PROPERTY. There is no prompt, no profile, no toolset, no model,
// no provider, no filesystem path, no shell command and no system prompt — and there is no
// `map[string]any` catch-all in which one could hide. The hosted side names a WORKFLOW and a
// SUBJECT; what that workflow means in terms of Hermes profiles, tools and prompts is decided
// entirely by hermes.go on this machine. A fully compromised server can ask this worker to research
// a different company. It cannot ask it to run a command.
type Job struct {
	SchemaVersion   string `json:"schemaVersion"`
	RunID           string `json:"runId"`
	UserID          string `json:"userId"`
	WorkflowVersion string `json:"workflowVersion"`
	Ticker          string `json:"ticker"`
	Question        string `json:"question"`
	AsOf            string `json:"asOf"`
	Attempt         int    `json:"attempt"`
	MaxAttempts     int    `json:"maxAttempts"`
	LeaseToken      string `json:"leaseToken"`
	LeaseExpiresAt  int64  `json:"leaseExpiresAt"`
}

type claimResponse struct {
	Claimed bool   `json:"claimed"`
	Reason  string `json:"reason"`
	Job     *Job   `json:"job"`
}

// ───────────────────────────────────────────────────────────────────────────────── the artifact

// Artifact is what this worker uploads. It is byte-compatible with journal/agency.go's
// AgencyArtifact, which decodes it with DisallowUnknownFields — so a field added here and not there
// is a 400, not a silent extension.
type Artifact struct {
	SchemaVersion   string `json:"schemaVersion"`
	RunID           string `json:"runId"`
	WorkflowVersion string `json:"workflowVersion"`
	Ticker          string `json:"ticker"`
	Question        string `json:"question"`
	AsOf            string `json:"asOf"`
	ProducedAt      string `json:"producedAt"`

	Sources []Source `json:"sources"`
	Stages  []Stage  `json:"stages"`

	UnresolvedQuestions []string        `json:"unresolvedQuestions"`
	Contradictions      []Contradiction `json:"contradictions"`

	Thesis       Position  `json:"thesis"`
	AntiThesis   Position  `json:"antiThesis"`
	RiskFindings []Finding `json:"riskFindings"`
	Chair        Chair     `json:"chairConclusion"`

	ResearchPriority string `json:"researchPriority"`
	Veto             Veto   `json:"veto"`

	Identity Identity `json:"identity"`
	Degraded []string `json:"degraded"`
}

type Source struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	URL         string `json:"url"`
	PublishedAt string `json:"publishedAt"`
	Publisher   string `json:"publisher,omitempty"`
}

type Finding struct {
	Statement  string   `json:"statement"`
	Provenance string   `json:"provenance"`
	SourceIDs  []string `json:"sourceIds"`
	Basis      string   `json:"basis,omitempty"`
}

type Stage struct {
	Profile   string    `json:"profile"`
	Status    string    `json:"status"`
	Findings  []Finding `json:"findings"`
	Notes     []string  `json:"notes,omitempty"`
	StartedAt string    `json:"startedAt"`
	EndedAt   string    `json:"endedAt"`
}

type Position struct {
	Statement string    `json:"statement"`
	Support   []Finding `json:"support"`
}

type Contradiction struct {
	Statement string   `json:"statement"`
	SourceIDs []string `json:"sourceIds"`
}

type Chair struct {
	Conclusion        string   `json:"conclusion"`
	KeyRisks          []string `json:"keyRisks"`
	WhatWouldChangeIt []string `json:"whatWouldChangeIt"`
}

type Veto struct {
	Raised  bool     `json:"raised"`
	Scope   string   `json:"scope"`
	Reasons []string `json:"reasons"`
}

// Identity is the SAFE operational metadata, and the comment on journal/agency.go's copy explains
// what is missing from it: no model, no provider, no quantization, no temperature, no token count,
// no cost, no session id, no hostname, no path. Those are the local facts this integration exists
// to keep local, and the way to keep them local is to have nowhere to put them.
type Identity struct {
	WorkflowVersion       string   `json:"workflowVersion"`
	ArtifactSchemaVersion string   `json:"artifactSchemaVersion"`
	Profiles              []string `json:"profiles"`
	StagesCompleted       int      `json:"stagesCompleted"`
	BridgeVersion         string   `json:"bridgeVersion"`
}

// ──────────────────────────────────────────────────────────────────────────────── job validation

// validate refuses anything this worker is not certain it understands.
//
// Every check is fail-closed and every one of them has a reason:
//   - the schema version, because a shape we half-understand is worse than one we refuse;
//   - the workflow, because that is the allowlist (see workflowCompanyResearch);
//   - the run id and lease token, because without them nothing can be reported back;
//   - the ticker and question, because they are the only server-controlled values that reach the
//     prompt, and they are re-validated here rather than trusted from a server that might be
//     compromised — the server's own limits are not this worker's guarantee;
//   - the cutoff, because an unparseable one cannot be echoed back and the server compares it.
func (j *Job) validate() error {
	if j == nil {
		return errf("the server claimed a job but sent none")
	}
	if j.SchemaVersion != jobSchemaVersion {
		return errf("job schemaVersion is %q; this bridge understands only %q",
			j.SchemaVersion, jobSchemaVersion)
	}
	if j.WorkflowVersion != workflowCompanyResearch {
		return errf("workflow %q is not on this bridge's allowlist (%q)",
			j.WorkflowVersion, workflowCompanyResearch)
	}
	if j.RunID == "" || j.LeaseToken == "" || j.UserID == "" {
		return errf("the job is missing its run id, user id or lease token")
	}
	if !tickerRE.MatchString(j.Ticker) {
		return errf("ticker %q is not a symbol this bridge accepts", j.Ticker)
	}
	if n := len(j.Question); n == 0 || n > maxQuestionLen {
		return errf("the question is %d characters; this bridge accepts 1..%d", n, maxQuestionLen)
	}
	if hasControlChars(j.Question) {
		return errf("the question contains control characters")
	}
	if _, err := time.Parse(time.RFC3339, j.AsOf); err != nil {
		return errf("the job's asOf is not an RFC3339 timestamp")
	}
	return nil
}
