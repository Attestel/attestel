package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// event_state_test.go — per-user event state (contract §2.3).
//
// Two properties matter more than the CRUD:
//
//  1. THIS FILE NEVER DUPLICATES THE GLOBAL EVENT. The stored object is exactly
//     {eventId, read, saved, dismissed, updatedAt}. A cached title or summary would be a second
//     source of truth for an object that legitimately mutates in `services/events`, and would put
//     global content inside a user partition that D-06 erases.
//  2. ABSENCE IS NOT AN ERROR. An untouched event has no row, and `GET` omits it. Returning a
//     skeleton would be indistinguishable from a "read: false" the user actually set.

// evtID mints a well-formed `evt_` id from a 16-hex suffix, so the test ids are obviously ids.
func evtID(hex16 string) string { return "evt_" + hex16 }

var (
	evtA = evtID("1a2b3c4d5e6f7081")
	evtB = evtID("bb11cc22dd33ee44")
	evtC = evtID("c0ffee1234abcd56")
)

func TestEventStateRoundTripOffline(t *testing.T) {
	e := newTestEnv(t)

	code, out := e.do(t, "u1", "POST", "/event-state", map[string]any{"eventId": evtA, "read": true})
	if code != http.StatusOK {
		t.Fatalf("POST /event-state = %d, body %v", code, out)
	}
	st, _ := out["state"].(map[string]any)
	if st["eventId"] != evtA || st["read"] != true || st["saved"] != false || st["dismissed"] != false {
		t.Fatalf("state = %v, want {read:true, saved:false, dismissed:false}", st)
	}
	updatedAt, ok := st["updatedAt"].(float64)
	if !ok || updatedAt <= 0 || updatedAt != float64(int64(updatedAt)) {
		t.Fatalf("updatedAt = %v, want a positive unix-second integer", st["updatedAt"])
	}
	// Exactly the five contract fields — no title, no summary, no url, no importance, no ticker.
	if len(st) != 5 {
		t.Fatalf("state has %d fields (%v); §2.3 defines exactly five", len(st), st)
	}
	for _, forbidden := range []string{"title", "summary", "url", "importance", "ticker", "publishedAt", "eventType"} {
		if _, present := st[forbidden]; present {
			t.Fatalf("state carries %q — the global event lives in services/events and is never copied here", forbidden)
		}
	}

	code, out = e.do(t, "u1", "GET", "/event-state?ids="+evtA, nil)
	if code != http.StatusOK {
		t.Fatalf("GET /event-state = %d", code)
	}
	states, _ := out["states"].([]any)
	if len(states) != 1 {
		t.Fatalf("states = %v, want one entry", states)
	}
}

