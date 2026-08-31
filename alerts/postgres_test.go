package main

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestPostgresAlertStateSurvivesReopen(t *testing.T) {
	url := os.Getenv("ALERTS_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("ALERTS_TEST_DATABASE_URL is not set")
	}
	schema := fmt.Sprintf("test_alerts_%d", time.Now().UnixNano())
	store, err := openPostgresStore(t.TempDir(), url, schema)
	if err != nil {
		t.Fatalf("open PostgreSQL store: %v", err)
	}
	rule, err := store.AddRuleE(Rule{UserID: "alice", Ticker: "NVDA", Type: "trend_flip", Active: true})
	if err != nil {
		t.Fatalf("add rule: %v", err)
	}
	event := Event{ID: "event-1", RuleID: rule.ID, UserID: "alice", Ticker: "NVDA", TS: time.Now().Unix()}
	if err := store.AppendEvent(event); err != nil {
		t.Fatalf("append event: %v", err)
	}
	if err := store.MarkEventReadE("alice", event.ID); err != nil {
		t.Fatalf("mark event read: %v", err)
	}
	monitor := monitorState{
		Markers: map[string]ThesisMarker{"thesis-1": {ThesisID: "thesis-1", UserID: "alice", Stale: true}},
		Queue:   []ResynthJob{{ID: "job-1", ThesisID: "thesis-1", UserID: "alice", State: resynthQueued}},
		Dropped: 2,
	}
	if err := store.saveMonitorPostgres(monitor); err != nil {
		t.Fatalf("save monitor state: %v", err)
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
	if rules := reopened.RulesForUser("alice"); len(rules) != 1 || rules[0].ID != rule.ID {
		t.Fatalf("rules did not survive reopen: %+v", rules)
	}
	events, unread, err := reopened.ListEventsE("alice", 10)
	if err != nil || len(events) != 1 || !events[0].Read || unread != 0 {
		t.Fatalf("events/read state did not survive: events=%+v unread=%d err=%v", events, unread, err)
	}
	loaded, found, err := reopened.loadMonitorPostgres()
	if err != nil || !found || loaded.Dropped != 2 || len(loaded.Queue) != 1 || !loaded.Markers["thesis-1"].Stale {
		t.Fatalf("monitor state did not survive: state=%+v found=%v err=%v", loaded, found, err)
	}
	if rules := reopened.RulesForUser("bob"); len(rules) != 0 {
		t.Fatalf("rule ownership leaked: %+v", rules)
	}
}
