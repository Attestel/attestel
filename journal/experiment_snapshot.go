package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// experiment_snapshot.go — an IMMUTABLE, deterministically-assembled record of what the live paper
// experiment said at one instant.
//
// ─────────────────────────────────────────────────────────────────────────────────────────────
// WHY A SNAPSHOT AND NOT A LIVE READ
// ─────────────────────────────────────────────────────────────────────────────────────────────
// A review that reads live endpoints is a review of five different moments. The dashboard can be
// read before a reset and the status after it, and the resulting document would describe an
// experiment that never existed — with nothing in it able to say so. So the evidence is frozen
// first, under ONE server-assigned `asOf`, and everything downstream (the Hermes chain, the stored
// review, the UI) refers to that frozen document by id.
//
// ─────────────────────────────────────────────────────────────────────────────────────────────
// THREE STATES, NEVER COLLAPSED: unavailable ≠ stale ≠ zero
// ─────────────────────────────────────────────────────────────────────────────────────────────
//   - `unavailable` — the source could not be read. The reason is recorded. NO payload is stored,
//     so nothing downstream can mistake absence for measurement.
//   - `stale` — the source answered, but its OWN reported `asOf` is older than `snapshotStaleAfter`.
//     The payload is kept, because it is real evidence about an earlier moment, and the state says
//     it is not current.
//   - `live` — the source answered and its clock agrees with ours. A payload of zeros in this state
//     is a MEASURED zero and is recorded as one.
//
// THERE IS NO FALLBACK PATH IN THIS FILE. No cached copy, no previous snapshot, no file on disk is
// ever substituted for a source that could not be read — `paper_client.go` has no such method and
// this assembler never asks for one. That is the rule "never substitute stale local files when live
// state is unavailable", enforced by the absence of the code that would do it.
//
// ─────────────────────────────────────────────────────────────────────────────────────────────
// ONE GENERATION, OR NOTHING
// ─────────────────────────────────────────────────────────────────────────────────────────────
// A reset changes the experiment generation. Every source now states its generation
// (paper/provenance.go and the three edits beside it), and assembly REFUSES when two readable
// sources disagree. Nothing is stored; the caller gets a 409 and may retry, which is the same
// posture `paper/dashboard.go` already takes inside its own composition.

const (
	// experimentSnapshotSchemaVersion is the shape of a stored snapshot. Bump on ANY change; the
	// review artifact pins it, so an old review against a new snapshot fails closed.
	experimentSnapshotSchemaVersion = "attestel.experiment.snapshot/1"

	// snapshotStaleAfter is how far a source's own `asOf` may trail ours before the source is
	// `stale`. Generous: every paper payload stamps `asOf` at the moment it is served, so anything
	// past this is a cache, a proxy or a clock problem — none of which should read as current.
	snapshotStaleAfter = 10 * time.Minute

	// maxExperimentSnapshotBytes caps one stored snapshot. Larger than an agency artifact because a
	// snapshot carries five upstream payloads verbatim, and bounded for the same reason.
	maxExperimentSnapshotBytes = 1 << 20

	// experimentSnapshotsPerUser is the ordinary retention cap. Snapshots are immutable evidence,
	// but an unbounded list on one owner is an unbounded document in one row.
	experimentSnapshotsPerUser = 50

	// experimentSnapshotsHardCap is the ceiling that applies even to PINNED snapshots — the ones a
	// queued or running review still needs. Retention keeps every pinned snapshot past the ordinary
	// cap (see applyRetention), and this is what stops that exception from being unbounded.
	experimentSnapshotsHardCap = 200
)

// The five composed sources. Named constants because the derived rules below address them by name
// and a typo in a map key would silently mean "that source was never read".
const (
	snapshotSourceReadiness   = "readiness"
	snapshotSourceDashboard   = "dashboard"
	snapshotSourceExperiments = "experiments"
	snapshotSourceStatus      = "status"
	snapshotSourceProvenance  = "provenance"
)

// Source states. See the header.
const (
	sourceLive        = "live"
	sourceStale       = "stale"
	sourceUnavailable = "unavailable"
)

// The three status vocabularies. Closed sets, spelled once.
const (
	paperUnjudged   = "unjudged"
	paperCollecting = "collecting"
	paperMeasurable = "measurable"

	candidateEdge         = "edge"
	candidateNoEdge       = "no_edge"
	candidateInconclusive = "inconclusive"
	candidateUnknown      = "unknown"

	// EVIDENCE VALIDITY IS A SEPARATE AXIS FROM THE VERDICT, and conflating the two is what made an
	// earlier version of this file report a real, recorded `NO EDGE` as though nobody had measured
	// anything.
	//
	// The evaluator's RESULT is what it returned. Its VALIDITY is whether that result can be applied
	// to the real market at all — which depends on the provenance of the data underneath it, not on
	// the verdict. A `NO EDGE` over a synthetic-trained model is a real result about invented
	// prices: the result is `NO EDGE`, its validity is `invalid`, and only the CONCLUSION drawn from
	// the pair is `unknown`. All three are served, so no reader has to infer one from another.
	evidenceValid   = "valid"   // the verdict rests on data that can characterise the real market
	evidenceInvalid = "invalid" // a verdict WAS read; its underlying data cannot characterise it
	evidenceUnknown = "unknown" // no verdict was read at all

	// evaluatorResultMixed is served when readable configs returned different verdicts. It is not a
	// verdict; it says "there is no single answer here, read the per-config list".
	evaluatorResultMixed = "MIXED"

	operationalVerified    = "verified"
	operationalDegraded    = "degraded"
	operationalUnavailable = "unavailable"
)

