package main

import (
	"regexp"
	"strings"
)

// redact.go — the last line before anything is printed or uploaded.
//
// TWO PLACES THIS RUNS, AND BOTH ARE MANDATORY.
//
//  1. Every error this bridge constructs goes through `errf`, which redacts. So an error can carry
//     a reason without carrying a token, a key or an absolute home path — and there is no call site
//     that has to remember to do it, because the constructor does it.
//  2. Every string in the assembled artifact is scanned by `scanArtifactForLeaks` before it is
//     uploaded. The server scans again on receipt (journal/agency.go), so this is belt and braces —
//     but the belt matters: a leak caught here never crosses the network at all, while one caught
//     there has already been transmitted before it is rejected.
//
// WHAT COUNTS AS A LEAK IS DELIBERATELY BROADER THAN "A SECRET". An absolute home path names the
// operator. A `.hermes` reference discloses the local agent's layout. A model name discloses which
// provider subscription is in use. None of those are credentials and all of them are the owner's
// business alone, so they are all refused.

var redactions = []struct {
	re   *regexp.Regexp
	with string
}{
	// Absolute personal paths -> keep the shape, drop the identity.
	{regexp.MustCompile(`(?i)(/Users/)[^\s"',:;)]+`), "${1}<redacted>"},
	{regexp.MustCompile(`(?i)(/home/)[^\s"',:;)]+`), "${1}<redacted>"},
	{regexp.MustCompile(`(?i)([A-Z]:\\Users\\)[^\s"',:;)]+`), "${1}<redacted>"},
	// Local agent state.
	{regexp.MustCompile(`(?i)(\.hermes)(/[^\s"',:;)]*)?`), "${1}/<redacted>"},
	// Credential shapes.
	{regexp.MustCompile(`(?i)\bsk-[A-Za-z0-9_\-]{16,}`), "<redacted-key>"},
	{regexp.MustCompile(`(?i)\bgh[pousr]_[A-Za-z0-9]{20,}`), "<redacted-key>"},
	{regexp.MustCompile(`(?i)\b(api[_-]?key|access[_-]?token|refresh[_-]?token|secret|password|bearer|token)\b(\s*[:=]\s*)\S+`), "${1}${2}<redacted>"},
	{regexp.MustCompile(`(?i)\b(bearer)\s+\S+`), "${1} <redacted>"},
	{regexp.MustCompile(`(?i)\b(x-worker-token|authorization)\b\s*:\s*\S+`), "${1}: <redacted>"},
	// Credentials embedded in a URL.
	{regexp.MustCompile(`(://[^/\s:@]+):[^/\s@]+@`), "${1}:<redacted>@"},
	// Long hex runs — the shape of this bridge's own lease token and worker credential. 32 hex
	// characters is already improbable in prose; the real tokens are 64.
	{regexp.MustCompile(`\b[0-9a-fA-F]{32,}\b`), "<redacted-token>"},
}

// redact scrubs a string. It is idempotent and it never returns the input unchanged when the input
// matched — a redactor that can be talked out of redacting is not one.
func redact(s string) string {
	out := s
	for _, r := range redactions {
		out = r.re.ReplaceAllString(out, r.with)
	}
	return out
}

// leakPatterns are the shapes that must never leave this machine inside an artifact.
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
// KEEP THIS LIST BYTE-IDENTICAL WITH journal/agency.go's `agencyLeakPatterns`. The worker refuses first so
// nothing crosses the network, and the server refuses again so a worker that skipped the check
// gains nothing. Two checks of one rule only work while they are the same rule; bridge/redact_test.go and
// journal/agency_test.go run the SAME accept/reject table against their own copy.
var leakPatterns = []struct {
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

// scanArtifactForLeaks returns the reason the artifact may not be uploaded, or "".
func scanArtifactForLeaks(a *Artifact) string {
	for _, text := range artifactStrings(a) {
		for _, p := range leakPatterns {
			if p.re.MatchString(text) {
				return p.reason
			}
		}
	}
	return ""
}

// bannedPhrases is the worker-side copy of the server's prescriptive-language scan
// (journal/agency.go::agencyBannedPhrases). Kept here as well so a violation is caught before it
// crosses the network, and so a failing run says "the agents produced prescriptive language"
// locally rather than only as a remote 400.
var bannedPhrases = []*regexp.Regexp{
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

// scanArtifactForBannedLanguage returns the offending fragment, or "".
//
// It scans ONLY agent-authored text (see authoredStrings). The owner's question and a source's
// title, publisher and URL are quoted subject matter: "Is the sell-side price target justified?" is
// a legitimate question and "Analyst raises price target on NVDA" is a real headline. An agent may
// REPORT that a third party issued a rating; it may not issue one itself.
func scanArtifactForBannedLanguage(a *Artifact) string {
	for _, text := range authoredStrings(a) {
		normalised := strings.Join(strings.Fields(text), " ")
		for _, re := range bannedPhrases {
			if m := re.FindString(normalised); m != "" {
				return m
			}
		}
	}
	return ""
}

// artifactStrings flattens EVERY free-text string in the artifact, quoted and authored alike. It is
// what the LEAK scan walks: a credential or a home path is a disclosure wherever it appears,
// including in a citation title.
//
// It mirrors the server's `agencyArtifactStrings` field for field: a field present in one flattener
// and missing from the other is a hole in whichever scan forgot it, so the two lists are kept
// identical on purpose.
func artifactStrings(a *Artifact) []string {
	if a == nil {
		return nil
	}
	var out []string
	add := func(vs ...string) { out = append(out, vs...) }
	addFindings := func(fs []Finding) {
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

// authoredStrings flattens only what the AGENTS COMPOSED — the subset the prescriptive-language
// scan walks.
//
// EXCLUDED, deliberately: `Question` (the owner typed it) and each source's `Title`, `URL` and
// `Publisher` (a third party published them).
//
// KEEP IN STEP WITH journal/agency.go's `agencyAuthoredStrings`, which excludes the same four
// fields. The server runs the same split, so a worker that scanned a wider set would simply refuse
// artifacts the server accepts.
func authoredStrings(a *Artifact) []string {
	if a == nil {
		return nil
	}
	var out []string
	add := func(vs ...string) { out = append(out, vs...) }
	addFindings := func(fs []Finding) {
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