// TestEventStateUpsertsPartially: a "mark read" must not un-save, and `read: false` is a deliberate
// un-read rather than an absent field.
func TestEventStateUpsertsPartially(t *testing.T) {
	e := newTestEnv(t)
	e.do(t, "u1", "POST", "/event-state", map[string]any{"eventId": evtA, "saved": true, "read": true})
	_, out := e.do(t, "u1", "POST", "/event-state", map[string]any{"eventId": evtA, "dismissed": true})
	st, _ := out["state"].(map[string]any)
	if st["saved"] != true || st["read"] != true || st["dismissed"] != true {
		t.Fatalf("partial upsert lost a flag: %v", st)
	}
	_, out = e.do(t, "u1", "POST", "/event-state", map[string]any{"eventId": evtA, "read": false})
	st, _ = out["state"].(map[string]any)
	if st["read"] != false || st["saved"] != true || st["dismissed"] != true {
		t.Fatalf("explicit false did not apply, or cleared a sibling: %v", st)
	}
	// One event, one row.
	b, err := os.ReadFile(filepath.Join(e.dir, "u1", eventStateFile))
	if err != nil {
		t.Fatalf("read event_state.json: %v", err)
	}
	var stored []EventState
	if err := json.Unmarshal(b, &stored); err != nil {
		t.Fatalf("event_state.json does not parse: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("four upserts produced %d rows, want 1", len(stored))
	}
}

// TestAbsentEventStateIsOmittedNotInvented is the contract's "absent state is not an error" rule.
func TestAbsentEventStateIsOmittedNotInvented(t *testing.T) {
	e := newTestEnv(t)
	e.do(t, "u1", "POST", "/event-state", map[string]any{"eventId": evtB, "read": true})

	code, out := e.do(t, "u1", "GET", "/event-state?ids="+evtA+","+evtB+","+evtC, nil)
	if code != http.StatusOK {
		t.Fatalf("GET = %d, want 200 — an untouched id is not an error", code)
	}
	states, _ := out["states"].([]any)
	if len(states) != 1 {
		t.Fatalf("states = %v, want only the one stored row", states)
	}
	got, _ := states[0].(map[string]any)
	if got["eventId"] != evtB {
		t.Fatalf("returned %v, want %s", got["eventId"], evtB)
	}
	// Asking about an event does not create a row for it.
	b, err := os.ReadFile(filepath.Join(e.dir, "u1", eventStateFile))
	if err != nil {
		t.Fatalf("read event_state.json: %v", err)
	}
	var stored []EventState
	json.Unmarshal(b, &stored)
	if len(stored) != 1 {
		t.Fatalf("a GET created rows: %+v", stored)
	}
}

// TestEventStateIDValidation: a malformed id is a 400, never a skipped entry — a client that
// mistyped an id must not be told "no state", which is indistinguishable from a real answer.
func TestEventStateIDValidation(t *testing.T) {
	e := newTestEnv(t)

	over := make([]string, 0, 201)
	for i := 0; i < 201; i++ {
		over = append(over, evtID("aaaaaaaaaaaa"+string("0123456789abcdef"[i%16])+string("0123456789abcdef"[(i/16)%16])+"ff"))
	}

	cases := []struct {
		name string
		path string
		want int
	}{
		{"no ids", "/event-state", http.StatusOK},
		{"empty ids", "/event-state?ids=", http.StatusOK},
		{"one good id", "/event-state?ids=" + evtA, http.StatusOK},
		{"wrong prefix", "/event-state?ids=ev_1a2b3c4d5e6f7081", http.StatusBadRequest},
		{"short id", "/event-state?ids=evt_1a2b3c4d", http.StatusBadRequest},
		{"uppercase hex", "/event-state?ids=evt_1A2B3C4D5E6F7081", http.StatusBadRequest},
		{"not hex", "/event-state?ids=evt_zzzzzzzzzzzzzzzz", http.StatusBadRequest},
		{"one bad among good", "/event-state?ids=" + evtA + ",nonsense", http.StatusBadRequest},
		{"201 ids", "/event-state?ids=" + strings.Join(over, ","), http.StatusBadRequest},
		{"200 ids", "/event-state?ids=" + strings.Join(over[:200], ","), http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := e.do(t, "u1", "GET", tc.path, nil)
			if code != tc.want {
				t.Fatalf("GET %s = %d, want %d (body %v)", tc.name, code, tc.want, body)
			}
		})
	}

	// The same id shape rule applies on write.
	for _, bad := range []string{"", "   ", "sub_1a2b3c4d5e6f7081", "evt_1a2b3c4d5e6f708", "evt_1a2b3c4d5e6f70811"} {
		code, _ := e.do(t, "u1", "POST", "/event-state", map[string]any{"eventId": bad, "read": true})
		if code != http.StatusBadRequest {
			t.Fatalf("POST with eventId %q = %d, want 400", bad, code)
		}
	}
}

// TestEventStateDeduplicatesRequestedIDs: asking twice about one event is one answer, and the
// response order follows the request order so the client can zip the two lists.
func TestEventStateDeduplicatesRequestedIDs(t *testing.T) {
	e := newTestEnv(t)
	for _, id := range []string{evtA, evtB} {
		e.do(t, "u1", "POST", "/event-state", map[string]any{"eventId": id, "read": true})
	}
	_, out := e.do(t, "u1", "GET", "/event-state?ids="+evtB+","+evtA+","+evtB, nil)
	states, _ := out["states"].([]any)
	if len(states) != 2 {
		t.Fatalf("states = %v, want two (duplicates collapse)", states)
	}
	first, _ := states[0].(map[string]any)
	second, _ := states[1].(map[string]any)
	if first["eventId"] != evtB || second["eventId"] != evtA {
		t.Fatalf("order = %v,%v; want the requested order %s,%s", first["eventId"], second["eventId"], evtB, evtA)
	}
}

