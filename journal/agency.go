package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// agency.go — the DOMAIN of the Hermes research agency lane: the run record, its state machine, its
// lease, and the strict validation every worker-supplied artifact must survive.
//
// WHY THIS LIVES IN THE JOURNAL. The journal is already the owner of durable, per-user research
// records (decisions, theses, portfolios, outcomes) and it already verifies the session cookie
// itself rather than trusting a header (auth.go). An agency run is one more per-user durable
// record, so it reuses the store, the scoping and the auth this service already has. The gateway
// cannot hold it — it is stdlib-only with no database — and adding a hosted process for it would
// buy nothing the journal does not already provide.
//
// THE ONE RULE THIS FILE EXISTS TO ENFORCE. A research artifact is RESEARCH. It carries findings,
// citations, contradictions, a thesis, an anti-thesis, risks and a chair's conclusion — and it has
// no field capable of expressing a direction, a price target, an expected return, a position size
// or a probability. That is not a prompt rule that a compromised worker could talk its way past; it
// is the shape of the struct, enforced by a strict decoder that refuses unknown fields and by a
// scan over every string it accepts. A local machine that is fully compromised can write a WRONG
// NARRATIVE into this record. It cannot write a signal, because there is nowhere to put one.
//
// See `actionability.go` for the deterministic LONG/FLAT/SHORT/NO_SIGNAL foundation this lane
// deliberately does NOT feed.

// ─────────────────────────────────────────────────────────────────────── versions and vocabulary

const (
	// agencyJobSchemaVersion is the shape the hosted side hands a worker. Bump on ANY change to the
	// job envelope; the bridge pins the exact string and refuses anything else, so an old worker
	// against a new server fails closed rather than guessing.
	agencyJobSchemaVersion = "attestel.agency.job/1"

	// agencyArtifactSchemaVersion is the shape a worker hands back. Same bump rule, same fail-closed
	// posture in the other direction.
	agencyArtifactSchemaVersion = "attestel.agency.artifact/1"

	// agencyWorkflowCompanyResearch is the ONLY workflow this lane knows. It is a closed vocabulary
	// of one, deliberately: the hosted side names a workflow, never a profile, a toolset, a model, a
	// path or a command. What `company_research_v1` means in terms of Hermes profiles is decided
	// entirely on the owner's machine (see bridge/hermes.go) and is not expressible over the wire.
	agencyWorkflowCompanyResearch = "company_research_v1"
)

// Run states. Seven, and the set is closed.
//
//	queued    -> nobody has it yet
//	claimed   -> a worker holds a lease but has not reported progress
//	running   -> the worker has heartbeated at least once
//	completed -> a validated artifact is stored (terminal)
//	failed    -> the worker reported a failure, or validation rejected its output (terminal)
//	cancelled -> the owner stopped it (terminal)
//	expired   -> the lease lapsed and no attempts remain (terminal)
//
// A lapsed lease with attempts remaining returns to `queued` rather than to `expired`: an outage is
// not a verdict on the job. This is the shape alerts/thesis_monitor.go already uses.
const (
	agencyQueued    = "queued"
	agencyClaimed   = "claimed"
	agencyRunning   = "running"
	agencyCompleted = "completed"
	agencyFailed    = "failed"
	agencyCancelled = "cancelled"
	agencyExpired   = "expired"
)

// Provenance labels. Every finding must say WHICH of these it is, so a reader can tell a fact that
// came from a document from a number someone computed from an opinion a model formed.
const (
	provenanceSourced    = "sourced"    // stated by a cited source
	provenanceCalculated = "calculated" // arithmetic over sourced values; `basis` must show the work
	provenanceInferred   = "inferred"   // the model's own reading; not a fact
	provenanceUnknown    = "unknown"    // explicitly not established — the honest answer
)

// Research-priority labels. NOT actions: "investigate" is a research instruction, not a Buy.
const (
	priorityInvestigate = "investigate"
	priorityWatch       = "watch"
	priorityReject      = "reject"
	priorityUnknown     = "unknown"
)

// agencyVetoScope is fixed. A veto may caution against NEW exposure and may do nothing else — it
// can never create exposure, strengthen it, reverse it, or stand in the way of closing or reducing
// a position. paper/gates.go makes the same asymmetry explicit for the quant gates ("a gate must
// not be able to trap a position that is already open"); this is that rule for research.
const agencyVetoScope = "new_exposure_only"

// ───────────────────────────────────────────────────────────────────────────────────────── limits
//
// Every one of these is an explicit ceiling rather than an implicit one. A limit nobody wrote down
// is a limit nobody can test.

const (
	agencyMaxTickersPerRun  = 1                // v1 researches one company per run
	agencyMaxQuestionLen    = 500              // characters, after trimming
	agencyMaxSources        = 40               // citations per artifact
	agencyMaxFindingsPerBox = 40               // findings per stage / support list
	agencyMaxStatementLen   = 2000             // characters, any single free-text statement
	agencyMaxArtifactBytes  = 256 << 10        // 256 KiB of encoded artifact
	agencyMaxAttempts       = 3                // claims before a run is terminally expired
	agencyLeaseDuration     = 15 * time.Minute // one lease
	agencyMaxLeaseDuration  = 30 * time.Minute // hard ceiling a worker cannot argue past
	agencyMaxRunAge         = 6 * time.Hour    // a queued run older than this is expired unclaimed
	agencyRunsPerUser       = 200              // retained runs per owner, newest kept
	agencyPollAfterMs       = 3000             // advisory client poll cadence
	agencyMaxErrorLen       = 500              // stored failure text, after redaction
)

// Citation-coverage floors for a research-positive outcome.
//
// WHY A FLOOR EXISTS AT ALL. `investigate` and `watch` are the two priorities that say "there is
// something here". An artifact that says `investigate` while citing nothing and grounding nothing
// is an artifact whose entire content is the model's own reading — which the provenance labels
// already call `inferred`, i.e. explicitly not a fact. Letting that reach the owner as a
// research-positive outcome would make the labels decorative in exactly the place they matter most.
//
// The floors are deliberately MODEST. They are not a judgement of research quality — no threshold
// could be — they are the boundary between "this rests on something" and "this rests on nothing".
// An honest run that found nothing is not blocked; it reports `unknown`, which needs no citations
// and is a first-class answer everywhere else in this lane.
const (
	agencyMinGroundedFindings = 2    // sourced-or-calculated findings across the whole artifact
	agencyMinGroundedFraction = 0.25 // of all findings, how many must rest on a source
)

