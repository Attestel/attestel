package main

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"
)

// analytics_test.go — the event contract itself (PARALLEL_CONTRACTS.md §6), tested without HTTP.
//
// The end-to-end path is covered in onboarding_test.go. What is tested here is the part that must
// hold for EVERY future call site: the catalog is closed, the property set is an allow-list, free
// text cannot ride in through a permitted field, identity is always hashed, the event id is
// deterministic, and the readout computes what it claims to compute.

func testSink(t *testing.T) *analyticsSink {
	t.Helper()
	s := openAnalyticsSink(t.TempDir(), "unit-salt", []string{"demo-user"})
	fixed := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return fixed }
	return s
}

// ---- the closed catalog ----

func TestCatalogHasExactlyFourteenEvents(t *testing.T) {
	if len(analyticsCatalog) != 14 {
		t.Fatalf("the event catalog is a CLOSED set of 14 (§6.2); found %d", len(analyticsCatalog))
	}
	for name, entry := range analyticsCatalog {
		if entry.Event != name {
			t.Errorf("catalog key %q does not match its entry %q", name, entry.Event)
		}
		if !validAnalyticsStage[entry.Stage] {
			t.Errorf("%s: stage %q is not a research-loop stage", name, entry.Stage)
		}
		// Every event must name the durable write that authorises it and the metric it feeds —
		// §6.2's own definition of what may be instrumented at all.
		if strings.TrimSpace(entry.Authority) == "" {
			t.Errorf("%s: no authoritative durable write recorded", name)
		}
		if strings.TrimSpace(entry.Metric) == "" {
			t.Errorf("%s: no product metric recorded", name)
		}
		if strings.TrimSpace(entry.ObjectType) == "" {
			t.Errorf("%s: no object type recorded", name)
		}
	}
}

func TestUnknownEventNameIsNeverWritten(t *testing.T) {
	s := testSink(t)
	if _, ok := s.build(eventInput{Event: "thesis_opened", UID: "u1", ObjectID: "abc0000000000000"}); ok {
		t.Fatal("an event outside the closed catalog was accepted")
	}
	if _, ok := s.build(eventInput{Event: "", UID: "u1"}); ok {
		t.Fatal("an unnamed event was accepted")
	}
}

// ---- the property allow-list ----

// The serialized event must carry EXACTLY the §6.3 property set — no more (a leak) and no fewer
// (a metric that cannot be computed). This is the test that fails if someone adds a field.
func TestEventCarriesExactlyTheAllowListedProperties(t *testing.T) {
	s := testSink(t)
	ev, ok := s.build(eventInput{
		Event: evEvidenceLinked, UID: "u1", TransitionKey: "contradicts",
		Ticker: "NVDA", ObjectID: "lnk_0123456789abcdef", ThesisID: "9f2c1b7a4e0d5a83",
		Bearing: "contradicts", Kind: "news", Provider: "marketaux", DataState: "live",
	})
	if !ok {
		t.Fatal("a valid event was rejected")
	}
	blob, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	want := []string{
		"at", "actor", "bearing", "closeReason", "dataState", "durationMs", "event", "eventId",
		"intent", "kind", "lensId", "level", "objectId", "objectType", "provider", "rating",
		"reasoningQuality", "schemaVersion", "stage", "synthetic", "task", "thesisId", "ticker",
	}
	sort.Strings(want)
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("property set drifted from §6.3\n got: %v\nwant: %v", keys, want)
	}

	actor, _ := got["actor"].(map[string]any)
	if len(actor) != 2 || actor["type"] == nil || actor["idHash"] == nil {
		t.Fatalf("actor must be exactly {type, idHash}, got %v", actor)
	}
}

// A value outside a closed vocabulary is DROPPED, not stored. This is what stops a permitted field
// from becoming a channel for free text.
func TestNonVocabularyValuesAreDropped(t *testing.T) {
	s := testSink(t)
	ev, ok := s.build(eventInput{
		Event: evEvidenceLinked, UID: "u1", ObjectID: "lnk_0123456789abcdef",
		Bearing:     "the user thinks this weakens the attach-rate assumption",
		Intent:      "buy 100 shares",
		CloseReason: "because I changed my mind about the datacenter cycle",
		Kind:        "a headline I read this morning",
		Provider:    "https://news.example.com/article?utm_source=x",
		DataState:   "probably live",
		Rating:      "it was quite good actually",
		Task:        "read the transcript and tell me what to do",
		LensID:      "Devil's Advocate, but harsher",
	})
	if !ok {
		t.Fatal("event rejected")
	}
	for name, got := range map[string]*string{
		"bearing": ev.Bearing, "intent": ev.Intent, "closeReason": ev.CloseReason,
		"kind": ev.Kind, "provider": ev.Provider, "dataState": ev.DataState,
		"rating": ev.Rating, "task": ev.Task, "lensId": ev.LensID,
	} {
		if got != nil {
			t.Errorf("%s kept a non-vocabulary value %q — free text reached the log", name, *got)
		}
	}
}

