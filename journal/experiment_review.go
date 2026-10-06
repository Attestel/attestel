package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// experiment_review.go — the `experiment_review_v1` artifact: what a Hermes chain is allowed to say
// about a stored evidence snapshot, and the rules that keep it from saying anything else.
//
// ─────────────────────────────────────────────────────────────────────────────────────────────
// WHAT THIS SCHEMA CANNOT EXPRESS
// ─────────────────────────────────────────────────────────────────────────────────────────────
// Read the field list the way agency.go's header asks you to read `AgencyArtifact`'s. There is no
// direction, no signal, no price, no target, no expected return, no probability, no confidence, no
// position size, no weight, no entry, no stop and no recommendation — and no `map[string]any` in
// which one could hide. A fully compromised worker can write a WRONG READING of the experiment into
// this record. It cannot write a trade, because there is nowhere to put one.
//
// The `agencyBannedPhrases` scan runs over every string an agent composed here, exactly as it does
// for the research artifact, so an attempt to write a recommendation into prose is a refusal too.
//
// ─────────────────────────────────────────────────────────────────────────────────────────────
// THE THREE STATUSES ARE THE SERVER'S, NOT THE MODEL'S
// ─────────────────────────────────────────────────────────────────────────────────────────────
// `deriveSnapshotVerdicts` (experiment_snapshot.go) computes `paperStatus`, `candidateStatus` and
// `operationalStatus` deterministically from the stored snapshot bytes. This validator REJECTS any
// artifact whose values differ. So:
//
//   - Hermes cannot create or manufacture an `EDGE` verdict. `candidateStatus: edge` is storable
//     only when the snapshot itself carries an EDGE verdict that is current and evidence-current,
//     which is what `paper/gates.go` requires before the engine may act.
//   - Hermes cannot drop a blocker. Every mandatory blocker code the server derived must appear in
//     `blockers[]`, so "the model decided the synthetic-training blocker was not important" is not
//     a reachable outcome.
//   - Hermes cannot turn missing information into a value. A source that could not be read derives
//     `unknown` / `unavailable` on the server side, and the artifact must agree.
//
// What the chain genuinely contributes is everything the server cannot compute: WHY a blocker
// matters, what is honestly unknown, which pieces of evidence contradict each other, and which
// checks are worth doing next. That is the review.
//
// ─────────────────────────────────────────────────────────────────────────────────────────────
// EVERY CLAIM CITES A FIELD THAT EXISTS
// ─────────────────────────────────────────────────────────────────────────────────────────────
// Each status, blocker, unknown, contradiction and next check carries `evidencePaths`, and every
// path must resolve in the snapshot's path index (`snapshotPathIndex`). An invented path is a
// rejected artifact, not a plausible-looking citation. This is the review lane's equivalent of the
// research lane's rule that a finding may not cite a source the run never declared.

const (
	// Review limits. Explicit ceilings, for the reason agency.go's limits block gives.
	reviewMaxItemsPerList  = 40
	reviewMaxPathsPerItem  = 12
	reviewMaxStatementLen  = 2000
	reviewMaxSummaryLen    = 4000
	reviewMaxArtifactBytes = 256 << 10
	reviewMaxNotesPerStage = 20
)

// Next-check priorities. RESEARCH/OPERATIONS instructions, not actions — "verify" is a thing to do
// to the deployment, never a thing to do to a position. There is deliberately no numeric score.
const (
	checkPriorityBlocking = "blocking" // nothing downstream is meaningful until this is resolved
	checkPriorityHigh     = "high"
	checkPriorityRoutine  = "routine"
)

var reviewCheckPriorities = []string{checkPriorityBlocking, checkPriorityHigh, checkPriorityRoutine}

// ReviewVerdict is one of the three statuses, with the evidence it rests on.
//
// It is an OBJECT rather than a bare string so that "every conclusion must reference a field in the
// captured snapshot" applies to the conclusions that matter most. A status with no citation would
// be the one place in this schema where an assertion needed no grounding.
type ReviewVerdict struct {
	Value         string   `json:"value"`
	EvidencePaths []string `json:"evidencePaths"`
	Rationale     string   `json:"rationale"`
}

