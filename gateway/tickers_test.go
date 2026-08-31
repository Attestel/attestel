package main

import "testing"

func TestSearchTickersRanksAndCapsResults(t *testing.T) {
	candidates := []Ticker{
		{Symbol: "MS", Name: "Morgan Sample"},
		{Symbol: "MSFT", Name: "Microsoft Corp"},
		{Symbol: "MSTR", Name: "Strategy"},
		{Symbol: "XMIC", Name: "Example Microsoft Holdings"},
	}

	got := searchTickers(candidates, "ms", 3)
	if len(got) != 3 {
		t.Fatalf("len(searchTickers) = %d, want 3", len(got))
	}
	want := []string{"MS", "MSFT", "MSTR"}
	for i, symbol := range want {
		if got[i].Symbol != symbol {
			t.Errorf("result %d = %q, want %q", i, got[i].Symbol, symbol)
		}
	}
}

func TestSearchTickersMatchesCompanyName(t *testing.T) {
	candidates := []Ticker{
		{Symbol: "AAPL", Name: "Apple Inc"},
		{Symbol: "MSFT", Name: "Microsoft Corp"},
	}

	got := searchTickers(candidates, "micro", 7)
	if len(got) != 1 || got[0].Symbol != "MSFT" {
		t.Fatalf("searchTickers by name = %#v, want MSFT", got)
	}
}

func TestSearchTickersPrefersCuratedCompanyForBroadName(t *testing.T) {
	candidates := append(offlineTickerDirectory(nil),
		Ticker{Symbol: "HOLO", Name: "MicroCloud Hologram Inc."},
		Ticker{Symbol: "MCHP", Name: "Microchip Technology Inc."},
	)

	got := searchTickers(candidates, "micro", 7)
	if len(got) == 0 || got[0].Symbol != "MSFT" {
		t.Fatalf("first broad name result = %#v, want MSFT", got)
	}
}

func TestOfflineTickerDirectoryKeepsConfiguredFirstAndDedupes(t *testing.T) {
	configured := []Ticker{{Symbol: "TSLA", Name: "Tesla"}, {Symbol: "SHOP", Name: "Shopify"}}
	got := offlineTickerDirectory(configured)
	if len(got) < 7 {
		t.Fatalf("offlineTickerDirectory returned %d entries, want at least 7", len(got))
	}
	if got[0].Symbol != "TSLA" || got[1].Symbol != "SHOP" {
		t.Fatalf("configured entries not first: %#v", got[:2])
	}
	seenTSLA := 0
	for _, ticker := range got {
		if ticker.Symbol == "TSLA" {
			seenTSLA++
		}
	}
	if seenTSLA != 1 {
		t.Fatalf("TSLA appears %d times, want 1", seenTSLA)
	}
}