// An id-shaped field only accepts an id shape. A claim, an email, or a URL is dropped whole.
func TestIdFieldsRejectAnythingThatIsNotAnID(t *testing.T) {
	s := testSink(t)
	ev, ok := s.build(eventInput{
		Event:    evThesisCreated,
		UID:      "u1",
		ObjectID: "Networking attach lifts blended ASPs through FY27",
		ThesisID: "user@example.com",
		Ticker:   "the company I am researching this week",
	})
	if !ok {
		t.Fatal("event rejected")
	}
	if ev.ObjectID != "" {
		t.Errorf("objectId kept prose: %q", ev.ObjectID)
	}
	if ev.ThesisID != "" {
		t.Errorf("thesisId kept an email address: %q", ev.ThesisID)
	}
	if ev.Ticker != "" {
		t.Errorf("ticker kept prose: %q", ev.Ticker)
	}

	// And the shapes that ARE ids survive intact.
	ev, _ = s.build(eventInput{
		Event: evThesisCreated, UID: "u1",
		ObjectID: "9f2c1b7a4e0d5a83", ThesisID: "9f2c1b7a4e0d5a83", Ticker: "nvda",
	})
	if ev.ObjectID != "9f2c1b7a4e0d5a83" || ev.ThesisID != "9f2c1b7a4e0d5a83" {
		t.Errorf("a real id was dropped: %+v", ev)
	}
	if ev.Ticker != "NVDA" {
		t.Errorf("ticker must be normalized to upper case, got %q", ev.Ticker)
	}
}

func TestLensIDAcceptsOnlyBuiltinOrPersona(t *testing.T) {
	for _, in := range []string{"builtin:skeptic", "persona:4b81e0c9d7a25f36"} {
		if got := analyticsLensID(in); got != in {
			t.Errorf("valid lens id %q was dropped", in)
		}
	}
	for _, in := range []string{
		"skeptic", "custom:mine", "persona:you are a ruthless short seller", "builtin:",
		"persona:" + strings.Repeat("x", 200),
	} {
		if got := analyticsLensID(in); got != "" {
			t.Errorf("lens id %q should have been dropped, got %q", in, got)
		}
	}
}

// ---- identity ----

func TestIdentityIsAlwaysHashed(t *testing.T) {
	s := testSink(t)

	user, ok := s.build(eventInput{Event: evThesisCreated, UID: "u-12345", ObjectID: "9f2c1b7a4e0d5a83"})
	if !ok {
		t.Fatal("signed-in event rejected")
	}
	if user.Actor.Type != "user" {
		t.Errorf("actor.type = %q, want user", user.Actor.Type)
	}
	if user.Actor.IDHash == "u-12345" {
		t.Fatal("the raw uid reached the event log")
	}
	if len(user.Actor.IDHash) != 16 {
		t.Errorf("idHash must be 16 hex chars, got %q", user.Actor.IDHash)
	}

	guest, ok := s.build(eventInput{Event: evOnboardingStarted, GuestSession: "sess-abc", ObjectID: "x"})
	if !ok {
		t.Fatal("guest event rejected")
	}
	if guest.Actor.Type != "guest" || guest.Actor.IDHash == "sess-abc" {
		t.Fatalf("guest identity leaked or mistyped: %+v", guest.Actor)
	}
	if guest.Actor.IDHash == user.Actor.IDHash {
		t.Fatal("a guest session and a uid hashed to the same identity")
	}

	// An event that can be attributed to nobody is not measurement.
	if _, ok := s.build(eventInput{Event: evOnboardingStarted, ObjectID: "x"}); ok {
		t.Fatal("an unattributable event was accepted")
	}
	// The unattributed pre-accounts bucket is nobody's activity (§1.10).
	if _, ok := s.build(eventInput{Event: evThesisCreated, UID: legacyBucket, ObjectID: "x"}); ok {
		t.Fatal("_legacy activity was measured")
	}
}

// ---- deduplication ----

func TestEventIDIsDeterministicPerTransition(t *testing.T) {
	a := analyticsEventID(evThesisActivated, "9f2c1b7a4e0d5a83", "active")
	b := analyticsEventID(evThesisActivated, "9f2c1b7a4e0d5a83", "active")
	if a != b {
		t.Fatal("the same transition produced two ids — a replay would double-count")
	}
	if len(a) != 32 {
		t.Fatalf("eventId must be 32 hex chars, got %d", len(a))
	}
	if a == analyticsEventID(evThesisActivated, "9f2c1b7a4e0d5a83", "closed") {
		t.Fatal("a different transition collided with the previous one")
	}
	if a == analyticsEventID(evThesisActivated, "0000000000000000", "active") {
		t.Fatal("a different object collided with the previous one")
	}
	if a == analyticsEventID(evThesisCreated, "9f2c1b7a4e0d5a83", "active") {
		t.Fatal("a different event name collided with the previous one")
	}
}