// The evaluator's own verdict vocabulary, as `services/prediction/app/evaluate.py` writes it.
// Compared verbatim rather than normalised: a verdict this journal does not recognise must produce
// `unknown` plus a blocker, never a guess.
const (
	evaluatorEdge         = "EDGE"
	evaluatorNoEdge       = "NO EDGE"
	evaluatorInconclusive = "INCONCLUSIVE"
	evaluatorSuspect      = "SUSPECT"
)

// Mandatory blocker codes. Every one of these is DERIVED from the snapshot by this file, and a
// review artifact that omits one is rejected (experiment_review.go). Hermes may add blockers of its
// own; it may not drop one of these.
const (
	blockerSourceUnavailable   = "source-unavailable"
	blockerSourceStale         = "source-stale"
	blockerMixedRevision       = "mixed-revision"
	blockerClockNotStarted     = "clock-not-started"
	blockerIntegrityDegraded   = "integrity-degraded"
	blockerStoreDesync         = "store-desync"
	blockerReadinessBlocked    = "readiness-blocked"
	blockerSyntheticModel      = "synthetic-trained-model"
	blockerSyntheticFrame      = "synthetic-or-unknown-frame"
	blockerNoVerdict           = "no-evaluator-verdict"
	blockerVerdictNotEdge      = "verdict-not-edge"
	blockerVerdictSuspect      = "verdict-suspect"
	blockerVerdictInconclusive = "verdict-inconclusive"
	blockerEvidenceNotCurrent  = "evidence-not-current"
	blockerStrategyMismatch    = "strategy-version-mismatch"
	blockerUnknownVerdict      = "unrecognised-verdict"
)

// experimentSnapshotIDRE is the exact shape `experimentSnapshotID` mints. An id that does not match
// is refused before any store is touched, so a caller cannot probe the store with arbitrary strings.
var experimentSnapshotIDRE = regexp.MustCompile(`^exs_[0-9a-f]{24}$`)

// errMixedGeneration is the refusal that keeps a snapshot from straddling a reset.
var errMixedGeneration = errors.New("the paper deployment reported more than one experiment generation")

// ───────────────────────────────────────────────────────────────────────────────── the snapshot

// SnapshotSource is one composed payload plus everything needed to judge it.
type SnapshotSource struct {
	Endpoint string `json:"endpoint"`
	State    string `json:"state"`
	// Reason is present only on `unavailable` and `stale`, and it is redacted before storage.
	Reason string `json:"reason,omitempty"`

	// ReportedAsOf is the source's OWN timestamp, verbatim. AgeSeconds is how far it trailed the
	// snapshot's `asOf`. Both are POINTERS/optional so "the source stated no time" survives as its
	// own answer rather than becoming a zero age.
	ReportedAsOf string `json:"reportedAsOf,omitempty"`
	AgeSeconds   *int64 `json:"ageSeconds"`

	// Generation and Revision as this source stated them. Nil means the source did not state one,
	// which is not the same as generation 0.
	Generation *int64 `json:"generation"`
	Revision   string `json:"revision,omitempty"`

	// Payload is what the service actually said, byte for byte. Absent on `unavailable`.
	Payload json.RawMessage `json:"payload,omitempty"`
}

// available reports whether this source contributed evidence at all.
func (s SnapshotSource) available() bool { return s.State == sourceLive || s.State == sourceStale }

// DerivedBlocker is one blocker this journal computed from the snapshot. It carries the paths that
// establish it, so the requirement "every blocker references a field in the captured snapshot" is
// satisfied by construction for the mandatory set.
type DerivedBlocker struct {
	Code          string   `json:"code"`
	Statement     string   `json:"statement"`
	EvidencePaths []string `json:"evidencePaths"`
}

// SnapshotDerived is the AUTHORITATIVE reading of the snapshot.
//
// IT IS COMPUTED HERE, ON THE SERVER, FROM THE STORED BYTES — and `experiment_review.go` rejects
// any artifact whose statuses differ from it. That is what makes "Hermes cannot manufacture an
// EDGE verdict" a structural property rather than a prompt instruction: the model's opinion about
// the verdict is never what gets stored, because the stored value has to equal this one.
//
// What Hermes contributes is everything this block cannot compute — why the blockers matter, what
// is genuinely unknown, which claims contradict each other, and what to check next.
// EvaluatorReading is ONE config's evaluator verdict and whether it can be applied.
//
// The verdict is passed through VERBATIM — `NO EDGE`, `SUSPECT`, whatever the evaluator actually
// wrote — rather than mapped onto the candidate vocabulary. A reader who needs to know that the
// evaluator ran and said something specific must be able to see the string it said.
type EvaluatorReading struct {
	Config string `json:"config"`
	// Verdict is empty when no verdict was recorded for this config at all. That is a different
	// fact from a verdict that WAS recorded and cannot be applied.
	Verdict string `json:"verdict"`
	// Validity is `valid`, `invalid` or `unknown`. It answers "can this result be applied to the
	// real market", never "what was the result".
	Validity       string   `json:"validity"`
	ValidityReason string   `json:"validityReason,omitempty"`
	EvidencePaths  []string `json:"evidencePaths"`
}

