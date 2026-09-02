package main

import (
	"strings"
	"testing"
)

// redact_test.go — the worker half of the privacy rule.

// ─────────────────────────────────────────────────────────────────────────────────────────────
// THE PRIVACY TABLE. KEEP IT IDENTICAL IN journal/agency_test.go AND bridge/redact_test.go.
//
// The two modules carry the same rule and must therefore be held to the same cases. These are the
// cases; each side runs them against its own copy of the pattern list, so a change to one that is
// not mirrored in the other fails on the side that was not updated.
//
// The ACCEPT half is the more important half. An earlier version of this scan rejected any artifact
// mentioning OpenAI, Anthropic or Qwen — which, for a tool pointed at semiconductor and software
// companies, would refuse most of the research worth having. A model vendor is a customer, a
// competitor, a counterparty; naming one is subject matter. What must never travel is what this
// RUN did on the owner's machine.
// ─────────────────────────────────────────────────────────────────────────────────────────────

// legitimateResearch must all be ACCEPTED. Every line is something a real analyst would write.
var legitimateResearch = []string{
	"OpenAI is a major customer of the issuer and accounted for a material share of Q3 demand.",
	"Anthropic's disclosed compute spend was cited by management on the earnings call.",
	"The company competes with Qwen-based offerings in the Chinese market.",
	"Data-centre revenue is driven by orders from OpenAI, Anthropic and Meta.",
	"Management said the business model: subscription-first, with usage-based overage.",
	"The filing lists its cloud provider: AWS, with a secondary region on Azure.",
	"Gross margin improved on GPT-4-class inference demand.",
	"Llama-derived open models are cited as a competitive risk in Item 1A.",
	"Junction temperature: 85C under sustained load, per the datasheet.",
	"The issuer reported total cost of revenue of $4.1bn.",
	"DeepSeek's pricing was cited as a factor in the guidance revision.",
	"The API charges $15 per million output tokens, versus $10 a year ago.",
	"Total cost of revenue rose 12% while inference cost per query fell.",
}

// operationalDisclosure must all be REFUSED. Every line is something only the owner's own machine
// could have produced, and none of it is research.
var operationalDisclosure = []string{
	"read the notes from /Users/someone/research/notes.md",
	"loaded from /home/someone/.config/thing",
	"config came from ~/.hermes/config.yaml",
	"credentials are in auth.json",
	"used key sk-abcdefghijklmnopqrstuvwxyz012345",
	"fetched from https://user:hunter2@internal.example.com/x",
	"model_used: qwen2.5:7b",
	"session_id: 9f2c41ab",
	"quantization: q8_0",
	"reasoning_effort: high",
	"provider: openrouter",
	"model: anthropic/claude-sonnet-4.6",
	"this analysis was generated with a local model",
	"served by ollama on this machine",
	"prompt tokens: 4,120 and completion tokens: 890",
	"tokens used: 12,003",
	"estimated cost: $0.42",
	"see the usage report for details",
	"my subscription quota was exhausted mid-run",
	"a ChatGPT Plus subscription was used for this",
}

func TestLegitimateResearchAboutModelVendorsIsAccepted(t *testing.T) {
	// The regression this exists to prevent: a scan so blunt that it refuses the research the tool
	// is for. Each line goes through the artifact scanner in the field an agent would put it in.
	for _, text := range legitimateResearch {
		a := &Artifact{
			AsOf:   testAsOf,
			Chair:  Chair{Conclusion: "fine"},
			Stages: []Stage{{Findings: []Finding{{Statement: text, Provenance: provenanceUnknown}}}},
		}
		if reason := scanArtifactForLeaks(a); reason != "" {
			t.Fatalf("legitimate research was refused as %q:\n  %s", reason, text)
		}
	}
}

func TestOperationalDisclosureIsRefused(t *testing.T) {
	for _, text := range operationalDisclosure {
		a := &Artifact{
			AsOf:   testAsOf,
			Chair:  Chair{Conclusion: "fine"},
			Stages: []Stage{{Notes: []string{text}}},
		}
		if reason := scanArtifactForLeaks(a); reason == "" {
			t.Fatalf("operational disclosure was allowed to be uploaded:\n  %s", text)
		}
	}
}

func TestTheScanReachesEveryFreeTextFieldOfTheArtifact(t *testing.T) {
	// A scan that only looked at stage notes would be evaded by putting the disclosure in a thesis
	// statement. Each case plants the same string in a different field.
	const leak = "model_used: qwen2.5:7b"
	cases := map[string]func(*Artifact){
		"a stage note":         func(a *Artifact) { a.Stages = []Stage{{Notes: []string{leak}}} },
		"a stage finding":      func(a *Artifact) { a.Stages = []Stage{{Findings: []Finding{{Statement: leak}}}} },
		"a thesis statement":   func(a *Artifact) { a.Thesis.Statement = leak },
		"thesis support":       func(a *Artifact) { a.Thesis.Support = []Finding{{Statement: leak}} },
		"an anti-thesis":       func(a *Artifact) { a.AntiThesis.Statement = leak },
		"a risk finding":       func(a *Artifact) { a.RiskFindings = []Finding{{Statement: leak}} },
		"a calculation basis":  func(a *Artifact) { a.RiskFindings = []Finding{{Basis: leak}} },
		"the chair conclusion": func(a *Artifact) { a.Chair.Conclusion = leak },
		"a key risk":           func(a *Artifact) { a.Chair.KeyRisks = []string{leak} },
		"an unresolved item":   func(a *Artifact) { a.UnresolvedQuestions = []string{leak} },
		"a veto reason":        func(a *Artifact) { a.Veto.Reasons = []string{leak} },
		"a contradiction":      func(a *Artifact) { a.Contradictions = []Contradiction{{Statement: leak}} },
		"a source title":       func(a *Artifact) { a.Sources = []Source{{Title: leak}} },
		"a degraded note":      func(a *Artifact) { a.Degraded = []string{leak} },
	}
	for name, plant := range cases {
		t.Run(name, func(t *testing.T) {
			a := &Artifact{AsOf: testAsOf, Chair: Chair{Conclusion: "fine"}}
			plant(a)
			if reason := scanArtifactForLeaks(a); reason == "" {
				t.Fatalf("a disclosure planted in %s was not caught", name)
			}
		})
	}
}

func TestTheRedactorScrubsWhatTheScannerRefuses(t *testing.T) {
	// The scanner REFUSES an artifact; the redactor SCRUBS an error message. Both must handle the
	// same shapes, because a refusal whose reason quotes the secret has published it.
	cases := []struct{ in, mustNotContain string }{
		{"failed at /Users/someone/projects/x", "someone"},
		{"could not read ~/.hermes/auth.json", "auth.json"},
		{"key was sk-abcdefghijklmnopqrstuvwxyz012345", "sk-abcdefghijklmnop"},
		{"api_key=super-secret-value", "super-secret-value"},
		{"authorization: Bearer abcdef123456", "abcdef123456"},
		{"postgres://user:hunter2@host/db", "hunter2"},
		{"x-worker-token: 0123456789abcdef0123456789abcdef", "0123456789abcdef"},
	}
	for _, tc := range cases {
		if got := redact(tc.in); strings.Contains(got, tc.mustNotContain) {
			t.Fatalf("redaction of %q left %q in: %q", tc.in, tc.mustNotContain, got)
		}
	}
}
