package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// getJSON does a GET and decodes JSON into a generic map.
func (s *Server) getJSON(ctx context.Context, url string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("upstream %s -> %d: %s", url, resp.StatusCode, truncate(body, 200))
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode %s: %w", url, err)
	}
	return out, nil
}

// postJSON does a POST with a JSON body and decodes the JSON response.
func (s *Server) postJSON(ctx context.Context, url string, payload any) (map[string]any, error) {
	return postJSONWith(ctx, s.http, url, payload)
}

// postJSONWith is postJSON against a CALLER-SUPPLIED client. The shared `s.http` carries a
// 130-second `Timeout`, which caps an exchange regardless of its context — right for every upstream
// in this service except the analyst pipeline, whose runs are bounded by their context and can take
// minutes (see `analystjobs.go::postAnalyst`).
func postJSONWith(ctx context.Context, client *http.Client, url string, payload any) (map[string]any, error) {
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("upstream %s -> %d: %s", url, resp.StatusCode, truncate(body, 200))
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode %s: %w", url, err)
	}
	return out, nil
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n])
	}
	return string(b)
}
