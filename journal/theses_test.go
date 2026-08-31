package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// theses_test.go — the migration and ownership gates for Thesis v2.
//
// Everything runs through the real HTTP surface (routes + auth + store + disk), because the
// properties under test are end-to-end ones: a legacy record must SURVIVE a round trip, and one user
// must not be able to touch another's record through any route. Deterministic, stdlib only, no
// network and no model.
//
// Fixtures live in testdata/theses/ and are the set MIGRATION_CONTRACT.md §9.3 requires.

const testSecret = "test-secret-not-a-real-one"

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "theses", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// seedUser installs a fixture as the given user's stored theses.json, exactly as a real deployment
// would have it on disk before this version of the binary ever ran.
func seedUser(t *testing.T, dir, uid, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, uid), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, uid, "theses.json"), fixture(t, name), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newTestServer builds a journal over `dir`. Call it twice on one dir to simulate a restart: the
// second server shares nothing but the files.
func newTestServer(t *testing.T, dir string) http.Handler {
	t.Helper()
	cfg := Config{TradesDir: dir, Secret: testSecret, CookieName: "nvda_session"}
	store, err := openStore(dir)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	theses, err := openThesisStore(dir)
	if err != nil {
		t.Fatalf("openThesisStore: %v", err)
	}
	srv := &Server{cfg: cfg, store: store, theses: theses, http: &http.Client{Timeout: time.Second}}
	return srv.routes()
}

// sessionFor mints the same signed cookie the auth service issues, so requests exercise the real
// verifier rather than a test-only bypass.
func sessionFor(uid string) *http.Cookie {
	p := sessionPayload{UID: uid, IAT: time.Now().Unix(), Exp: time.Now().Add(time.Hour).Unix()}
	raw, _ := json.Marshal(p)
	body := b64.EncodeToString(raw)
	return &http.Cookie{Name: "nvda_session", Value: body + "." + signPayload(testSecret, body)}
}

// do issues one request as `uid` ("" = a signed-out guest).
func do(t *testing.T, h http.Handler, uid, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if uid != "" {
		req.AddCookie(sessionFor(uid))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeThesis(t *testing.T, rec *httptest.ResponseRecorder) Thesis {
	t.Helper()
	var out struct {
		Thesis Thesis `json:"thesis"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode thesis: %v (body=%s)", err, rec.Body.String())
	}
	return out.Thesis
}

func decodeList(t *testing.T, rec *httptest.ResponseRecorder) []Thesis {
	t.Helper()
	var out struct {
		Theses []Thesis `json:"theses"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode theses: %v (body=%s)", err, rec.Body.String())
	}
	return out.Theses
}

func decodeErr(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	out := map[string]any{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode error body: %v (body=%s)", err, rec.Body.String())
	}
	return out
}

func wantStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d (body=%s)", rec.Code, want, rec.Body.String())
	}
}

// oneFrom loads a single-record fixture through a fresh server and returns it.
func oneFrom(t *testing.T, name string) Thesis {
	t.Helper()
	dir := t.TempDir()
	seedUser(t, dir, "userA", name)
	got := decodeList(t, do(t, newTestServer(t, dir), "userA", "GET", "/theses", ""))
	if len(got) != 1 {
		t.Fatalf("%s: got %d theses, want 1", name, len(got))
	}
	return got[0]
}

// ---- legacy compatibility ----

func TestLegacyRecordLoadsWithDeterministicDefaults(t *testing.T) {
	got := oneFrom(t, "v1-active-no-check.json")

	const wantText = "Data-center revenue keeps compounding because hyperscaler capex is still front-loaded."
	if got.Claim != wantText {
		t.Errorf("claim = %q, want the stored text verbatim", got.Claim)
	}
	if got.Text != got.Claim {
		t.Errorf("text (%q) must mirror claim (%q)", got.Text, got.Claim)
	}
	if got.ID != "9f2c1b7a4e0d5a83" {
		t.Errorf("id = %q, must be preserved verbatim", got.ID)
	}
	if got.Status != statusActive {
		t.Errorf("status = %q, want active", got.Status)
	}
	if got.SchemaVersion != thesisSchemaVersion {
		t.Errorf("schemaVersion = %d, want %d", got.SchemaVersion, thesisSchemaVersion)
	}
	if got.CreatedAt != 1750000000 || got.UpdatedAt != 1750000000 {
		t.Errorf("timestamps = %d/%d, must be preserved", got.CreatedAt, got.UpdatedAt)
	}

	// A migrated thesis is a VALID thesis with only a claim: every new collection is present and
	// empty, and every new scalar is unknown rather than guessed.
	for _, l := range got.itemLists() {
		if *l.Items == nil {
			t.Errorf("%s is nil, want an empty slice", l.Field)
		}
		if len(*l.Items) != 0 {
			t.Errorf("%s = %v, want empty", l.Field, *l.Items)
		}
	}
	if got.Versions == nil || len(got.Versions) != 0 {
		t.Errorf("versions = %v, want empty — no synthetic 'created' event", got.Versions)
	}
	if got.Horizon != nil || got.Confidence != nil || got.CloseReason != nil || got.LastReview != nil {
		t.Errorf("absent scalars must stay nil, got horizon=%v confidence=%v closeReason=%v lastReview=%v",
			got.Horizon, got.Confidence, got.CloseReason, got.LastReview)
	}
	if got.Notes != "" {
		t.Errorf("notes = %q, want empty", got.Notes)
	}
}

// A legacy claim must survive byte-for-byte: no trimming beyond what v1 already did, no re-wrapping,
// no re-wording, and nothing lost to encoding.
func TestLegacyClaimIsByteIdenticalIncludingUnicode(t *testing.T) {
	var stored []map[string]any
	if err := json.Unmarshal(fixture(t, "v1-unicode-text.json"), &stored); err != nil {
		t.Fatal(err)
	}
	want := stored[0]["text"].(string)

	got := oneFrom(t, "v1-unicode-text.json")
	if got.Claim != want {
		t.Errorf("claim = %q\nwant %q", got.Claim, want)
	}
	if got.Text != want {
		t.Errorf("text = %q\nwant %q", got.Text, want)
	}
}

func TestLegacyTextAtTruncationBoundaryIsUnchanged(t *testing.T) {
	got := oneFrom(t, "v1-max-length-text.json")
	if len(got.Claim) != maxThesisLen {
		t.Errorf("claim length = %d, want %d (the stored value must not be re-truncated or padded)", len(got.Claim), maxThesisLen)
	}
	if got.Claim != got.Text {
		t.Error("claim and text diverged at the boundary")
	}
}

