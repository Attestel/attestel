package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// research.go — the "qwen reads text" feature set:
//
//   * transcript analyzer  POST /api/transcript/{ticker}   (paste text or give a URL to fetch)
//   * thesis checks        run during the Brief (briefFor) + POST /api/theses/{id}/check
//   * news digest          GET /api/digest/{ticker}        (cluster + impact-score the feed)
//   * scenario mode        POST /api/scenario/{ticker}     (what-if grounded in seed facts)
//
// All LLM work happens in the llm service (with its own stub fallbacks); the gateway only
// assembles facts, fetches URLs, caches, and persists thesis-check results back to the journal.
// Everything stays on-demand — none of this runs in a polling loop (invariant #4).

const maxTranscriptFetchBytes = 2 << 20 // 2 MB cap on fetched transcript pages

// ---------------------------------------------------------------- transcript

type transcriptBody struct {
	Label string `json:"label"`
	Text  string `json:"text"`
	URL   string `json:"url"`
}

func (s *Server) handleTranscript(w http.ResponseWriter, r *http.Request) {
	ticker := strings.ToUpper(r.PathValue("ticker"))
	var body transcriptBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body: " + err.Error()})
		return
	}
	text := strings.TrimSpace(body.Text)
	if text == "" && body.URL != "" {
		fetched, err := s.fetchTranscriptURL(r.Context(), body.URL)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"error": "could not fetch transcript URL: " + err.Error() +
					" — paste the transcript text instead"})
			return
		}
		text = fetched
	}
	if text == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "provide transcript text or a url"})
		return
	}
	// Long transcripts mean several sequential model calls.
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	res, err := s.postJSON(ctx, s.cfg.LLMURL+"/transcript", map[string]any{
		"ticker": ticker, "label": body.Label, "text": text,
	})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleTranscriptHistory(w http.ResponseWriter, r *http.Request) {
	ticker := strings.ToUpper(r.PathValue("ticker"))
	res, err := s.getJSON(r.Context(), s.cfg.LLMURL+"/transcript/"+ticker)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// fetchTranscriptURL downloads a page and reduces it to plain text (best-effort — paywalled or
// JS-rendered pages won't yield usable text, and the caller says so to the user).
func (s *Server) fetchTranscriptURL(ctx context.Context, raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("invalid URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", s.cfg.SECUserAgent) // a real contact UA; some IR sites require one
	resp, err := s.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxTranscriptFetchBytes))
	if err != nil {
		return "", err
	}
	text := strings.TrimSpace(stripHTML(string(b)))
	if len(text) < 500 {
		return "", fmt.Errorf("page yielded almost no text (paywall or JS-rendered?)")
	}
	return text, nil
}

// stripHTML reduces an HTML page to readable text: drops script/style/nav blocks, strips tags,
// decodes common entities. Deliberately simple — stdlib only, good enough for IR pages.
func stripHTML(s string) string {
	for _, tag := range []string{"script", "style", "noscript", "svg", "header", "nav", "footer"} {
		for {
			start := strings.Index(strings.ToLower(s), "<"+tag)
			if start == -1 {
				break
			}
			end := strings.Index(strings.ToLower(s[start:]), "</"+tag+">")
			if end == -1 {
				s = s[:start]
				break
			}
			s = s[:start] + " " + s[start+end+len(tag)+3:]
		}
	}
	var b strings.Builder
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
			b.WriteRune(' ')
		case !inTag:
			b.WriteRune(r)
		}
	}
	out := b.String()
	for entity, repl := range map[string]string{
		"&amp;": "&", "&lt;": "<", "&gt;": ">", "&quot;": `"`, "&#39;": "'",
		"&apos;": "'", "&nbsp;": " ", "&mdash;": "—", "&ndash;": "–", "&rsquo;": "'",
	} {
		out = strings.ReplaceAll(out, entity, repl)
	}
	return strings.Join(strings.Fields(out), " ")
}