type SnapshotDerived struct {
	PaperStatus       string `json:"paperStatus"`
	CandidateStatus   string `json:"candidateStatus"`
	OperationalStatus string `json:"operationalStatus"`

	// THE EVALUATOR'S OWN RESULT, KEPT SEPARATE FROM THE CONCLUSION DRAWN FROM IT.
	//
	// `EvaluatorResult` is the raw verdict every readable config agreed on, `MIXED` when they
	// disagreed, and empty when none was recorded. `EvidenceValidity` says whether that result can
	// be applied. `CandidateStatus` above is the CONCLUSION, and it may legitimately be `unknown`
	// while `EvaluatorResult` is `NO EDGE` — which is the state this deployment is actually in.
	//
	// Serving only the conclusion is what buried a real, recorded `NO EDGE` inside a blocker and
	// left the UI saying "no usable evaluator verdict was read" about a verdict that had been read.
	EvaluatorResult      string             `json:"evaluatorResult"`
	EvidenceValidity     string             `json:"evidenceValidity"`
	EvaluatorReadings    []EvaluatorReading `json:"evaluatorReadings"`
	EvaluatorResultPaths []string           `json:"evaluatorResultPaths"`

	// StatusPaths are the snapshot paths each status was derived from, so the reviewer can cite the
	// same evidence the server used rather than inventing its own.
	PaperStatusPaths       []string `json:"paperStatusPaths"`
	CandidateStatusPaths   []string `json:"candidateStatusPaths"`
	OperationalStatusPaths []string `json:"operationalStatusPaths"`

	MandatoryBlockers []DerivedBlocker `json:"mandatoryBlockers"`
	Notes             []string         `json:"notes"`
}

// ExperimentSnapshot is the stored record. It is written once and never updated.
type ExperimentSnapshot struct {
	SchemaVersion string `json:"schemaVersion"`
	ID            string `json:"id"`
	UserID        string `json:"userId"`

	// AsOf is the ONE instant this snapshot answers at, assigned by this server at assembly.
	// Every source's age is measured against it.
	AsOf       string `json:"asOf"`
	CapturedAt int64  `json:"capturedAt"`

	// Generation is the single generation every readable source agreed on. Assembly refuses when
	// they do not, so this field can never be an average, a first-wins or a guess.
	Generation int64 `json:"generation"`
	// Revision is the deployed build. `revisionDisagreement` when readable sources disagreed —
	// recorded as a blocker rather than a refusal, because a rolling deploy is a real state.
	Revision string `json:"revision"`

	Sources map[string]SnapshotSource `json:"sources"`
	Derived SnapshotDerived           `json:"derived"`
}

const revisionDisagreement = "disagreement"

// ───────────────────────────────────────────────────────────────────────────────────── assembly

// assembleExperimentSnapshot reads all five sources and composes one snapshot.
//
// SEQUENTIAL, deliberately. `/paper/readiness` re-evaluates every gate against live upstreams, and
// firing five requests at a paper service that is already fanning out to prediction and analysis
// would put this lane in contention with the reads that gate real decisions. Five small requests
// against one local service do not need the parallelism.
func (s *Server) assembleExperimentSnapshot(ctx context.Context, uid string, now time.Time) (*ExperimentSnapshot, error) {
	asOf := now.UTC()
	snap := &ExperimentSnapshot{
		SchemaVersion: experimentSnapshotSchemaVersion,
		UserID:        uid,
		AsOf:          asOf.Format(time.RFC3339),
		CapturedAt:    asOf.Unix(),
		Sources:       map[string]SnapshotSource{},
	}

	for _, ep := range paperSourceEndpoints {
		snap.Sources[ep.name] = readSnapshotSource(ctx, s, ep.path, ep.timeout, asOf)
	}

	generation, err := agreedGeneration(snap.Sources)
	if err != nil {
		return nil, err
	}
	snap.Generation = generation
	snap.Revision = agreedRevision(snap.Sources)

	snap.Derived = deriveSnapshotVerdicts(snap)

	// The id is CONTENT-ADDRESSED over everything above, so an identical deployment state
	// re-snapshotted yields the same id and the store can treat a repeat as the same record. The
	// id field itself is excluded from its own input, for the obvious reason.
	id, err := experimentSnapshotID(snap)
	if err != nil {
		return nil, err
	}
	snap.ID = id

	encoded, err := json.Marshal(snap)
	if err != nil {
		return nil, fmt.Errorf("the snapshot could not be encoded")
	}
	if len(encoded) > maxExperimentSnapshotBytes {
		return nil, fmt.Errorf("the assembled snapshot is %d bytes; the limit is %d",
			len(encoded), maxExperimentSnapshotBytes)
	}
	return snap, nil
}

// readSnapshotSource fetches one source and classifies it. Every failure becomes a STATE, never a
// returned error: a snapshot with four live sources and one unavailable is a legitimate — and
// legible — record of a partly-degraded deployment.
func readSnapshotSource(ctx context.Context, s *Server, path string, timeout time.Duration, asOf time.Time) SnapshotSource {
	src := SnapshotSource{Endpoint: path}
	body, err := s.fetchPaperSource(ctx, path, timeout)
	if err != nil {
		src.State = sourceUnavailable
		src.Reason = redactAgencyText(err.Error())
		return src
	}

	// The three fields every composed payload now states. Decoded into pointers so an absent field
	// stays absent rather than becoming a zero generation or an empty revision that reads as one.
	var meta struct {
		AsOf       string `json:"asOf"`
		CheckedAt  string `json:"checkedAt"` // readiness names its timestamp differently
		Generation *int64 `json:"generation"`
		Revision   string `json:"revision"`
	}
	// A payload that decodes into this envelope but has none of the fields is still a payload; the
	// derived rules treat a nil generation as "did not state one".
	_ = json.Unmarshal(body, &meta)

	src.Payload = body
	src.Generation = meta.Generation
	src.Revision = meta.Revision

	reported := strings.TrimSpace(meta.AsOf)
	if reported == "" {
		reported = strings.TrimSpace(meta.CheckedAt)
	}
	src.ReportedAsOf = reported

	// A source that stated no time cannot be shown to be stale, and must not be assumed fresh
	// either. It is `live` with a nil age and a note — the derived rules then have the nil to work
	// with rather than a fabricated zero.
	stamped, ok := parseSnapshotTime(reported)
	if !ok {
		src.State = sourceLive
		return src
	}
	age := int64(asOf.Sub(stamped) / time.Second)
	src.AgeSeconds = &age
	if stamped.Before(asOf.Add(-snapshotStaleAfter)) {
		src.State = sourceStale
		src.Reason = fmt.Sprintf("the source reported %s, %ds before this snapshot's cutoff",
			reported, age)
		return src
	}
	src.State = sourceLive
	return src
}