// A record stored before the journal recorded timestamps keeps its zeros. Back-filling `now` would
// invent a creation date; the UI renders "unknown" instead.
func TestLegacyMissingTimestampsAreNotBackfilled(t *testing.T) {
	got := oneFrom(t, "v1-no-timestamps.json")
	if got.CreatedAt != 0 || got.UpdatedAt != 0 {
		t.Errorf("timestamps = %d/%d, want 0/0 (never fabricated)", got.CreatedAt, got.UpdatedAt)
	}
}

// Both shapes present: `claim` wins, and `text` is re-mirrored from it.
func TestDualShapeRecordResolvesToClaim(t *testing.T) {
	got := oneFrom(t, "dual-shape.json")
	const wantClaim = "Inference demand outgrows training demand by the end of next year."
	if got.Claim != wantClaim {
		t.Errorf("claim = %q, want %q", got.Claim, wantClaim)
	}
	if got.Text != wantClaim {
		t.Errorf("text = %q, want the claim (stale mirror must not win)", got.Text)
	}
	if got.Horizon == nil || *got.Horizon != "quarters" {
		t.Errorf("horizon = %v, want quarters", got.Horizon)
	}
	if got.Confidence == nil || *got.Confidence != 60 {
		t.Errorf("confidence = %v, want 60", got.Confidence)
	}
}

func TestV2ClaimOnlyRecordLoads(t *testing.T) {
	got := oneFrom(t, "v2-claim-only.json")
	if got.Status != statusDraft {
		t.Errorf("status = %q, want draft", got.Status)
	}
	if len(got.Assumptions) != 0 || got.Confidence != nil {
		t.Error("a claim-only v2 record must load without inventing collections or scalars")
	}
}

func TestEmptyStoreLoads(t *testing.T) {
	dir := t.TempDir()
	seedUser(t, dir, "userA", "empty.json")
	rec := do(t, newTestServer(t, dir), "userA", "GET", "/theses", "")
	wantStatus(t, rec, http.StatusOK)
	if got := decodeList(t, rec); len(got) != 0 {
		t.Errorf("got %d theses, want 0", len(got))
	}
}

// The v1 lastCheck keeps rendering, attributed to the model, and its ABSENT confidence stays absent —
// historical checks have no confidence and never will, so nil must not become 0.
func TestLegacyLastCheckSurvivesUnchanged(t *testing.T) {
	got := oneFrom(t, "v1-active-challenged-check.json")
	if got.LastCheck == nil {
		t.Fatal("lastCheck was dropped")
	}
	c := got.LastCheck
	if c.Verdict != "challenged" {
		t.Errorf("verdict = %q, want challenged", c.Verdict)
	}
	if len(c.Evidence) != 3 {
		t.Errorf("evidence items = %d, want 3", len(c.Evidence))
	}
	if len(c.WatchFor) != 2 {
		t.Errorf("watchFor items = %d, want 2", len(c.WatchFor))
	}
	if c.ModelUsed != "qwen2.5:7b" {
		t.Errorf("modelUsed = %q, want qwen2.5:7b", c.ModelUsed)
	}
	if c.At != 1749500000 {
		t.Errorf("at = %d, want 1749500000", c.At)
	}
	if c.Confidence != nil {
		t.Errorf("confidence = %v, want nil (absence is unknown, not zero)", c.Confidence)
	}
}

// The unattributed pre-auth bucket is read-never / write-never: it is moved once, then no route,
// normalization, or version back-fill may touch it — not even while the same process is serving a
// real user who is creating and editing records.
func TestLegacyBucketRemainsUnattributedAndByteIdentical(t *testing.T) {
	dir := t.TempDir()
	original := fixture(t, "legacy-bucket.json")
	if err := os.WriteFile(filepath.Join(dir, "theses.json"), original, 0o644); err != nil {
		t.Fatal(err)
	}

	h := newTestServer(t, dir) // openThesisStore performs the one-time move
	legacyPath := filepath.Join(dir, legacyBucket, "theses.json")
	moved, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatalf("legacy bucket missing: %v", err)
	}
	if !reflect.DeepEqual(original, moved) {
		t.Fatal("legacy bucket was rewritten during the move")
	}

	// A real user sees none of it, and their own writes leave it alone.
	if got := decodeList(t, do(t, h, "userA", "GET", "/theses", "")); len(got) != 0 {
		t.Fatalf("userA sees %d legacy theses, want 0 — legacy data is never attributed", len(got))
	}
	created := decodeThesis(t, do(t, h, "userA", "POST", "/theses", `{"ticker":"nvda","claim":"My own thesis."}`))
	do(t, h, "userA", "PATCH", "/theses/"+created.ID, `{"claim":"Edited."}`)
	do(t, h, "userA", "POST", "/theses/"+created.ID+"/review-checkpoint", `{"itemCount":0}`)

	after, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, after) {
		t.Errorf("legacy bucket changed after user activity:\nbefore %s\nafter  %s", original, after)
	}
	if _, err := os.Stat(filepath.Join(dir, "theses.json")); !os.IsNotExist(err) {
		t.Error("pre-auth theses.json should have been moved, not copied")
	}
}

// Restarting must not re-run anything: the move is idempotent and the bucket is untouched.
func TestLegacyMigrationIsIdempotentAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	original := fixture(t, "legacy-bucket.json")
	if err := os.WriteFile(filepath.Join(dir, "theses.json"), original, 0o644); err != nil {
		t.Fatal(err)
	}
	newTestServer(t, dir)
	newTestServer(t, dir)
	newTestServer(t, dir)

	after, err := os.ReadFile(filepath.Join(dir, legacyBucket, "theses.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, after) {
		t.Error("legacy bucket changed across restarts")
	}
}

// ---- old clients keep working ----

