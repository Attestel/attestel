package main

import (
	"encoding/json"
	"sort"
	"strings"
	"time"
)

// review.go — the `experiment_review_v1` wire contract, restated on the worker side, plus the
// strict decoding of what its three stages produce.
//
// WHY IT IS RESTATED RATHER THAN IMPORTED: schema.go's header. `bridge` is an independent
// zero-dependency module that must build on a machine with no copy of the server.
//
// ─────────────────────────────────────────────────────────────────────────────────────────────
// A REVIEW JOB CARRIES A SNAPSHOT ID, NOT A DOCUMENT AND NOT A URL
// ─────────────────────────────────────────────────────────────────────────────────────────────
// `ReviewJob` has one subject field: `snapshotId`. It is redeemed against ONE lease-scoped route on
// the deployment this worker is already configured for — not fetched from an address the server
// chose. A compromised server can therefore ask this worker to review a different snapshot of the
// owner's own experiment. It cannot ask it to fetch an arbitrary URL, because there is no field in
// which to put one, and `client.go` builds that request's path from constants and the run id.
//
// ─────────────────────────────────────────────────────────────────────────────────────────────
// THE THREE STATUSES ARE NOT THIS WORKER'S TO DECIDE
// ─────────────────────────────────────────────────────────────────────────────────────────────
// The snapshot carries a `derived` block the SERVER computed from the evidence, and the server
// rejects any review whose statuses differ from it. So this bridge copies those three values
// through verbatim and does not let a stage restate them: `assembleReview` reads them from the
// snapshot, never from a stage's output, and no stage schema below has a field for them.
//
// That is why a stage cannot manufacture an `EDGE` verdict here either — not because the prompt
// forbids it, but because there is nowhere in `auditorOutput`, `skepticOutput` or `reviewChairOutput`
// to write one.

const (
	reviewJobSchemaVersion      = "attestel.agency.review-job/1"
	reviewArtifactSchemaVersion = "attestel.agency.review-artifact/1"

	// workflowExperimentReview is the second workflow on this bridge's allowlist. It is declared on
	// every claim and checked again on every job that comes back, exactly as
	// `workflowCompanyResearch` is.
	workflowExperimentReview = "experiment_review_v1"

	// The snapshot schema this bridge understands. A snapshot of another version is refused rather
	// than half-read — the paths a review cites are the paths of a specific schema.
	experimentSnapshotSchemaVersion = "attestel.experiment.snapshot/1"
)

// Next-check priorities. Mirrors journal/experiment_review.go.
var reviewCheckPriorities = []string{"blocking", "high", "routine"}

// ──────────────────────────────────────────────────────────────────────────────────── the job

// ReviewJob is what the hosted side hands this worker for a review.
//
// READ THE FIELD LIST AS A SECURITY PROPERTY, exactly as `Job`'s. No prompt, no profile, no
// toolset, no model, no provider, no filesystem path, no shell command, no URL — and no
// `map[string]any` catch-all in which one could hide.
type ReviewJob struct {
	SchemaVersion   string `json:"schemaVersion"`
	RunID           string `json:"runId"`
	UserID          string `json:"userId"`
	WorkflowVersion string `json:"workflowVersion"`
	SnapshotID      string `json:"snapshotId"`
	AsOf            string `json:"asOf"`
	Attempt         int    `json:"attempt"`
	MaxAttempts     int    `json:"maxAttempts"`
	LeaseToken      string `json:"leaseToken"`
	LeaseExpiresAt  int64  `json:"leaseExpiresAt"`
}

// validate refuses anything this worker is not certain it understands. Same fail-closed posture and
// the same reasons as `Job.validate`.
func (j *ReviewJob) validate() error {
	if j == nil {
		return errf("the server claimed a review but sent none")
	}
	if j.SchemaVersion != reviewJobSchemaVersion {
		return errf("review job schemaVersion is %q; this bridge understands only %q",
			j.SchemaVersion, reviewJobSchemaVersion)
	}
	if j.WorkflowVersion != workflowExperimentReview {
		return errf("workflow %q is not this bridge's review workflow (%q)",
			j.WorkflowVersion, workflowExperimentReview)
	}
	if j.RunID == "" || j.LeaseToken == "" || j.UserID == "" {
		return errf("the review job is missing its run id, user id or lease token")
	}
	if !snapshotIDRE.MatchString(j.SnapshotID) {
		return errf("snapshotId %q is not a snapshot id this bridge accepts", j.SnapshotID)
	}
	if _, err := time.Parse(time.RFC3339, j.AsOf); err != nil {
		return errf("the review job's asOf is not an RFC3339 timestamp")
	}
	return nil
}

// ──────────────────────────────────────────────────────────────────────────────── the snapshot