// agencyProfileChain is the FIXED, ORDERED chain `company_research_v1` means. It is recorded here
// so the hosted side can display and verify what ran — it is NOT sent to the worker as an
// instruction, and the worker does not take its chain from this list. The bridge owns the real
// mapping (bridge/hermes.go) and this is the copy the server checks the returned artifact against.
// The two must agree; agency_test.go and the bridge's own test both pin the same four strings.
var agencyProfileChain = []string{
	"stock-scout",
	"stock-fundamentals",
	"stock-risk",
	"stock-chair",
}

var (
	agencyTickerRE = regexp.MustCompile(`^[A-Z0-9][A-Z0-9.\-]{0,15}$`)
	agencySourceRE = regexp.MustCompile(`^s[1-9][0-9]{0,2}$`)
)

// ─────────────────────────────────────────────────────────────────────────────────────── the run

// AgencyRun is one research job, from request to artifact. It is the stored record; the owner sees
// a projection of it (agencyRunView) that never includes the lease token.
type AgencyRun struct {
	ID              string `json:"id"`
	UserID          string `json:"userId"`
	SchemaVersion   string `json:"schemaVersion"`
	WorkflowVersion string `json:"workflowVersion"`

	// IdempotencyKey is deterministic over (workflow, ticker, question, uid). A second create while
	// a run with the same key is still live ATTACHES to it and starts nothing — the same rule
	// gateway/analystjobs.go applies to analyst runs, for the same reason.
	IdempotencyKey string `json:"idempotencyKey"`

	Ticker   string `json:"ticker"`
	Question string `json:"question"`
	// AsOf is the point-in-time cutoff the research is answered at. Always server-assigned at
	// creation and never worker-supplied: a cutoff a worker could choose is a cutoff a worker could
	// backdate, and a backdated cutoff is how hindsight enters a research record.
	AsOf string `json:"asOf"`

	Status   string `json:"status"`
	Attempts int    `json:"attempts"`

	// Lease. LeaseToken is opaque and NEVER leaves the server except to the worker that claimed the
	// run; agencyRunView drops it.
	LeaseToken     string `json:"leaseToken,omitempty"`
	LeaseExpiresAt int64  `json:"leaseExpiresAt,omitempty"`
	WorkerID       string `json:"workerId,omitempty"`

	CreatedAt   int64 `json:"createdAt"`
	ClaimedAt   int64 `json:"claimedAt,omitempty"`
	HeartbeatAt int64 `json:"heartbeatAt,omitempty"`
	FinishedAt  int64 `json:"finishedAt,omitempty"`

	// CompletedByLease records WHICH lease produced the stored result. A duplicate completion from
	// that same lease is idempotent; one from any other lease is a 409 and cannot overwrite it.
	CompletedByLease string `json:"completedByLease,omitempty"`

	// Progress is the worker's own bounded, structured note of where it is. Free text is redacted
	// and capped before it is stored.
	Stage string `json:"stage,omitempty"`

	Error    string          `json:"error,omitempty"`
	Artifact *AgencyArtifact `json:"artifact,omitempty"`
}

// terminal reports whether no further transition is possible.
func (r AgencyRun) terminal() bool {
	switch r.Status {
	case agencyCompleted, agencyFailed, agencyCancelled, agencyExpired:
		return true
	}
	return false
}

// leaseHeld reports whether the run is held by a live lease at `now`.
func (r AgencyRun) leaseHeld(now time.Time) bool {
	if r.Status != agencyClaimed && r.Status != agencyRunning {
		return false
	}
	return r.LeaseToken != "" && r.LeaseExpiresAt > now.Unix()
}

// claimable reports whether a worker may take this run at `now`.
//
// A queued run is claimable. A claimed/running run whose lease has LAPSED is claimable again, which
// is what makes an abandoned run recoverable without an operator: the worker that vanished cannot
// complete it afterwards, because `complete` requires the CURRENT token.
func (r AgencyRun) claimable(now time.Time) bool {
	if r.terminal() {
		return false
	}
	if r.Attempts >= agencyMaxAttempts {
		return false
	}
	if r.Status == agencyQueued {
		return true
	}
	return !r.leaseHeld(now)
}

// claimableAt is `claimable` for a run that has NOT been reconciled yet — the answer reconciliation
// would give, computed without performing it.
//
// The difference is exactly one rule: `reconcileLocked` expires a run that has sat queued past
// `agencyMaxRunAge`, and `claimable` (which runs after reconciliation, on an already-corrected row)
// therefore never has to consider it. A pure reader — `QueuedCount`, serving the side-effect-free
// worker preflight — does have to, or it would count runs that the very next write will expire.
//
// A value receiver, on a copy, so a caller cannot write through it.
func (r AgencyRun) claimableAt(now time.Time) bool {
	if !r.claimable(now) {
		return false
	}
	if r.Status == agencyQueued && now.Sub(time.Unix(r.CreatedAt, 0)) > agencyMaxRunAge {
		return false // reconciliation would expire this before anyone could claim it
	}
	return true
}

// agencyRunView is what an owner is allowed to see. The lease token is absent by construction
// rather than by remembering to strip it: this struct simply has no field for it.
type agencyRunView struct {
	ID              string          `json:"id"`
	SchemaVersion   string          `json:"schemaVersion"`
	WorkflowVersion string          `json:"workflowVersion"`
	Ticker          string          `json:"ticker"`
	Question        string          `json:"question"`
	AsOf            string          `json:"asOf"`
	Status          string          `json:"status"`
	Stage           string          `json:"stage,omitempty"`
	Attempts        int             `json:"attempts"`
	CreatedAt       int64           `json:"createdAt"`
	ClaimedAt       int64           `json:"claimedAt,omitempty"`
	HeartbeatAt     int64           `json:"heartbeatAt,omitempty"`
	FinishedAt      int64           `json:"finishedAt,omitempty"`
	LeaseExpiresAt  int64           `json:"leaseExpiresAt,omitempty"`
	Error           string          `json:"error,omitempty"`
	Artifact        *AgencyArtifact `json:"artifact,omitempty"`

	// Actionability is served on EVERY run, in every state, so a reader can never mistake "the
	// research lane did not answer that question" for "there is nothing to act on". In v1 it is
	// always NO_SIGNAL with the gate list showing exactly which evidence was never evaluated.
	Actionability agencyActionabilityView `json:"actionability"`

	PollAfterMs int    `json:"pollAfterMs"`
	Disclaimer  string `json:"disclaimer"`
}

const agencyDisclaimer = "Research output from AI agents run on the owner's own machine. It is " +
	"descriptive research with citations — NOT a recommendation, not a price target, and not " +
	"advice. The only actionable signal in this application remains the backtest-gated quant " +
	"signal, and this lane cannot produce or influence one."