// ---------------------------------------------------------------- thesis checks

// patchJSON mirrors postJSON for PATCH (used to persist check results onto journal theses). It
// forwards the browser's session `cookie` so the journal applies per-user scoping + ownership.
func (s *Server) patchJSON(ctx context.Context, urlStr string, payload any, cookie string) (map[string]any, error) {
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, urlStr, bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("upstream %s -> %d: %s", urlStr, resp.StatusCode, truncate(body, 200))
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode %s: %w", urlStr, err)
	}
	return out, nil
}

// activeTheses lists the CALLER'S active theses for a ticker (best-effort; nil on any error). The
// browser's session `cookie` is forwarded so the journal returns only this user's theses.
func (s *Server) activeTheses(ctx context.Context, ticker, cookie string) []map[string]any {
	res, err := s.journalGet(ctx, s.cfg.JournalURL+"/theses?ticker="+url.QueryEscape(ticker)+"&status=active", cookie)
	if err != nil {
		return nil
	}
	raw, _ := res["theses"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, t := range raw {
		if m, ok := t.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// runThesisCheck asks the llm to evaluate one thesis against the supplied facts, then persists
// the result onto the thesis in the journal (best-effort). Returns the check response.
func (s *Server) runThesisCheck(ctx context.Context, ticker, thesisID, thesisText string, factCtx map[string]any, cookie string) map[string]any {
	res, err := s.postJSON(ctx, s.cfg.LLMURL+"/thesis-check", map[string]any{
		"ticker": ticker, "thesis": thesisText, "context": factCtx,
	})
	if err != nil {
		return map[string]any{"error": err.Error(), "thesisId": thesisID}
	}
	res["thesisId"] = thesisID
	if st, ok := res["structured"].(map[string]any); ok && thesisID != "" {
		check := map[string]any{
			"verdict":   st["verdict"],
			"summary":   st["summary"],
			"evidence":  st["evidence"],
			"watchFor":  st["watchFor"],
			"modelUsed": res["modelUsed"],
		}
		// The llm emits confidence in [0,1]; the journal stores the MODEL's check confidence as an
		// int 0..100 (PARALLEL_CONTRACTS §1.6). It used to be dropped here, which is why no stored
		// check has one. It lands ONLY on lastCheck — never on thesis.confidence, which is the
		// user's own conviction and is not the model's to overwrite.
		if c, ok := st["confidence"].(float64); ok {
			check["confidence"] = pctFromUnit(c)
		}
		_, _ = s.patchJSON(ctx, s.cfg.JournalURL+"/theses/"+thesisID, map[string]any{"lastCheck": check}, cookie)
	}
	return res
}

// pctFromUnit converts a model's [0,1] score to the int 0..100 the journal stores, clamped.
func pctFromUnit(v float64) int {
	n := int(v*100 + 0.5)
	if n < 0 {
		return 0
	}
	if n > 100 {
		return 100
	}
	return n
}

// thesisChecksFor runs checks for the CALLER'S active theses on a ticker (cap 3 — sequential model
// calls). The fact context is built only when the user actually has theses. `cookie` forwards the
// session to the journal so both the read and the persisted result scope to the user. Best-effort.
func (s *Server) thesisChecksFor(ctx context.Context, ticker, cookie string) []map[string]any {
	theses := s.activeTheses(ctx, ticker, cookie)
	if len(theses) == 0 {
		return nil
	}
	if len(theses) > 3 {
		theses = theses[:3]
	}
	factCtx := s.briefContextFor(ctx, ticker)
	var out []map[string]any
	for _, t := range theses {
		id, _ := t["id"].(string)
		text, _ := t["text"].(string)
		if strings.TrimSpace(text) == "" {
			continue
		}
		check := s.runThesisCheck(ctx, ticker, id, text, factCtx, cookie)
		check["thesisText"] = text
		out = append(out, check)
	}
	return out
}

// handleThesisCheck re-runs the evidence check for ONE thesis on demand.
func (s *Server) handleThesisCheck(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cookie := r.Header.Get("Cookie") // forwarded so the journal scopes to this user (guests: none)
	ctx, cancel := context.WithTimeout(r.Context(), 130*time.Second)
	defer cancel()

	// find the thesis among the CALLER'S theses (journal has list-only; the list is small).
	res, err := s.journalGet(ctx, s.cfg.JournalURL+"/theses", cookie)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "journal unreachable: " + err.Error()})
		return
	}
	var ticker, text string
	if raw, ok := res["theses"].([]any); ok {
		for _, t := range raw {
			if m, ok := t.(map[string]any); ok && m["id"] == id {
				ticker, _ = m["ticker"].(string)
				text, _ = m["text"].(string)
				break
			}
		}
	}
	if ticker == "" {
		// Not among the caller's theses (or a guest with none). Ownership is also enforced by the
		// journal on the persist step; this is the friendly 404.
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "thesis not found"})
		return
	}
	factCtx := s.briefContextFor(ctx, ticker) // same compact fact set the brief uses
	writeJSON(w, http.StatusOK, s.runThesisCheck(ctx, ticker, id, text, factCtx, cookie))
}