// The shipped ThesisTracker posts {ticker, text} and patches {text} / {status}. None of that may
// break, and `text` must keep coming back populated.
func TestV1ClientRequestsStillWork(t *testing.T) {
	h := newTestServer(t, t.TempDir())

	rec := do(t, h, "userA", "POST", "/theses", `{"ticker":"nvda","text":"Old client thesis."}`)
	wantStatus(t, rec, http.StatusCreated)
	created := decodeThesis(t, rec)
	if created.Text != "Old client thesis." || created.Claim != created.Text {
		t.Fatalf("text/claim = %q/%q", created.Text, created.Claim)
	}
	if created.Ticker != "NVDA" {
		t.Errorf("ticker = %q, want uppercased", created.Ticker)
	}
	if created.Status != statusActive {
		t.Errorf("status = %q, want active by default", created.Status)
	}

	// PATCH {"text": ...} writes BOTH fields.
	patched := decodeThesis(t, do(t, h, "userA", "PATCH", "/theses/"+created.ID, `{"text":"Revised by the old client."}`))
	if patched.Text != "Revised by the old client." || patched.Claim != patched.Text {
		t.Errorf("after text patch: text=%q claim=%q", patched.Text, patched.Claim)
	}

	archived := decodeThesis(t, do(t, h, "userA", "PATCH", "/theses/"+created.ID, `{"status":"archived"}`))
	if archived.Status != statusArchived {
		t.Errorf("status = %q, want archived", archived.Status)
	}

	if listed := decodeList(t, do(t, h, "userA", "GET", "/theses?ticker=NVDA&status=archived", "")); len(listed) != 1 {
		t.Errorf("filtered list returned %d, want 1", len(listed))
	}
	wantStatus(t, do(t, h, "userA", "DELETE", "/theses/"+created.ID, ""), http.StatusOK)
}

// v1 truncated an over-long claim rather than failing the save. Preserved.
func TestOverlongClaimIsTruncatedNotRejected(t *testing.T) {
	h := newTestServer(t, t.TempDir())
	long := strings.Repeat("a", maxThesisLen+500)
	rec := do(t, h, "userA", "POST", "/theses", `{"ticker":"NVDA","text":"`+long+`"}`)
	wantStatus(t, rec, http.StatusCreated)
	created := decodeThesis(t, rec)
	if len(created.Claim) != maxThesisLen || len(created.Text) != maxThesisLen {
		t.Errorf("lengths = %d/%d, want %d", len(created.Claim), len(created.Text), maxThesisLen)
	}
}

// ---- structured authoring ----

const fullThesisBody = `{
  "ticker": "nvda",
  "claim": "Networking attach rate carries datacenter growth through the next two quarters.",
  "status": "draft",
  "horizon": "quarters",
  "confidence": 60,
  "notes": "Started from the Q2 call.",
  "assumptions": [{"text":"Hyperscaler capex stays front-loaded"},{"text":"No packaging regression"}],
  "catalysts": [{"text":"Q3 earnings","dueAt":1760000000}],
  "risks": [{"text":"Custom silicon takes share"}],
  "invalidationConditions": [{"text":"Two consecutive quarters of declining datacenter revenue"}],
  "nextQuestions": [{"text":"What is the actual attach rate today?"}]
}`

func TestStructuredThesisRoundTripsThroughDisk(t *testing.T) {
	dir := t.TempDir()
	created := decodeThesis(t, do(t, newTestServer(t, dir), "userA", "POST", "/theses", fullThesisBody))

	if created.Status != statusDraft {
		t.Errorf("status = %q, want draft", created.Status)
	}
	prefixes := map[string]string{
		"assumptions": prefixAssumption, "catalysts": prefixCatalyst, "risks": prefixRisk,
		"invalidationConditions": prefixInvalidation, "nextQuestions": prefixQuestion,
	}
	for _, l := range created.itemLists() {
		for _, it := range *l.Items {
			if !strings.HasPrefix(it.ID, prefixes[l.Field]) {
				t.Errorf("%s item id %q lacks prefix %q", l.Field, it.ID, prefixes[l.Field])
			}
			if it.CreatedAt == 0 {
				t.Errorf("%s item %q has no createdAt", l.Field, it.ID)
			}
		}
	}
	if created.Catalysts[0].DueAt == nil || *created.Catalysts[0].DueAt != 1760000000 {
		t.Errorf("catalyst dueAt = %v, want 1760000000", created.Catalysts[0].DueAt)
	}
	// dueAt/resolvedAt belong to one list each and are dropped elsewhere.
	if created.Assumptions[0].DueAt != nil || created.Assumptions[0].ResolvedAt != nil {
		t.Error("assumption carried a dueAt/resolvedAt it should not have")
	}

	// Restart: a brand-new server reading only the files must see the same record.
	reloaded := decodeThesis(t, do(t, newTestServer(t, dir), "userA", "GET", "/theses/"+created.ID, ""))
	if !reflect.DeepEqual(created, reloaded) {
		t.Errorf("record changed across restart:\nbefore %+v\nafter  %+v", created, reloaded)
	}
}

// A partial update touches exactly what it names.
func TestPatchingOneFieldDoesNotClearOmittedFields(t *testing.T) {
	h := newTestServer(t, t.TempDir())
	created := decodeThesis(t, do(t, h, "userA", "POST", "/theses", fullThesisBody))

	got := decodeThesis(t, do(t, h, "userA", "PATCH", "/theses/"+created.ID, `{"notes":"Only the notes changed."}`))
	if got.Notes != "Only the notes changed." {
		t.Errorf("notes = %q", got.Notes)
	}
	if got.Claim != created.Claim {
		t.Error("claim was disturbed by a notes-only patch")
	}
	if got.Horizon == nil || *got.Horizon != "quarters" {
		t.Errorf("horizon = %v, want quarters preserved", got.Horizon)
	}
	if got.Confidence == nil || *got.Confidence != 60 {
		t.Errorf("confidence = %v, want 60 preserved", got.Confidence)
	}
	if !reflect.DeepEqual(got.Assumptions, created.Assumptions) {
		t.Error("assumptions were disturbed by a notes-only patch")
	}
	if !reflect.DeepEqual(got.Catalysts, created.Catalysts) {
		t.Error("catalysts were disturbed by a notes-only patch")
	}
}