// DerivedBlocker is one blocker the SERVER derived from the evidence. Mirrors
// journal/experiment_snapshot.go.
type DerivedBlocker struct {
	Code          string   `json:"code"`
	Statement     string   `json:"statement"`
	EvidencePaths []string `json:"evidencePaths"`
}

// SnapshotDerived is the server's authoritative reading. This bridge COPIES it and never overrides
// it. See the header.
// EvaluatorReading mirrors journal/experiment_snapshot.go.
type EvaluatorReading struct {
	Config         string   `json:"config"`
	Verdict        string   `json:"verdict"`
	Validity       string   `json:"validity"`
	ValidityReason string   `json:"validityReason,omitempty"`
	EvidencePaths  []string `json:"evidencePaths"`
}

type SnapshotDerived struct {
	PaperStatus       string `json:"paperStatus"`
	CandidateStatus   string `json:"candidateStatus"`
	OperationalStatus string `json:"operationalStatus"`

	// The evaluator's raw result and whether it can be applied. Copied verbatim into the artifact:
	// the server rejects any restatement, and no stage schema has a field for either.
	EvaluatorResult      string             `json:"evaluatorResult"`
	EvidenceValidity     string             `json:"evidenceValidity"`
	EvaluatorReadings    []EvaluatorReading `json:"evaluatorReadings"`
	EvaluatorResultPaths []string           `json:"evaluatorResultPaths"`

	PaperStatusPaths       []string `json:"paperStatusPaths"`
	CandidateStatusPaths   []string `json:"candidateStatusPaths"`
	OperationalStatusPaths []string `json:"operationalStatusPaths"`

	MandatoryBlockers []DerivedBlocker `json:"mandatoryBlockers"`
	Notes             []string         `json:"notes"`
}

// SnapshotSource is one composed payload. `Payload` is kept as raw JSON: the stage prompt shows the
// snapshot as it was stored, and re-encoding a decoded map would show something else.
type SnapshotSource struct {
	Endpoint     string          `json:"endpoint"`
	State        string          `json:"state"`
	Reason       string          `json:"reason,omitempty"`
	ReportedAsOf string          `json:"reportedAsOf,omitempty"`
	AgeSeconds   *int64          `json:"ageSeconds"`
	Generation   *int64          `json:"generation"`
	Revision     string          `json:"revision,omitempty"`
	Payload      json.RawMessage `json:"payload,omitempty"`
}

// ExperimentSnapshot is the frozen evidence this workflow reviews.
type ExperimentSnapshot struct {
	SchemaVersion string                    `json:"schemaVersion"`
	ID            string                    `json:"id"`
	UserID        string                    `json:"userId"`
	AsOf          string                    `json:"asOf"`
	CapturedAt    int64                     `json:"capturedAt"`
	Generation    int64                     `json:"generation"`
	Revision      string                    `json:"revision"`
	Sources       map[string]SnapshotSource `json:"sources"`
	Derived       SnapshotDerived           `json:"derived"`
}

// validate refuses a snapshot this bridge cannot review honestly.
func (s *ExperimentSnapshot) validate(job *ReviewJob) error {
	if s == nil {
		return errf("the server served no evidence snapshot for this review")
	}
	if s.SchemaVersion != experimentSnapshotSchemaVersion {
		return errf("the snapshot's schemaVersion is %q; this bridge understands only %q",
			s.SchemaVersion, experimentSnapshotSchemaVersion)
	}
	if s.ID != job.SnapshotID {
		return errf("the server served snapshot %q for a review of %q", s.ID, job.SnapshotID)
	}
	if len(s.Sources) == 0 {
		return errf("the snapshot carries no sources")
	}
	// The three derived statuses must be present and in vocabulary. This bridge copies them into
	// the artifact verbatim, so a snapshot that did not state them would produce a review the
	// server rejects — better to say so here, with a reason the operator can act on.
	if !containsString([]string{"unjudged", "collecting", "measurable"}, s.Derived.PaperStatus) ||
		!containsString([]string{"edge", "no_edge", "inconclusive", "unknown"}, s.Derived.CandidateStatus) ||
		!containsString([]string{"verified", "degraded", "unavailable"}, s.Derived.OperationalStatus) {
		return errf("the snapshot's derived statuses are missing or outside the vocabularies this " +
			"bridge understands")
	}
	return nil
}

// snapshotPaths returns every addressable field path in the snapshot.
//
// It mirrors journal/experiment_snapshot.go's `snapshotPathIndex` exactly, INCLUDING the notation,
// and exists for the reason stage.go's local validation exists: a citation the server would reject
// should fail here, with a message naming the offending path, rather than as a remote 400 after the
// owner's machine has already done the work.
func snapshotPaths(snap *ExperimentSnapshot) map[string]bool {
	index := map[string]bool{}
	encoded, err := json.Marshal(snap)
	if err != nil {
		return index
	}
	var tree any
	if err := json.Unmarshal(encoded, &tree); err != nil {
		return index
	}
	walkPaths("", tree, index)
	return index
}