// ------------------------------------------------- thesis pass-throughs (§1.9)

// proxyJournal forwards one request to the journal verbatim — method, body, and the browser's session
// cookie — and copies the journal's status and JSON body straight back.
//
// Unlike journalGet, this deliberately does NOT collapse upstream failures into a generic gateway
// error: the caller needs the journal's own status and `code` (404 for an id that is not theirs, 409
// `active_thesis_cap`, 400 `validation_failed` with the offending field) to render an explained limit
// instead of a shrug. Ownership is still resolved entirely by the journal from the forwarded session
// — the gateway adds no authority of its own. Fail-soft: an unreachable journal is a readable 502.
func (s *Server) proxyJournal(w http.ResponseWriter, r *http.Request, method, path string) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	var body io.Reader
	if r.Body != nil {
		body = r.Body
	}
	req, err := http.NewRequestWithContext(ctx, method, s.cfg.JournalURL+path, body)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "bad journal request: " + err.Error()})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if cookie := r.Header.Get("Cookie"); cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "journal unreachable: " + err.Error()})
		return
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(out)
}

// handleThesisGet fetches ONE thesis (the Research workspace deep-links straight to a thesis, so it
// should not have to list everything to render one).
func (s *Server) handleThesisGet(w http.ResponseWriter, r *http.Request) {
	s.proxyJournal(w, r, http.MethodGet, "/theses/"+url.PathEscape(r.PathValue("id")))
}

// handleThesisVersions returns the thesis's append-only revision history, newest first.
func (s *Server) handleThesisVersions(w http.ResponseWriter, r *http.Request) {
	path := "/theses/" + url.PathEscape(r.PathValue("id")) + "/versions"
	if q := r.URL.RawQuery; q != "" {
		path += "?" + q
	}
	s.proxyJournal(w, r, http.MethodGet, path)
}

// handleThesisReviewCheckpoint advances the user's "reviewed up to here" boundary. Explicit user
// action only: nothing on a timer, in the alert evaluator, or on page load may call this.
func (s *Server) handleThesisReviewCheckpoint(w http.ResponseWriter, r *http.Request) {
	s.proxyJournal(w, r, http.MethodPost, "/theses/"+url.PathEscape(r.PathValue("id"))+"/review-checkpoint")
}

// ---------------------------------------------------------------- news digest