// "absent" and "clear this value" must not be the same request.
func TestPatchDistinguishesAbsentFromExplicitNull(t *testing.T) {
	h := newTestServer(t, t.TempDir())
	created := decodeThesis(t, do(t, h, "userA", "POST", "/theses", fullThesisBody))

	// Absent: nothing moves.
	got := decodeThesis(t, do(t, h, "userA", "PATCH", "/theses/"+created.ID, `{"notes":"x"}`))
	if got.Horizon == nil || got.Confidence == nil {
		t.Fatal("absent fields were cleared")
	}

	// Explicit null: cleared, and only the named one.
	got = decodeThesis(t, do(t, h, "userA", "PATCH", "/theses/"+created.ID, `{"horizon":null}`))
	if got.Horizon != nil {
		t.Errorf("horizon = %v, want nil after explicit null", got.Horizon)
	}
	if got.Confidence == nil || *got.Confidence != 60 {
		t.Errorf("confidence = %v, want 60 still", got.Confidence)
	}

	got = decodeThesis(t, do(t, h, "userA", "PATCH", "/theses/"+created.ID, `{"confidence":null}`))
	if got.Confidence != nil {
		t.Errorf("confidence = %v, want nil after explicit null", got.Confidence)
	}

	// An empty list clears that list, and only that list.
	got = decodeThesis(t, do(t, h, "userA", "PATCH", "/theses/"+created.ID, `{"risks":[]}`))
	if len(got.Risks) != 0 {
		t.Errorf("risks = %v, want cleared", got.Risks)
	}
	if len(got.Assumptions) != 2 {
		t.Errorf("assumptions = %d, want 2 untouched", len(got.Assumptions))
	}
}

// Item ids are link targets for Step 06 evidence and Step 08 rules, so an ordinary save must not
// re-point them — and a client must not be able to mint or borrow one.
func TestItemIdsSurviveEditsAndUnknownIdsAreReminted(t *testing.T) {
	h := newTestServer(t, t.TempDir())
	created := decodeThesis(t, do(t, h, "userA", "POST", "/theses", fullThesisBody))
	keep, drop := created.Assumptions[0], created.Assumptions[1]

	body := `{"assumptions":[` +
		`{"id":"` + keep.ID + `","text":"Hyperscaler capex stays front-loaded through FY27"},` +
		`{"text":"New assumption with no id"},` +
		`{"id":"asm_ffffffffffffffff","text":"Forged id"}` +
		`]}`
	got := decodeThesis(t, do(t, h, "userA", "PATCH", "/theses/"+created.ID, body))

	if len(got.Assumptions) != 3 {
		t.Fatalf("assumptions = %d, want 3", len(got.Assumptions))
	}
	if got.Assumptions[0].ID != keep.ID {
		t.Errorf("known id was not preserved: %q != %q", got.Assumptions[0].ID, keep.ID)
	}
	if got.Assumptions[0].CreatedAt != keep.CreatedAt {
		t.Error("a known item's createdAt must survive a text edit")
	}
	if got.Assumptions[2].ID == "asm_ffffffffffffffff" {
		t.Error("a client-supplied unknown id was honoured; it must be reminted")
	}
	for _, it := range got.Assumptions[1:] {
		if !strings.HasPrefix(it.ID, prefixAssumption) {
			t.Errorf("reminted id %q lacks the asm_ prefix", it.ID)
		}
	}

	// The version event describes the change by id only.
	if len(got.Versions) != 1 {
		t.Fatalf("versions = %d, want 1", len(got.Versions))
	}
	d := got.Versions[0].ItemDelta["assumptions"]
	if len(d.Added) != 2 || len(d.Edited) != 1 || len(d.Removed) != 1 {
		t.Errorf("delta = %+v, want 2 added / 1 edited / 1 removed", d)
	}
	if d.Edited[0] != keep.ID || d.Removed[0] != drop.ID {
		t.Errorf("delta ids = edited %v removed %v, want %q / %q", d.Edited, d.Removed, keep.ID, drop.ID)
	}
	if strings.Contains(rawJSON(t, got.Versions[0]), "Hyperscaler") {
		t.Error("item text leaked into the version event; deltas carry ids only")
	}
}

func rawJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ---- validation ----

func TestInvalidValuesAreRejected(t *testing.T) {
	h := newTestServer(t, t.TempDir())
	created := decodeThesis(t, do(t, h, "userA", "POST", "/theses", fullThesisBody))
	id := created.ID

	cases := []struct {
		name, body, wantField string
	}{
		// "invalidated" is a close REASON, never a status.
		{"status invalidated", `{"status":"invalidated"}`, "status"},
		{"status nonsense", `{"status":"paused"}`, "status"},
		{"confidence above range", `{"confidence":101}`, "confidence"},
		{"confidence below range", `{"confidence":-1}`, "confidence"},
		{"confidence fractional", `{"confidence":60.5}`, "confidence"},
		{"horizon nonsense", `{"horizon":"decades"}`, "horizon"},
		{"close reason nonsense", `{"status":"active","closeReason":"because"}`, "closeReason"},
		{"notes too long", `{"notes":"` + strings.Repeat("n", maxNotesLen+1) + `"}`, "notes"},
		{"item text too long", `{"risks":[{"text":"` + strings.Repeat("r", maxItemTextLen+1) + `"}]}`, "risks"},
		{"empty item text", `{"risks":[{"text":"   "}]}`, "risks"},
		{"too many items", `{"risks":[` + strings.TrimSuffix(strings.Repeat(`{"text":"r"},`, maxItemsPerList+1), ",") + `]}`, "risks"},
		{"unknown close condition", `{"closedByConditionId":"inv_ffffffffffffffff"}`, "closedByConditionId"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := do(t, h, "userA", "PATCH", "/theses/"+id, c.body)
			wantStatus(t, rec, http.StatusBadRequest)
			body := decodeErr(t, rec)
			if got := body["field"]; got != c.wantField {
				t.Errorf("field = %v, want %q (body=%s)", got, c.wantField, rec.Body.String())
			}
			if body["error"] == nil || body["error"] == "" {
				t.Error("a validation failure must carry a human-readable error")
			}
		})
	}

	// The record is untouched by every rejected write.
	after := decodeThesis(t, do(t, h, "userA", "GET", "/theses/"+id, ""))
	if !reflect.DeepEqual(created, after) {
		t.Error("a rejected patch mutated the stored record")
	}
}

func TestCreateRequiresTickerAndClaim(t *testing.T) {
	h := newTestServer(t, t.TempDir())
	for _, c := range []struct{ name, body, field string }{
		{"no ticker", `{"claim":"Something."}`, "ticker"},
		{"no claim", `{"ticker":"NVDA"}`, "claim"},
		{"blank claim", `{"ticker":"NVDA","text":"   "}`, "claim"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := do(t, h, "userA", "POST", "/theses", c.body)
			wantStatus(t, rec, http.StatusBadRequest)
			if got := decodeErr(t, rec)["field"]; got != c.field {
				t.Errorf("field = %v, want %q", got, c.field)
			}
		})
	}
}