func parseSnapshotTime(v string) (time.Time, bool) {
	if v = strings.TrimSpace(v); v == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, false
	}
	return t.UTC(), true
}

// agreedGeneration returns the one generation every AVAILABLE source stated, or `errMixedGeneration`.
//
// Sources that are `unavailable` contribute nothing — they stated nothing. A source that answered
// without a generation also contributes nothing; that is a gap in the deployment, recorded as a
// note rather than treated as agreement with whatever the others said.
//
// WITH NO SOURCE STATING A GENERATION AT ALL the snapshot's generation is 0 and a note says so. It
// is not an error: a paper service that is entirely unreachable is exactly the state this lane must
// be able to record.
func agreedGeneration(sources map[string]SnapshotSource) (int64, error) {
	var seen *int64
	names := sortedSourceNames(sources)
	for _, name := range names {
		src := sources[name]
		if !src.available() || src.Generation == nil {
			continue
		}
		if seen == nil {
			g := *src.Generation
			seen = &g
			continue
		}
		if *seen != *src.Generation {
			return 0, fmt.Errorf("%w: %d and %d were both reported", errMixedGeneration,
				*seen, *src.Generation)
		}
	}
	if seen == nil {
		return 0, nil
	}
	return *seen, nil
}

// agreedRevision returns the build every available source named, or `revisionDisagreement`.
//
// Unlike a generation mismatch this is NOT a refusal. A rolling deploy genuinely serves two
// revisions for a few seconds, and refusing would make the lane unusable exactly when an operator
// most wants to look at it. It is recorded as a mandatory blocker instead.
func agreedRevision(sources map[string]SnapshotSource) string {
	seen := ""
	for _, name := range sortedSourceNames(sources) {
		src := sources[name]
		if !src.available() || strings.TrimSpace(src.Revision) == "" {
			continue
		}
		if seen == "" {
			seen = src.Revision
			continue
		}
		if seen != src.Revision {
			return revisionDisagreement
		}
	}
	if seen == "" {
		return revisionUnknownValue
	}
	return seen
}

// revisionUnknownValue mirrors paper/config.go's `revisionUnknown`. Restated rather than imported:
// `journal` and `paper` are separate modules, exactly as the session-token verifier is restated in
// six services. Keep the two in step.
const revisionUnknownValue = "unavailable"

// experimentSnapshotID is a content address over the snapshot minus its own id.
func experimentSnapshotID(snap *ExperimentSnapshot) (string, error) {
	clone := *snap
	clone.ID = ""
	// `encoding/json` sorts map keys, so the source map serialises deterministically.
	encoded, err := json.Marshal(clone)
	if err != nil {
		return "", fmt.Errorf("the snapshot could not be hashed")
	}
	sum := sha256.Sum256(encoded)
	return "exs_" + hex.EncodeToString(sum[:])[:24], nil
}