func agencyView(r AgencyRun) agencyRunView {
	return agencyRunView{
		ID: r.ID, SchemaVersion: r.SchemaVersion, WorkflowVersion: r.WorkflowVersion,
		Ticker: r.Ticker, Question: r.Question, AsOf: r.AsOf,
		Status: r.Status, Stage: r.Stage, Attempts: r.Attempts,
		CreatedAt: r.CreatedAt, ClaimedAt: r.ClaimedAt, HeartbeatAt: r.HeartbeatAt,
		FinishedAt: r.FinishedAt, LeaseExpiresAt: r.LeaseExpiresAt,
		Error: r.Error, Artifact: r.Artifact,
		Actionability: agencyActionability(r),
		PollAfterMs:   agencyPollAfterMs,
		Disclaimer:    agencyDisclaimer,
	}
}

// ────────────────────────────────────────────────────────────────────────────────────── artifact

// AgencyArtifact is the validated research record. Read the field list as a claim about what this
// lane may express — and note what is NOT here: direction, signal, target, price, expected return,
// probability, confidence, size, weight, entry, stop, recommendation. See the header.
type AgencyArtifact struct {
	SchemaVersion   string `json:"schemaVersion"`
	RunID           string `json:"runId"`
	WorkflowVersion string `json:"workflowVersion"`
	Ticker          string `json:"ticker"`
	Question        string `json:"question"`
	AsOf            string `json:"asOf"`
	ProducedAt      string `json:"producedAt"`

	Sources []AgencySource `json:"sources"`
	Stages  []AgencyStage  `json:"stages"`

	UnresolvedQuestions []string              `json:"unresolvedQuestions"`
	Contradictions      []AgencyContradiction `json:"contradictions"`

	Thesis       AgencyPosition  `json:"thesis"`
	AntiThesis   AgencyPosition  `json:"antiThesis"`
	RiskFindings []AgencyFinding `json:"riskFindings"`
	Chair        AgencyChair     `json:"chairConclusion"`

	ResearchPriority string     `json:"researchPriority"`
	Veto             AgencyVeto `json:"veto"`

	Identity AgencyIdentity `json:"identity"`
	Degraded []string       `json:"degraded"`
}

// AgencySource is one citation. `PublishedAt` may be the literal "unknown" — an undated source is a
// fact about the source, and inventing a date to satisfy a schema is exactly the dishonesty this
// lane is built to avoid.
type AgencySource struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	URL         string `json:"url"`
	PublishedAt string `json:"publishedAt"`
	Publisher   string `json:"publisher,omitempty"`
}

// AgencyFinding is one statement plus the provenance that makes it auditable.
type AgencyFinding struct {
	Statement  string   `json:"statement"`
	Provenance string   `json:"provenance"`
	SourceIDs  []string `json:"sourceIds"`
	// Basis shows the arithmetic for a `calculated` finding. Required there and forbidden elsewhere:
	// a calculation nobody can re-do is an assertion wearing a number's clothes.
	Basis string `json:"basis,omitempty"`
}

type AgencyStage struct {
	Profile   string          `json:"profile"`
	Status    string          `json:"status"` // ok | skipped | failed
	Findings  []AgencyFinding `json:"findings"`
	Notes     []string        `json:"notes,omitempty"`
	StartedAt string          `json:"startedAt"`
	EndedAt   string          `json:"endedAt"`
}

type AgencyPosition struct {
	Statement string          `json:"statement"`
	Support   []AgencyFinding `json:"support"`
}

type AgencyContradiction struct {
	Statement string   `json:"statement"`
	SourceIDs []string `json:"sourceIds"`
}

type AgencyChair struct {
	Conclusion        string   `json:"conclusion"`
	KeyRisks          []string `json:"keyRisks"`
	WhatWouldChangeIt []string `json:"whatWouldChangeIt"`
}

type AgencyVeto struct {
	Raised  bool     `json:"raised"`
	Scope   string   `json:"scope"`
	Reasons []string `json:"reasons"`
}

// AgencyIdentity is deliberately NARROWER than services/llm's IDENTITY_KEYS. That block carries
// `modelUsed`, `quantization` and `generationSettings`, which are exactly the local details that
// must never leave the owner's machine. What travels is what the hosted side needs in order to
// version, audit and later revoke a range of artifacts: the schema, the workflow, the chain, and
// how much of it actually ran.
type AgencyIdentity struct {
	WorkflowVersion       string   `json:"workflowVersion"`
	ArtifactSchemaVersion string   `json:"artifactSchemaVersion"`
	Profiles              []string `json:"profiles"`
	StagesCompleted       int      `json:"stagesCompleted"`
	BridgeVersion         string   `json:"bridgeVersion"`
}

// ──────────────────────────────────────────────────────────────────────────────────── validation

// agencyValidationError is a refusal with a stated reason. Every one of them is a 400 and a FAILED
// run: a partially-accepted artifact is worse than none, because it is indistinguishable from a
// whole one once it is stored.
type agencyValidationError struct{ reason string }

func (e agencyValidationError) Error() string { return e.reason }

func invalidArtifact(format string, args ...any) error {
	return agencyValidationError{reason: fmt.Sprintf(format, args...)}
}