// ---- synthetic / demo ----

func TestSyntheticFlagSources(t *testing.T) {
	s := testSink(t)

	fromData, _ := s.build(eventInput{Event: evEvidenceSaved, UID: "u1", ObjectID: "ev_1", DataState: "synthetic"})
	if !fromData.Synthetic {
		t.Error("dataState synthetic did not set the exclusion flag")
	}
	explicit, _ := s.build(eventInput{Event: evOutcomeReviewed, UID: "u1", ObjectID: "out_1", Synthetic: true})
	if !explicit.Synthetic {
		t.Error("an explicit synthetic signal from the call site was ignored")
	}
	demo, _ := s.build(eventInput{Event: evThesisCreated, UID: "demo-user", ObjectID: "abc0000000000000", DataState: "live"})
	if !demo.Synthetic {
		t.Error("a server-flagged demo account was not excluded")
	}
	real, _ := s.build(eventInput{Event: evThesisCreated, UID: "u1", ObjectID: "abc0000000000000", DataState: "live"})
	if real.Synthetic {
		t.Error("live activity by a real user was wrongly flagged")
	}
}

// ---- the sink never breaks a user's write ----

func TestEmitNeverFailsTheCallerEvenWhenBroken(t *testing.T) {
	var nilSink *analyticsSink
	nilSink.emit(eventInput{Event: evThesisCreated, UID: "u1", ObjectID: "abc0000000000000"}) // must not panic

	s := testSink(t)
	s.dir = "/nonexistent-directory-for-analytics"
	s.emit(eventInput{Event: evThesisCreated, UID: "u1", ObjectID: "abc0000000000000"}) // must not panic
}

// ---- the readout ----

