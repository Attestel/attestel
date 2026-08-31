package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// clients.go — read-only HTTP clients for the upstream analysis + gateway services. The journal
// only ever READS signals (a fresh quote to mark open trades, and the dashboard read to snapshot at
// entry). It never posts anything upstream and never touches a broker.

type quoteResp struct {
	Symbol    string   `json:"symbol"`
	Price     *float64 `json:"price"`
	ChangePct *float64 `json:"changePct"`
	AsOf      string   `json:"asOf"`
	Source    string   `json:"source"`
}

// dashResp is the slice of the gateway dashboard the journal snapshots at entry.
type dashResp struct {
	Regime map[string]any `json:"regime"`
	Read   struct {
		Structured map[string]any `json:"structured"`
	} `json:"read"`
}

func (s *Server) getJSON(ctx context.Context, u string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("GET %s -> %d", u, resp.StatusCode)
	}
	return json.Unmarshal(body, target)
}

// fetchQuote returns the current quote for a ticker (used to mark open trades). Errors are returned
// so callers can leave unrealized P&L blank rather than guess.
func (s *Server) fetchQuote(ctx context.Context, ticker string) (*quoteResp, error) {
	var q quoteResp
	u := fmt.Sprintf("%s/quote/%s", s.cfg.AnalysisURL, url.PathEscape(ticker))
	if err := s.getJSON(ctx, u, &q); err != nil {
		return nil, err
	}
	return &q, nil
}

// captureRead best-effort snapshots the analytical read live at entry. Returns nil on any failure —
// trade creation must never fail because the read couldn't be fetched.
func (s *Server) captureRead(ctx context.Context, ticker, timeframe string) *AttachedRead {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var d dashResp
	u := fmt.Sprintf("%s/api/dashboard/%s?timeframe=%s",
		s.cfg.GatewayURL, url.PathEscape(ticker), url.QueryEscape(timeframe))
	if err := s.getJSON(ctx, u, &d); err != nil {
		return nil
	}
	if d.Read.Structured == nil && d.Regime == nil {
		return nil
	}
	return &AttachedRead{
		Structured: d.Read.Structured,
		Regime:     d.Regime,
		CapturedAt: time.Now().UTC().Format(time.RFC3339),
	}
}

// quoteBatch fetches quotes for a set of tickers once each, memoized. A failed fetch maps to nil so
// the caller marks that trade's unrealized P&L as unavailable.
func (s *Server) quoteBatch(ctx context.Context, tickers map[string]bool) map[string]*quoteResp {
	out := map[string]*quoteResp{}
	for t := range tickers {
		q, err := s.fetchQuote(ctx, t)
		if err != nil {
			out[t] = nil
			continue
		}
		out[t] = q
	}
	return out
}
