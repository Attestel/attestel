package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// handlers_test.go — data compatibility with pre-ownership records, the no-leak rule for the
// persisted owner id, and the behaviour that existed before D-16 and must survive it (validation,
// server-owned fields, atomic persistence, zero-config startup).

// legacyFixture is a feedback.json written before ownership existed: no `owner` key anywhere, mixed
// statuses and categories. Byte-exact on purpose — the reader is what is under test.
const legacyFixture = `[
  {
    "id": "legacy-a",
    "createdAt": 1700000000,
    "updatedAt": 1700000000,
    "category": "bug",
    "rating": 2,
    "message": "legacy: chart froze on 1H",
    "name": "Early Tester",
    "status": "new",
    "context": {"ticker": "NVDA", "timeframe": "1H"}
  },
  {
    "id": "legacy-b",
    "createdAt": 1700000100,
    "updatedAt": 1700000200,
    "category": "idea",
    "rating": 5,
    "message": "legacy: add a compare view",
    "status": "reviewed"
  }
]`

// Old records load exactly as written, with an empty owner — not a claimed one, not a dropped record.
func TestLegacyRecordsLoadUnchanged(t *testing.T) {
	e := newTestEnv(t, []string{uidAdmin}, legacyFixture)

	items := e.srv.store.List()
	if len(items) != 2 {
		t.Fatalf("want 2 legacy records loaded, got %d", len(items))
	}
	byID := map[string]Feedback{}
	for _, f := range items {
		byID[f.ID] = f
	}
	a, ok := byID["legacy-a"]
	if !ok {
		t.Fatal("legacy-a did not load")
	}
	if a.Owner != "" {
		t.Errorf("a legacy record must stay ownerless, got owner %q", a.Owner)
	}
	if a.Message != "legacy: chart froze on 1H" || a.Rating != 2 || a.Status != "new" ||
		a.Name != "Early Tester" || a.CreatedAt != 1700000000 {
		t.Errorf("legacy record altered on load: %+v", a)
	}
	if a.Context["ticker"] != "NVDA" {
		t.Errorf("legacy context snapshot lost: %+v", a.Context)
	}
	if b := byID["legacy-b"]; b.Status != "reviewed" || b.UpdatedAt != 1700000200 {
		t.Errorf("legacy record b altered on load: %+v", b)
	}
}