// A controlled fixture with known answers. Every number below is computed by hand from the events
// constructed here, so a change in the aggregation logic fails loudly instead of drifting.
func TestReadoutMatchesAControlledFixture(t *testing.T) {
	base := time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC) // a Monday: ISO 2026-W02
	day := int64(86400)
	at := func(offset int64) int64 { return base.Unix() + offset }

	n := 0
	mk := func(event, actor string, ts int64, mods func(*analyticsEvent)) analyticsEvent {
		n++
		ev := analyticsEvent{
			Event:         event,
			EventID:       event + "-" + actor + "-" + itoa(n),
			At:            ts,
			SchemaVersion: analyticsSchemaVersion,
			Actor:         analyticsActor{Type: "user", IDHash: actor},
			Stage:         analyticsCatalog[event].Stage,
			ObjectType:    analyticsCatalog[event].ObjectType,
		}
		if mods != nil {
			mods(&ev)
		}
		return ev
	}
	rating := func(v string) func(*analyticsEvent) {
		return func(e *analyticsEvent) { e.Rating = &v }
	}
	onThesis := func(id string) func(*analyticsEvent) {
		return func(e *analyticsEvent) { e.ThesisID = id }
	}

	events := []analyticsEvent{
		// a1 starts, activates after two hours, and keeps coming back.
		mk(evOnboardingStarted, "a1", at(0), nil),
		mk(evOnboardingCompleted, "a1", at(7200), nil),
		mk(evChallengeReviewCompleted, "a1", at(7200), onThesis("t1")),
		mk(evThesisReviewed, "a1", at(8*day), onThesis("t1")),
		mk(evDecisionRecorded, "a1", at(22*day), nil),
		mk(evOutcomeReviewed, "a1", at(23*day), nil),
		// a2 starts and never activates.
		mk(evOnboardingStarted, "a2", at(0), nil),
		// a3 activates in an hour and is never seen again.
		mk(evOnboardingStarted, "a3", at(60), nil),
		mk(evOnboardingCompleted, "a3", at(3660), nil),
		mk(evChallengeReviewCompleted, "a3", at(3660), onThesis("t3")),
		mk(evDecisionRecorded, "a3", at(3700), nil),
		// quality signals
		mk(evReviewRated, "a1", at(7300), rating("useful")),
		mk(evReviewRated, "a3", at(3700), rating("useful")),
		mk(evReviewRated, "a2", at(400), rating("not_useful")),
		mk(evCitationFeedback, "a1", at(7301), rating("accepted")),
		mk(evCitationFeedback, "a1", at(7302), rating("accepted")),
		mk(evCitationFeedback, "a3", at(3701), rating("accepted")),
		mk(evCitationFeedback, "a3", at(3702), rating("rejected")),
	}
	// One synthetic row, and one exact replay of an earlier event.
	synthetic := mk(evEvidenceSaved, "a4", at(100), func(e *analyticsEvent) { e.Synthetic = true })
	events = append(events, synthetic, events[2])

	now := base.Add(100 * 24 * time.Hour)
	r := buildReadout(events, readoutOptions{Now: now})

	if r.TotalEvents != 20 {
		t.Errorf("TotalEvents = %d, want 20", r.TotalEvents)
	}
	if r.DuplicateEvents != 1 {
		t.Errorf("DuplicateEvents = %d, want 1", r.DuplicateEvents)
	}
	if r.SyntheticEvents != 1 {
		t.Errorf("SyntheticEvents = %d, want 1", r.SyntheticEvents)
	}
	if r.CountedEvents != 18 {
		t.Errorf("CountedEvents = %d, want 18 (20 - 1 replay - 1 synthetic)", r.CountedEvents)
	}

	// North star: two distinct theses reviewed in W02, one in W03.
	want := map[string]int{"2026-W02": 2, "2026-W03": 1}
	if len(r.WeeklyThesesReviewed) != len(want) {
		t.Fatalf("weekly rows = %v, want %v", r.WeeklyThesesReviewed, want)
	}
	for _, row := range r.WeeklyThesesReviewed {
		if want[row.Week] != row.Theses {
			t.Errorf("%s: %d theses reviewed, want %d", row.Week, row.Theses, want[row.Week])
		}
	}

	if r.StartedActors != 3 || r.ActivatedActors != 2 {
		t.Errorf("funnel = %d started / %d activated, want 3/2", r.StartedActors, r.ActivatedActors)
	}
	if pct(r.ActivatedActors, r.StartedActors) != "66.7%" {
		t.Errorf("activation conversion = %s, want 66.7%%", pct(r.ActivatedActors, r.StartedActors))
	}
	if r.MedianTimeToActivationSec != 5400 {
		t.Errorf("median time to activation = %ds, want 5400 (mean of 3600 and 7200)", r.MedianTimeToActivationSec)
	}
	if r.ActivationSamples != 2 {
		t.Errorf("activation samples = %d, want 2", r.ActivationSamples)
	}

	wantRetention := map[int][2]int{2: {1, 2}, 4: {1, 2}, 8: {0, 2}, 12: {0, 2}}
	for _, b := range r.Retention {
		w := wantRetention[b.Week]
		if b.Returned != w[0] || b.Eligible != w[1] {
			t.Errorf("week %d retention = %d/%d, want %d/%d", b.Week, b.Returned, b.Eligible, w[0], w[1])
		}
	}

	if r.DepthTheses != 2 {
		t.Errorf("depth covered %d theses, want 2", r.DepthTheses)
	}
	for _, d := range r.Depth {
		if d.Label == "1 item" && d.Theses != 2 {
			t.Errorf("depth bucket %q = %d, want 2", d.Label, d.Theses)
		}
		if d.Label != "1 item" && d.Theses != 0 {
			t.Errorf("depth bucket %q = %d, want 0", d.Label, d.Theses)
		}
	}

	if r.DecisionsRecorded != 2 || r.OutcomesReviewed != 1 {
		t.Errorf("decision loop = %d/%d, want 2/1", r.OutcomesReviewed, r.DecisionsRecorded)
	}
	if pct(r.OutcomesReviewed, r.DecisionsRecorded) != "50.0%" {
		t.Errorf("decision-loop completion = %s, want 50.0%%", pct(r.OutcomesReviewed, r.DecisionsRecorded))
	}
	if r.RatedUseful != 2 || r.RatedNotUseful != 1 {
		t.Errorf("usefulness = %d useful / %d not useful, want 2/1", r.RatedUseful, r.RatedNotUseful)
	}
	if r.CitesAccepted != 3 || r.CitesRejected != 1 {
		t.Errorf("citations = %d accepted / %d rejected, want 3/1", r.CitesAccepted, r.CitesRejected)
	}

	// Including synthetic rows changes the totals and says so.
	inc := buildReadout(events, readoutOptions{Now: now, IncludeSynthetic: true})
	if inc.CountedEvents != 19 {
		t.Errorf("with synthetic included, CountedEvents = %d, want 19", inc.CountedEvents)
	}
	out := renderReadout(r)
	if !strings.Contains(out, "EXCLUDING synthetic/demo activity") {
		t.Error("the readout must state whether synthetic/demo activity was excluded")
	}
	if !strings.Contains(renderReadout(inc), "INCLUDING synthetic/demo activity") {
		t.Error("the inclusive readout must say so too")
	}
	if !strings.Contains(out, "RETENTION of ACTIVATED users") {
		t.Error("retention must be reported from activated users, not all signups")
	}
}