// agencyBannedPhrases are the directional spellings this lane may never carry. It mirrors the
// intent of services/llm/app/prompt.py's BANNED list: the research surface describes, it does not
// prescribe. Matching is case-insensitive over a whitespace-normalised copy of the text, so
// "B U Y" is not a hole but "buyback" and "sell-through" are not false positives (see the word
// boundaries).
var agencyBannedPhrases = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\brecommend(?:s|ed|ation|ations)?\s+(?:a\s+)?(?:buy|sell|hold|long|short)\b`),
	regexp.MustCompile(`(?i)\b(?:we|i|you)\s+(?:should|must|ought\s+to)\s+(?:buy|sell|short|go\s+long)\b`),
	regexp.MustCompile(`(?i)\bprice\s+target\b`),
	regexp.MustCompile(`(?i)\btarget\s+price\b`),
	regexp.MustCompile(`(?i)\bfair\s+value\s+(?:of|is|:)\s*\$`),
	regexp.MustCompile(`(?i)\bexpected\s+return\b`),
	regexp.MustCompile(`(?i)\bupside\s+of\s+\d`),
	regexp.MustCompile(`(?i)\bdownside\s+of\s+\d`),
	regexp.MustCompile(`(?i)\b(?:strong\s+)?(?:buy|sell)\s+rating\b`),
	regexp.MustCompile(`(?i)\brated\s+(?:a\s+)?(?:buy|sell|hold)\b`),
	regexp.MustCompile(`(?i)\b(?:position\s+siz(?:e|ing)|allocate\s+\d+\s*%)\b`),
	regexp.MustCompile(`(?i)\bstop[\s-]?loss\b`),
	regexp.MustCompile(`(?i)\bentry\s+point\b`),
}

// agencyLeakPatterns are the shapes that must never leave the worker's machine inside an artifact.
//
// THEY MATCH OPERATIONAL DISCLOSURE, NOT SUBJECT MATTER — and that distinction is the whole design.
//
// An earlier version of this list rejected any artifact that mentioned "anthropic", "openai",
// "qwen" and so on. For a research tool pointed at semiconductor and software companies that is
// close to useless: "OpenAI is a major customer of the issuer" is exactly the kind of sourced fact
// this lane exists to collect, and refusing it would train the operator to disable the check.
//
// What must never travel is what the run did on the owner's MACHINE: which model answered, which
// provider served it, how many tokens it cost, what the subscription is, which session it was, and
// where anything lives on disk. So the ambiguous keys — `model:`, `provider:` — are only a match
// when paired with the name of an inference runtime, and the unambiguous ones (`model_used`,
// `quantization`, `session_id`) match on their own.
//
// KEEP THIS LIST BYTE-IDENTICAL WITH bridge/redact.go's `leakPatterns`. The worker refuses first so
// nothing crosses the network, and the server refuses again so a worker that skipped the check
// gains nothing. Two checks of one rule only work while they are the same rule; agency_test.go and
// bridge/redact_test.go run the SAME accept/reject table against their own copy.
var agencyLeakPatterns = []struct {
	re     *regexp.Regexp
	reason string
}{
	// ── paths and credentials ────────────────────────────────────────────────────────────────
	{regexp.MustCompile(`(?i)/Users/[^\s"']+`), "an absolute macOS home path"},
	{regexp.MustCompile(`(?i)/home/[^\s"']+`), "an absolute Linux home path"},
	{regexp.MustCompile(`(?i)[A-Z]:\\Users\\[^\s"']+`), "an absolute Windows home path"},
	{regexp.MustCompile(`(?i)\.hermes\b`), "a reference to local Hermes state"},
	{regexp.MustCompile(`(?i)\bauth\.json\b`), "a reference to a credential file"},
	{regexp.MustCompile(`(?i)\bsk-[A-Za-z0-9_\-]{16,}`), "an API-key-shaped string"},
	{regexp.MustCompile(`(?i)\bgh[pousr]_[A-Za-z0-9]{20,}`), "a token-shaped string"},
	{regexp.MustCompile(`://[^/\s:@]+:[^/\s@]+@`), "a URL containing credentials"},

	// ── operational metadata: keys that can only be about how THIS run was executed ───────────
	{regexp.MustCompile(`(?i)\b(?:model[_\s-]?(?:used|name|id)|llm[_\s-]?(?:model|provider)|inference[_\s-]?(?:model|provider)|quantization|reasoning[_\s-]?effort|max[_\s-]?tokens|top[_\s-]?p|system[_\s-]?prompt|prompt[_\s-]?version|session[_\s-]?id|api[_\s-]?(?:key|token))\s*[:=]`), "local model or session configuration"},

	// ── operational metadata: ambiguous keys, disambiguated by an INFERENCE-PROVIDER value ────
	// `model:` and `provider:` are ordinary words in investment research — "business model:
	// subscription", "cloud provider: AWS" — so the key alone proves nothing. What proves it is the
	// key paired with the name of a model runtime or an inference vendor.
	{regexp.MustCompile(`(?i)\b(?:model|provider|engine|backend)\s*[:=]\s*["']?(?:anthropic|openai|openrouter|ollama|vllm|sglang|lm[\s_-]?studio|together|groq|fireworks|bedrock|claude|gpt-|o[1-9]-|qwen|llama|mistral|gemini|deepseek|phi-|command-r)`), "the inference model or provider this run used"},

	// ── operational metadata: self-referential statements about how the answer was produced ───
	{regexp.MustCompile(`(?i)\b(?:this|the)\s+(?:run|stage|artifact|analysis|response|answer|report)\s+(?:was\s+)?(?:generated|produced|created|written|answered)\s+(?:by|with|using)\b`), "a statement about how this run was generated"},
	{regexp.MustCompile(`(?i)\b(?:i|we)\s+(?:am|are|was|were)\s+(?:running\s+on|powered\s+by|built\s+on|based\s+on)\s+(?:anthropic|openai|claude|gpt|qwen|llama|mistral|gemini)`), "a statement about the local model"},

	// ── operational metadata: token accounting, cost and quota ────────────────────────────────
	// "prompt tokens" / "completion tokens" are API-response field names. "input tokens" and
	// "output tokens" are NOT on this list: they are ordinary pricing vocabulary for any AI company
	// an analyst might cover ("charges $15 per million output tokens").
	{regexp.MustCompile(`(?i)\b(?:prompt|completion)\s+tokens\b`), "provider token accounting"},
	{regexp.MustCompile(`(?i)\btokens?\s+(?:used|consumed|spent|remaining)\b`), "provider token accounting"},
	// A COLON OR EQUALS IS REQUIRED, and that requirement is load-bearing: "total cost of revenue of
	// $4.1bn" is a standard income-statement line, while "estimated cost: $0.42" is a usage report.
	// The first version of this rule matched the bare phrase and rejected the accounting line — the
	// exact class of false positive that would make an analyst switch the scan off.
	{regexp.MustCompile(`(?i)\b(?:estimated|total|api|inference)[_\s-]?cost\b\s*[:=]`), "inference cost detail"},
	{regexp.MustCompile(`(?i)\busage\s+report\b`), "a provider usage report"},
	{regexp.MustCompile(`(?i)\b(?:chatgpt|claude|openai|anthropic)\s+(?:plus|pro|max|team|enterprise|api)\s+(?:subscription|plan|account|credits?|key)\b`), "a model-subscription detail"},
	{regexp.MustCompile(`(?i)\b(?:my|our)\s+(?:subscription|quota|api\s+key|credits|rate\s+limit)\b`), "a subscription or quota detail"},

	// ── operational metadata: LOCAL INFERENCE RUNTIMES ────────────────────────────────────────
	// These names are treated differently from "OpenAI" or "Anthropic" on purpose. A public model
	// vendor is ordinary subject matter for equity research — it is a customer, a competitor or a
	// counterparty, and rejecting it would gut the tool. A local serving runtime is not: nothing an
	// analyst writes about a semiconductor or software company needs to name the thing running the
	// model on this laptop, so its appearance is operational disclosure rather than a finding.
	{regexp.MustCompile(`(?i)\b(?:ollama|vllm|sglang|llama\.cpp|lm[\s_-]?studio|text-generation-webui|openrouter)\b`), "a local inference-runtime reference"},
}