func (s *Server) handleDigest(w http.ResponseWriter, r *http.Request) {
	ticker := strings.ToUpper(r.PathValue("ticker"))
	key := "digest:" + ticker
	if r.URL.Query().Get("refresh") == "" {
		if cached, ok := s.cache.get(key); ok {
			w.Header().Set("X-Cache", "HIT")
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(cached)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 130*time.Second)
	defer cancel()
	items, source := s.newsFor(ctx, ticker)
	res, err := s.postJSON(ctx, s.cfg.LLMURL+"/digest", map[string]any{
		"ticker": ticker, "items": items,
	})
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ticker": ticker, "available": false, "error": err.Error(), "newsSource": source})
		return
	}
	res["available"] = true
	res["items"] = items // the digest references headlines by index — ship them together
	res["newsSource"] = source
	buf, _ := json.Marshal(res)
	s.cache.set(key, buf, s.cfg.DigestTTL)
	w.Header().Set("X-Cache", "MISS")
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(buf)
}

// ---------------------------------------------------------------- scenario

type scenarioBody struct {
	Question string `json:"question"`
}

func (s *Server) handleScenario(w http.ResponseWriter, r *http.Request) {
	ticker := strings.ToUpper(r.PathValue("ticker"))
	var body scenarioBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Question) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "body must be {\"question\": \"...\"}"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 130*time.Second)
	defer cancel()

	// Grounding facts: the supply-chain/roadmap/risks seed (NVDA-verified) + live regime + news.
	factCtx := map[string]any{"ticker": ticker}
	if isSeedTicker(ticker) {
		factCtx["suppliers"] = seedData["suppliers"]
		factCtx["roadmap"] = seedData["roadmap"]
		factCtx["risks"] = seedData["risks"]
	}
	if res, err := s.getJSON(ctx, s.cfg.AnalysisURL+"/analysis/"+ticker+"?timeframe=1D"); err == nil {
		if reg, ok := res["regime"].(map[string]any); ok {
			factCtx["regime"] = trimRegime(reg)
		}
	}
	if items, _ := s.newsFor(ctx, ticker); len(items) > 0 {
		if len(items) > 6 {
			items = items[:6]
		}
		factCtx["news"] = items
	}

	res, err := s.postJSON(ctx, s.cfg.LLMURL+"/scenario", map[string]any{
		"ticker": ticker, "question": body.Question, "context": factCtx,
	})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// ---------------------------------------------------------------- keyless RSS news (Google News)

type rssFeed struct {
	Channel struct {
		Items []struct {
			Title   string `xml:"title"`
			Link    string `xml:"link"`
			PubDate string `xml:"pubDate"`
			Source  struct {
				Name string `xml:",chardata"`
			} `xml:"source"`
		} `xml:"item"`
	} `xml:"channel"`
}

// fetchRSSNews pulls Google News RSS for the ticker (keyless). Additive, fail-soft; items carry
// no provider sentiment (the digest's language read covers that).
func (s *Server) fetchRSSNews(ctx context.Context, ticker string, limit int) ([]map[string]any, error) {
	q := url.QueryEscape(ticker + " stock")
	u := "https://news.google.com/rss/search?q=" + q + "&hl=en-US&gl=US&ceid=US:en"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", s.cfg.SECUserAgent)
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("rss HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var feed rssFeed
	if err := xml.Unmarshal(b, &feed); err != nil {
		return nil, err
	}
	var out []map[string]any
	for _, it := range feed.Channel.Items {
		if len(out) >= limit {
			break
		}
		pub := it.PubDate
		if t, err := time.Parse(time.RFC1123, it.PubDate); err == nil {
			pub = t.UTC().Format(time.RFC3339)
		} else if t, err := time.Parse(time.RFC1123Z, it.PubDate); err == nil {
			pub = t.UTC().Format(time.RFC3339)
		}
		src := it.Source.Name
		if src == "" {
			src = "rss"
		}
		out = append(out, map[string]any{
			"headline": strings.TrimSpace(it.Title), "source": src, "url": it.Link,
			"publishedAt": pub, "sentiment": nil, "category": "news",
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("rss returned no items")
	}
	return out, nil
}