func TestInvalidJSONIsRejected(t *testing.T) {
	h := newTestServer(t, t.TempDir())
	wantStatus(t, do(t, h, "userA", "POST", "/theses", `{"ticker":`), http.StatusBadRequest)
}

// ---- lifecycle ----

func TestLifecycleTransitions(t *testing.T) {
	h := newTestServer(t, t.TempDir())
	created := decodeThesis(t, do(t, h, "userA", "POST", "/theses", `{"ticker":"NVDA","claim":"Draft first.","status":"draft"}`))
	id := created.ID

	// draft -> active
	got := decodeThesis(t, do(t, h, "userA", "PATCH", "/theses/"+id, `{"status":"active"}`))
	if got.Status != statusActive {
		t.Fatalf("status = %q, want active", got.Status)
	}
	// active -> draft is not legal
	rec := do(t, h, "userA", "PATCH", "/theses/"+id, `{"status":"draft"}`)
	wantStatus(t, rec, http.StatusBadRequest)
	if code := decodeErr(t, rec)["code"]; code != "illegal_transition" {
		t.Errorf("code = %v, want illegal_transition", code)
	}
	// closing needs a reason
	wantStatus(t, do(t, h, "userA", "PATCH", "/theses/"+id, `{"status":"closed"}`), http.StatusBadRequest)

	// active -> closed, with reason and a condition link
	withCond := decodeThesis(t, do(t, h, "userA", "PATCH", "/theses/"+id, `{"invalidationConditions":[{"text":"Datacenter revenue declines two quarters running"}]}`))
	condID := withCond.InvalidationConditions[0].ID
	closed := decodeThesis(t, do(t, h, "userA", "PATCH", "/theses/"+id,
		`{"status":"closed","closeReason":"invalidated","closedByConditionId":"`+condID+`"}`))
	if closed.Status != statusClosed {
		t.Fatalf("status = %q, want closed", closed.Status)
	}
	if closed.CloseReason == nil || *closed.CloseReason != "invalidated" {
		t.Errorf("closeReason = %v", closed.CloseReason)
	}
	if closed.ClosedAt == nil || *closed.ClosedAt == 0 {
		t.Errorf("closedAt = %v, want a timestamp", closed.ClosedAt)
	}
	if closed.ClosedByConditionID != condID {
		t.Errorf("closedByConditionId = %q, want %q", closed.ClosedByConditionID, condID)
	}

	// closed -> active is NOT legal: a reopen must go through archived so history shows it.
	rec = do(t, h, "userA", "PATCH", "/theses/"+id, `{"status":"active"}`)
	wantStatus(t, rec, http.StatusBadRequest)
	if code := decodeErr(t, rec)["code"]; code != "illegal_transition" {
		t.Errorf("code = %v, want illegal_transition", code)
	}

	// closed -> archived clears the close record; the prior reason survives in history.
	arch := decodeThesis(t, do(t, h, "userA", "PATCH", "/theses/"+id, `{"status":"archived"}`))
	if arch.CloseReason != nil || arch.ClosedAt != nil {
		t.Errorf("close metadata survived leaving closed: reason=%v at=%v", arch.CloseReason, arch.ClosedAt)
	}
	last := arch.Versions[len(arch.Versions)-1]
	if last.Before["closeReason"] != "invalidated" {
		t.Errorf("version before.closeReason = %v, want invalidated", last.Before["closeReason"])
	}

	// archived -> active reopens.
	if got := decodeThesis(t, do(t, h, "userA", "PATCH", "/theses/"+id, `{"status":"active"}`)); got.Status != statusActive {
		t.Errorf("reopen failed: status = %q", got.Status)
	}
}

// ---- the active cap (D-03) ----

func TestActiveThesisCap(t *testing.T) {
	dir := t.TempDir()
	seedUser(t, dir, "userA", "v1-three-active-one-ticker.json")
	h := newTestServer(t, dir)

	// A fourth ACTIVE thesis on the same ticker is refused with a distinguishable code, so the UI
	// can explain the limit instead of showing a generic failure.
	rec := do(t, h, "userA", "POST", "/theses", `{"ticker":"NVDA","claim":"A fourth active idea."}`)
	wantStatus(t, rec, http.StatusConflict)
	body := decodeErr(t, rec)
	if body["code"] != "active_thesis_cap" {
		t.Errorf("code = %v, want active_thesis_cap", body["code"])
	}
	if body["cap"] != float64(maxActiveTheses) || body["activeCount"] != float64(3) {
		t.Errorf("cap/activeCount = %v/%v, want %d/3", body["cap"], body["activeCount"], maxActiveTheses)
	}

	// draft is unbounded, and another ticker is unaffected.
	wantStatus(t, do(t, h, "userA", "POST", "/theses", `{"ticker":"NVDA","claim":"A draft is fine.","status":"draft"}`), http.StatusCreated)
	wantStatus(t, do(t, h, "userA", "POST", "/theses", `{"ticker":"AMD","claim":"Different ticker."}`), http.StatusCreated)

	// Activating the archived one is refused too — the cap is enforced on transitions INTO active.
	wantStatus(t, do(t, h, "userA", "PATCH", "/theses/4444444444444444", `{"status":"active"}`), http.StatusConflict)

	// Reads never fail, and nothing was closed or archived to make room.
	all := decodeList(t, do(t, h, "userA", "GET", "/theses?ticker=NVDA&status=active", ""))
	if len(all) != 3 {
		t.Errorf("active NVDA theses = %d, want the original 3 intact", len(all))
	}
}

// A user already over the cap keeps every record; only new activations are refused.
func TestOverCapUsersAreGrandfathered(t *testing.T) {
	dir := t.TempDir()
	seedUser(t, dir, "userA", "v1-three-active-one-ticker.json")

	// Hand-place a fourth active record, as a pre-cap deployment would have.
	path := filepath.Join(dir, "userA", "theses.json")
	var raw []map[string]any
	if err := json.Unmarshal(fixture(t, "v1-three-active-one-ticker.json"), &raw); err != nil {
		t.Fatal(err)
	}
	raw = append(raw, map[string]any{
		"id": "5555555555555555", "ticker": "NVDA", "text": "A fifth, from before the cap existed.",
		"status": "active", "createdAt": 1748500000, "updatedAt": 1748500000,
	})
	b, _ := json.Marshal(raw)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}

	h := newTestServer(t, dir)
	got := decodeList(t, do(t, h, "userA", "GET", "/theses?status=active", ""))
	if len(got) != 4 {
		t.Errorf("active theses = %d, want all 4 preserved", len(got))
	}
	// Editing an existing over-cap thesis still works — the cap only gates activation.
	wantStatus(t, do(t, h, "userA", "PATCH", "/theses/5555555555555555", `{"notes":"still editable"}`), http.StatusOK)
	wantStatus(t, do(t, h, "userA", "POST", "/theses", `{"ticker":"NVDA","claim":"One more."}`), http.StatusConflict)
}

