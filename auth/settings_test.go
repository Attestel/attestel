package main

import "testing"

func TestSettingsDefaultAndLegacyActiveTicker(t *testing.T) {
	if got := defaultSettings().ActiveTicker; got != "NVDA" {
		t.Fatalf("default active ticker = %q, want NVDA", got)
	}
	if got := withDefaults(Settings{DefaultHorizon: 5, DefaultRiskPct: 1}).ActiveTicker; got != "NVDA" {
		t.Fatalf("legacy active ticker = %q, want NVDA", got)
	}
}

func TestPutSettingsNormalizesAndPersistsActiveTicker(t *testing.T) {
	store, err := openSettingsStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	in := defaultSettings()
	in.ActiveTicker = " brk-b "
	if err := store.PutSettings("user-1", in); err != nil {
		t.Fatalf("PutSettings: %v", err)
	}
	if got := store.GetSettings("user-1").ActiveTicker; got != "BRK-B" {
		t.Fatalf("saved active ticker = %q, want BRK-B", got)
	}
}

func TestPutSettingsRejectsInvalidActiveTicker(t *testing.T) {
	store, err := openSettingsStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	in := defaultSettings()
	in.ActiveTicker = "NVDA/../../"
	if err := store.PutSettings("user-1", in); err == nil {
		t.Fatal("PutSettings accepted invalid active ticker")
	}
}

func TestLegacySettingsWritePreservesSavedActiveTicker(t *testing.T) {
	store, err := openSettingsStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	in := defaultSettings()
	in.ActiveTicker = "MSFT"
	if err := store.PutSettings("user-1", in); err != nil {
		t.Fatal(err)
	}
	in.AllowShort = true
	in.ActiveTicker = "" // older frontend payload
	if err := store.PutSettings("user-1", in); err != nil {
		t.Fatal(err)
	}
	got := store.GetSettings("user-1")
	if got.ActiveTicker != "MSFT" || !got.AllowShort {
		t.Fatalf("legacy write = %#v, want MSFT with allowShort", got)
	}
}