// ReviewClaim is one blocker, unknown or contradiction.
//
// `Code` is set on a blocker that answers one of the server's mandatory codes and empty on a claim
// the chain raised itself. Both are legitimate; only the mandatory ones are compulsory.
type ReviewClaim struct {
	Code          string   `json:"code,omitempty"`
	Statement     string   `json:"statement"`
	EvidencePaths []string `json:"evidencePaths"`
}

// ReviewCheck is one prioritized next step. It is a check to run against the EXPERIMENT — read a
// log, re-run the evaluator, fix a desync — and the schema has no field in which it could become an
// instruction about a position.
type ReviewCheck struct {
	Statement     string   `json:"statement"`
	Priority      string   `json:"priority"`
	EvidencePaths []string `json:"evidencePaths"`
}

// EvidenceReference is one snapshot path the review actually leaned on, with what was found there.
type EvidenceReference struct {
	Path     string `json:"path"`
	Presence string `json:"presence"` // present | absent
}

const (
	evidencePresent = "present"
	evidenceAbsent  = "absent"
)

// ReviewStage is one Hermes stage's account of itself.
type ReviewStage struct {
	Profile   string   `json:"profile"`
	Status    string   `json:"status"` // ok | skipped | failed
	Notes     []string `json:"notes,omitempty"`
	StartedAt string   `json:"startedAt"`
	EndedAt   string   `json:"endedAt"`
}

// ReviewIdentity is the SAFE operational metadata. Same rule as AgencyIdentity: no model, no
// provider, no quantization, no temperature, no token count, no cost, no session id, no hostname
// and no path. Those are the local facts this integration exists to keep local.
type ReviewIdentity struct {
	WorkflowVersion       string   `json:"workflowVersion"`
	ArtifactSchemaVersion string   `json:"artifactSchemaVersion"`
	Profiles              []string `json:"profiles"`
	StagesCompleted       int      `json:"stagesCompleted"`
	BridgeVersion         string   `json:"bridgeVersion"`
}

