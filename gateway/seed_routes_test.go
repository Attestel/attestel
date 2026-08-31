package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func callTickerHandler(t *testing.T, handler http.HandlerFunc, ticker string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/test/"+ticker, nil)
	req.SetPathValue("ticker", ticker)
	rec := httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

func TestFundamentalsDoesNotRelabelNVDASeedAsAnotherTicker(t *testing.T) {
	srv := &Server{}
	body := callTickerHandler(t, srv.handleFundamentals, "TSLA")
	if body["ticker"] != "TSLA" || body["hasSeed"] != false {
		t.Fatalf("unsupported ticker response = %#v", body)
	}
	if body["fundamentals"] != nil || body["accountingTraps"] != nil {
		t.Fatal("TSLA received NVDA seed fundamentals under the wrong ticker label")
	}
}

func TestCatalystsDoesNotRelabelNVDASeedAsAnotherTicker(t *testing.T) {
	srv := &Server{}
	body := callTickerHandler(t, srv.handleCatalysts, "GOOGL")
	if body["ticker"] != "GOOGL" || body["hasSeed"] != false {
		t.Fatalf("unsupported ticker response = %#v", body)
	}
	if body["roadmap"] != nil || body["events"] != nil {
		t.Fatal("GOOGL received NVDA seed catalysts under the wrong ticker label")
	}
}

func TestSeedRoutesStillServeNVDA(t *testing.T) {
	srv := &Server{}
	body := callTickerHandler(t, srv.handleFundamentals, "nvda")
	if body["hasSeed"] != true || body["fundamentals"] == nil {
		t.Fatalf("NVDA seed was not served: %#v", body)
	}
}