func walkPaths(prefix string, node any, index map[string]bool) {
	if len(index) >= maxSnapshotPaths {
		return
	}
	if prefix != "" {
		index[prefix] = true
	}
	switch typed := node.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for k := range typed {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			child := k
			if prefix != "" {
				child = prefix + "." + k
			}
			walkPaths(child, typed[k], index)
		}
	case []any:
		for i, item := range typed {
			walkPaths(prefix+"["+itoa(i)+"]", item, index)
		}
	}
}

const maxSnapshotPaths = 20000

// ──────────────────────────────────────────────────────────────────────── what a stage may return

// rawReviewClaim is one blocker, unknown or contradiction as a stage reports it.
//
// NOTE WHAT IS ABSENT: there is no severity number, no probability, no direction and no
// recommendation. A stage that concluded "sell" has nowhere to write it — the same structural
// argument stage.go makes about `riskOutput`.
type rawReviewClaim struct {
	Code          string   `json:"code"`
	Statement     string   `json:"statement"`
	EvidencePaths []string `json:"evidencePaths"`
}

type rawReviewCheck struct {
	Statement     string   `json:"statement"`
	Priority      string   `json:"priority"`
	EvidencePaths []string `json:"evidencePaths"`
}

// auditorOutput is what `experiment-auditor` returns: operational, integrity and readiness
// blockers, and the things it could not establish.
type auditorOutput struct {
	Blockers []rawReviewClaim `json:"blockers"`
	Unknowns []rawReviewClaim `json:"unknowns"`
	Notes    []string         `json:"notes"`
}

// skepticOutput is what `evidence-skeptic` returns.
//
// Its three lists are the distinction this whole lane exists to keep straight, and they are
// SEPARATE FIELDS so a stage cannot blur them: `missingEvidence` is "nobody measured this",
// `insufficientSample` is "it was measured and the sample cannot decide", and `negativeEvidence` is
// "it was measured and the answer is no". Collapsing those three into one list is precisely how a
// paper experiment that has never run gets reported as a strategy that failed.
type skepticOutput struct {
	MissingEvidence    []rawReviewClaim `json:"missingEvidence"`
	InsufficientSample []rawReviewClaim `json:"insufficientSample"`
	NegativeEvidence   []rawReviewClaim `json:"negativeEvidence"`
	Contradictions     []rawReviewClaim `json:"contradictions"`
	Notes              []string         `json:"notes"`
}

// reviewChairOutput is the final synthesis.
//
// There is NO field here for any of the three statuses. The chair explains the evidence; it does not
// grade it, because the grading was already done deterministically by the server and copied into
// the artifact from the snapshot. See the header.
type reviewChairOutput struct {
	Summary              string           `json:"summary"`
	PaperRationale       string           `json:"paperRationale"`
	CandidateRationale   string           `json:"candidateRationale"`
	OperationalRationale string           `json:"operationalRationale"`
	Blockers             []rawReviewClaim `json:"blockers"`
	Unknowns             []rawReviewClaim `json:"unknowns"`
	Contradictions       []rawReviewClaim `json:"contradictions"`
	NextChecks           []rawReviewCheck `json:"nextChecks"`
	Notes                []string         `json:"notes"`
}

// ──────────────────────────────────────────────────────────────────────────────── the artifact

// ReviewVerdict, ReviewClaim, ReviewCheck, EvidenceReference, ReviewStage, ReviewIdentity and
// ExperimentReviewArtifact are byte-compatible with journal/experiment_review.go, which decodes
// them with DisallowUnknownFields — so a field added here and not there is a 400, not a silent
// extension.
type ReviewVerdict struct {
	Value         string   `json:"value"`
	EvidencePaths []string `json:"evidencePaths"`
	Rationale     string   `json:"rationale"`
}

type ReviewClaim struct {
	Code          string   `json:"code,omitempty"`
	Statement     string   `json:"statement"`
	EvidencePaths []string `json:"evidencePaths"`
}

type ReviewCheck struct {
	Statement     string   `json:"statement"`
	Priority      string   `json:"priority"`
	EvidencePaths []string `json:"evidencePaths"`
}

type EvidenceReference struct {
	Path     string `json:"path"`
	Presence string `json:"presence"`
}

type ReviewStage struct {
	Profile   string   `json:"profile"`
	Status    string   `json:"status"`
	Notes     []string `json:"notes,omitempty"`
	StartedAt string   `json:"startedAt"`
	EndedAt   string   `json:"endedAt"`
}

type ReviewIdentity struct {
	WorkflowVersion       string   `json:"workflowVersion"`
	ArtifactSchemaVersion string   `json:"artifactSchemaVersion"`
	Profiles              []string `json:"profiles"`
	StagesCompleted       int      `json:"stagesCompleted"`
	BridgeVersion         string   `json:"bridgeVersion"`
}

