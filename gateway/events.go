package main

// events.go — the ONE client for the events service (`services/events`, :8004, D-26).
//
// This file exists to be the single declaration site. `gateway` is one flat `package main`, so a
// second `func (s *Server) eventsGet` anywhere in the package is a compile error rather than a
// merge conflict — and a compile error is the wrong way to find out that two waves both needed an
// events client. Wave 2 Lane 2C (`feeds.go`) and Wave 3 Lane 3A both call this; neither declares
// it. `config.go:19`'s comment has pointed here since Wave 0.
//
// It returns RAW BYTES rather than a decoded `map[string]any`, which is the one design decision in
// the file. The events service returns several different envelopes — `{documents:…}`, `{events:…}`,
// `{macro:…}`, `{scheduled:…}`, each with its own `asOf` and `degraded` — and the callers know which
// one they asked for. Decoding to a generic map here would force every caller to re-assert a shape
// this function had already thrown away; handing back bytes lets each unmarshal into the struct it
// actually wants. `getJSON` in clients.go stays the right tool for the map-shaped upstreams.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// eventsResponseCap bounds one response. The corpus is paginated (`limit` is capped at MAX_LIMIT
// server-side), so a body larger than this means something is wrong upstream rather than that a
// caller asked for a lot.
const eventsResponseCap = 8 << 20 // 8 MiB

// errEventsUnconfigured is returned when no events service is configured. Callers treat it like any
// other upstream failure and fall back to seed or to an empty section — never to a fabricated
// answer. It is a distinct error so a caller that wants to omit a UI section entirely, rather than
// show it as degraded, can tell "not deployed" from "deployed and failing".
var errEventsUnconfigured = errors.New("events service not configured (EVENTS_URL is empty)")

// eventsGet does a GET against the events service and returns the response body verbatim.
//
// `path` is a service-relative path, with or without its leading slash: "events?ticker=NVDA",
// "/documents", "/health". It is NOT url-escaped here — callers build their own query strings, and
// escaping an already-built query would corrupt it.
//
// Fail-soft, like every other upstream client in this package: a transport error, a non-2xx, or an
// unreachable service comes back as an error the caller can degrade on. Nothing here retries — the
// events service does its own budget-aware backoff, and a gateway retry on top of it would multiply
// a rate limit rather than absorb it.
func (s *Server) eventsGet(ctx context.Context, path string) ([]byte, error) {
	base := strings.TrimRight(s.cfg.EventsURL, "/")
	if base == "" {
		return nil, errEventsUnconfigured
	}
	url := base + "/" + strings.TrimLeft(path, "/")

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("events unreachable: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, eventsResponseCap))
	if err != nil {
		return nil, fmt.Errorf("events %s: %w", url, err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("events %s -> %d: %s", url, resp.StatusCode, truncate(body, 200))
	}
	return body, nil
}