// Startup and reads must not rewrite, claim or normalise the legacy file. A read sweep by both a
// normal user and an admin leaves it byte-for-byte as it was found.
func TestReadingNeverRewritesLegacyRecords(t *testing.T) {
	e := newTestEnv(t, []string{uidAdmin}, legacyFixture)
	path := filepath.Join(e.dir, "feedback.json")

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if string(before) != legacyFixture {
		t.Fatal("openStore rewrote feedback.json at startup")
	}

	e.do(t, "GET", "/feedback", session(t, uidAlice), "")
	e.do(t, "GET", "/feedback", session(t, uidAdmin), "")
	e.do(t, "GET", "/feedback?status=new", session(t, uidAdmin), "")
	e.do(t, "GET", "/feedback/summary", session(t, uidAdmin), "")

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read fixture: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("a read sweep rewrote feedback.json:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// Legacy records are admin-only, and a later submission never adopts them.
func TestLegacyRecordsAreAdminOnlyAndUnclaimed(t *testing.T) {
	e := newTestEnv(t, []string{uidAdmin}, legacyFixture)

	items, _, _ := decodeList(t, e.do(t, "GET", "/feedback", session(t, uidAlice), ""))
	if len(items) != 0 {
		t.Errorf("a normal user must see no legacy records, got %d", len(items))
	}

	e.submit(t, uidAlice, "alice new note")
	items, _, _ = decodeList(t, e.do(t, "GET", "/feedback", session(t, uidAlice), ""))
	if len(items) != 1 || items[0].Message != "alice new note" {
		t.Errorf("submitting must not adopt legacy records: %+v", items)
	}

	adminItems, _, _ := decodeList(t, e.do(t, "GET", "/feedback", session(t, uidAdmin), ""))
	if len(adminItems) != 3 {
		t.Fatalf("admin must see 2 legacy + 1 owned = 3, got %d", len(adminItems))
	}

	// The legacy records on disk still carry no owner after all of it.
	for _, id := range []string{"legacy-a", "legacy-b"} {
		rec, ok := e.srv.store.Get(id)
		if !ok {
			t.Fatalf("%s disappeared", id)
		}
		if rec.Owner != "" {
			t.Errorf("%s was claimed by %q", id, rec.Owner)
		}
	}
}

// Legacy records survive a restart still ownerless, alongside owned ones.
func TestRestartPreservesOwnedAndLegacyRecords(t *testing.T) {
	e := newTestEnv(t, []string{uidAdmin}, legacyFixture)
	aliceID := e.submit(t, uidAlice, "alice persists")

	// Reopen the same directory — the restart path.
	store2, err := openStore(e.dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	srv2 := &Server{cfg: e.srv.cfg, store: store2}
	e2 := &testEnv{srv: srv2, h: srv2.routes(), dir: e.dir}

	rec, ok := store2.Get(aliceID)
	if !ok {
		t.Fatal("owned record did not survive the restart")
	}
	if rec.Owner != uidAlice {
		t.Errorf("ownership did not survive the restart: got %q", rec.Owner)
	}
	if legacyRec, ok := store2.Get("legacy-a"); !ok || legacyRec.Owner != "" {
		t.Errorf("legacy record changed across the restart: %+v", legacyRec)
	}

	items, _, _ := decodeList(t, e2.do(t, "GET", "/feedback", session(t, uidAlice), ""))
	if len(items) != 1 || items[0].ID != aliceID {
		t.Errorf("after restart a user must still see exactly their own: %+v", items)
	}
	adminItems, _, _ := decodeList(t, e2.do(t, "GET", "/feedback", session(t, uidAdmin), ""))
	if len(adminItems) != 3 {
		t.Errorf("after restart the admin must still see all 3, got %d", len(adminItems))
	}
}

// The persisted owner id is an authorization key, not content: it is stored, and it never ships.
func TestResponsesNeverCarryTheOwnerID(t *testing.T) {
	e := newTestEnv(t, []string{uidAdmin}, legacyFixture)
	id := e.submit(t, uidAlice, "check the wire shape")

	// It IS at rest — otherwise scoping could not work across a restart.
	raw, err := os.ReadFile(filepath.Join(e.dir, "feedback.json"))
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	if !strings.Contains(string(raw), uidAlice) {
		t.Error("ownership was not persisted")
	}

	responses := map[string]string{
		"create":       e.do(t, "POST", "/feedback", session(t, uidBob), `{"category":"ux","message":"bob note"}`).Body.String(),
		"own list":     e.do(t, "GET", "/feedback", session(t, uidAlice), "").Body.String(),
		"admin list":   e.do(t, "GET", "/feedback", session(t, uidAdmin), "").Body.String(),
		"admin patch":  e.do(t, "PATCH", "/feedback/"+id, session(t, uidAdmin), `{"status":"reviewed"}`).Body.String(),
		"summary":      e.do(t, "GET", "/feedback/summary", session(t, uidAdmin), "").Body.String(),
		"health":       e.do(t, "GET", "/health", "", "").Body.String(),
		"filtered":     e.do(t, "GET", "/feedback?status=new", session(t, uidAdmin), "").Body.String(),
		"unauthorized": e.do(t, "GET", "/feedback", "", "").Body.String(),
	}
	for name, body := range responses {
		for _, uid := range []string{uidAlice, uidBob, uidAdmin} {
			if strings.Contains(body, uid) {
				t.Errorf("%s response leaked the raw owner id %q: %s", name, uid, body)
			}
		}
		if strings.Contains(body, `"owner"`) {
			t.Errorf("%s response carries an owner field: %s", name, body)
		}
	}
}

// ───────────────────────────────── pre-existing behaviour, still true ─────────────────────────────

// Validation is unchanged by the auth work.
func TestValidationStillRejectsBadSubmissions(t *testing.T) {
	e := newTestEnv(t, []string{uidAdmin}, "")
	c := session(t, uidAlice)

	bad := map[string]string{
		"empty message":      `{"category":"bug","message":""}`,
		"whitespace message": `{"category":"bug","message":"   "}`,
		"missing message":    `{"category":"bug","rating":3}`,
		"unknown category":   `{"category":"malware","message":"hi"}`,
		"rating too high":    `{"category":"bug","message":"hi","rating":6}`,
		"rating negative":    `{"category":"bug","message":"hi","rating":-1}`,
		"not JSON":           `{nope}`,
	}
	for name, body := range bad {
		if w := e.do(t, "POST", "/feedback", c, body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %d (%s)", name, w.Code, w.Body.String())
		}
	}

	// The permissive defaults still hold: no category means "other", no rating is fine.
	w := e.do(t, "POST", "/feedback", c, `{"message":"no category, no rating"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d (%s)", w.Code, w.Body.String())
	}
	var got struct {
		Feedback publicFeedback `json:"feedback"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.Feedback.Category != "other" || got.Feedback.Rating != 0 {
		t.Errorf("defaults changed: %+v", got.Feedback)
	}

	// An oversized context snapshot is still refused.
	big := make(map[string]any, maxContextKey+5)
	for i := 0; i < maxContextKey+5; i++ {
		big[string(rune('a'+i%26))+string(rune('0'+i/26))] = i
	}
	payload, _ := json.Marshal(map[string]any{"category": "bug", "message": "big", "context": big})
	if w := e.do(t, "POST", "/feedback", c, string(payload)); w.Code != http.StatusBadRequest {
		t.Errorf("oversized context: want 400, got %d", w.Code)
	}

	// A long message is truncated, not rejected.
	long, _ := json.Marshal(map[string]any{"category": "bug", "message": strings.Repeat("x", maxMessageLen+500)})
	w = e.do(t, "POST", "/feedback", c, string(long))
	if w.Code != http.StatusCreated {
		t.Fatalf("long message: want 201, got %d", w.Code)
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Feedback.Message) != maxMessageLen {
		t.Errorf("message truncation changed: got %d chars", len(got.Feedback.Message))
	}
}

// Identity and lifecycle stay server-owned: a client cannot choose its id, backdate a submission, or
// submit something pre-resolved.
func TestServerControlledFieldsAreUnforgeable(t *testing.T) {
	e := newTestEnv(t, []string{uidAdmin}, "")

	body := `{"id":"chosen-id","status":"resolved","createdAt":1,"updatedAt":2,` +
		`"category":"bug","message":"forge the metadata"}`
	w := e.do(t, "POST", "/feedback", session(t, uidAlice), body)
	if w.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d", w.Code)
	}
	var got struct {
		Feedback publicFeedback `json:"feedback"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &got)

	f := got.Feedback
	if f.ID == "chosen-id" || f.ID == "" {
		t.Errorf("client chose the id: %q", f.ID)
	}
	if f.Status != "new" {
		t.Errorf("client chose the status: %q", f.Status)
	}
	if f.CreatedAt <= 2 || f.UpdatedAt <= 2 {
		t.Errorf("client backdated the record: created=%d updated=%d", f.CreatedAt, f.UpdatedAt)
	}

	// PATCH still validates the transition surface, for the admin too.
	if w := e.do(t, "PATCH", "/feedback/"+f.ID, session(t, uidAdmin), `{"status":"banana"}`); w.Code != http.StatusBadRequest {
		t.Errorf("invalid status: want 400, got %d", w.Code)
	}
	for _, s := range []string{"new", "reviewed", "resolved"} {
		if w := e.do(t, "PATCH", "/feedback/"+f.ID, session(t, uidAdmin), `{"status":"`+s+`"}`); w.Code != http.StatusOK {
			t.Errorf("status %q: want 200, got %d", s, w.Code)
		}
	}
	// createdAt is preserved across triage; updatedAt moves forward.
	rec, _ := e.srv.store.Get(f.ID)
	if rec.CreatedAt != f.CreatedAt {
		t.Errorf("createdAt changed under triage: %d -> %d", f.CreatedAt, rec.CreatedAt)
	}
}

// Persistence stays atomic (temp file + rename) and leaves no partial file behind.
func TestPersistenceIsAtomic(t *testing.T) {
	e := newTestEnv(t, []string{uidAdmin}, "")
	for i := 0; i < 5; i++ {
		e.submit(t, uidAlice, "note")
	}
	id := e.submit(t, uidBob, "bob note")
	e.do(t, "PATCH", "/feedback/"+id, session(t, uidAdmin), `{"status":"reviewed"}`)
	e.do(t, "DELETE", "/feedback/"+id, session(t, uidAdmin), "")

	entries, err := os.ReadDir(e.dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, en := range entries {
		if strings.HasSuffix(en.Name(), ".tmp") {
			t.Errorf("a temp file was left behind: %s", en.Name())
		}
	}
	raw, err := os.ReadFile(filepath.Join(e.dir, "feedback.json"))
	if err != nil {
		t.Fatalf("read store: %v", err)
	}
	var items []Feedback
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatalf("store is not valid JSON after writes: %v", err)
	}
	if len(items) != 5 {
		t.Errorf("want 5 records after 6 writes and 1 delete, got %d", len(items))
	}
	for _, f := range items {
		if f.Owner != uidAlice {
			t.Errorf("owner not persisted for %s: %q", f.ID, f.Owner)
		}
	}
}

// Zero-key, zero-network startup: with nothing configured the service boots, refuses admin, and still
// accepts a signed-in tester's submission. This is invariant #1 for the feedback service.
func TestZeroConfigStartup(t *testing.T) {
	for _, k := range []string{"PORT", "FEEDBACK_DIR", "AUTH_SECRET", "COOKIE_NAME", "CORS_ORIGINS", "FEEDBACK_ADMIN_UIDS"} {
		t.Setenv(k, "")
	}
	cfg := loadConfig()

	if cfg.Port != "8098" || cfg.FeedbackDir != "./data/feedback" {
		t.Errorf("service defaults changed: %+v", cfg)
	}
	if cfg.Secret != "dev-insecure-change-me" {
		t.Errorf("AUTH_SECRET default must match the other services, got %q", cfg.Secret)
	}
	if cfg.CookieName != "nvda_session" {
		t.Errorf("COOKIE_NAME default must match the other services, got %q", cfg.CookieName)
	}
	if len(cfg.CORSOrigins) != 2 || cfg.CORSOrigins[0] != "http://localhost:5173" || cfg.CORSOrigins[1] != "http://localhost:4173" {
		t.Errorf("CORS default must be the local dev origins, got %v", cfg.CORSOrigins)
	}
	if len(cfg.AdminUIDs) != 0 {
		t.Errorf("the admin allow-list must default to EMPTY, got %v", cfg.AdminUIDs)
	}

	// Boot the real server on those defaults (only the data dir redirected) — no keys, no network.
	dir := t.TempDir()
	store, err := openStore(dir)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	cfg.FeedbackDir = dir
	srv := &Server{cfg: cfg, store: store}
	e := &testEnv{srv: srv, h: srv.routes(), dir: dir}

	if w := e.do(t, "GET", "/health", "", ""); w.Code != http.StatusOK {
		t.Errorf("health on defaults: want 200, got %d", w.Code)
	}
	tok := "nvda_session=" + mint(t, cfg.Secret, uidAlice, time.Hour)
	if w := e.do(t, "POST", "/feedback", tok, `{"category":"bug","message":"offline submission"}`); w.Code != http.StatusCreated {
		t.Errorf("submission on defaults: want 201, got %d (%s)", w.Code, w.Body.String())
	}
	if w := e.do(t, "GET", "/feedback/summary", tok, ""); w.Code != http.StatusForbidden {
		t.Errorf("summary on defaults (no admin configured): want 403, got %d", w.Code)
	}
}
