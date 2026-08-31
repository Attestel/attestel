package main

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestPostgresAuthStateSurvivesReopen(t *testing.T) {
	url := os.Getenv("AUTH_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AUTH_TEST_DATABASE_URL is not set")
	}
	schema := fmt.Sprintf("test_auth_%d", time.Now().UnixNano())
	users, settings, err := openPostgresStores(t.TempDir(), url, schema)
	if err != nil {
		t.Fatalf("open PostgreSQL stores: %v", err)
	}
	created, err := users.CreateUser("Owner@Example.com", "long-enough-password")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	prefs := defaultSettings()
	prefs.ActiveTicker = "GOOGL"
	if err := settings.PutSettings(created.ID, prefs); err != nil {
		t.Fatalf("put settings: %v", err)
	}
	users.db.Close()

	reopenedUsers, reopenedSettings, err := openPostgresStores(t.TempDir(), url, schema)
	if err != nil {
		t.Fatalf("reopen PostgreSQL stores: %v", err)
	}
	defer func() {
		_, _ = reopenedUsers.db.ExecContext(context.Background(), `DROP SCHEMA "`+schema+`" CASCADE`)
		reopenedUsers.db.Close()
	}()
	got, ok, err := reopenedUsers.LookupByEmail("owner@example.com")
	if err != nil || !ok || got.ID != created.ID || !verifyPassword(got.PasswordHash, "long-enough-password") {
		t.Fatalf("user did not survive reopen: user=%+v ok=%v err=%v", got.public(), ok, err)
	}
	gotPrefs, err := reopenedSettings.ReadSettings(created.ID)
	if err != nil || gotPrefs.ActiveTicker != "GOOGL" {
		t.Fatalf("settings did not survive reopen: settings=%+v err=%v", gotPrefs, err)
	}
}
