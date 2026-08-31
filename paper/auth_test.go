package main

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// auth_test.go — the service credential, and its blast radius.
//
// The defect this fixes is narrow and the fix must stay narrow: the engine gains an identity when it
// talks to the journal, and gains nothing anywhere else. In particular it must not start sending a
// credential to the read-only prediction and analysis services, and with no AUTH_SECRET it must
// behave exactly as it did before.

// captureHeaders returns a stub server that records the Cookie header of the last request it saw.
func captureHeaders(t *testing.T, seen *string, body any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = r.Header.Get("Cookie")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The token the engine mints verifies with the SAME algorithm the journal uses, for the system uid.
func TestSystemCookieVerifiesForTheSystemUser(t *testing.T) {
	c := newClients(Config{
		AuthSecret: "shared-secret", CookieName: "nvda_session", SystemUID: "paper-engine",
	})
	raw := c.systemCookie()
	if raw == "" {
		t.Fatal("expected a cookie when a secret is configured")
	}
	name, value, ok := strings.Cut(raw, "=")
	if !ok || name != "nvda_session" {
		t.Fatalf("unexpected cookie shape %q", raw)
	}

	// Verify exactly as journal/auth.go's parseToken does.
	body, sig, ok := strings.Cut(value, ".")
	if !ok {
		t.Fatalf("malformed token %q", value)
	}
	if !hmac.Equal([]byte(sig), []byte(signPayload("shared-secret", body))) {
		t.Error("token does not verify with the shared secret")
	}
	decoded, err := b64.DecodeString(body)
	if err != nil {
		t.Fatalf("payload not base64url: %v", err)
	}
	var p sessionPayload
	if err := json.Unmarshal(decoded, &p); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	if p.UID != "paper-engine" {
		t.Errorf("the engine must identify as its system user, got %q", p.UID)
	}
	if p.Exp <= time.Now().Unix() {
		t.Error("token is already expired")
	}

	// A different secret must not verify — the credential is not a bearer string anyone can forge.
	if hmac.Equal([]byte(sig), []byte(signPayload("other-secret", body))) {
		t.Error("token verified under the wrong secret")
	}
}

// The credential reaches the journal and nowhere else.
func TestCredentialIsSentOnlyToTheJournal(t *testing.T) {
	var journalSaw, predictionSaw, analysisSaw string
	journal := captureHeaders(t, &journalSaw, map[string]any{"trade": map[string]any{"id": "t1"}})
	prediction := captureHeaders(t, &predictionSaw, map[string]any{"signal": nil})
	analysis := captureHeaders(t, &analysisSaw, map[string]any{"symbol": "NVDA", "source": "synthetic"})

	cfg := Config{
		JournalURL: journal.URL, PredictionURL: prediction.URL, AnalysisURL: analysis.URL,
		AuthSecret: "shared-secret", CookieName: "nvda_session", SystemUID: "paper-engine",
	}
	c := newClients(cfg)
	ctx := context.Background()

	if _, err := c.journalCreate(ctx, map[string]any{"ticker": "NVDA"}); err != nil {
		t.Fatalf("journalCreate: %v", err)
	}
	if _, err := c.predict(ctx, PaperCfg{Ticker: "NVDA", Timeframe: "1D", Horizon: 5}); err != nil {
		t.Fatalf("predict: %v", err)
	}
	if _, err := c.quote(ctx, "NVDA"); err != nil {
		t.Fatalf("quote: %v", err)
	}

	if !strings.HasPrefix(journalSaw, "nvda_session=") {
		t.Errorf("the journal must receive the credential, got %q", journalSaw)
	}
	if predictionSaw != "" {
		t.Errorf("the prediction service must not receive a credential, got %q", predictionSaw)
	}
	if analysisSaw != "" {
		t.Errorf("the analysis service must not receive a credential, got %q", analysisSaw)
	}
}

// With no AUTH_SECRET the engine sends nothing — the pre-existing behaviour, unchanged. The zero-key
// path must still boot and run; it simply cannot record, which the API reports rather than hides.
func TestNoSecretMeansNoCredential(t *testing.T) {
	var journalSaw string
	journal := captureHeaders(t, &journalSaw, map[string]any{"trade": map[string]any{"id": "t1"}})

	c := newClients(Config{JournalURL: journal.URL, CookieName: "nvda_session", SystemUID: "paper-engine"})
	if c.systemCookie() != "" {
		t.Error("no secret must mean no cookie")
	}
	if _, err := c.journalCreate(context.Background(), map[string]any{"ticker": "NVDA"}); err != nil {
		t.Fatalf("journalCreate: %v", err)
	}
	if journalSaw != "" {
		t.Errorf("expected no credential, got %q", journalSaw)
	}
}

// The book identity is explicit and honest: a system book, not the reader's, and it says whether it
// can record at all.
func TestBookIdentityIsNotPresentedAsTheUsers(t *testing.T) {
	store, err := openStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}

	withSecret := newAPI(Config{AuthSecret: "s"}, store, newClients(Config{}), nil).bookIdentity()
	if withSecret["owner"] != "system" || withSecret["perUser"] != false {
		t.Errorf("the interim book must declare itself a system book, got %v", withSecret)
	}
	if withSecret["recording"] != true {
		t.Errorf("with a secret the engine can record, got %v", withSecret["recording"])
	}
	label, _ := withSecret["label"].(string)
	if !strings.Contains(label, "validation engine") {
		t.Errorf("the label must name the validation engine, got %q", label)
	}
	note, _ := withSecret["note"].(string)
	for _, required := range []string{"not your own", "Nothing here executes"} {
		if !strings.Contains(note, required) {
			t.Errorf("the note must contain %q, got %q", required, note)
		}
	}

	without := newAPI(Config{}, store, newClients(Config{}), nil).bookIdentity()
	if without["recording"] != false {
		t.Errorf("with no secret the engine cannot record and must say so, got %v", without["recording"])
	}
}