// ---- version history ----

func TestHistoryRecordsMaterialChangeExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	h := newTestServer(t, dir)
	created := decodeThesis(t, do(t, h, "userA", "POST", "/theses", `{"ticker":"NVDA","claim":"Original claim."}`))
	if len(created.Versions) != 0 {
		t.Fatalf("a new thesis starts with %d versions, want 0 (no synthetic created event)", len(created.Versions))
	}
	id := created.ID

	const patch = `{"claim":"Revised claim.","reason":"sharpened the wording"}`
	got := decodeThesis(t, do(t, h, "userA", "PATCH", "/theses/"+id, patch))
	if len(got.Versions) != 1 {
		t.Fatalf("versions = %d, want 1", len(got.Versions))
	}
	ev := got.Versions[0]
	if !reflect.DeepEqual(ev.ChangedFields, []string{"claim"}) {
		t.Errorf("changedFields = %v, want [claim]", ev.ChangedFields)
	}
	if ev.Before["claim"] != "Original claim." {
		t.Errorf("before.claim = %v, want the prior value", ev.Before["claim"])
	}
	if ev.Actor != "user" {
		t.Errorf("actor = %q, want user", ev.Actor)
	}
	if ev.Reason != "sharpened the wording" {
		t.Errorf("reason = %q", ev.Reason)
	}
	if !strings.HasPrefix(ev.ID, "ver_") {
		t.Errorf("event id = %q, want a ver_ prefix", ev.ID)
	}

	// A replayed identical PATCH is a no-op: retry must not duplicate history.
	got = decodeThesis(t, do(t, h, "userA", "PATCH", "/theses/"+id, patch))
	if len(got.Versions) != 1 {
		t.Errorf("after replay: versions = %d, want 1", len(got.Versions))
	}
	// So is an empty patch.
	got = decodeThesis(t, do(t, h, "userA", "PATCH", "/theses/"+id, `{}`))
	if len(got.Versions) != 1 {
		t.Errorf("after empty patch: versions = %d, want 1", len(got.Versions))
	}

	// A reload appends nothing either: normalization is not a change.
	reloaded := decodeThesis(t, do(t, newTestServer(t, dir), "userA", "GET", "/theses/"+id, ""))
	if len(reloaded.Versions) != 1 {
		t.Errorf("after restart: versions = %d, want 1", len(reloaded.Versions))
	}
	// Repeated reads do not accumulate either.
	h2 := newTestServer(t, dir)
	for i := 0; i < 3; i++ {
		if got := decodeThesis(t, do(t, h2, "userA", "GET", "/theses/"+id, "")); len(got.Versions) != 1 {
			t.Fatalf("read %d: versions = %d, want 1", i, len(got.Versions))
		}
	}
}

func TestVersionsEndpointIsNewestFirst(t *testing.T) {
	h := newTestServer(t, t.TempDir())
	created := decodeThesis(t, do(t, h, "userA", "POST", "/theses", `{"ticker":"NVDA","claim":"One."}`))
	for _, c := range []string{"Two.", "Three."} {
		do(t, h, "userA", "PATCH", "/theses/"+created.ID, `{"claim":"`+c+`"}`)
	}

	var out struct {
		Versions  []VersionEvent `json:"versions"`
		Truncated bool           `json:"truncated"`
	}
	rec := do(t, h, "userA", "GET", "/theses/"+created.ID+"/versions", "")
	wantStatus(t, rec, http.StatusOK)
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Versions) != 2 {
		t.Fatalf("versions = %d, want 2", len(out.Versions))
	}
	if out.Versions[0].Before["claim"] != "Two." {
		t.Errorf("newest event before.claim = %v, want Two. (newest first)", out.Versions[0].Before["claim"])
	}
	if out.Truncated {
		t.Error("truncated = true, want false")
	}

	rec = do(t, h, "userA", "GET", "/theses/"+created.ID+"/versions?limit=1", "")
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Versions) != 1 || !out.Truncated {
		t.Errorf("limit=1 gave %d events truncated=%v, want 1/true", len(out.Versions), out.Truncated)
	}
}

func TestVersionHistoryIsBounded(t *testing.T) {
	h := newTestServer(t, t.TempDir())
	created := decodeThesis(t, do(t, h, "userA", "POST", "/theses", `{"ticker":"NVDA","claim":"c0"}`))
	var got Thesis
	for i := 1; i <= maxVersionEvents+5; i++ {
		got = decodeThesis(t, do(t, h, "userA", "PATCH", "/theses/"+created.ID, `{"claim":"c`+strconv.Itoa(i)+`"}`))
	}
	if len(got.Versions) != maxVersionEvents {
		t.Errorf("versions = %d, want the cap %d", len(got.Versions), maxVersionEvents)
	}
	if !got.VersionsTruncated {
		t.Error("versionsTruncated = false, want true once the cap dropped the oldest entries")
	}
}

// A long claim must not put an unbounded payload into history.
func TestVersionBeforeValuesAreBounded(t *testing.T) {
	h := newTestServer(t, t.TempDir())
	long := strings.Repeat("x", maxThesisLen)
	created := decodeThesis(t, do(t, h, "userA", "POST", "/theses", `{"ticker":"NVDA","claim":"`+long+`"}`))
	got := decodeThesis(t, do(t, h, "userA", "PATCH", "/theses/"+created.ID, `{"claim":"short"}`))
	before, _ := got.Versions[0].Before["claim"].(string)
	if len(before) > maxVersionBeforeLen {
		t.Errorf("before.claim length = %d, want <= %d", len(before), maxVersionBeforeLen)
	}
}

// ---- system-authored vs user-owned ----