// validateAgencyArtifact is the whole gate. It runs on the SERVER, over what a worker actually
// sent, and every failure is fail-closed.
//
// `run` is the stored run the artifact claims to answer. The ticker, the run id, the workflow and
// the as-of cutoff are checked against it rather than trusted from the payload — a worker that
// could restate its own cutoff could backdate one.
func validateAgencyArtifact(a *AgencyArtifact, run AgencyRun) error {
	if a == nil {
		return invalidArtifact("no artifact was supplied")
	}
	if a.SchemaVersion != agencyArtifactSchemaVersion {
		return invalidArtifact("artifact schemaVersion is %q; this server accepts only %q",
			a.SchemaVersion, agencyArtifactSchemaVersion)
	}
	if a.RunID != run.ID {
		return invalidArtifact("artifact runId %q does not belong to run %q", a.RunID, run.ID)
	}
	if a.WorkflowVersion != run.WorkflowVersion {
		return invalidArtifact("artifact workflowVersion is %q; the run is %q",
			a.WorkflowVersion, run.WorkflowVersion)
	}
	if a.Ticker != run.Ticker {
		return invalidArtifact("artifact ticker %q does not match the run's %q", a.Ticker, run.Ticker)
	}
	if a.AsOf != run.AsOf {
		return invalidArtifact("artifact asOf %q does not match the run's server-assigned cutoff %q",
			a.AsOf, run.AsOf)
	}
	if _, err := time.Parse(time.RFC3339, a.ProducedAt); err != nil {
		return invalidArtifact("artifact producedAt is not an RFC3339 timestamp")
	}

	// --- sources ---------------------------------------------------------------------------
	if len(a.Sources) > agencyMaxSources {
		return invalidArtifact("artifact carries %d sources; the limit is %d",
			len(a.Sources), agencyMaxSources)
	}
	seen := map[string]bool{}
	for i, s := range a.Sources {
		if !agencySourceRE.MatchString(s.ID) {
			return invalidArtifact("source %d has id %q; ids must look like s1, s2, s3", i, s.ID)
		}
		if seen[s.ID] {
			return invalidArtifact("source id %q appears twice", s.ID)
		}
		seen[s.ID] = true
		if strings.TrimSpace(s.Title) == "" {
			return invalidArtifact("source %s has no title", s.ID)
		}
		if err := validateAgencySourceURL(s.URL); err != nil {
			return invalidArtifact("source %s: %v", s.ID, err)
		}
		if err := validateAgencySourceDate(s.PublishedAt); err != nil {
			return invalidArtifact("source %s: %v", s.ID, err)
		}
	}

	// --- research priority and veto ----------------------------------------------------------
	switch a.ResearchPriority {
	case priorityInvestigate, priorityWatch, priorityReject, priorityUnknown:
	default:
		return invalidArtifact("researchPriority %q is not one of investigate, watch, reject, unknown",
			a.ResearchPriority)
	}
	if a.Veto.Scope != agencyVetoScope {
		return invalidArtifact("veto scope is %q; the only permitted scope is %q — a veto may "+
			"caution against new exposure and may do nothing else", a.Veto.Scope, agencyVetoScope)
	}
	if a.Veto.Raised && len(a.Veto.Reasons) == 0 {
		return invalidArtifact("a raised veto must state at least one reason")
	}
	if !a.Veto.Raised && len(a.Veto.Reasons) > 0 {
		return invalidArtifact("veto reasons were supplied without raising the veto")
	}

	// --- stages ------------------------------------------------------------------------------
	if len(a.Stages) != len(agencyProfileChain) {
		return invalidArtifact("artifact carries %d stages; %s runs exactly %d",
			len(a.Stages), run.WorkflowVersion, len(agencyProfileChain))
	}
	completed := 0
	for i, st := range a.Stages {
		if st.Profile != agencyProfileChain[i] {
			return invalidArtifact("stage %d is %q; %s runs %v in that order",
				i, st.Profile, run.WorkflowVersion, agencyProfileChain)
		}
		switch st.Status {
		case "ok":
			completed++
		case "skipped", "failed":
		default:
			return invalidArtifact("stage %s has status %q; expected ok, skipped or failed",
				st.Profile, st.Status)
		}
		if len(st.Findings) > agencyMaxFindingsPerBox {
			return invalidArtifact("stage %s carries %d findings; the limit is %d",
				st.Profile, len(st.Findings), agencyMaxFindingsPerBox)
		}
		for j, f := range st.Findings {
			if err := validateAgencyFinding(f, seen); err != nil {
				return invalidArtifact("stage %s finding %d: %v", st.Profile, j, err)
			}
		}
		if _, err := time.Parse(time.RFC3339, st.StartedAt); err != nil {
			return invalidArtifact("stage %s startedAt is not an RFC3339 timestamp", st.Profile)
		}
		if _, err := time.Parse(time.RFC3339, st.EndedAt); err != nil {
			return invalidArtifact("stage %s endedAt is not an RFC3339 timestamp", st.Profile)
		}
	}

	// --- thesis, anti-thesis, risks ------------------------------------------------------------
	for label, pos := range map[string]AgencyPosition{"thesis": a.Thesis, "antiThesis": a.AntiThesis} {
		if strings.TrimSpace(pos.Statement) == "" {
			return invalidArtifact("%s has no statement", label)
		}
		if len(pos.Support) > agencyMaxFindingsPerBox {
			return invalidArtifact("%s carries %d support findings; the limit is %d",
				label, len(pos.Support), agencyMaxFindingsPerBox)
		}
		for j, f := range pos.Support {
			if err := validateAgencyFinding(f, seen); err != nil {
				return invalidArtifact("%s support %d: %v", label, j, err)
			}
		}
	}
	if len(a.RiskFindings) > agencyMaxFindingsPerBox {
		return invalidArtifact("artifact carries %d risk findings; the limit is %d",
			len(a.RiskFindings), agencyMaxFindingsPerBox)
	}
	for j, f := range a.RiskFindings {
		if err := validateAgencyFinding(f, seen); err != nil {
			return invalidArtifact("risk finding %d: %v", j, err)
		}
	}
	for j, c := range a.Contradictions {
		if strings.TrimSpace(c.Statement) == "" {
			return invalidArtifact("contradiction %d has no statement", j)
		}
		for _, id := range c.SourceIDs {
			if !seen[id] {
				return invalidArtifact("contradiction %d cites unknown source %q", j, id)
			}
		}
	}
	if strings.TrimSpace(a.Chair.Conclusion) == "" {
		return invalidArtifact("the chair conclusion is empty")
	}

	// --- identity -------------------------------------------------------------------------------
	if a.Identity.ArtifactSchemaVersion != agencyArtifactSchemaVersion ||
		a.Identity.WorkflowVersion != run.WorkflowVersion {
		return invalidArtifact("identity does not restate the artifact schema and workflow versions")
	}
	if len(a.Identity.Profiles) != len(agencyProfileChain) {
		return invalidArtifact("identity names %d profiles; the chain has %d",
			len(a.Identity.Profiles), len(agencyProfileChain))
	}
	for i, p := range a.Identity.Profiles {
		if p != agencyProfileChain[i] {
			return invalidArtifact("identity profile %d is %q; expected %q",
				i, p, agencyProfileChain[i])
		}
	}
	if a.Identity.StagesCompleted != completed {
		return invalidArtifact("identity claims %d completed stages; %d stages reported ok",
			a.Identity.StagesCompleted, completed)
	}

	// --- research quality: a positive outcome must rest on something --------------------------
	if err := validateAgencyCoverage(a); err != nil {
		return err
	}

	// --- point in time: a citation cannot postdate the cutoff it was gathered under ------------
	//
	// The run's `asOf` is server-assigned and the artifact had to echo it back exactly (checked
	// above), so this compares a source's own publication date against the instant the research was
	// supposed to answer at. A source published AFTER that instant is either a hallucinated date or
	// research that reached past its own cutoff; both make the artifact unusable as a point-in-time
	// record, and this lane is built to feed one.
	//
	// Day-granularity dates are compared at the START of their day, so a source dated the same day
	// as the cutoff is accepted — a `YYYY-MM-DD` source does not claim an instant and must not be
	// rejected for one it never stated. `unknown` is skipped entirely.
	cutoff, err := time.Parse(time.RFC3339, run.AsOf)
	if err == nil {
		for _, s := range a.Sources {
			published, ok := parseAgencySourceDate(s.PublishedAt)
			if !ok {
				continue // "unknown", already accepted by validateAgencySourceDate
			}
			if published.After(cutoff) {
				return invalidArtifact("source %s is dated %s, after this run's cutoff of %s — a "+
					"citation cannot postdate the point in time the research answers at",
					s.ID, s.PublishedAt, run.AsOf)
			}
		}
	}

	// --- the two content scans, over EVERY string the artifact carries ---------------------------
	// The LEAK scan and the length cap run over EVERY string, quoted or authored. A credential or a
	// home path is a disclosure wherever it appears, including in a citation title.
	for _, text := range agencyArtifactStrings(a) {
		if len(text) > agencyMaxStatementLen {
			return invalidArtifact("a statement is %d characters; the limit is %d",
				len(text), agencyMaxStatementLen)
		}
		if reason := matchAgencyLeak(text); reason != "" {
			return invalidArtifact("the artifact contains %s, which may not leave the worker", reason)
		}
	}

	// The PRESCRIPTIVE-LANGUAGE scan runs only over what the AGENTS WROTE.
	//
	// The rule is about what this lane may CLAIM, not about which words may be transcribed. "Is the
	// sell-side price target justified by the filings?" is a legitimate question for an owner to
	// ask, and "Analyst raises price target on NVDA" is the real headline of a real citation.
	// Refusing either would mean the research tool cannot research the analyst commentary that is
	// half of what moves a stock — and would push agents into paraphrasing headlines, which is a
	// worse outcome than quoting them.
	//
	// So the owner's question and a source's title, publisher and URL are QUOTED SUBJECT MATTER and
	// are exempt. Everything an agent composed — findings and their bases, notes, contradictions,
	// the thesis and anti-thesis, risk findings, the chair's conclusion and the veto's reasons — is
	// scanned, at every provenance including `inferred`. An agent may report that an analyst issued
	// a rating; it may not issue one.
	for _, text := range agencyAuthoredStrings(a) {
		if re := matchAgencyBanned(text); re != "" {
			return invalidArtifact("the artifact contains prescriptive language (%s) in agent-"+
				"authored output. This lane produces research, never a recommendation, target or "+
				"position — a third party's rating may be reported as a quoted source, not adopted", re)
		}
	}
	return nil
}

