package main

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestPostgresFeedbackSurvivesReopen(t *testing.T) {
	url := os.Getenv("FEEDBACK_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("FEEDBACK_TEST_DATABASE_URL is not set")
	}
	schema := fmt.Sprintf("test_feedback_%d", time.Now().UnixNano())
	store, err := openPostgresStore(t.TempDir(), url, schema)
	if err != nil {
		t.Fatalf("open PostgreSQL store: %v", err)
	}
	created, err := store.AddE(Feedback{Owner: "alice", Category: "idea", Message: "keep this", Rating: 5})
	if err != nil {
		t.Fatalf("add feedback: %v", err)
	}
	store.db.Close()

	reopened, err := openPostgresStore(t.TempDir(), url, schema)
	if err != nil {
		t.Fatalf("reopen PostgreSQL store: %v", err)
	}
	defer func() {
		_, _ = reopened.db.ExecContext(context.Background(), `DROP SCHEMA "`+schema+`" CASCADE`)
		reopened.db.Close()
	}()
	items := reopened.List()
	if len(items) != 1 || items[0].ID != created.ID || items[0].Owner != "alice" || items[0].Message != "keep this" {
		t.Fatalf("feedback did not survive reopen: %+v", items)
	}
}