func sortedSourceNames(sources map[string]SnapshotSource) []string {
	names := make([]string, 0, len(sources))
	for name := range sources {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ────────────────────────────────────────────────────────────────────── the authoritative reading

// provenanceRow is the slice of `/paper/provenance` the derived rules judge. Decoded into pointers
// throughout, so "the field was absent" and "the field was false" stay different facts all the way
// through — which is the entire reason paper/provenance.go serves them as pointers.
type provenanceRow struct {
	Config             string `json:"config"`
	Availability       string `json:"availability"`
	TrainedOnSynthetic *bool  `json:"trainedOnSynthetic"`
	StrategyVersion    string `json:"strategyVersion"`
	CurrentData        *struct {
		Source    string `json:"source"`
		Synthetic bool   `json:"synthetic"`
	} `json:"currentData"`
	Evaluation *struct {
		Verdict         string `json:"verdict"`
		StrategyVersion string `json:"strategyVersion"`
		Current         *bool  `json:"current"`
		EvidenceCurrent *bool  `json:"evidenceCurrent"`
	} `json:"evaluation"`
}

// deriveSnapshotVerdicts computes the three statuses and the mandatory blocker set.
//
// EVERY RULE HERE FAILS TOWARDS `unknown` AND TOWARDS A BLOCKER. A field that is absent, a source
// that could not be read, a verdict spelled in a way this code does not recognise — none of them
// produces a favourable reading, and none of them is silently skipped.
func deriveSnapshotVerdicts(snap *ExperimentSnapshot) SnapshotDerived {
	d := SnapshotDerived{
		MandatoryBlockers:    []DerivedBlocker{},
		Notes:                []string{},
		EvaluatorReadings:    []EvaluatorReading{},
		EvaluatorResultPaths: []string{},
	}
	add := func(code, statement string, paths ...string) {
		d.MandatoryBlockers = append(d.MandatoryBlockers, DerivedBlocker{
			Code: code, Statement: statement, EvidencePaths: paths,
		})
	}

	// --- source health, which decides operationalStatus's floor -------------------------------
	anyUnavailable, anyStale := false, false
	for _, name := range sortedSourceNames(snap.Sources) {
		src := snap.Sources[name]
		path := "sources." + name + ".state"
		switch src.State {
		case sourceUnavailable:
			anyUnavailable = true
			add(blockerSourceUnavailable,
				fmt.Sprintf("the %s source could not be read (%s), so nothing it would have "+
					"reported is known — this is not the same as reporting nothing", name, src.Reason),
				path, "sources."+name+".reason")
		case sourceStale:
			anyStale = true
			add(blockerSourceStale,
				fmt.Sprintf("the %s source answered with a timestamp older than the snapshot's "+
					"cutoff (%s)", name, src.ReportedAsOf),
				path, "sources."+name+".reportedAsOf")
		}
		if src.available() && src.Generation == nil {
			d.Notes = append(d.Notes,
				fmt.Sprintf("the %s source stated no experiment generation", name))
		}
	}
	if snap.Revision == revisionDisagreement {
		add(blockerMixedRevision, "two sources named different deployment revisions; this snapshot "+
			"may straddle a rolling deploy", "revision")
	}

	// --- paperStatus, from the dashboard ------------------------------------------------------
	d.PaperStatus, d.PaperStatusPaths = derivePaperStatus(snap, add, &d)

	// --- candidateStatus, from the provenance rows ---------------------------------------------
	d.CandidateStatus, d.CandidateStatusPaths = deriveCandidateStatus(snap, add, &d)

	// --- operationalStatus ---------------------------------------------------------------------
	d.OperationalStatus, d.OperationalStatusPaths = deriveOperationalStatus(snap, add,
		anyUnavailable, anyStale)

	return d
}

// derivePaperStatus answers "has the paper experiment been measured", and nothing else.
//
// It is deliberately NOT a statement about the candidate strategy. An experiment whose clock has
// never started is `unjudged` no matter how emphatic the evaluator's verdict is, and an experiment
// with two hundred snapshot days is `measurable` no matter how bad the result. Conflating the two
// is the confusion this whole lane exists to remove.
func derivePaperStatus(snap *ExperimentSnapshot, add func(string, string, ...string), d *SnapshotDerived) (string, []string) {
	src := snap.Sources[snapshotSourceDashboard]
	if !src.available() {
		// UNAVAILABLE IS NOT "not started". The status has no `unknown` member, so `unjudged` is
		// the only honest value — and the note plus the `source-unavailable` blocker (already
		// added) are what keep it from reading as a measurement.
		d.Notes = append(d.Notes, "the paper result could not be read: the dashboard source was "+
			"unavailable, so `unjudged` here means UNREAD, not measured-and-not-started")
		return paperUnjudged, []string{"sources.dashboard.state"}
	}

	var dash struct {
		Experiment struct {
			OfficialStartedAt string `json:"officialStartedAt"`
		} `json:"experiment"`
		Dimensions struct {
			Clock  string `json:"clock"`
			Sample string `json:"sample"`
		} `json:"dimensions"`
	}
	if err := json.Unmarshal(src.Payload, &dash); err != nil {
		d.Notes = append(d.Notes, "the dashboard payload could not be decoded for the paper status")
		return paperUnjudged, []string{"sources.dashboard.state"}
	}

	startedPath := "sources.dashboard.payload.experiment.officialStartedAt"
	samplePath := "sources.dashboard.payload.dimensions.sample"
	if strings.TrimSpace(dash.Experiment.OfficialStartedAt) == "" {
		add(blockerClockNotStarted, "the official experiment clock has never started, so no paper "+
			"result exists to judge", startedPath, "sources.dashboard.payload.dimensions.clock")
		return paperUnjudged, []string{startedPath}
	}
	if dash.Dimensions.Sample == "measurable" {
		return paperMeasurable, []string{startedPath, samplePath}
	}
	return paperCollecting, []string{startedPath, samplePath}
}

// deriveCandidateStatus answers "what did the offline evaluator establish about the candidate".
//
// THE PRECEDENCE IS LEAST-ESTABLISHED-WINS: unknown, then inconclusive, then no_edge, then edge.
// A missing verdict anywhere makes the aggregate `unknown` — because with one config uncharacterised
// there is no honest sentence about "the candidate". The per-config negative evidence is not lost:
// every NO EDGE still raises its own blocker with its own path.
//
// `edge` is reachable ONLY when every readable config carries an EDGE verdict that is both `current`
// and `evidenceCurrent` — the same three conditions paper/gates.go's fourth gate spends. Anything
// weaker falls to `inconclusive`, so a stale or unsupported EDGE can never be read as one.
func deriveCandidateStatus(snap *ExperimentSnapshot, add func(string, string, ...string), d *SnapshotDerived) (string, []string) {
	src := snap.Sources[snapshotSourceProvenance]
	if !src.available() {
		d.EvidenceValidity = evidenceUnknown
		d.EvaluatorResultPaths = []string{"sources.provenance.state"}
		return candidateUnknown, []string{"sources.provenance.state"}
	}
	var body struct {
		Configs []provenanceRow `json:"configs"`
	}
	if err := json.Unmarshal(src.Payload, &body); err != nil {
		d.EvidenceValidity = evidenceUnknown
		d.EvaluatorResultPaths = []string{"sources.provenance.state"}
		return candidateUnknown, []string{"sources.provenance.state"}
	}
	if len(body.Configs) == 0 {
		d.EvidenceValidity = evidenceUnknown
		d.EvaluatorResultPaths = []string{"sources.provenance.payload.configs"}
		return candidateUnknown, []string{"sources.provenance.payload.configs"}
	}

	paths := []string{}
	worst := candidateEdge // improved downwards as rows are examined
	demote := func(to string) {
		if candidateRank(to) < candidateRank(worst) {
			worst = to
		}
	}

	for i, row := range body.Configs {
		// Tracked per row so the reading below can distinguish "no verdict was recorded" from "a
		// verdict was recorded and the DATA UNDER IT makes it uninterpretable". Those two produce
		// the same `unknown` conclusion, and a reader who cannot tell them apart will assume the
		// first — which is why both are served as their own fields rather than inferred.
		verdictRead, demotedByProvenance := "", false
		provenanceReason := ""
		base := "sources.provenance.payload.configs[" + strconv.Itoa(i) + "]"
		if row.Availability != "live" {
			// The row exists but says it could not be read. Unknown, and a blocker was already
			// raised for the source only if the WHOLE source failed — this is the per-config case.
			add(blockerNoVerdict, fmt.Sprintf("no model record could be read for %s, so nothing is "+
				"established about it", row.Config), base+".availability")
			demote(candidateUnknown)
			paths = append(paths, base+".availability")
			d.EvaluatorReadings = append(d.EvaluatorReadings, EvaluatorReading{
				Config: row.Config, Verdict: "", Validity: evidenceUnknown,
				ValidityReason: "the model record for this config could not be read",
				EvidencePaths:  []string{base + ".availability"},
			})
			continue
		}

		// Synthetic training is a blocker regardless of the verdict, and it stays one: a backtest
		// of a model fitted on invented prices is not evidence (paper/gates.go, gate 1).
		if row.TrainedOnSynthetic == nil {
			add(blockerSyntheticModel, fmt.Sprintf("%s does not state whether its model was trained "+
				"on synthetic data; unknown provenance is not clean provenance", row.Config),
				base+".trainedOnSynthetic")
			demote(candidateUnknown)
			demotedByProvenance = true
			provenanceReason = "the model record does not state whether it was trained on synthetic data"
		} else if *row.TrainedOnSynthetic {
			add(blockerSyntheticModel, fmt.Sprintf("%s is served by a model trained on SYNTHETIC "+
				"data; its backtest is not evidence about the real market", row.Config),
				base+".trainedOnSynthetic")
			demote(candidateUnknown)
			demotedByProvenance = true
			provenanceReason = "the model was trained on synthetic data"
		}
		if row.CurrentData == nil {
			add(blockerSyntheticFrame, fmt.Sprintf("%s served no provenance for the feature frame it "+
				"scored, so the frame is of unknown origin", row.Config), base+".currentData")
			demote(candidateUnknown)
			demotedByProvenance = true
			if provenanceReason == "" {
				provenanceReason = "the scored feature frame's origin is unstated"
			}
		} else if row.CurrentData.Synthetic {
			add(blockerSyntheticFrame, fmt.Sprintf("%s scored a SYNTHETIC feature frame (source %q)",
				row.Config, row.CurrentData.Source), base+".currentData.synthetic")
			demote(candidateUnknown)
			demotedByProvenance = true
			if provenanceReason == "" {
				provenanceReason = "the scored feature frame is synthetic"
			}
		}

		ev := row.Evaluation
		if ev == nil {
			add(blockerNoVerdict, fmt.Sprintf("no persisted evaluator verdict covers %s; nothing is "+
				"established about the candidate for it", row.Config), base+".evaluation")
			demote(candidateUnknown)
			paths = append(paths, base+".evaluation")
			d.EvaluatorReadings = append(d.EvaluatorReadings, EvaluatorReading{
				Config: row.Config, Verdict: "", Validity: evidenceUnknown,
				ValidityReason: "no evaluator verdict is recorded for this config",
				EvidencePaths:  []string{base + ".evaluation"},
			})
			continue
		}
		verdictPath := base + ".evaluation.verdict"
		paths = append(paths, verdictPath)
		verdictRead = strings.TrimSpace(ev.Verdict)

		switch verdictRead {
		case evaluatorEdge:
			// An EDGE is only an EDGE if it can actually be spent. Both conditions below are the
			// evaluator gate's, restated.
			if ev.EvidenceCurrent == nil || !*ev.EvidenceCurrent {
				add(blockerEvidenceNotCurrent, fmt.Sprintf("%s carries an %s verdict whose sample "+
					"evidence does not meet the evaluator's hard floors", row.Config, evaluatorEdge),
					base+".evaluation.evidenceCurrent")
				demote(candidateInconclusive)
			}
			if ev.Current == nil || !*ev.Current {
				add(blockerStrategyMismatch, fmt.Sprintf("%s carries an %s verdict made under "+
					"strategy version %q, which is not the one being served", row.Config,
					evaluatorEdge, ev.StrategyVersion),
					base+".evaluation.current", base+".evaluation.strategyVersion")
				demote(candidateInconclusive)
			}
		case evaluatorNoEdge:
			// GENUINELY NEGATIVE EVIDENCE. The evaluator ran, had enough sample, and found no edge.
			// That is a RESULT about the candidate — and it says nothing at all about whether the
			// paper experiment has been measured, which is why paperStatus is derived separately.
			add(blockerVerdictNotEdge, fmt.Sprintf("the evaluator's pooled verdict for %s is %q: "+
				"this is negative evidence about the CANDIDATE STRATEGY, not an unmeasured paper "+
				"experiment", row.Config, evaluatorNoEdge), verdictPath)
			demote(candidateNoEdge)
		case evaluatorInconclusive:
			add(blockerVerdictInconclusive, fmt.Sprintf("the evaluator's verdict for %s is %q: the "+
				"sample was insufficient to establish or rule out an edge", row.Config,
				evaluatorInconclusive), verdictPath)
			demote(candidateInconclusive)
		case evaluatorSuspect:
			// SUSPECT is the evaluator's WORST verdict, and it is not the same thing as NO EDGE.
			// It means the measurement itself is untrustworthy — a pooled Sharpe high enough to
			// indicate leakage. Reporting it as `no_edge` would claim an absence of edge was
			// established, when what was established is that the number cannot be believed.
			add(blockerVerdictSuspect, fmt.Sprintf("the evaluator's verdict for %s is %q — probable "+
				"leakage. The measurement is untrustworthy; this is NOT a finding that there is no "+
				"edge", row.Config, evaluatorSuspect), verdictPath)
			demote(candidateInconclusive)
		case "":
			add(blockerNoVerdict, fmt.Sprintf("%s carries an evaluation block with no verdict",
				row.Config), verdictPath)
			demote(candidateUnknown)
		default:
			add(blockerUnknownVerdict, fmt.Sprintf("%s carries the verdict %q, which this server "+
				"does not recognise; it is treated as establishing nothing", row.Config, ev.Verdict),
				verdictPath)
			demote(candidateUnknown)
		}

		// THE READING: what the evaluator said, and separately whether it can be applied.
		//
		// A VERDICT THAT WAS READ BUT CANNOT BE APPLIED IS NOT A MISSING VERDICT. Both end in an
		// `unknown` CONCLUSION, and if the conclusion were the only thing served, a reader would
		// conclude the evaluator never ran — when in fact it ran and returned a real answer about a
		// model fitted on invented prices. So the result and its validity are stored as their own
		// fields, and the UI renders both.
		reading := EvaluatorReading{
			Config: row.Config, Verdict: verdictRead, Validity: evidenceValid,
			EvidencePaths: []string{verdictPath},
		}
		if verdictRead == "" {
			reading.Validity = evidenceUnknown
			reading.ValidityReason = "the evaluation block carries no verdict"
		} else if demotedByProvenance {
			reading.Validity = evidenceInvalid
			reading.ValidityReason = provenanceReason + ", so this verdict characterises invented " +
				"or unverified data and cannot be applied to the real market"
			reading.EvidencePaths = append(reading.EvidencePaths, base+".trainedOnSynthetic")
			d.Notes = append(d.Notes, fmt.Sprintf("the evaluator recorded %q for %s, but %s. The "+
				"RESULT stands and is served as `evaluatorResult`; its VALIDITY is `invalid`; and "+
				"only the CONCLUSION is `unknown` — this is NOT `no_edge` and NOT a missing verdict",
				verdictRead, row.Config, provenanceReason))
		}
		d.EvaluatorReadings = append(d.EvaluatorReadings, reading)
	}

	// Aggregate the readings into the two top-level fields.
	d.EvaluatorResult, d.EvidenceValidity, d.EvaluatorResultPaths = aggregateEvaluatorReadings(
		d.EvaluatorReadings)
	if len(paths) == 0 {
		paths = []string{"sources.provenance.payload.configs"}
	}
	return worst, paths
}

// candidateRank orders the candidate vocabulary by HOW MUCH IS ESTABLISHED, most first. `demote`
// keeps the least-established value seen.
func candidateRank(v string) int {
	switch v {
	case candidateEdge:
		return 3
	case candidateNoEdge:
		return 2
	case candidateInconclusive:
		return 1
	default: // candidateUnknown
		return 0
	}
}

// deriveOperationalStatus answers "can this deployment be trusted to keep honest score right now".
// It is about plumbing — reachability, integrity, store agreement — and never about the result.
func deriveOperationalStatus(snap *ExperimentSnapshot, add func(string, string, ...string), anyUnavailable, anyStale bool) (string, []string) {
	paths := []string{}
	degraded := anyStale

	if dash := snap.Sources[snapshotSourceDashboard]; dash.available() {
		var body struct {
			Dimensions struct {
				Integrity        string   `json:"integrity"`
				IntegrityReasons []string `json:"integrityReasons"`
			} `json:"dimensions"`
		}
		if err := json.Unmarshal(dash.Payload, &body); err == nil {
			paths = append(paths, "sources.dashboard.payload.dimensions.integrity")
			if body.Dimensions.Integrity != "healthy" || len(body.Dimensions.IntegrityReasons) > 0 {
				degraded = true
				add(blockerIntegrityDegraded, fmt.Sprintf("the experiment's integrity dimension is "+
					"%q with %d stated reason(s)", body.Dimensions.Integrity,
					len(body.Dimensions.IntegrityReasons)),
					"sources.dashboard.payload.dimensions.integrity",
					"sources.dashboard.payload.dimensions.integrityReasons")
			}
		}
	}

	if st := snap.Sources[snapshotSourceStatus]; st.available() {
		var body struct {
			Reconciliation struct {
				DesyncedConfigs []string `json:"desyncedConfigs"`
				PendingBookings int      `json:"pendingBookings"`
			} `json:"reconciliation"`
		}
		if err := json.Unmarshal(st.Payload, &body); err == nil {
			paths = append(paths, "sources.status.payload.reconciliation.desyncedConfigs")
			if len(body.Reconciliation.DesyncedConfigs) > 0 || body.Reconciliation.PendingBookings > 0 {
				degraded = true
				add(blockerStoreDesync, fmt.Sprintf("%d config(s) are desynchronized and %d ledger "+
					"booking(s) are pending; the three stores do not agree",
					len(body.Reconciliation.DesyncedConfigs), body.Reconciliation.PendingBookings),
					"sources.status.payload.reconciliation.desyncedConfigs",
					"sources.status.payload.reconciliation.pendingBookings")
			}
		}
	}

	if rd := snap.Sources[snapshotSourceReadiness]; rd.available() {
		var body struct {
			Ready    bool     `json:"ready"`
			Blockers []string `json:"blockers"`
		}
		if err := json.Unmarshal(rd.Payload, &body); err == nil {
			paths = append(paths, "sources.readiness.payload.ready")
			if !body.Ready || len(body.Blockers) > 0 {
				degraded = true
				add(blockerReadinessBlocked, fmt.Sprintf("the launch checklist is not satisfied: %d "+
					"blocker(s) are recorded", len(body.Blockers)),
					"sources.readiness.payload.ready", "sources.readiness.payload.blockers")
			}
		}
	}

	// UNAVAILABLE OUTRANKS DEGRADED. A source nobody could read means the operational picture is
	// incomplete, and an incomplete picture must not be reported as a merely-imperfect one.
	if anyUnavailable {
		for _, name := range sortedSourceNames(snap.Sources) {
			if snap.Sources[name].State == sourceUnavailable {
				paths = append(paths, "sources."+name+".state")
			}
		}
		return operationalUnavailable, paths
	}
	if len(paths) == 0 {
		paths = []string{"sources.dashboard.state"}
	}
	if degraded {
		return operationalDegraded, paths
	}
	return operationalVerified, paths
}

// ─────────────────────────────────────────────────────────────────────────────────── path index

// maxSnapshotPaths bounds the index. The snapshot itself is capped at 1 MiB, so this is a backstop
// against a pathological document rather than the primary limit.
const maxSnapshotPaths = 20000

// snapshotPathIndex is the set of every addressable field path in a stored snapshot.
//
// THIS IS WHAT MAKES "every blocker references a field in the snapshot" CHECKABLE. Without it the
// requirement is a prompt instruction, and a model that invents `sources.dashboard.payload.edge`
// would satisfy it as convincingly as one that cites a real field. With it, an invented path is a
// rejected artifact.
//
// Paths are dotted, with `[n]` for array elements — the same notation the derived blockers above
// emit, so a reviewer copying a mandatory blocker's paths is always citing valid ones.
func snapshotPathIndex(snap *ExperimentSnapshot) (map[string]bool, error) {
	encoded, err := json.Marshal(snap)
	if err != nil {
		return nil, fmt.Errorf("the snapshot could not be indexed")
	}
	var tree any
	if err := json.Unmarshal(encoded, &tree); err != nil {
		return nil, fmt.Errorf("the snapshot could not be indexed")
	}
	index := map[string]bool{}
	walkSnapshotPaths("", tree, index)
	return index, nil
}

func walkSnapshotPaths(prefix string, node any, index map[string]bool) {
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
			walkSnapshotPaths(child, typed[k], index)
		}
	case []any:
		for i, item := range typed {
			walkSnapshotPaths(prefix+"["+strconv.Itoa(i)+"]", item, index)
		}
	}
}