func validateAgencyFinding(f AgencyFinding, sources map[string]bool) error {
	if strings.TrimSpace(f.Statement) == "" {
		return errors.New("the statement is empty")
	}
	switch f.Provenance {
	case provenanceSourced:
		if len(f.SourceIDs) == 0 {
			return errors.New("a sourced finding must cite at least one source")
		}
	case provenanceCalculated:
		if strings.TrimSpace(f.Basis) == "" {
			return errors.New("a calculated finding must show its basis")
		}
		if len(f.SourceIDs) == 0 {
			return errors.New("a calculated finding must cite the sources its inputs came from")
		}
	case provenanceInferred, provenanceUnknown:
		if strings.TrimSpace(f.Basis) != "" {
			return fmt.Errorf("a %s finding must not carry a calculation basis", f.Provenance)
		}
	default:
		return fmt.Errorf("provenance %q is not one of sourced, calculated, inferred, unknown",
			f.Provenance)
	}
	for _, id := range f.SourceIDs {
		if !sources[id] {
			return fmt.Errorf("cites unknown source %q", id)
		}
	}
	return nil
}

// validateAgencySourceURL requires an absolute http(s) URL with a host and no embedded credentials.
// A citation nobody can open is not a citation.
func validateAgencySourceURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return errors.New("the url is not parseable")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("the url must be http or https")
	}
	if u.Host == "" {
		return errors.New("the url has no host")
	}
	if u.User != nil {
		return errors.New("the url carries credentials")
	}
	return nil
}

// validateAgencySourceDate accepts YYYY-MM-DD, an RFC3339 instant, or the literal "unknown".
// "unknown" is a real answer and is preserved as one — see AgencySource.
func validateAgencySourceDate(raw string) error {
	v := strings.TrimSpace(raw)
	if v == provenanceUnknown {
		return nil
	}
	if _, ok := parseAgencySourceDate(v); ok {
		return nil
	}
	return errors.New(`publishedAt must be YYYY-MM-DD, an RFC3339 instant, or the literal "unknown"`)
}