type ExperimentReviewArtifact struct {
	SchemaVersion   string `json:"schemaVersion"`
	RunID           string `json:"runId"`
	WorkflowVersion string `json:"workflowVersion"`

	SnapshotID            string `json:"snapshotId"`
	Generation            int64  `json:"generation"`
	Cutoff                string `json:"cutoff"`
	SnapshotSchemaVersion string `json:"snapshotSchemaVersion"`

	AsOf       string `json:"asOf"`
	ProducedAt string `json:"producedAt"`

	PaperStatus       ReviewVerdict `json:"paperStatus"`
	CandidateStatus   ReviewVerdict `json:"candidateStatus"`
	OperationalStatus ReviewVerdict `json:"operationalStatus"`

	EvaluatorResult  string `json:"evaluatorResult"`
	EvidenceValidity string `json:"evidenceValidity"`

	Blockers       []ReviewClaim `json:"blockers"`
	Unknowns       []ReviewClaim `json:"unknowns"`
	Contradictions []ReviewClaim `json:"contradictions"`
	NextChecks     []ReviewCheck `json:"nextChecks"`

	EvidenceReferences []EvidenceReference `json:"evidenceReferences"`

	Summary  string         `json:"summary"`
	Stages   []ReviewStage  `json:"stages"`
	Identity ReviewIdentity `json:"identity"`
	Degraded []string       `json:"degraded"`
}

// ─────────────────────────────────────────────────────────────────────────────── claim conversion

// convertReviewClaims validates a stage's claims against the snapshot's real paths.
//
// A CLAIM THAT CITES NOTHING IS DROPPED, NOT ACCEPTED — and a claim that cites a path the snapshot
// does not have FAILS THE RUN. The asymmetry is deliberate: an uncited observation is a stage being
// vague, which costs the review a sentence; an invented path is a stage fabricating evidence, which
// is the failure this schema exists to catch, and it must be loud.
func convertReviewClaims(in []rawReviewClaim, index map[string]bool, label string) ([]ReviewClaim, error) {
	if len(in) > reviewMaxItems {
		return nil, errf("%s returned %d entries; the limit is %d", label, len(in), reviewMaxItems)
	}
	out := make([]ReviewClaim, 0, len(in))
	for i, c := range in {
		statement := strings.TrimSpace(c.Statement)
		if statement == "" {
			return nil, errf("%s[%d] has no statement", label, i)
		}
		paths, err := cleanEvidencePaths(c.EvidencePaths, index, label, i)
		if err != nil {
			return nil, err
		}
		if len(paths) == 0 {
			// Dropped rather than failed. See the header.
			continue
		}
		out = append(out, ReviewClaim{
			Code:          strings.TrimSpace(c.Code),
			Statement:     clip(statement, maxStatementLen),
			EvidencePaths: paths,
		})
	}
	return out, nil
}

func convertReviewChecks(in []rawReviewCheck, index map[string]bool) ([]ReviewCheck, error) {
	if len(in) > reviewMaxItems {
		return nil, errf("the chair returned %d next checks; the limit is %d", len(in), reviewMaxItems)
	}
	out := make([]ReviewCheck, 0, len(in))
	for i, c := range in {
		statement := strings.TrimSpace(c.Statement)
		if statement == "" {
			return nil, errf("nextChecks[%d] has no statement", i)
		}
		priority := strings.TrimSpace(c.Priority)
		if !containsString(reviewCheckPriorities, priority) {
			return nil, errf("nextChecks[%d] has priority %q; expected one of %s",
				i, priority, strings.Join(reviewCheckPriorities, ", "))
		}
		paths, err := cleanEvidencePaths(c.EvidencePaths, index, "nextChecks", i)
		if err != nil {
			return nil, err
		}
		if len(paths) == 0 {
			continue
		}
		out = append(out, ReviewCheck{
			Statement: clip(statement, maxStatementLen), Priority: priority, EvidencePaths: paths,
		})
	}
	return out, nil
}

// cleanEvidencePaths de-duplicates, caps, and REFUSES a path the snapshot does not contain.
func cleanEvidencePaths(in []string, index map[string]bool, label string, i int) ([]string, error) {
	out := make([]string, 0, len(in))
	for _, p := range in {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !index[p] {
			return nil, errf("%s[%d] cites %q, which is not a field in this snapshot. Every "+
				"blocker and conclusion must reference evidence that actually exists", label, i, p)
		}
		if !containsString(out, p) {
			out = append(out, p)
		}
	}
	if len(out) > reviewMaxPaths {
		out = out[:reviewMaxPaths]
	}
	return out, nil
}

const (
	reviewMaxItems   = 40
	reviewMaxPaths   = 12
	reviewMaxSummary = 4000
)