// ───────────────────────────────────────────────────────────────────────────────────── the view

// experimentSnapshotView is what an owner is served. It is the whole snapshot — there is nothing
// secret in it — but it is a distinct type so a future private field cannot leak by default.
type experimentSnapshotView struct {
	ExperimentSnapshot
	// Note is fixed prose that keeps the three statuses from being read as one verdict.
	Note string `json:"note"`
}

const experimentSnapshotNote = "Paper status, candidate status and operational status are three " +
	"SEPARATE readings and must not be collapsed. `unjudged` means the paper experiment has not " +
	"produced a measured result; it is not a finding about the strategy. `no_edge` means the " +
	"offline evaluator found no edge in the candidate; it is not a statement about whether the " +
	"paper experiment has run. A source that could not be read is `unavailable` — never zero."

func experimentSnapshotViewOf(snap ExperimentSnapshot) experimentSnapshotView {
	return experimentSnapshotView{ExperimentSnapshot: snap, Note: experimentSnapshotNote}
}

// aggregateEvaluatorReadings collapses the per-config readings into the two top-level fields.
//
//	result   — the verdict every readable config agreed on, `MIXED` when they disagreed, and "" when
//	           none was recorded at all.
//	validity — `unknown` when no verdict was read anywhere; `invalid` when any verdict that WAS read
//	           rests on data that cannot characterise the real market; `valid` otherwise.
//
// The two are independent on purpose: `("NO EDGE", invalid)` is the state this deployment is in,
// and neither half of that pair can be recovered from the other.
func aggregateEvaluatorReadings(readings []EvaluatorReading) (string, string, []string) {
	result := ""
	mixed := false
	anyRead, anyInvalid := false, false
	paths := []string{}
	for _, r := range readings {
		paths = append(paths, r.EvidencePaths...)
		if r.Verdict == "" {
			continue
		}
		anyRead = true
		if r.Validity == evidenceInvalid {
			anyInvalid = true
		}
		switch {
		case result == "":
			result = r.Verdict
		case result != r.Verdict:
			mixed = true
		}
	}
	if mixed {
		result = evaluatorResultMixed
	}
	validity := evidenceUnknown
	if anyRead {
		validity = evidenceValid
		if anyInvalid {
			validity = evidenceInvalid
		}
	}
	if len(paths) == 0 {
		paths = []string{"sources.provenance.payload.configs"}
	}
	return result, validity, dedupeStrings(paths)
}