// The gateway's check PATCH must persist the model's verdict AND its confidence, without creating a
// belief-change event and without touching the user's own confidence.
func TestLastCheckPersistsWithoutVersioningOrTouchingUserConfidence(t *testing.T) {
	dir := t.TempDir()
	h := newTestServer(t, dir)
	created := decodeThesis(t, do(t, h, "userA", "POST", "/theses",
		`{"ticker":"NVDA","claim":"Margins hold.","confidence":40}`))

	body := `{"lastCheck":{"verdict":"challenged","summary":"Two facts point the other way.",` +
		`"evidence":[{"fact":"lead times extended","bearing":"contradicts"}],` +
		`"watchFor":["next gross-margin guide"],"modelUsed":"qwen2.5:7b","confidence":72}}`
	got := decodeThesis(t, do(t, h, "userA", "PATCH", "/theses/"+created.ID, body))

	if got.LastCheck == nil || got.LastCheck.Verdict != "challenged" {
		t.Fatalf("lastCheck = %+v", got.LastCheck)
	}
	if got.LastCheck.Confidence == nil || *got.LastCheck.Confidence != 72 {
		t.Errorf("lastCheck.confidence = %v, want 72", got.LastCheck.Confidence)
	}
	if got.LastCheck.At == 0 {
		t.Error("lastCheck.at was not stamped")
	}
	// The user's own conviction is untouched: the model gets its own field.
	if got.Confidence == nil || *got.Confidence != 40 {
		t.Errorf("thesis.confidence = %v, want the user's 40 — a model response must not overwrite it", got.Confidence)
	}
	if len(got.Versions) != 0 {
		t.Errorf("versions = %d, want 0 — a system check is not a belief change", len(got.Versions))
	}

	// It survives a restart.
	reloaded := decodeThesis(t, do(t, newTestServer(t, dir), "userA", "GET", "/theses/"+created.ID, ""))
	if reloaded.LastCheck == nil || reloaded.LastCheck.Confidence == nil || *reloaded.LastCheck.Confidence != 72 {
		t.Errorf("lastCheck did not survive a restart: %+v", reloaded.LastCheck)
	}
}

