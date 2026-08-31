package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPostgresTradeStoreSurvivesReopen(t *testing.T) {
	url := os.Getenv("JOURNAL_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("JOURNAL_TEST_DATABASE_URL is not set")
	}
	schema := fmt.Sprintf("test_journal_%d", time.Now().UnixNano())
	store, err := openPostgresTradeStore(t.TempDir(), url, schema)
	if err != nil {
		t.Fatalf("open PostgreSQL store: %v", err)
	}
	created, err := store.Add("paper-engine", Trade{
		Ticker: "NVDA", Side: "long", Status: "open", Mode: "paper", Origin: "signal",
		Entry: Entry{Date: "2026-08-25", Price: 180, Size: 10, Timeframe: "1D"},
	})
	if err != nil {
		t.Fatalf("add trade: %v", err)
	}
	store.db.Close()

	reopened, err := openPostgresTradeStore(t.TempDir(), url, schema)
	if err != nil {
		t.Fatalf("reopen PostgreSQL store: %v", err)
	}
	defer func() {
		_, _ = reopened.db.ExecContext(context.Background(), `DROP SCHEMA "`+schema+`" CASCADE`)
		reopened.db.Close()
	}()
	got, ok, err := reopened.Get("paper-engine", created.ID)
	if err != nil || !ok {
		t.Fatalf("get after reopen: ok=%v err=%v", ok, err)
	}
	if got.Mode != "paper" || got.Ticker != "NVDA" {
		t.Fatalf("wrong persisted trade: %+v", got)
	}
	if rows, err := reopened.List("another-user"); err != nil || len(rows) != 0 {
		t.Fatalf("user partition leaked: rows=%v err=%v", rows, err)
	}
}

func TestPostgresResearchDocumentsAndAnalyticsSurviveReopen(t *testing.T) {
	url := os.Getenv("JOURNAL_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("JOURNAL_TEST_DATABASE_URL is not set")
	}
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "alice"), 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := []byte(`[{"id":"thesis-1","ticker":"NVDA"}]`)
	if err := os.WriteFile(filepath.Join(base, "alice", "theses.json"), legacy, 0o644); err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("test_journal_docs_%d", time.Now().UnixNano())
	store, err := openPostgresTradeStore(base, url, schema)
	if err != nil {
		t.Fatalf("open PostgreSQL store: %v", err)
	}
	docs := newDocumentRepository(store.db, store.schema, base)
	loaded, found, err := docs.load("alice", "theses", filepath.Join(base, "alice", "theses.json"))
	if err != nil || !found || !json.Valid(loaded) {
		t.Fatalf("legacy document import: found=%v err=%v payload=%s", found, err, loaded)
	}
	if err := docs.saveMany("alice", map[string]any{
		"evidence":       []map[string]any{{"id": "evidence-1"}},
		"evidence_links": []map[string]any{{"id": "link-1", "evidenceId": "evidence-1"}},
	}); err != nil {
		t.Fatalf("save related documents: %v", err)
	}
	event := analyticsEvent{Event: "thesis_created", EventID: "event-1", At: time.Now().Unix(), SchemaVersion: 1}
	if err := docs.appendAnalytics(event); err != nil {
		t.Fatalf("append analytics: %v", err)
	}
	store.db.Close()

	reopened, err := openPostgresTradeStore(t.TempDir(), url, schema)
	if err != nil {
		t.Fatalf("reopen PostgreSQL store: %v", err)
	}
	defer func() {
		_, _ = reopened.db.ExecContext(context.Background(), `DROP SCHEMA "`+schema+`" CASCADE`)
		reopened.db.Close()
	}()
	reopenedDocs := newDocumentRepository(reopened.db, reopened.schema, t.TempDir())
	for _, collection := range []string{"theses", "evidence", "evidence_links"} {
		payload, ok, err := reopenedDocs.load("alice", collection, filepath.Join(t.TempDir(), "missing"))
		if err != nil || !ok || !json.Valid(payload) {
			t.Fatalf("%s did not survive reopen: found=%v err=%v payload=%s", collection, ok, err, payload)
		}
	}
	events, err := reopenedDocs.analyticsEvents()
	if err != nil || len(events) != 1 || events[0].EventID != "event-1" {
		t.Fatalf("analytics did not survive reopen: events=%v err=%v", events, err)
	}
	owners, err := reopenedDocs.userIDs("theses", "theses.json")
	if err != nil || len(owners) != 1 || owners[0] != "alice" {
		t.Fatalf("document owner enumeration: owners=%v err=%v", owners, err)
	}
}