// ExperimentReviewArtifact is the stored review.
type ExperimentReviewArtifact struct {
	SchemaVersion   string `json:"schemaVersion"`
	RunID           string `json:"runId"`
	WorkflowVersion string `json:"workflowVersion"`

	// The snapshot this review is ABOUT, restated so the artifact is self-describing. All three are
	// checked against the stored snapshot rather than trusted: a worker that could restate the
	// generation or the cutoff could attach a review of one experiment to another.
	SnapshotID            string `json:"snapshotId"`
	Generation            int64  `json:"generation"`
	Cutoff                string `json:"cutoff"`
	SnapshotSchemaVersion string `json:"snapshotSchemaVersion"`

	// AsOf is the RUN's server-assigned cutoff; ProducedAt is when the chain finished.
	AsOf       string `json:"asOf"`
	ProducedAt string `json:"producedAt"`

	PaperStatus       ReviewVerdict `json:"paperStatus"`
	CandidateStatus   ReviewVerdict `json:"candidateStatus"`
	OperationalStatus ReviewVerdict `json:"operationalStatus"`

	// THE EVALUATOR'S RAW RESULT AND ITS VALIDITY, carried through so the UI can render them beside
	// the conclusion rather than leaving a reader to infer a `NO EDGE` from a blocker. Both are
	// copied from the snapshot's derived block and checked against it below — a worker may no more
	// restate the result than it may restate a status.
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

// ──────────────────────────────────────────────────────────────────────────────────── validation

// validateExperimentReview is the whole gate. It runs on the SERVER, over what a worker actually
// sent, against the snapshot this server stored. Every failure is fail-closed and every one of them
// makes the run FAILED with a stated reason — a partially-accepted review is worse than none,
// because it is indistinguishable from a whole one once it is stored.
func validateExperimentReview(a *ExperimentReviewArtifact, run AgencyRun, snap ExperimentSnapshot) error {
	if a == nil {
		return invalidArtifact("no review artifact was supplied")
	}
	if a.SchemaVersion != agencyReviewArtifactSchemaVersion {
		return invalidArtifact("review schemaVersion is %q; this server accepts only %q",
			a.SchemaVersion, agencyReviewArtifactSchemaVersion)
	}
	if a.RunID != run.ID {
		return invalidArtifact("review runId %q does not belong to run %q", a.RunID, run.ID)
	}
	if a.WorkflowVersion != run.WorkflowVersion {
		return invalidArtifact("review workflowVersion is %q; the run is %q",
			a.WorkflowVersion, run.WorkflowVersion)
	}

	// --- the snapshot this review claims to be about ------------------------------------------
	//
	// Checked against the STORED snapshot, never trusted from the payload. A worker that could
	// restate the generation or the cutoff could attach a review of one generation's evidence to
	// another's run, which is the cross-generation confusion the snapshot exists to prevent.
	if a.SnapshotID != snap.ID {
		return invalidArtifact("the review names snapshot %q; run %q was created against %q",
			a.SnapshotID, run.ID, snap.ID)
	}
	if a.Generation != snap.Generation {
		return invalidArtifact("the review states generation %d; the snapshot is generation %d",
			a.Generation, snap.Generation)
	}
	if a.Cutoff != snap.AsOf {
		return invalidArtifact("the review states cutoff %q; the snapshot's cutoff is %q",
			a.Cutoff, snap.AsOf)
	}
	if a.SnapshotSchemaVersion != snap.SchemaVersion {
		return invalidArtifact("the review states snapshot schema %q; the snapshot is %q",
			a.SnapshotSchemaVersion, snap.SchemaVersion)
	}
	if a.AsOf != run.AsOf {
		return invalidArtifact("review asOf %q does not match the run's server-assigned cutoff %q",
			a.AsOf, run.AsOf)
	}
	if _, err := time.Parse(time.RFC3339, a.ProducedAt); err != nil {
		return invalidArtifact("review producedAt is not an RFC3339 timestamp")
	}

	index, err := snapshotPathIndex(&snap)
	if err != nil {
		return invalidArtifact("the snapshot could not be indexed for citation checking")
	}

	// --- the evaluator's raw result and its validity -------------------------------------------
	//
	// Restated by the worker, checked against the server's own derivation. These are the two fields
	// that keep a real `NO EDGE` visible when the CONCLUSION is `unknown`, so a worker that could
	// alter them could hide the very result this pair exists to surface.
	if a.EvaluatorResult != snap.Derived.EvaluatorResult {
		return invalidArtifact("evaluatorResult is %q; this server read %q from the snapshot. The "+
			"evaluator's own result is evidence, not a conclusion, and may not be restated",
			a.EvaluatorResult, snap.Derived.EvaluatorResult)
	}
	if a.EvidenceValidity != snap.Derived.EvidenceValidity {
		return invalidArtifact("evidenceValidity is %q; this server derives %q",
			a.EvidenceValidity, snap.Derived.EvidenceValidity)
	}

	// --- the three statuses -------------------------------------------------------------------
	//
	// EACH MUST EQUAL THE VALUE THIS SERVER DERIVED. See the header: this is the check that makes
	// "Hermes cannot manufacture an EDGE verdict" structural.
	for _, s := range []struct {
		label   string
		got     ReviewVerdict
		want    string
		allowed []string
	}{
		{"paperStatus", a.PaperStatus, snap.Derived.PaperStatus,
			[]string{paperUnjudged, paperCollecting, paperMeasurable}},
		{"candidateStatus", a.CandidateStatus, snap.Derived.CandidateStatus,
			[]string{candidateEdge, candidateNoEdge, candidateInconclusive, candidateUnknown}},
		{"operationalStatus", a.OperationalStatus, snap.Derived.OperationalStatus,
			[]string{operationalVerified, operationalDegraded, operationalUnavailable}},
	} {
		if !containsString(s.allowed, s.got.Value) {
			return invalidArtifact("%s is %q; the vocabulary is %s",
				s.label, s.got.Value, strings.Join(s.allowed, ", "))
		}
		if s.got.Value != s.want {
			return invalidArtifact("%s is %q, but this server derives %q from the stored snapshot. "+
				"The three statuses are computed from the evidence and may not be restated by a "+
				"worker; a review that disagrees with the snapshot is rejected rather than stored",
				s.label, s.got.Value, s.want)
		}
		if len(s.got.EvidencePaths) == 0 {
			return invalidArtifact("%s cites no snapshot field; every conclusion must reference "+
				"one", s.label)
		}
		if err := validateReviewPaths(s.label, s.got.EvidencePaths, index); err != nil {
			return err
		}
		if strings.TrimSpace(s.got.Rationale) == "" {
			return invalidArtifact("%s carries no rationale", s.label)
		}
	}

	// --- claims -------------------------------------------------------------------------------
	for _, group := range []struct {
		label string
		items []ReviewClaim
	}{
		{"blockers", a.Blockers},
		{"unknowns", a.Unknowns},
		{"contradictions", a.Contradictions},
	} {
		if len(group.items) > reviewMaxItemsPerList {
			return invalidArtifact("%s carries %d entries; the limit is %d",
				group.label, len(group.items), reviewMaxItemsPerList)
		}
		for i, item := range group.items {
			if strings.TrimSpace(item.Statement) == "" {
				return invalidArtifact("%s[%d] has no statement", group.label, i)
			}
			if len(item.EvidencePaths) == 0 {
				return invalidArtifact("%s[%d] cites no snapshot field; every blocker, unknown and "+
					"contradiction must reference one", group.label, i)
			}
			if err := validateReviewPaths(fmt.Sprintf("%s[%d]", group.label, i),
				item.EvidencePaths, index); err != nil {
				return err
			}
		}
	}

	// --- EVERY MANDATORY BLOCKER MUST BE PRESENT, BY IDENTITY ----------------------------------
	//
	// The server derived these from the snapshot. A review that omits one is a review that dropped a
	// finding the evidence compels — the failure mode a reviewer is most tempted into, because a
	// report with fewer blockers reads better.
	//
	// MATCHING ON THE CODE ALONE WAS NOT ENOUGH, AND THE HOLE WAS REAL. Several codes are raised
	// PER CONFIG and PER SOURCE: three unreadable sources produce three `source-unavailable`
	// blockers, each about a different source and each citing different paths. A code-only check was
	// satisfied by any ONE of them, so a review could silently drop two thirds of the evidence's
	// blockers and still be accepted. The same hole covered `synthetic-trained-model` across several
	// configs — exactly the blocker this lane most needs to survive.
	//
	// So a mandatory blocker is identified by its CODE **and its evidence paths**: the pair is what
	// makes it *this* blocker rather than another with the same label. A candidate matches when it
	// carries the same code and cites EVERY path the derived blocker cites. Citing more is fine —
	// a reviewer may add corroborating evidence — but citing fewer means it is answering a
	// different, weaker claim.
	missing := []string{}
	for _, want := range snap.Derived.MandatoryBlockers {
		if !blockerAnswered(want, a.Blockers) {
			missing = append(missing, want.Code+" ("+strings.Join(want.EvidencePaths, ", ")+")")
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return invalidArtifact("the review omits %d blocker(s) the evidence compels: %s. These are "+
			"derived from the snapshot by this server, are identified by their code AND the fields "+
			"they cite, and may not be dropped or merged by a reviewer",
			len(missing), strings.Join(dedupeStrings(missing), "; "))
	}

	// --- next checks ---------------------------------------------------------------------------
	if len(a.NextChecks) > reviewMaxItemsPerList {
		return invalidArtifact("nextChecks carries %d entries; the limit is %d",
			len(a.NextChecks), reviewMaxItemsPerList)
	}
	for i, c := range a.NextChecks {
		if strings.TrimSpace(c.Statement) == "" {
			return invalidArtifact("nextChecks[%d] has no statement", i)
		}
		if !containsString(reviewCheckPriorities, c.Priority) {
			return invalidArtifact("nextChecks[%d] has priority %q; the vocabulary is %s",
				i, c.Priority, strings.Join(reviewCheckPriorities, ", "))
		}
		if len(c.EvidencePaths) == 0 {
			return invalidArtifact("nextChecks[%d] cites no snapshot field", i)
		}
		if err := validateReviewPaths(fmt.Sprintf("nextChecks[%d]", i), c.EvidencePaths,
			index); err != nil {
			return err
		}
	}

	// --- evidence references --------------------------------------------------------------------
	if len(a.EvidenceReferences) == 0 {
		return invalidArtifact("the review lists no evidence references")
	}
	for i, ref := range a.EvidenceReferences {
		if !index[ref.Path] {
			return invalidArtifact("evidenceReferences[%d] cites %q, which is not a field in "+
				"snapshot %s", i, ref.Path, snap.ID)
		}
		if ref.Presence != evidencePresent && ref.Presence != evidenceAbsent {
			return invalidArtifact("evidenceReferences[%d] has presence %q; expected %q or %q",
				i, ref.Presence, evidencePresent, evidenceAbsent)
		}
	}

	// --- summary and stages ----------------------------------------------------------------------
	if strings.TrimSpace(a.Summary) == "" {
		return invalidArtifact("the review has no summary")
	}
	if len(a.Summary) > reviewMaxSummaryLen {
		return invalidArtifact("the summary is %d characters; the limit is %d",
			len(a.Summary), reviewMaxSummaryLen)
	}

	chain := agencyChainFor(run.WorkflowVersion)
	if len(chain) == 0 {
		return invalidArtifact("run %q names workflow %q, which this server does not dispatch",
			run.ID, run.WorkflowVersion)
	}
	if len(a.Stages) != len(chain) {
		return invalidArtifact("the review carries %d stages; %s runs exactly %d",
			len(a.Stages), run.WorkflowVersion, len(chain))
	}
	completed := 0
	for i, st := range a.Stages {
		if st.Profile != chain[i] {
			return invalidArtifact("stage %d is %q; %s runs %v in that order",
				i, st.Profile, run.WorkflowVersion, chain)
		}
		switch st.Status {
		case "ok":
			completed++
		case "skipped", "failed":
		default:
			return invalidArtifact("stage %s has status %q; expected ok, skipped or failed",
				st.Profile, st.Status)
		}
		if len(st.Notes) > reviewMaxNotesPerStage {
			return invalidArtifact("stage %s carries %d notes; the limit is %d",
				st.Profile, len(st.Notes), reviewMaxNotesPerStage)
		}
		if _, err := time.Parse(time.RFC3339, st.StartedAt); err != nil {
			return invalidArtifact("stage %s startedAt is not an RFC3339 timestamp", st.Profile)
		}
		if _, err := time.Parse(time.RFC3339, st.EndedAt); err != nil {
			return invalidArtifact("stage %s endedAt is not an RFC3339 timestamp", st.Profile)
		}
	}

	// --- identity ---------------------------------------------------------------------------------
	if a.Identity.ArtifactSchemaVersion != agencyReviewArtifactSchemaVersion ||
		a.Identity.WorkflowVersion != run.WorkflowVersion {
		return invalidArtifact("identity does not restate the review schema and workflow versions")
	}
	if len(a.Identity.Profiles) != len(chain) {
		return invalidArtifact("identity names %d profiles; the chain has %d",
			len(a.Identity.Profiles), len(chain))
	}
	for i, p := range a.Identity.Profiles {
		if p != chain[i] {
			return invalidArtifact("identity profile %d is %q; expected %q", i, p, chain[i])
		}
	}
	if a.Identity.StagesCompleted != completed {
		return invalidArtifact("identity claims %d completed stages; %d stages reported ok",
			a.Identity.StagesCompleted, completed)
	}

	// --- the two content scans, over EVERY string the review carries -------------------------------
	//
	// THE SAME TABLES THE RESEARCH LANE USES, deliberately. A leak is a leak and a recommendation is
	// a recommendation whichever workflow produced it, and two divergent copies of one rule is how
	// the weaker copy becomes the one that matters.
	for _, text := range reviewArtifactStrings(a) {
		if len(text) > reviewMaxStatementLen && text != a.Summary {
			return invalidArtifact("a statement is %d characters; the limit is %d",
				len(text), reviewMaxStatementLen)
		}
		if reason := matchAgencyLeak(text); reason != "" {
			return invalidArtifact("the review contains %s, which may not leave the worker", reason)
		}
		if re := matchAgencyBanned(text); re != "" {
			return invalidArtifact("the review contains prescriptive language (%s). This lane "+
				"explains an experiment; it never produces a recommendation, a target or a "+
				"position", re)
		}
	}
	return nil
}

// blockerAnswered reports whether any of the artifact's blockers actually answers `want`.
//
// The test is: same code, and cites every path `want` cites. A derived blocker with no paths at all
// cannot exist (every one this server emits carries at least one), but if one ever did, the code
// match alone would answer it — which is the correct degenerate behaviour rather than an
// unsatisfiable requirement.
func blockerAnswered(want DerivedBlocker, got []ReviewClaim) bool {
	for _, c := range got {
		if c.Code != want.Code {
			continue
		}
		cited := make(map[string]bool, len(c.EvidencePaths))
		for _, p := range c.EvidencePaths {
			cited[strings.TrimSpace(p)] = true
		}
		covers := true
		for _, p := range want.EvidencePaths {
			if !cited[strings.TrimSpace(p)] {
				covers = false
				break
			}
		}
		if covers {
			return true
		}
	}
	return false
}

// validateReviewPaths refuses a citation that does not resolve in the snapshot.
func validateReviewPaths(label string, paths []string, index map[string]bool) error {
	if len(paths) > reviewMaxPathsPerItem {
		return invalidArtifact("%s cites %d snapshot fields; the limit is %d",
			label, len(paths), reviewMaxPathsPerItem)
	}
	for _, p := range paths {
		if !index[strings.TrimSpace(p)] {
			return invalidArtifact("%s cites %q, which is not a field in this snapshot. Every "+
				"blocker and conclusion must reference a field that actually exists in the "+
				"captured evidence", label, p)
		}
	}
	return nil
}

// reviewArtifactStrings flattens EVERY free-text string the review carries, so neither scan can be
// evaded by putting the text in a field nobody remembered to check.
//
// UNLIKE the research artifact there is no quoted/authored split here, and there should not be: a
// review of this deployment's own snapshot quotes no third party. Every sentence in it was composed
// by a stage, so every sentence is scanned.
func reviewArtifactStrings(a *ExperimentReviewArtifact) []string {
	var out []string
	add := func(vs ...string) { out = append(out, vs...) }
	addClaims := func(items []ReviewClaim) {
		for _, c := range items {
			add(c.Statement)
		}
	}
	add(a.PaperStatus.Rationale, a.CandidateStatus.Rationale, a.OperationalStatus.Rationale)
	addClaims(a.Blockers)
	addClaims(a.Unknowns)
	addClaims(a.Contradictions)
	for _, c := range a.NextChecks {
		add(c.Statement)
	}
	add(a.Summary)
	for _, st := range a.Stages {
		add(st.Notes...)
	}
	add(a.Degraded...)
	add(a.Identity.BridgeVersion)
	return out
}

func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// ──────────────────────────────────────────────────────────────────────────── the create request

// experimentReviewRequest is the owner's request, and its field list is the security property.
//
// TWO FIELDS. A workflow NAME and a SNAPSHOT ID. There is no prompt, no question, no ticker, no
// URL, no filesystem path, no profile, no toolset, no model and no command, and there must never
// be one: that absence is what keeps this from being a remote-execution API. It is decoded with
// DisallowUnknownFields, so a request that tried to add one is a 400 rather than a silently
// ignored key.
type experimentReviewRequest struct {
	Workflow   string `json:"workflow"`
	SnapshotID string `json:"snapshotId"`
}

// normalise validates and returns the snapshot id. Fail-closed on everything.
func (req experimentReviewRequest) normalise() (string, error) {
	workflow := strings.TrimSpace(req.Workflow)
	if workflow == "" {
		workflow = agencyWorkflowExperimentReview
	}
	if workflow != agencyWorkflowExperimentReview {
		return "", fmt.Errorf("workflow %q is not a review workflow; this route runs only %q",
			workflow, agencyWorkflowExperimentReview)
	}
	id := strings.TrimSpace(req.SnapshotID)
	if id == "" {
		return "", fmt.Errorf("a snapshotId is required; a review is always ABOUT a stored snapshot")
	}
	if !experimentSnapshotIDRE.MatchString(id) {
		return "", fmt.Errorf("snapshotId %q is not a snapshot id this server issues", id)
	}
	return id, nil
}