// parseAgencySourceDate returns the instant a source's date names, and whether it named one at all.
// A day-granularity date resolves to the START of that day, so it is never treated as later than it
// claims (see the cutoff comparison in validateAgencyArtifact).
func parseAgencySourceDate(raw string) (time.Time, bool) {
	v := strings.TrimSpace(raw)
	if v == "" || v == provenanceUnknown {
		return time.Time{}, false
	}
	if t, err := time.Parse("2006-01-02", v); err == nil {
		return t.UTC(), true
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
}

// agencyGroundedCoverage counts DISTINCT CLAIMS, not appearances.
//
// It walks every list a finding can live in — each stage, both positions' support, and the risk
// findings — because a rule that looked at only one of them would be satisfied by moving the
// ungrounded material elsewhere. But walking them all naively counts the same claim several times:
// the chair's thesis support quite properly repeats a finding the scout already reported, and the
// risk stage restates what it is attacking. Counting those as separate evidence would let ONE
// sourced sentence, echoed across four stages, clear a floor meant to say "this rests on more than
// one thing".
//
// So claims are deduplicated on their normalised statement text, and a claim counts as grounded if
// ANY of its appearances is sourced or calculated — a finding restated as `inferred` further down
// the chain does not un-ground the sourced original.
func agencyGroundedCoverage(a *AgencyArtifact) (grounded, total int) {
	// statement -> whether any appearance of it rests on a source.
	seen := map[string]bool{}
	order := make([]string, 0, 16)
	count := func(fs []AgencyFinding) {
		for _, f := range fs {
			key := agencyClaimKey(f.Statement)
			if key == "" {
				continue
			}
			isGrounded := f.Provenance == provenanceSourced || f.Provenance == provenanceCalculated
			if _, ok := seen[key]; !ok {
				order = append(order, key)
				seen[key] = isGrounded
				continue
			}
			seen[key] = seen[key] || isGrounded
		}
	}
	for _, st := range a.Stages {
		count(st.Findings)
	}
	count(a.Thesis.Support)
	count(a.AntiThesis.Support)
	count(a.RiskFindings)

	for _, key := range order {
		total++
		if seen[key] {
			grounded++
		}
	}
	return grounded, total
}

// agencyClaimKey normalises a statement for deduplication: whitespace collapsed, case folded, and
// trailing punctuation dropped, so "Revenue rose 12%." and "revenue rose 12%" are one claim rather
// than two. It is deliberately conservative — it merges restatements, not paraphrases, because
// merging paraphrases would need a judgement this validator has no business making.
func agencyClaimKey(statement string) string {
	key := strings.ToLower(strings.Join(strings.Fields(statement), " "))
	return strings.TrimRight(key, ".!? ")
}

// validateAgencyCoverage enforces the floors described at agencyMinGroundedFindings.
//
// It applies ONLY to `investigate` and `watch`. `unknown` and `reject` are the outcomes for a run
// that did not establish anything, and requiring citations to say "I found nothing" would be
// incoherent — it would leave a scrupulous run with no legal way to report itself.
func validateAgencyCoverage(a *AgencyArtifact) error {
	if a.ResearchPriority != priorityInvestigate && a.ResearchPriority != priorityWatch {
		return nil
	}
	if len(a.Sources) == 0 {
		return invalidArtifact("researchPriority %q was returned with no sources at all; an "+
			"artifact that cites nothing must report %q", a.ResearchPriority, priorityUnknown)
	}
	grounded, total := agencyGroundedCoverage(a)
	if grounded < agencyMinGroundedFindings {
		return invalidArtifact("researchPriority %q rests on %d DISTINCT sourced or calculated "+
			"finding(s); at least %d are required, or the run must report %q. Repeating one claim "+
			"across stages does not make it two",
			a.ResearchPriority, grounded, agencyMinGroundedFindings, priorityUnknown)
	}
	if total > 0 && float64(grounded)/float64(total) < agencyMinGroundedFraction {
		return invalidArtifact("researchPriority %q rests on %d grounded claim(s) out of %d "+
			"distinct; at least %.0f%% must rest on a source, or the run must report %q",
			a.ResearchPriority, grounded, total, agencyMinGroundedFraction*100, priorityUnknown)
	}
	return nil
}

// agencyArtifactStrings flattens EVERY free-text string in the artifact — quoted and authored alike
// — so the leak scan and the length cap cannot be evaded by putting the text in a field nobody
// remembered to check. It is the superset of agencyAuthoredStrings.
func agencyArtifactStrings(a *AgencyArtifact) []string {
	var out []string
	add := func(vs ...string) { out = append(out, vs...) }
	addFindings := func(fs []AgencyFinding) {
		for _, f := range fs {
			add(f.Statement, f.Basis)
		}
	}
	add(a.Question)
	for _, s := range a.Sources {
		add(s.Title, s.URL, s.Publisher)
	}
	for _, st := range a.Stages {
		add(st.Notes...)
		addFindings(st.Findings)
	}
	add(a.UnresolvedQuestions...)
	for _, c := range a.Contradictions {
		add(c.Statement)
	}
	add(a.Thesis.Statement, a.AntiThesis.Statement)
	addFindings(a.Thesis.Support)
	addFindings(a.AntiThesis.Support)
	addFindings(a.RiskFindings)
	add(a.Chair.Conclusion)
	add(a.Chair.KeyRisks...)
	add(a.Chair.WhatWouldChangeIt...)
	add(a.Veto.Reasons...)
	add(a.Degraded...)
	add(a.Identity.BridgeVersion)
	return out
}

// agencyAuthoredStrings flattens only what the AGENTS COMPOSED. See the scan in
// validateAgencyArtifact for why the split exists.
//
// EXCLUDED, deliberately: `Question` (the owner typed it) and each source's `Title`, `URL` and
// `Publisher` (a third party published them). Everything else an artifact carries was written by a
// stage and is therefore a claim this lane is making.
//
// KEEP IN STEP WITH bridge/redact.go's `authoredStrings`, which excludes the same four fields.
func agencyAuthoredStrings(a *AgencyArtifact) []string {
	var out []string
	add := func(vs ...string) { out = append(out, vs...) }
	addFindings := func(fs []AgencyFinding) {
		for _, f := range fs {
			add(f.Statement, f.Basis)
		}
	}
	for _, st := range a.Stages {
		add(st.Notes...)
		addFindings(st.Findings)
	}
	add(a.UnresolvedQuestions...)
	for _, c := range a.Contradictions {
		add(c.Statement)
	}
	add(a.Thesis.Statement, a.AntiThesis.Statement)
	addFindings(a.Thesis.Support)
	addFindings(a.AntiThesis.Support)
	addFindings(a.RiskFindings)
	add(a.Chair.Conclusion)
	add(a.Chair.KeyRisks...)
	add(a.Chair.WhatWouldChangeIt...)
	add(a.Veto.Reasons...)
	add(a.Degraded...)
	return out
}

func matchAgencyBanned(text string) string {
	normalised := strings.Join(strings.Fields(text), " ")
	for _, re := range agencyBannedPhrases {
		if m := re.FindString(normalised); m != "" {
			// Quoted so an operator can see exactly which words tripped the scan.
			return strconv.Quote(m)
		}
	}
	return ""
}

func matchAgencyLeak(text string) string {
	for _, p := range agencyLeakPatterns {
		if p.re.MatchString(text) {
			return p.reason
		}
	}
	return ""
}

// ───────────────────────────────────────────────────────────────────────────────────── redaction

// agencyRedactPatterns are applied to every free-text error string BEFORE it is stored on a run or
// written to a log. A refusal must be legible without becoming a disclosure: an error that quotes
// the failing token has published it to every reader of the run record.
var agencyRedactPatterns = []struct {
	re   *regexp.Regexp
	with string
}{
	{regexp.MustCompile(`(?i)(/Users/)[^\s"']+`), "${1}<redacted>"},
	{regexp.MustCompile(`(?i)(/home/)[^\s"']+`), "${1}<redacted>"},
	{regexp.MustCompile(`(?i)([A-Z]:\\Users\\)[^\s"']+`), "${1}<redacted>"},
	{regexp.MustCompile(`(?i)(\.hermes)(/[^\s"']*)?`), "${1}/<redacted>"},
	{regexp.MustCompile(`(?i)\bsk-[A-Za-z0-9_\-]{16,}`), "<redacted-key>"},
	{regexp.MustCompile(`(?i)\b(api[_-]?key|access[_-]?token|refresh[_-]?token|secret|password|bearer)\b(\s*[:=]\s*)\S+`), "${1}${2}<redacted>"},
	{regexp.MustCompile(`(://[^/\s:@]+):[^/\s@]+@`), "${1}:<redacted>@"},
	// `Bearer <token>` has no delimiter, so the assignment pattern above does not reach it.
	{regexp.MustCompile(`(?i)\b(bearer)\s+\S+`), "${1} <redacted>"},
	{regexp.MustCompile(`(?i)\b(x-worker-token|authorization)\b\s*[:=]\s*\S+`), "${1}: <redacted>"},
}

// redactAgencyText strips secret-shaped and path-shaped material, then caps the result. Applied to
// every worker-supplied error and to every error this service logs about the lane.
func redactAgencyText(s string) string {
	out := strings.TrimSpace(s)
	for _, p := range agencyRedactPatterns {
		out = p.re.ReplaceAllString(out, p.with)
	}
	out = strings.Join(strings.Fields(out), " ")
	if len(out) > agencyMaxErrorLen {
		out = out[:agencyMaxErrorLen] + "…"
	}
	return out
}

// ──────────────────────────────────────────────────────────────────────────────── ids and hashing

// newAgencyRunID mints a run id. Random rather than derived: the IDEMPOTENCY key is what makes a
// repeat request find the same run, and it is stored separately. An id that encoded the question
// would put the question in every URL and every log line.
func newAgencyRunID() (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "agr_" + hex.EncodeToString(buf), nil
}

// newAgencyLeaseToken mints an opaque lease token. 32 bytes of CSPRNG; compared in constant time.
func newAgencyLeaseToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// agencyIdempotencyKey is deterministic over exactly the four things that make two requests "the
// same request". A repeat while a run with this key is still live attaches to it and starts nothing.
func agencyIdempotencyKey(uid, workflow, ticker, question string) string {
	h := sha256.New()
	for _, part := range []string{uid, workflow, ticker, agencyNormaliseQuestion(question)} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// agencyNormaliseQuestion collapses whitespace and lowercases, so "Why  the MARGIN drop?" and
// "why the margin drop?" are one request rather than two identical jobs on one laptop.
func agencyNormaliseQuestion(q string) string {
	return strings.ToLower(strings.Join(strings.Fields(q), " "))
}

// ─────────────────────────────────────────────────────────────────────────── request validation

// agencyCreateRequest is the owner's request. It names a WORKFLOW and a subject — never a profile,
// a toolset, a model, a provider, a path, a command or a system prompt. There is no field for any
// of those and there must never be one: that absence is what keeps this from being a remote-command
// API. The bridge owns the mapping from `workflow` to what actually runs.
type agencyCreateRequest struct {
	Workflow string   `json:"workflow"`
	Ticker   string   `json:"ticker"`
	Tickers  []string `json:"tickers"`
	Question string   `json:"question"`
}

// normalise validates and returns (ticker, question). Fail-closed on everything.
func (req agencyCreateRequest) normalise() (string, string, error) {
	workflow := strings.TrimSpace(req.Workflow)
	if workflow == "" {
		workflow = agencyWorkflowCompanyResearch
	}
	if workflow != agencyWorkflowCompanyResearch {
		return "", "", fmt.Errorf("workflow %q is not available; this server runs only %q",
			workflow, agencyWorkflowCompanyResearch)
	}

	tickers := req.Tickers
	if t := strings.TrimSpace(req.Ticker); t != "" {
		tickers = append(tickers, t)
	}
	// De-duplicate before counting, so `{"ticker":"NVDA","tickers":["nvda"]}` is one company.
	uniq := map[string]bool{}
	var ordered []string
	for _, t := range tickers {
		up := strings.ToUpper(strings.TrimSpace(t))
		if up == "" {
			continue
		}
		if !uniq[up] {
			uniq[up] = true
			ordered = append(ordered, up)
		}
	}
	if len(ordered) == 0 {
		return "", "", errors.New("a ticker is required")
	}
	if len(ordered) > agencyMaxTickersPerRun {
		return "", "", fmt.Errorf("%d tickers were requested; %s researches %d company per run",
			len(ordered), agencyWorkflowCompanyResearch, agencyMaxTickersPerRun)
	}
	ticker := ordered[0]
	if !agencyTickerRE.MatchString(ticker) {
		return "", "", fmt.Errorf("ticker %q is not a symbol this server accepts", ticker)
	}

	question := strings.Join(strings.Fields(req.Question), " ")
	if question == "" {
		return "", "", errors.New("a research question is required")
	}
	if len(question) > agencyMaxQuestionLen {
		return "", "", fmt.Errorf("the question is %d characters; the limit is %d",
			len(question), agencyMaxQuestionLen)
	}
	// The question is the ONLY free text that crosses to the worker. It reaches Hermes through a
	// query FILE (never a shell argument), but a control character in a stored record is still a
	// defect, so it is refused here rather than sanitised silently.
	if strings.ContainsFunc(question, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", "", errors.New("the question contains control characters")
	}
	return ticker, question, nil
}

// sortAgencyRunsNewestFirst orders by creation time, then id, so a listing is stable.
func sortAgencyRunsNewestFirst(runs []AgencyRun) {
	sort.SliceStable(runs, func(i, j int) bool {
		if runs[i].CreatedAt != runs[j].CreatedAt {
			return runs[i].CreatedAt > runs[j].CreatedAt
		}
		return runs[i].ID > runs[j].ID
	})
}
