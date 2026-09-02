package main

import (
	"strings"
	"testing"
)

// stage_test.go — a stage's output is untrusted input, and this is where that is proved.
//
// Every case below is a thing a compromised, confused or prompt-injected agent could emit. The
// assertion is always the same: it is REFUSED, not repaired.

func decodeResearch(t *testing.T, stdout string) (researchOutput, error) {
	t.Helper()
	var out researchOutput
	err := decodeStage(stdout, &out)
	return out, err
}

func TestAStageOutputWithAnUndeclaredFieldIsRefused(t *testing.T) {
	// Closed schemas are the structural defence. A field we do not know about is a field we cannot
	// reason about, and "carry it along in case it matters" is how a signal field arrives.
	cases := []string{
		`{"sources":[],"findings":[],"notes":[],"unresolvedQuestions":[],"direction":"Buy"}`,
		`{"sources":[],"findings":[],"notes":[],"unresolvedQuestions":[],"priceTarget":180}`,
		`{"sources":[],"findings":[],"notes":[],"unresolvedQuestions":[],"modelUsed":"some-model"}`,
		`{"sources":[],"findings":[],"notes":[],"unresolvedQuestions":[],"estimatedCost":0.42}`,
	}
	for _, tc := range cases {
		if _, err := decodeResearch(t, tc); err == nil {
			t.Fatalf("a stage output carrying an undeclared field was accepted: %s", tc)
		}
	}
}

func TestJSONIsExtractedFromAroundConversationalPadding(t *testing.T) {
	// Tolerant of the shape a model actually produces; not tolerant of its content.
	out, err := decodeResearch(t, "Here is the result:\n```json\n"+
		`{"sources":[],"findings":[],"notes":["ok"],"unresolvedQuestions":[]}`+"\n```\nHope that helps.")
	if err != nil {
		t.Fatalf("padded JSON was not extracted: %v", err)
	}
	if len(out.Notes) != 1 || out.Notes[0] != "ok" {
		t.Fatalf("extraction lost content: %+v", out)
	}
}

func TestABraceInsideAStringDoesNotTerminateExtraction(t *testing.T) {
	// A citation title containing a brace is ordinary. Terminating the scan on it would truncate
	// the document and turn a valid artifact into a schema failure.
	body := `{"sources":[],"findings":[],"notes":["a title with a } brace and a \" quote"],"unresolvedQuestions":[]}`
	out, err := decodeResearch(t, body)
	if err != nil {
		t.Fatalf("a brace inside a string broke extraction: %v", err)
	}
	if len(out.Notes) != 1 {
		t.Fatalf("notes = %v", out.Notes)
	}
}

func TestNonJSONAndUnterminatedJSONAreRefused(t *testing.T) {
	if _, err := decodeResearch(t, "I could not complete this task."); err == nil {
		t.Fatal("prose with no JSON was accepted")
	}
	if _, err := decodeResearch(t, `{"sources":[`); err == nil {
		t.Fatal("an unterminated object was accepted")
	}
}

func TestCitationsMustResolveToDeclaredSources(t *testing.T) {
	// "Missing citations" is a fail-closed condition. A citation pointing at nothing would make the
	// provenance labels decorative — which is worse than no labels, because they look like proof.
	table := newSourceTable()
	if err := registerSources([]rawSource{{
		Title: "Filing", URL: "https://www.sec.gov/x", PublishedAt: "2026-08-01",
	}}, table); err != nil {
		t.Fatal(err)
	}
	if _, err := convertFindings([]rawFinding{{
		Statement: "x", Provenance: provenanceSourced,
		SourceURLs: []string{"https://example.com/never-declared"},
	}}, table); err == nil {
		t.Fatal("a finding citing an undeclared source was accepted")
	}
	if _, err := convertFindings([]rawFinding{{
		Statement: "x", Provenance: provenanceSourced,
		SourceURLs: []string{"https://www.sec.gov/x"},
	}}, table); err != nil {
		t.Fatalf("a finding citing a declared source was refused: %v", err)
	}
}