func TestEventStateRejectsGuests(t *testing.T) {
	e := newTestEnv(t)
	if code, _ := e.do(t, "", "GET", "/event-state?ids="+evtA, nil); code != http.StatusUnauthorized {
		t.Fatalf("GET /event-state as a guest = %d, want 401", code)
	}
	if code, _ := e.do(t, "", "POST", "/event-state", map[string]any{"eventId": evtA, "read": true}); code != http.StatusUnauthorized {
		t.Fatalf("POST /event-state as a guest = %d, want 401", code)
	}
}

func TestEventStateUserIsolation(t *testing.T) {
	e := newTestEnv(t)
	e.do(t, "u1", "POST", "/event-state", map[string]any{"eventId": evtA, "saved": true})

	_, out := e.do(t, "u2", "GET", "/event-state?ids="+evtA, nil)
	if states, _ := out["states"].([]any); len(states) != 0 {
		t.Fatalf("u2 sees u1's event state: %v", states)
	}
	// u2 marking the same global event does not touch u1's row.
	e.do(t, "u2", "POST", "/event-state", map[string]any{"eventId": evtA, "dismissed": true})
	_, out = e.do(t, "u1", "GET", "/event-state?ids="+evtA, nil)
	states, _ := out["states"].([]any)
	mine, _ := states[0].(map[string]any)
	if mine["saved"] != true || mine["dismissed"] != false {
		t.Fatalf("u1's state was changed by u2: %v", mine)
	}
}

func TestConcurrentEventStateWritesProduceOneRowPerEvent(t *testing.T) {
	e := newTestEnv(t)
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := evtA
			if i%2 == 1 {
				id = evtB
			}
			req := httptest.NewRequest("POST", "/event-state",
				strings.NewReader(`{"eventId":"`+id+`","read":true}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Cookie", e.cookieFor("u1"))
			e.handler.ServeHTTP(httptest.NewRecorder(), req)
		}(i)
	}
	wg.Wait()

	b, err := os.ReadFile(filepath.Join(e.dir, "u1", eventStateFile))
	if err != nil {
		t.Fatalf("read event_state.json: %v", err)
	}
	var stored []EventState
	if err := json.Unmarshal(b, &stored); err != nil {
		t.Fatalf("event_state.json does not parse after concurrent writes: %v\n%s", err, string(b))
	}
	if len(stored) != 2 {
		t.Fatalf("40 concurrent writes over 2 events produced %d rows, want 2", len(stored))
	}
	for _, st := range stored {
		if !st.Read || st.UpdatedAt <= 0 {
			t.Fatalf("row not written correctly: %+v", st)
		}
	}
}

// TestEventStateSurvivesRestart: state is durable, and a fresh process reads it off disk.
func TestEventStateSurvivesRestart(t *testing.T) {
	e := newTestEnv(t)
	e.do(t, "u1", "POST", "/event-state", map[string]any{"eventId": evtC, "saved": true, "dismissed": true})

	fresh := newFollowEnvAt(t, e.dir)
	_, out := fresh.do(t, "u1", "GET", "/event-state?ids="+evtC, nil)
	states, _ := out["states"].([]any)
	if len(states) != 1 {
		t.Fatalf("after restart states = %v, want one row", states)
	}
	st, _ := states[0].(map[string]any)
	if st["saved"] != true || st["dismissed"] != true || st["read"] != false {
		t.Fatalf("restored row = %v", st)
	}
}

// TestMalformedEventStateIsNeverOverwritten: same refuse-to-clobber guarantee as the follow graph.
func TestMalformedEventStateIsNeverOverwritten(t *testing.T) {
	e := newTestEnv(t)
	if err := os.MkdirAll(filepath.Join(e.dir, "u1"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	corrupt := []byte("[[[not event state")
	path := filepath.Join(e.dir, "u1", eventStateFile)
	if err := os.WriteFile(path, corrupt, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if code, _ := e.do(t, "u1", "GET", "/event-state?ids="+evtA, nil); code != http.StatusServiceUnavailable {
		t.Fatalf("GET on an unreadable store = %d, want 503", code)
	}
	if code, _ := e.do(t, "u1", "POST", "/event-state", map[string]any{"eventId": evtA, "read": true}); code != http.StatusServiceUnavailable {
		t.Fatalf("POST on an unreadable store = %d, want 503", code)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != string(corrupt) {
		t.Fatalf("the unreadable file was modified: %q", string(got))
	}
}