func TestLastCheckValidation(t *testing.T) {
	h := newTestServer(t, t.TempDir())
	created := decodeThesis(t, do(t, h, "userA", "POST", "/theses", `{"ticker":"NVDA","claim":"c"}`))
	for _, c := range []struct{ name, body string }{
		{"bad verdict", `{"lastCheck":{"verdict":"bullish","summary":"s"}}`},
		{"confidence out of range", `{"lastCheck":{"verdict":"neutral","summary":"s","confidence":180}}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			wantStatus(t, do(t, h, "userA", "PATCH", "/theses/"+created.ID, c.body), http.StatusBadRequest)
		})
	}
}

// lastReview moves only through its own route, and looking is not editing.
func TestReviewCheckpointDoesNotVersionOrBumpUpdatedAt(t *testing.T) {
	h := newTestServer(t, t.TempDir())
	created := decodeThesis(t, do(t, h, "userA", "POST", "/theses", `{"ticker":"NVDA","claim":"c"}`))

	// A PATCH cannot set it.
	patched := decodeThesis(t, do(t, h, "userA", "PATCH", "/theses/"+created.ID,
		`{"lastReview":{"checkpointId":"chk_forged","at":123,"itemCount":9,"source":"user"}}`))
	if patched.LastReview != nil {
		t.Error("lastReview was writable through PATCH; it must move only through its own route")
	}

	rec := do(t, h, "userA", "POST", "/theses/"+created.ID+"/review-checkpoint", `{"itemCount":7,"source":"user"}`)
	wantStatus(t, rec, http.StatusOK)
	got := decodeThesis(t, rec)
	if got.LastReview == nil {
		t.Fatal("lastReview was not set")
	}
	if !strings.HasPrefix(got.LastReview.CheckpointID, "chk_") {
		t.Errorf("checkpointId = %q, want a chk_ prefix", got.LastReview.CheckpointID)
	}
	if got.LastReview.ItemCount != 7 || got.LastReview.Source != "user" {
		t.Errorf("lastReview = %+v", got.LastReview)
	}
	if got.LastReview.At == 0 {
		t.Error("checkpoint has no timestamp")
	}
	if len(got.Versions) != 0 {
		t.Errorf("versions = %d, want 0 — advancing a checkpoint is not a belief change", len(got.Versions))
	}
	if got.UpdatedAt != created.UpdatedAt {
		t.Errorf("updatedAt moved from %d to %d; marking a review read is not an edit", created.UpdatedAt, got.UpdatedAt)
	}

	// An empty body means "reviewed, nothing else to say".
	wantStatus(t, do(t, h, "userA", "POST", "/theses/"+created.ID+"/review-checkpoint", ""), http.StatusOK)
	// A bad source is rejected.
	wantStatus(t, do(t, h, "userA", "POST", "/theses/"+created.ID+"/review-checkpoint", `{"source":"scheduler"}`), http.StatusBadRequest)
}

// Server-owned fields cannot be dictated by a client on create.
func TestCreateIgnoresClientSuppliedServerFields(t *testing.T) {
	h := newTestServer(t, t.TempDir())
	body := `{"ticker":"NVDA","claim":"c","id":"deadbeefdeadbeef","createdAt":1,"updatedAt":2,
	  "lastCheck":{"verdict":"confirming","summary":"forged"},
	  "lastReview":{"checkpointId":"chk_forged","at":1,"itemCount":1,"source":"user"},
	  "versions":[{"id":"ver_forged","at":1,"actor":"user","changedFields":["claim"]}]}`
	got := decodeThesis(t, do(t, h, "userA", "POST", "/theses", body))

	if got.ID == "deadbeefdeadbeef" {
		t.Error("client dictated the id")
	}
	if got.LastCheck != nil {
		t.Error("client planted a lastCheck")
	}
	if got.LastReview != nil {
		t.Error("client planted a lastReview")
	}
	if len(got.Versions) != 0 {
		t.Error("client planted version history")
	}
	if got.CreatedAt == 1 || got.UpdatedAt == 2 {
		t.Error("client dictated timestamps")
	}
}

// ---- authorization ----

func TestUserCannotReadOrMutateAnotherUsersThesis(t *testing.T) {
	dir := t.TempDir()
	seedUser(t, dir, "userA", "v1-active-challenged-check.json")
	seedUser(t, dir, "userB", "v1-archived-with-check.json")
	h := newTestServer(t, dir)

	const aID = "a1b2c3d4e5f60718"
	const bID = "bb11cc22dd33ee44"

	// Lists are partitioned.
	bList := decodeList(t, do(t, h, "userB", "GET", "/theses", ""))
	if len(bList) != 1 || bList[0].ID != bID {
		t.Fatalf("userB sees %+v, want only their own record", bList)
	}

	// Every route on a foreign id is a 404 — never a 403, which would confirm it exists.
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/theses/" + aID, ""},
		{"GET", "/theses/" + aID + "/versions", ""},
		{"PATCH", "/theses/" + aID, `{"claim":"hijacked"}`},
		{"POST", "/theses/" + aID + "/review-checkpoint", `{"itemCount":1}`},
		{"DELETE", "/theses/" + aID, ""},
	} {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			rec := do(t, h, "userB", c.method, c.path, c.body)
			wantStatus(t, rec, http.StatusNotFound)
		})
	}

	// userA's record is intact and still theirs.
	a := decodeThesis(t, do(t, h, "userA", "GET", "/theses/"+aID, ""))
	if a.Claim != "Gross margin holds above 70% through the next two quarters despite the CoWoS supply crunch." {
		t.Errorf("userA's claim was altered: %q", a.Claim)
	}
	if a.LastCheck == nil {
		t.Error("userA's lastCheck was lost")
	}
}

func TestGuestsCanReadNothingAndWriteNothing(t *testing.T) {
	dir := t.TempDir()
	seedUser(t, dir, "userA", "v1-active-no-check.json")
	h := newTestServer(t, dir)

	// A guest's read is scoped to a user id of "" — an empty journal, not someone else's.
	if got := decodeList(t, do(t, h, "", "GET", "/theses", "")); len(got) != 0 {
		t.Errorf("guest sees %d theses, want 0", len(got))
	}
	for _, c := range []struct{ method, path, body string }{
		{"POST", "/theses", `{"ticker":"NVDA","claim":"c"}`},
		{"PATCH", "/theses/9f2c1b7a4e0d5a83", `{"claim":"c"}`},
		{"POST", "/theses/9f2c1b7a4e0d5a83/review-checkpoint", `{"itemCount":1}`},
		{"DELETE", "/theses/9f2c1b7a4e0d5a83", ""},
	} {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			wantStatus(t, do(t, h, "", c.method, c.path, c.body), http.StatusUnauthorized)
		})
	}
}

// ---- store safety ----

// A theses.json that does not parse must not be silently replaced with a fresh one: that would
// destroy every thesis the user ever wrote.
func TestMalformedStoreIsNeverOverwritten(t *testing.T) {
	dir := t.TempDir()
	seedUser(t, dir, "userA", "malformed.json")
	original := fixture(t, "malformed.json")
	h := newTestServer(t, dir)

	// Reads degrade to empty rather than erroring.
	rec := do(t, h, "userA", "GET", "/theses", "")
	wantStatus(t, rec, http.StatusOK)
	if got := decodeList(t, rec); len(got) != 0 {
		t.Errorf("got %d theses from an unparseable file, want 0", len(got))
	}

	// Writes refuse, with a distinguishable code.
	rec = do(t, h, "userA", "POST", "/theses", `{"ticker":"NVDA","claim":"would clobber"}`)
	wantStatus(t, rec, http.StatusServiceUnavailable)
	if code := decodeErr(t, rec)["code"]; code != "store_unreadable" {
		t.Errorf("code = %v, want store_unreadable", code)
	}

	after, err := os.ReadFile(filepath.Join(dir, "userA", "theses.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, after) {
		t.Error("the unparseable file was modified")
	}

	// One user's broken file does not affect another's.
	wantStatus(t, do(t, h, "userB", "POST", "/theses", `{"ticker":"NVDA","claim":"fine"}`), http.StatusCreated)
}

// New writes must be readable after a restart, alongside untouched legacy records.
func TestMixedLegacyAndNewRecordsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	seedUser(t, dir, "userA", "v1-active-challenged-check.json")
	h := newTestServer(t, dir)

	created := decodeThesis(t, do(t, h, "userA", "POST", "/theses", fullThesisBody))

	got := decodeList(t, do(t, newTestServer(t, dir), "userA", "GET", "/theses", ""))
	if len(got) != 2 {
		t.Fatalf("got %d theses after restart, want 2", len(got))
	}
	byID := map[string]Thesis{}
	for _, th := range got {
		byID[th.ID] = th
	}
	legacy, ok := byID["a1b2c3d4e5f60718"]
	if !ok {
		t.Fatal("the legacy record vanished")
	}
	if legacy.LastCheck == nil || legacy.CreatedAt != 1748000000 {
		t.Error("the legacy record was damaged by a neighbouring write")
	}
	if len(legacy.Versions) != 0 {
		t.Error("history was fabricated for an untouched legacy record")
	}
	fresh, ok := byID[created.ID]
	if !ok {
		t.Fatal("the new record vanished")
	}
	if len(fresh.Assumptions) != 2 || fresh.Horizon == nil {
		t.Errorf("the new record lost structure across the restart: %+v", fresh)
	}
}

// The lazy migration only rewrites a legacy record when the USER mutates it, and even then the id and
// createdAt are preserved exactly.
func TestLegacyRecordIsRewrittenOnlyOnUserMutation(t *testing.T) {
	dir := t.TempDir()
	seedUser(t, dir, "userA", "v1-active-no-check.json")
	path := filepath.Join(dir, "userA", "theses.json")
	original := fixture(t, "v1-active-no-check.json")

	h := newTestServer(t, dir)
	// Reads, including a version listing, must not touch the file.
	do(t, h, "userA", "GET", "/theses", "")
	do(t, h, "userA", "GET", "/theses/9f2c1b7a4e0d5a83", "")
	do(t, h, "userA", "GET", "/theses/9f2c1b7a4e0d5a83/versions", "")
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, onDisk) {
		t.Error("a read rewrote the stored file; normalization must stay in memory")
	}

	// A user edit persists the v2 shape.
	do(t, h, "userA", "PATCH", "/theses/9f2c1b7a4e0d5a83", `{"confidence":80}`)
	var stored []map[string]any
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &stored); err != nil {
		t.Fatal(err)
	}
	if stored[0]["id"] != "9f2c1b7a4e0d5a83" {
		t.Errorf("id changed on rewrite: %v", stored[0]["id"])
	}
	if stored[0]["createdAt"] != float64(1750000000) {
		t.Errorf("createdAt changed on rewrite: %v", stored[0]["createdAt"])
	}
	if stored[0]["schemaVersion"] != float64(thesisSchemaVersion) {
		t.Errorf("schemaVersion = %v, want %d", stored[0]["schemaVersion"], thesisSchemaVersion)
	}
	if stored[0]["claim"] != stored[0]["text"] {
		t.Error("claim and text diverged on rewrite")
	}
}