func TestProvenanceRulesAreEnforcedPerLabel(t *testing.T) {
	table := newSourceTable()
	_ = registerSources([]rawSource{{
		Title: "Filing", URL: "https://www.sec.gov/x", PublishedAt: "2026-08-01",
	}}, table)
	good := "https://www.sec.gov/x"

	cases := []struct {
		name    string
		finding rawFinding
		wantErr string
	}{
		{"sourced with no citation",
			rawFinding{Statement: "x", Provenance: provenanceSourced}, "cites nothing"},
		{"sourced with a basis",
			rawFinding{Statement: "x", Provenance: provenanceSourced, SourceURLs: []string{good}, Basis: "1+1"},
			"carries a calculation basis"},
		{"calculated with no basis",
			rawFinding{Statement: "x", Provenance: provenanceCalculated, SourceURLs: []string{good}},
			"shows no basis"},
		{"calculated with no inputs",
			rawFinding{Statement: "x", Provenance: provenanceCalculated, Basis: "1+1"},
			"cites no inputs"},
		{"inferred with a basis",
			rawFinding{Statement: "x", Provenance: provenanceInferred, Basis: "1+1"},
			"carries a calculation basis"},
		{"an invented label",
			rawFinding{Statement: "x", Provenance: "probably"}, "provenance"},
		{"an empty statement",
			rawFinding{Statement: "  ", Provenance: provenanceUnknown}, "no statement"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := convertFindings([]rawFinding{tc.finding}, table)
			if err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("refusal says %q, expected it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestUnknownIsAlwaysAcceptableAndCostsNothing(t *testing.T) {
	// The whole point of the label: an agent must be able to say "I could not establish this"
	// without needing a citation it does not have. If `unknown` were expensive, agents would avoid
	// it, and the schema would be manufacturing confidence.
	table := newSourceTable()
	out, err := convertFindings([]rawFinding{{
		Statement: "The driver was not established.", Provenance: provenanceUnknown,
	}}, table)
	if err != nil {
		t.Fatalf("an unknown finding with no citation was refused: %v", err)
	}
	if len(out) != 1 || out[0].Provenance != provenanceUnknown {
		t.Fatalf("unknown finding did not survive conversion: %+v", out)
	}
}

func TestSourceURLsAreValidatedAndDeduplicated(t *testing.T) {
	table := newSourceTable()
	for _, bad := range []string{"notaurl", "ftp://example.com/x", "https://user:pw@example.com/x", "https://"} {
		if err := registerSources([]rawSource{{Title: "t", URL: bad, PublishedAt: "unknown"}}, table); err == nil {
			t.Fatalf("source url %q was accepted", bad)
		}
	}
	// The same page cited twice, spelled differently, is one source.
	if err := registerSources([]rawSource{
		{Title: "A", URL: "https://Example.com/Path/", PublishedAt: "unknown"},
		{Title: "B", URL: "https://example.com/Path", PublishedAt: "unknown"},
	}, table); err != nil {
		t.Fatal(err)
	}
	if len(table.sources) != 1 {
		t.Fatalf("the same page produced %d sources, want 1", len(table.sources))
	}
	if table.sources[0].Title != "A" {
		t.Fatalf("a later stage rewrote an earlier citation: %q", table.sources[0].Title)
	}
}

func TestASourceDateIsEitherRealOrExplicitlyUnknown(t *testing.T) {
	// An undated source is a fact about the source. Inventing a date to satisfy the schema is the
	// dishonesty this whole lane is built to avoid.
	table := newSourceTable()
	if err := registerSources([]rawSource{{
		Title: "t", URL: "https://example.com/a", PublishedAt: "unknown",
	}}, table); err != nil {
		t.Fatalf(`"unknown" was refused as a publication date: %v`, err)
	}
	if err := registerSources([]rawSource{{
		Title: "t", URL: "https://example.com/b", PublishedAt: "last tuesday",
	}}, table); err == nil {
		t.Fatal("a made-up publication date was accepted")
	}
}

func TestTheSourceCapIsEnforced(t *testing.T) {
	table := newSourceTable()
	for i := 0; i <= maxSources; i++ {
		err := registerSources([]rawSource{{
			Title: "t", URL: "https://example.com/" + itoa(i), PublishedAt: "unknown",
		}}, table)
		if i < maxSources && err != nil {
			t.Fatalf("source %d was refused early: %v", i, err)
		}
		if i == maxSources && err == nil {
			t.Fatalf("the %dth source was accepted; the cap is %d", i+1, maxSources)
		}
	}
}

func TestTheRiskStageCannotEmitADirection(t *testing.T) {
	// The risk stage is the one most likely to want to say "sell". Its schema has nowhere to put it.
	var out riskOutput
	err := decodeStage(`{"sources":[],"findings":[],"notes":[],"unresolvedQuestions":[],
		"contradictions":[],"veto":{"raised":false,"reasons":[]},"direction":"Sell"}`, &out)
	if err == nil {
		t.Fatal("the risk stage was allowed to emit a direction")
	}
}

func TestTheChairPriorityVocabularyIsClosed(t *testing.T) {
	// investigate / watch / reject / unknown. Not a rating, and specifically not a fifth value that
	// reads like one.
	job := &Job{RunID: "r", WorkflowVersion: workflowCompanyResearch, Ticker: "NVDA",
		Question: "q", AsOf: "2026-09-02T00:00:00Z"}
	for _, bad := range []string{"strong_buy", "buy", "accumulate", "", "INVESTIGATE"} {
		_, err := assembleArtifact(job, newSourceTable(), nil, nil, Veto{Scope: vetoScope},
			chairOutput{ResearchPriority: bad, Conclusion: "c"},
			Position{Statement: "t"}, Position{Statement: "a"}, nil, nil, nowForTest())
		if err == nil {
			t.Fatalf("researchPriority %q was accepted", bad)
		}
	}
}

func TestTheFinalCheckRefusesPrescriptiveLanguageBeforeUpload(t *testing.T) {
	// Caught locally, so it never crosses the network. The server checks again on receipt.
	a := &Artifact{AsOf: testAsOf, Chair: Chair{Conclusion: "We recommend a buy at these levels."}}
	if err := finalCheck(a); err == nil {
		t.Fatal("prescriptive language was allowed to be uploaded")
	} else if !strings.Contains(err.Error(), "prescriptive") {
		t.Fatalf("unexpected refusal: %v", err)
	}
}

func TestTheFinalCheckRefusesLocalDetailBeforeUpload(t *testing.T) {
	cases := []struct{ note, want string }{
		{"read from /Users/someone/notes.md", "home path"},
		{"config in ~/.hermes/config.yaml", "Hermes state"},
		{"key sk-abcdefghijklmnopqrstuvwxyz01", "API-key"},
		{"this analysis was generated with a local model", "how this run was generated"},
		{"served by ollama on this machine", "local inference-runtime"},
		{"model_used: qwen2.5:7b", "local model or session configuration"},
		{"prompt tokens: 4,120", "token accounting"},
	}
	for _, tc := range cases {
		a := &Artifact{
			AsOf:   testAsOf,
			Chair:  Chair{Conclusion: "fine"},
			Stages: []Stage{{Notes: []string{tc.note}}},
		}
		err := finalCheck(a)
		if err == nil {
			t.Fatalf("%q was allowed to be uploaded", tc.note)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("refusal for %q says %q, expected it to mention %q", tc.note, err, tc.want)
		}
	}
}

// ───────────────────────────────────────────── quoted subject matter vs agent-authored output

func TestAQuestionOrCitationMayQuoteAnalystTerminology(t *testing.T) {
	// The owner's question and a source's title, publisher and URL are QUOTED SUBJECT MATTER.
	// Refusing them for containing "price target" would mean the tool cannot research the analyst
	// commentary that is half of what moves a stock, and would push agents into paraphrasing real
	// headlines — a worse outcome than quoting them.
	a := &Artifact{
		AsOf:     testAsOf,
		Question: "Is the sell-side price target justified by what the filings actually show?",
		Sources: []Source{{
			ID:          "s1",
			Title:       "Analyst raises price target on NVDA to $210, reiterates buy rating",
			URL:         "https://example.com/analyst-note",
			PublishedAt: "2026-08-30",
			Publisher:   "Buy Rating Weekly",
		}},
		Chair: Chair{Conclusion: "The filings do not settle the question."},
	}
	if fragment := scanArtifactForBannedLanguage(a); fragment != "" {
		t.Fatalf("a legitimate question or citation was refused for quoting %q", fragment)
	}
	if err := finalCheck(a); err != nil {
		t.Fatalf("an artifact quoting analyst terminology in its question and citation was "+
			"refused: %v", err)
	}
}

func TestAgentAuthoredPrescriptionIsStillRefused(t *testing.T) {
	// The other half: an agent may REPORT that a third party issued a rating; it may not issue one.
	// Every field here was composed by a stage.
	cases := map[string]func(*Artifact){
		"the chair conclusion": func(a *Artifact) {
			a.Chair.Conclusion = "We recommend a buy at these levels."
		},
		"a key risk": func(a *Artifact) {
			a.Chair.KeyRisks = []string{"Missing the entry point."}
		},
		"the thesis": func(a *Artifact) {
			a.Thesis.Statement = "Our price target is well above spot."
		},
		"the anti-thesis": func(a *Artifact) {
			a.AntiThesis.Statement = "The expected return does not justify it."
		},
		"an inferred finding": func(a *Artifact) {
			a.Stages = []Stage{{Findings: []Finding{{
				Statement: "You should buy before the print.", Provenance: provenanceInferred,
			}}}}
		},
		"a sourced finding": func(a *Artifact) {
			a.Stages = []Stage{{Findings: []Finding{{
				Statement: "We recommend a buy.", Provenance: provenanceSourced,
			}}}}
		},
		"a risk finding": func(a *Artifact) {
			a.RiskFindings = []Finding{{Statement: "Set a stop-loss below support."}}
		},
		"a veto reason": func(a *Artifact) {
			a.Veto.Reasons = []string{"the position size is wrong"}
		},
		"a stage note": func(a *Artifact) {
			a.Stages = []Stage{{Notes: []string{"rated a buy by the desk, and we agree"}}}
		},
	}
	for name, plant := range cases {
		t.Run(name, func(t *testing.T) {
			a := &Artifact{AsOf: testAsOf, Chair: Chair{Conclusion: "fine"}}
			plant(a)
			if fragment := scanArtifactForBannedLanguage(a); fragment == "" {
				t.Fatalf("prescriptive language in %s was accepted", name)
			}
		})
	}
}

func TestTheLeakScanStillCoversQuotedFieldsEvenThoughTheLanguageScanDoesNot(t *testing.T) {
	// The two scans have deliberately different scopes. A credential or a home path is a disclosure
	// wherever it appears — including in a citation title, which the language scan skips.
	a := &Artifact{
		AsOf:    testAsOf,
		Chair:   Chair{Conclusion: "fine"},
		Sources: []Source{{ID: "s1", Title: "notes from /Users/someone/research.md"}},
	}
	if reason := scanArtifactForLeaks(a); reason == "" {
		t.Fatal("a home path in a citation title was not caught")
	}
	a = &Artifact{
		AsOf:     testAsOf,
		Chair:    Chair{Conclusion: "fine"},
		Question: "why did model_used: qwen2.5:7b appear in the logs",
	}
	if reason := scanArtifactForLeaks(a); reason == "" {
		t.Fatal("operational configuration in the question was not caught")
	}
}

// ────────────────────────────────────────────────────── distinct claims, not repeated appearances

func TestOneClaimRepeatedAcrossStagesIsStillOneClaim(t *testing.T) {
	// The chair's thesis support properly repeats what the scout found, and the risk stage restates
	// what it is attacking. Counting those as separate evidence would let ONE sourced sentence,
	// echoed four times, clear a floor meant to say "this rests on more than one thing".
	const claim = "The issuer filed a 10-Q covering the period."
	sourced := Finding{Statement: claim, Provenance: provenanceSourced, SourceIDs: []string{"s1"}}

	a := &Artifact{
		AsOf:             testAsOf,
		ResearchPriority: "investigate",
		Sources:          []Source{{ID: "s1", Title: "10-Q", URL: "https://example.com/10q", PublishedAt: "2026-08-01"}},
		Stages: []Stage{
			{Profile: "stock-scout", Findings: []Finding{sourced}},
			{Profile: "stock-fundamentals", Findings: []Finding{sourced}},
		},
		Chair: Chair{Conclusion: "fine"},
	}

	grounded, total := groundedCoverage(a)
	if grounded != 1 || total != 1 {
		t.Fatalf("one claim stated twice counted as %d grounded of %d; it is one claim",
			grounded, total)
	}
	if err := checkCoverage(a); err == nil {
		t.Fatal("a repeated claim satisfied the two-grounded-findings floor")
	} else if !strings.Contains(err.Error(), "DISTINCT") {
		t.Fatalf("the refusal does not explain that appearances are not evidence: %v", err)
	}

	// Two genuinely different sourced claims clear it.
	second := Finding{
		Statement:  "Segment revenue was disclosed separately.",
		Provenance: provenanceSourced, SourceIDs: []string{"s1"},
	}
	a.Stages[1].Findings = []Finding{second}
	if grounded, total = groundedCoverage(a); grounded != 2 || total != 2 {
		t.Fatalf("two distinct claims counted as %d grounded of %d", grounded, total)
	}
	if err := checkCoverage(a); err != nil {
		t.Fatalf("two distinct grounded claims were refused: %v", err)
	}
}

func TestClaimDeduplicationIgnoresWhitespaceCaseAndTrailingPunctuation(t *testing.T) {
	// Conservative on purpose: it merges restatements, not paraphrases. A validator that tried to
	// merge paraphrases would be making a judgement it has no business making.
	same := []string{
		"Revenue rose 12%.",
		"revenue rose 12%",
		"Revenue  rose   12%!",
	}
	first := claimKey(same[0])
	for _, s := range same[1:] {
		if claimKey(s) != first {
			t.Fatalf("%q and %q were treated as different claims", same[0], s)
		}
	}
	if claimKey("Revenue rose 13%.") == first {
		t.Fatal("two different numbers were merged into one claim")
	}
}

func TestARestatementDoesNotUngroundASourcedClaim(t *testing.T) {
	// The same sentence may appear as `sourced` in one stage and `inferred` in another. It is one
	// claim, and it rests on a source.
	const claim = "Gross margin fell in the quarter."
	a := &Artifact{
		AsOf:    testAsOf,
		Sources: []Source{{ID: "s1"}},
		Stages: []Stage{
			{Findings: []Finding{{Statement: claim, Provenance: provenanceSourced, SourceIDs: []string{"s1"}}}},
			{Findings: []Finding{{Statement: claim, Provenance: provenanceInferred}}},
		},
	}
	if grounded, total := groundedCoverage(a); grounded != 1 || total != 1 {
		t.Fatalf("a sourced claim restated as inferred counted %d grounded of %d, want 1 of 1",
			grounded, total)
	}
}
