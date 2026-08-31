package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// evidence.go — the gateway half of the evidence layer. Contract: PARALLEL_CONTRACTS.md §2.8.
//
// Two jobs, and the second one is the reason this file exists at all:
//
//  1. PASS-THROUGH. The eight `journal` evidence/link routes are proxied under /api/evidence… with the
//     session cookie forwarded, and the journal's status code and error `code` passed through
//     UNCHANGED — `provenance_incomplete`, `evidence_linked` and `immutable_field` each drive a
//     different thing in the UI, so collapsing them into a generic 502 would destroy the surface.
//
//  2. PROVENANCE COMPOSITION — POST /api/evidence/from-fact. The browser sends a SELECTOR ("the RSI on
//     the 1D chart", "this headline", "quote 3 of the Q1 call") and NEVER provenance. The gateway
//     re-resolves that selector against the data it just computed or fetched, and stamps `provider`,
//     `dataState`, `sourceTime`, `attribution` and the excerpt itself server-side.
//
// Why a selector and not a record: the client is the one place that cannot be trusted about where a
// fact came from. If the browser could post `{"provider":"sec-edgar","dataState":"live"}` next to text
// it typed, every provenance badge in the product would be decoration. Client-supplied provenance is
// not "ignored" here by policy — the request struct has no fields for it, so it is dropped at decode.
//
// Two D-08 rules are enforced structurally rather than by review:
//
//   - the gateway's URL-fetch-and-strip path (research.go fetchTranscriptURL) has no route into this
//     file: a transcript excerpt can ONLY be resolved out of a STORED transcript snapshot owned by
//     the llm service, and the returned quote is the snapshot's own bytes;
//   - no excerpt anywhere is taken from the request body.

// evidenceProvenanceNote is shown by the save dialog above the record preview. It says what the
// server will attach, so "show exactly what will be stored" is not a client-side promise.
const evidenceProvenanceNote = "Provenance (source, provider, capture time, live/seed/synthetic state) is " +
	"stamped by the server from the data it resolved, not from this page."

// registerEvidenceRoutes mounts the evidence surface. One call from main.go; every route lives here.
func (s *Server) registerEvidenceRoutes(mux *http.ServeMux) {
	// Literal paths beat wildcards in net/http's pattern precedence, so /evidence/links and
	// /evidence/from-fact win over /evidence/{id}.
	mux.HandleFunc("GET /api/evidence", s.handleJournalProxy)
	mux.HandleFunc("POST /api/evidence", s.handleJournalProxy)
	mux.HandleFunc("PATCH /api/evidence/{id}", s.handleJournalProxy)
	mux.HandleFunc("DELETE /api/evidence/{id}", s.handleJournalProxy)
	mux.HandleFunc("GET /api/evidence/links", s.handleJournalProxy)
	mux.HandleFunc("POST /api/evidence/links", s.handleJournalProxy)
	mux.HandleFunc("PATCH /api/evidence/links/{id}", s.handleJournalProxy)
	mux.HandleFunc("DELETE /api/evidence/links/{id}", s.handleJournalProxy)
	mux.HandleFunc("POST /api/evidence/from-fact", s.handleEvidenceFromFact)
}

// ---------------------------------------------------------------- pass-through

// handleJournalProxy forwards the request to the journal with the session cookie and copies the
// upstream status + body back verbatim. Fail-soft: an unreachable journal is a 502 with a readable
// reason, never a fabricated success.
//
// PATH-AGNOSTIC, and named for that: it strips `/api` and forwards whatever is left, so it serves
// `/api/evidence*` here and `/api/subscriptions*` + `/api/event-state` from subscriptions.go. It
// was called `handleEvidenceProxy` through Wave 0; Lane 1A flagged the rename as the integrator's
// call, and the second caller is what settled it — a shared helper named after one of its two
// callers is a comment that lies.
func (s *Server) handleJournalProxy(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api")
	target := s.cfg.JournalURL + path
	if q := r.URL.RawQuery; q != "" {
		target += "?" + q
	}

	var body io.Reader
	if r.Body != nil {
		buf, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "could not read request body"})
			return
		}
		body = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, body)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if c := r.Header.Get("Cookie"); c != "" {
		req.Header.Set("Cookie", c)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": "journal unreachable: " + err.Error(), "code": "journal_unreachable"})
		return
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(out)
}

// ---------------------------------------------------------------- from-fact

// fromFactBody is the ONLY shape the browser may send. Note what is absent by design: provider,
// dataState, attribution, sourceTime, sourceUrl, sourceRef, excerpt, fact, capturedAt, inference. A
// client cannot describe provenance because there is nowhere to put it.
type fromFactBody struct {
	Ticker    string         `json:"ticker"`
	Kind      string         `json:"kind"`
	Timeframe string         `json:"timeframe"`
	Selector  map[string]any `json:"selector"`

	// The user's own contribution, which is the one thing they DO own.
	Note string `json:"note"`

	// Optional one-shot link, so "save to thesis" is a single request.
	ThesisID     string  `json:"thesisId"`
	Bearing      string  `json:"bearing"`
	ThesisItemID *string `json:"thesisItemId"`

	// CaptureNew forces a second record for the same upstream item (a deliberate re-capture at a
	// later time) instead of returning the existing one.
	CaptureNew bool `json:"captureNew"`
}

// resolvedEvidence is the server-built record. It maps 1:1 onto the journal's EvidenceRecord input.
type resolvedEvidence struct {
	Ticker      string         `json:"ticker"`
	Kind        string         `json:"kind"`
	Title       string         `json:"title"`
	Excerpt     string         `json:"excerpt"`
	Fact        map[string]any `json:"fact,omitempty"`
	Note        string         `json:"note"`
	Provider    string         `json:"provider"`
	SourceURL   string         `json:"sourceUrl"`
	SourceRef   map[string]any `json:"sourceRef,omitempty"`
	SourceTime  int64          `json:"sourceTime"`
	DataState   string         `json:"dataState"`
	Attribution string         `json:"attribution"`
}

// evidenceResolveError carries an HTTP status + machine code out of a resolver.
type evidenceResolveError struct {
	status int
	code   string
	msg    string
}

func (e *evidenceResolveError) Error() string { return e.msg }

func resolveErr(status int, code, msg string) *evidenceResolveError {
	return &evidenceResolveError{status: status, code: code, msg: msg}
}

func (s *Server) handleEvidenceFromFact(w http.ResponseWriter, r *http.Request) {
	id := s.identityFrom(r)
	if id.userID == "" {
		// Evidence is user-owned; there is no anonymous capture. 401 so the frontend routes to
		// sign-in rather than silently discarding what the user tried to save.
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "sign in required"})
		return
	}

	var body fromFactBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<18)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "invalid body: " + err.Error(), "code": "invalid_json"})
		return
	}
	body.Ticker = strings.ToUpper(strings.TrimSpace(body.Ticker))
	if body.Ticker == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "ticker is required", "code": "validation_failed"})
		return
	}
	body.Timeframe = normalizeTimeframe(body.Timeframe)

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	rec, rerr := s.resolveEvidence(ctx, body)
	if rerr != nil {
		writeJSON(w, rerr.status, map[string]any{"error": rerr.msg, "code": rerr.code})
		return
	}
	rec.Note = body.Note

	payload := map[string]any{
		"ticker": rec.Ticker, "kind": rec.Kind, "title": rec.Title, "excerpt": rec.Excerpt,
		"note": rec.Note, "provider": rec.Provider, "sourceUrl": rec.SourceURL,
		"sourceTime": rec.SourceTime, "dataState": rec.DataState, "attribution": rec.Attribution,
		"captureNew": body.CaptureNew,
	}
	if rec.Fact != nil {
		payload["fact"] = rec.Fact
	}
	if rec.SourceRef != nil {
		payload["sourceRef"] = rec.SourceRef
	}
	if body.ThesisID != "" {
		if !validBearing(body.Bearing) {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "bearing must be supports, contradicts, or context", "code": "validation_failed"})
			return
		}
		payload["link"] = map[string]any{
			"thesisId": body.ThesisID, "bearing": body.Bearing,
			"thesisItemId": body.ThesisItemID, "note": body.Note,
		}
	}

	status, out, err := s.journalPost(ctx, s.cfg.JournalURL+"/evidence", payload, id.cookie)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": "journal unreachable: " + err.Error(), "code": "journal_unreachable"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(out)
}

func validBearing(b string) bool {
	return b == "supports" || b == "contradicts" || b == "context"
}

// journalPost POSTs JSON to the journal with the caller's cookie and returns the raw status + body so
// the journal's own error codes survive the hop.
func (s *Server) journalPost(ctx context.Context, url string, payload any, cookie string) (int, []byte, error) {
	buf, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(buf))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, out, nil
}

// resolveEvidence dispatches on kind. Every branch resolves against data the SERVER holds.
func (s *Server) resolveEvidence(ctx context.Context, b fromFactBody) (*resolvedEvidence, *evidenceResolveError) {
	switch b.Kind {
	case "deterministic_fact":
		return s.resolveDeterministicFact(ctx, b)
	case "chart_context":
		return s.resolveChartContext(ctx, b)
	case "fundamental_fact":
		return s.resolveFundamentalFact(ctx, b)
	case "news", "filing":
		return s.resolveNewsItem(ctx, b)
	case "transcript_excerpt", "management_statement":
		return s.resolveTranscriptExcerpt(ctx, b)
	case "catalyst":
		return s.resolveCatalyst(b)
	case "user_note":
		return s.resolveUserNote(ctx, b)
	}
	return nil, resolveErr(http.StatusBadRequest, "unknown_kind", "unknown evidence kind: "+b.Kind)
}

// ---- deterministic facts (analysis) ----

// savableIndicators are the latest-row indicator values, with the unit each carries. A closed list:
// the save dialog offers exactly these, and a selector outside the list is refused rather than
// resolved by reflection into whatever the analysis service happens to return.
var savableIndicators = map[string]struct{ label, unit string }{
	"rsi":      {"RSI(14)", ""},
	"macdHist": {"MACD histogram", ""},
	"sma50":    {"SMA(50)", "USD"},
	"sma200":   {"SMA(200)", "USD"},
	"ema20":    {"EMA(20)", "USD"},
	"atr":      {"ATR(14)", "USD"},
	"stochK":   {"Stochastic %K", ""},
	"stochD":   {"Stochastic %D", ""},
	"adx":      {"ADX(14)", ""},
	"vwap":     {"VWAP", "USD"},
}

// savableRegimeFields are the plain-language regime classifications.
var savableRegimeFields = map[string]string{
	"trend":          "Trend",
	"shortTermTrend": "Short-term trend",
	"momentum":       "Momentum",
	"macdState":      "MACD state",
	"volatility":     "Volatility",
	"priceVsBands":   "Price vs Bollinger bands",
	"volume":         "Volume",
	"trendStrength":  "Trend strength",
	"stochastic":     "Stochastic",
	"volumeTrend":    "Volume trend",
	"vwapState":      "VWAP state",
	"lastClose":      "Last close",
}

// analysisSnapshot is one analysis call plus the provenance derived from it.
type analysisSnapshot struct {
	regime     map[string]any
	indicators map[string]any
	price      map[string]any
	dataState  string // synthetic when the bars are synthetic, else live
	priceSrc   string // tiingo | yfinance | synthetic
	asOf       string
	sourceTime int64
}

func (s *Server) analysisSnapshotFor(ctx context.Context, ticker, timeframe string) (*analysisSnapshot, *evidenceResolveError) {
	res, err := s.getJSON(ctx, s.cfg.AnalysisURL+"/analysis/"+ticker+"?timeframe="+timeframe)
	if err != nil {
		return nil, resolveErr(http.StatusBadGateway, "analysis_unreachable",
			"the analysis service could not be reached, so this fact cannot be captured with honest provenance: "+err.Error())
	}
	snap := &analysisSnapshot{priceSrc: asString(res["source"])}
	snap.regime, _ = res["regime"].(map[string]any)
	if ind, ok := res["indicators"].(map[string]any); ok {
		snap.indicators, _ = ind["latest"].(map[string]any)
	}
	snap.price, _ = res["price"].(map[string]any)
	if synth, _ := res["sourceIsSynthetic"].(bool); synth {
		snap.dataState = "synthetic"
	} else {
		snap.dataState = "live"
	}
	if snap.price != nil {
		snap.asOf = asString(snap.price["asOf"])
		snap.sourceTime = parseEvidenceTime(snap.asOf)
	}
	return snap, nil
}

// priceProvenance describes WHERE the numbers came from. The provider is `analysis` because the value
// is a computation, and the price feed behind it is named in the attribution — conflating the two
// would hide either the computation or its input.
func priceProvenance(snap *analysisSnapshot) string {
	src := snap.priceSrc
	if src == "" {
		src = "unknown"
	}
	if snap.dataState == "synthetic" {
		return "Computed by the analysis service from SYNTHETIC price data (offline demo bars — not market data)"
	}
	return "Computed by the analysis service from " + src + " price data"
}

// factAsOfDate reduces an ISO instant to YYYY-MM-DD for fact.asOf, keeping the full instant in
// sourceTime. A daily bar's identity is its date; intraday keeps the time in sourceTime.
func factAsOfDate(asOf string) string {
	if len(asOf) >= 10 {
		return asOf[:10]
	}
	return asOf
}

func (s *Server) resolveDeterministicFact(ctx context.Context, b fromFactBody) (*resolvedEvidence, *evidenceResolveError) {
	field := strings.TrimSpace(asString(b.Selector["field"]))
	if field == "" {
		return nil, resolveErr(http.StatusBadRequest, "validation_failed", "selector.field is required")
	}
	snap, rerr := s.analysisSnapshotFor(ctx, b.Ticker, b.Timeframe)
	if rerr != nil {
		return nil, rerr
	}
	if snap.asOf == "" {
		// fact.asOf is REQUIRED for this kind. Without a bar timestamp the capture would be a
		// timeless-looking number, which §2.5 exists to prevent — so refuse instead of stamping now().
		return nil, resolveErr(http.StatusBadGateway, "provenance_incomplete",
			"the analysis service returned no bar timestamp, so this value cannot be dated")
	}

	var label, unit string
	var value any
	if meta, ok := savableIndicators[field]; ok {
		if snap.indicators == nil {
			return nil, resolveErr(http.StatusBadGateway, "provenance_incomplete", "no indicator values available")
		}
		v, present := snap.indicators[field]
		if !present || v == nil {
			return nil, resolveErr(http.StatusUnprocessableEntity, "value_unavailable",
				meta.label+" is not available on this timeframe yet (not enough bars)")
		}
		label, unit, value = meta.label, meta.unit, v
	} else if lbl, ok := savableRegimeFields[field]; ok {
		if snap.regime == nil {
			return nil, resolveErr(http.StatusBadGateway, "provenance_incomplete", "no regime available")
		}
		v, present := snap.regime[field]
		if !present || v == nil {
			return nil, resolveErr(http.StatusUnprocessableEntity, "value_unavailable",
				lbl+" is not available on this timeframe")
		}
		label, value = lbl, v
		if field == "lastClose" {
			unit = "USD"
		}
	} else {
		return nil, resolveErr(http.StatusBadRequest, "unknown_selector",
			"selector.field is not a savable deterministic value: "+field)
	}

	return &resolvedEvidence{
		Ticker: b.Ticker,
		Kind:   "deterministic_fact",
		Title:  fmt.Sprintf("%s · %s = %v", b.Ticker, label, value),
		Fact: map[string]any{
			"key": field, "value": value, "unit": unit,
			"asOf": factAsOfDate(snap.asOf), "timeframe": b.Timeframe,
		},
		Provider:    "analysis",
		SourceTime:  snap.sourceTime,
		DataState:   snap.dataState,
		Attribution: priceProvenance(snap),
	}, nil
}

// ---- chart context ----

var savableConfluenceFields = map[string]string{
	"trendAlignment": "Trend alignment across timeframes",
	"conflicts":      "Cross-timeframe conflicts",
	"note":           "Confluence note",
}

func (s *Server) resolveChartContext(ctx context.Context, b fromFactBody) (*resolvedEvidence, *evidenceResolveError) {
	// (a) a computed support/resistance level.
	if lvl := strings.TrimSpace(asString(b.Selector["level"])); lvl != "" {
		if lvl != "support" && lvl != "resistance" {
			return nil, resolveErr(http.StatusBadRequest, "unknown_selector", "selector.level must be support or resistance")
		}
		snap, rerr := s.analysisSnapshotFor(ctx, b.Ticker, b.Timeframe)
		if rerr != nil {
			return nil, rerr
		}
		levels, _ := snap.regime["keyLevels"].(map[string]any)
		arr, _ := levels[lvl].([]any)
		idx := selectorIndex(b.Selector["index"])
		if idx < 0 || idx >= len(arr) {
			return nil, resolveErr(http.StatusUnprocessableEntity, "value_unavailable",
				"that "+lvl+" level is no longer present in the computed levels")
		}
		return &resolvedEvidence{
			Ticker: b.Ticker,
			Kind:   "chart_context",
			Title:  fmt.Sprintf("%s · computed %s at %v (%s)", b.Ticker, lvl, arr[idx], b.Timeframe),
			Excerpt: "Server-computed swing " + lvl +
				". Levels are derived from recent local swing highs/lows; the model quotes them and never invents them.",
			Fact: map[string]any{
				"key": "keyLevels." + lvl, "value": arr[idx], "unit": "USD",
				"asOf": factAsOfDate(snap.asOf), "timeframe": b.Timeframe,
			},
			Provider:    "analysis",
			SourceTime:  snap.sourceTime,
			DataState:   snap.dataState,
			Attribution: priceProvenance(snap),
		}, nil
	}

	// (b) a cross-timeframe confluence observation.
	field := strings.TrimSpace(asString(b.Selector["field"]))
	label, ok := savableConfluenceFields[field]
	if !ok {
		return nil, resolveErr(http.StatusBadRequest, "unknown_selector",
			"selector must be {level,index} or {field} for a confluence observation")
	}
	res, err := s.getJSON(ctx, s.cfg.AnalysisURL+"/analysis/"+b.Ticker+"/confluence?timeframes="+confluenceTimeframes)
	if err != nil {
		return nil, resolveErr(http.StatusBadGateway, "analysis_unreachable",
			"the confluence computation could not be reached: "+err.Error())
	}
	conf, _ := res["confluence"].(map[string]any)
	if conf == nil {
		return nil, resolveErr(http.StatusBadGateway, "provenance_incomplete", "no confluence available")
	}
	value, present := conf[field]
	if !present || value == nil {
		return nil, resolveErr(http.StatusUnprocessableEntity, "value_unavailable", label+" is not available")
	}
	// The confluence run reports across a fixed timeframe set; the SAVED timeframe is the chart the
	// user is looking at, and the set that was compared is stated in the excerpt. A cross-timeframe
	// observation has no single timeframe, and pretending otherwise would misdate it.
	snap, rerr := s.analysisSnapshotFor(ctx, b.Ticker, b.Timeframe)
	if rerr != nil {
		return nil, rerr
	}
	return &resolvedEvidence{
		Ticker:  b.Ticker,
		Kind:    "chart_context",
		Title:   fmt.Sprintf("%s · %s", b.Ticker, label),
		Excerpt: fmt.Sprintf("%s: %s (computed across %s)", label, flattenValue(value), confluenceTimeframes),
		Fact: map[string]any{
			"key": "confluence." + field, "value": value, "unit": "",
			"asOf": factAsOfDate(snap.asOf), "timeframe": b.Timeframe,
		},
		Provider:    "analysis",
		SourceTime:  snap.sourceTime,
		DataState:   snap.dataState,
		Attribution: priceProvenance(snap) + "; confluence computed across " + confluenceTimeframes,
	}, nil
}

// ---- fundamentals / earnings ----

func (s *Server) resolveFundamentalFact(ctx context.Context, b fromFactBody) (*resolvedEvidence, *evidenceResolveError) {
	// (a) a reported EPS beat/miss point from the earnings series.
	if want := strings.TrimSpace(asString(b.Selector["earnings"])); want != "" {
		items, source := s.earningsFor(ctx, b.Ticker)
		if len(items) == 0 {
			return nil, resolveErr(http.StatusUnprocessableEntity, "value_unavailable",
				"no earnings history is available for "+b.Ticker)
		}
		point := items[0]
		if want != "latest" {
			found := false
			for _, it := range items {
				if asString(it["fiscalDateEnding"]) == want {
					point, found = it, true
					break
				}
			}
			if !found {
				return nil, resolveErr(http.StatusUnprocessableEntity, "value_unavailable",
					"no earnings point for fiscal period "+want)
			}
		}
		field := strings.TrimSpace(asString(b.Selector["field"]))
		if field == "" {
			field = "reportedEPS"
		}
		value, present := point[field]
		if !present || value == nil {
			return nil, resolveErr(http.StatusUnprocessableEntity, "value_unavailable",
				field+" is not present on that earnings point")
		}
		provider, dataState, attribution := earningsProvenance(source)
		asOf := asString(point["fiscalDateEnding"])
		return &resolvedEvidence{
			Ticker: b.Ticker,
			Kind:   "fundamental_fact",
			Title:  fmt.Sprintf("%s · %s %s = %v", b.Ticker, asOf, field, value),
			Excerpt: strings.TrimSpace(fmt.Sprintf("Reported %s. EPS actual %v vs estimate %v.",
				asString(point["reportedDate"]), point["reportedEPS"], point["estimatedEPS"])),
			Fact: map[string]any{
				"key": "earnings." + field, "value": value, "unit": "USD", "asOf": asOf, "timeframe": "",
			},
			Provider:    provider,
			SourceTime:  parseEvidenceTime(asString(point["reportedDate"])),
			DataState:   dataState,
			Attribution: attribution,
		}, nil
	}

	// (b) a datum from a seeded quarter (NVDA-verified reference data).
	quarter := strings.TrimSpace(asString(b.Selector["quarter"]))
	field := strings.TrimSpace(asString(b.Selector["field"]))
	if quarter == "" || field == "" {
		return nil, resolveErr(http.StatusBadRequest, "unknown_selector",
			"selector must be {earnings,field} or {quarter,field}")
	}
	if !isSeedTicker(b.Ticker) {
		return nil, resolveErr(http.StatusUnprocessableEntity, "value_unavailable",
			"no reference fundamentals are available for "+b.Ticker)
	}
	q := seedQuarter(quarter)
	if q == nil {
		return nil, resolveErr(http.StatusUnprocessableEntity, "value_unavailable", "unknown quarter: "+quarter)
	}
	value, ok := dottedValue(q, field)
	if !ok || value == nil {
		return nil, resolveErr(http.StatusUnprocessableEntity, "value_unavailable",
			field+" is not present on "+quarter)
	}
	unit := ""
	if parent, ok := dottedValue(q, parentPath(field)); ok {
		if pm, ok := parent.(map[string]any); ok {
			unit = asString(pm["unit"])
		}
	}
	periodEnded := asString(q["periodEnded"])
	return &resolvedEvidence{
		Ticker:      b.Ticker,
		Kind:        "fundamental_fact",
		Title:       fmt.Sprintf("%s · %s %s = %v", b.Ticker, quarter, field, value),
		Excerpt:     seedQuarterNote(q, field),
		Fact:        map[string]any{"key": field, "value": value, "unit": unit, "asOf": periodEnded, "timeframe": ""},
		Provider:    "seed",
		SourceTime:  parseEvidenceTime(asString(q["reported"])),
		DataState:   "seed",
		Attribution: "Verified reference dataset (docs/NVIDIA_RESEARCH.md), quarter " + quarter,
	}, nil
}

// earningsProvenance maps earningsFor's source label onto the closed provider set.
func earningsProvenance(source string) (provider, dataState, attribution string) {
	if strings.Contains(source, "alphavantage") {
		return "alphavantage", "live", "Alpha Vantage EARNINGS"
	}
	return "seed", "seed", "Verified reference dataset (docs/NVIDIA_RESEARCH.md)"
}

func seedQuarter(label string) map[string]any {
	fund, _ := seedData["fundamentals"].(map[string]any)
	quarters, _ := fund["quarters"].([]any)
	for _, q := range quarters {
		m, ok := q.(map[string]any)
		if !ok {
			continue
		}
		if asString(m["label"]) == label || asString(m["periodEnded"]) == label {
			return m
		}
	}
	return nil
}

// seedQuarterNote surfaces the seed's own annotation for the selected metric, when it has one. It is
// reference text from the repo's verified dataset, not fetched page text.
func seedQuarterNote(q map[string]any, field string) string {
	if parent, ok := dottedValue(q, parentPath(field)); ok {
		if pm, ok := parent.(map[string]any); ok {
			if n := asString(pm["note"]); n != "" {
				return n
			}
		}
	}
	return ""
}

func parentPath(field string) string {
	if i := strings.LastIndex(field, "."); i > 0 {
		return field[:i]
	}
	return field
}

// dottedValue walks a dotted path through nested maps.
func dottedValue(m map[string]any, path string) (any, bool) {
	cur := any(m)
	for _, part := range strings.Split(path, ".") {
		node, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = node[part]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// ---- news and filings ----

// resolveNewsItem matches a URL against the CACHED feed the server assembled, and derives provider,
// attribution, dataState and publication time from the matched item. Matching by URL rather than by
// list index matters: the feed re-sorts as items arrive, and an index would silently capture a
// different article than the one the user clicked.
func (s *Server) resolveNewsItem(ctx context.Context, b fromFactBody) (*resolvedEvidence, *evidenceResolveError) {
	target := strings.TrimSpace(asString(b.Selector["url"]))
	if target == "" {
		return nil, resolveErr(http.StatusBadRequest, "validation_failed", "selector.url is required")
	}
	items, _ := s.newsFor(ctx, b.Ticker)
	var item map[string]any
	for _, it := range items {
		if asString(it["url"]) == target {
			item = it
			break
		}
	}
	if item == nil {
		return nil, resolveErr(http.StatusUnprocessableEntity, "source_stale",
			"that item is no longer in the current feed for "+b.Ticker+
				" — reload the news list so the capture records the source it actually came from")
	}

	category := asString(item["category"])
	isFiling := category == "filing"
	if b.Kind == "filing" && !isFiling {
		return nil, resolveErr(http.StatusBadRequest, "validation_failed", "that item is not a filing")
	}
	if b.Kind == "news" && isFiling {
		// Kind is a provenance claim, so the server corrects it rather than storing a filing as news.
		b.Kind = "filing"
	}

	provider, dataState, attribution := newsProvenance(item, isFiling)
	sourceTime := parseEvidenceTime(asString(item["publishedAt"]))
	rec := &resolvedEvidence{
		Ticker:      b.Ticker,
		Kind:        b.Kind,
		Title:       asString(item["headline"]),
		Provider:    provider,
		SourceURL:   asString(item["url"]),
		SourceTime:  sourceTime,
		DataState:   dataState,
		Attribution: attribution,
	}
	// The seed feed carries its own short annotation; provider feeds give a headline only. No article
	// body is ever stored — the gateway does not fetch one, and D-08 forbids persisting it if it did.
	if n := asString(item["note"]); n != "" {
		rec.Excerpt = n
	}
	if isFiling {
		// The SEC item's URL is the accession-numbered document; use it as the stable filing id.
		rec.Kind = "filing"
		rec.SourceRef = map[string]any{
			"store": "sec-edgar", "id": secAccessionFromURL(asString(item["url"])), "locator": "",
		}
		if rec.SourceRef["id"] == "" {
			return nil, resolveErr(http.StatusUnprocessableEntity, "provenance_incomplete",
				"this filing has no recoverable accession number, so it cannot be cited as a filing")
		}
	}
	return rec, nil
}

// newsProvenance names the actual source of ONE item. The feed is a merge of up to four streams, so
// the per-item label is derived from the item, never from the composite feed label.
func newsProvenance(item map[string]any, isFiling bool) (provider, dataState, attribution string) {
	src := asString(item["source"])
	switch {
	case isFiling:
		// The SEC feed already labels itself, so only append a distinct outlet name.
		if src == "" || strings.Contains("SEC EDGAR", strings.ToUpper(src)) {
			return "sec-edgar", "live", "SEC EDGAR"
		}
		return "sec-edgar", "live", "SEC EDGAR — " + src
	case strings.Contains(strings.ToLower(src), "(seed)"):
		return "seed", "seed", "Seeded reference headline (offline dataset) — " + src
	case item["score"] != nil:
		// Only the Marketaux path carries a provider sentiment score.
		return "marketaux", "live", "Marketaux — " + src
	default:
		return "google-news-rss", "live", "Google News (RSS) — " + src
	}
}

// secAccessionFromURL pulls the accession number out of an EDGAR document URL
// (…/Archives/edgar/data/1045810/000104581026000012/…). Empty when the shape is unfamiliar, which
// the caller turns into a refusal rather than a guess.
func secAccessionFromURL(u string) string {
	parts := strings.Split(u, "/")
	for _, p := range parts {
		if len(p) == 18 && isAllDigits(p) {
			return p[:10] + "-" + p[10:12] + "-" + p[12:]
		}
	}
	return ""
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(s) > 0
}

// ---- transcript excerpts ----

// transcriptLocators are the addressable strings inside a STORED transcript analysis. Each is a
// quotation the analysis pipeline already verified verbatim against the source text (transcript.py's
// enforce_grounding drops anything it cannot find), or a derived line from the same snapshot.
//
// This is also the only door into transcript evidence. There is no path from a pasted or fetched
// transcript body to an evidence record: the source text is not persisted anywhere (D-08 (a)), so the
// snapshot's own verified strings are all there is — and all there needs to be.
func (s *Server) resolveTranscriptExcerpt(ctx context.Context, b fromFactBody) (*resolvedEvidence, *evidenceResolveError) {
	key := strings.TrimSpace(asString(b.Selector["transcriptKey"]))
	locator := strings.TrimSpace(asString(b.Selector["locator"]))
	if key == "" || locator == "" {
		return nil, resolveErr(http.StatusBadRequest, "validation_failed",
			"selector.transcriptKey and selector.locator are required")
	}
	res, err := s.getJSON(ctx, s.cfg.LLMURL+"/transcript/"+b.Ticker)
	if err != nil {
		return nil, resolveErr(http.StatusBadGateway, "transcript_unreachable",
			"the stored transcript analyses could not be read: "+err.Error())
	}
	analyses, _ := res["analyses"].([]any)
	var snap map[string]any
	for _, a := range analyses {
		m, ok := a.(map[string]any)
		if !ok {
			continue
		}
		if asString(m["key"]) == key {
			snap = m
			break
		}
	}
	if snap == nil {
		return nil, resolveErr(http.StatusUnprocessableEntity, "source_stale",
			"no stored transcript analysis with key "+key+" for "+b.Ticker)
	}
	structured, _ := snap["structured"].(map[string]any)
	quote, ok := transcriptLocatorValue(structured, locator)
	if !ok || strings.TrimSpace(quote) == "" {
		// §2.4(4): an unverifiable quotation is refused. The only strings this endpoint will store are
		// ones present in the stored snapshot — a quotation that is not there cannot be attributed.
		return nil, resolveErr(http.StatusBadRequest, "quote_unverified",
			"that quotation is not present in the stored transcript analysis, so it cannot be verified")
	}

	label := asString(snap["label"])
	if label == "" {
		label = key
	}
	kind := b.Kind
	title := label + " — earnings call"
	speaker := strings.TrimSpace(asString(b.Selector["speaker"]))
	if kind == "management_statement" {
		// §2.3 requires a named speaker + role for a management statement. The stored analysis does not
		// record speakers, so the attribution has to come from the user — and it is labelled as theirs.
		if speaker == "" {
			return nil, resolveErr(http.StatusBadRequest, "provenance_incomplete",
				"a management statement needs the speaker and role; the stored transcript analysis does not "+
					"record speakers, so name them or save this as a transcript excerpt instead")
		}
		title = speaker + " — " + label
	}

	attribution := label + " earnings call transcript (user-supplied text). Quotation verified verbatim " +
		"against the source at analysis time; the source text itself is not retained."
	if kind == "management_statement" {
		attribution = "Speaker attribution supplied by you. " + attribution
	}

	return &resolvedEvidence{
		Ticker:  b.Ticker,
		Kind:    kind,
		Title:   title,
		Excerpt: quote,
		// The transcript text was supplied by the user, so `user` is the provider of the material.
		// `llm` is reserved for content the model itself produced (§2.7) and this is a quotation, not
		// an inference — mislabelling it would put model prose and management's own words in one bucket.
		Provider:    "user",
		SourceRef:   map[string]any{"store": "llm-transcript", "id": b.Ticker + "/" + key, "locator": locator},
		SourceTime:  parseEvidenceTime(asString(snap["timestamp"])),
		DataState:   "live",
		Attribution: attribution,
	}, nil
}

// transcriptLocatorValue addresses one string inside a stored analysis:
//
//	summary
//	keyPoints:<n>
//	riskFlags:<n>
//	notableLanguage:<n>
//	evasiveMoments:<n>:question | evasiveMoments:<n>:answer
func transcriptLocatorValue(structured map[string]any, locator string) (string, bool) {
	if structured == nil {
		return "", false
	}
	parts := strings.Split(locator, ":")
	switch parts[0] {
	case "summary":
		return asString(structured["summary"]), structured["summary"] != nil
	case "keyPoints", "riskFlags":
		if len(parts) != 2 {
			return "", false
		}
		return stringAtIndex(structured[parts[0]], parts[1])
	case "notableLanguage":
		if len(parts) != 2 {
			return "", false
		}
		tone, _ := structured["tone"].(map[string]any)
		return stringAtIndex(tone["notableLanguage"], parts[1])
	case "evasiveMoments":
		if len(parts) != 3 || (parts[2] != "question" && parts[2] != "answer") {
			return "", false
		}
		arr, _ := structured["evasiveMoments"].([]any)
		i, err := strconv.Atoi(parts[1])
		if err != nil || i < 0 || i >= len(arr) {
			return "", false
		}
		m, ok := arr[i].(map[string]any)
		if !ok {
			return "", false
		}
		v, present := m[parts[2]]
		return asString(v), present && v != nil
	}
	return "", false
}

func stringAtIndex(list any, rawIdx string) (string, bool) {
	arr, ok := list.([]any)
	if !ok {
		return "", false
	}
	i, err := strconv.Atoi(rawIdx)
	if err != nil || i < 0 || i >= len(arr) {
		return "", false
	}
	return asString(arr[i]), arr[i] != nil
}

// ---- catalysts / roadmap ----

func (s *Server) resolveCatalyst(b fromFactBody) (*resolvedEvidence, *evidenceResolveError) {
	group := strings.TrimSpace(asString(b.Selector["group"]))
	if group != "events" && group != "roadmap" {
		return nil, resolveErr(http.StatusBadRequest, "unknown_selector", "selector.group must be events or roadmap")
	}
	if !isSeedTicker(b.Ticker) {
		return nil, resolveErr(http.StatusUnprocessableEntity, "value_unavailable",
			"no roadmap or catalyst reference data is available for "+b.Ticker)
	}
	seedKey := "catalysts"
	if group == "roadmap" {
		seedKey = "roadmap"
	}
	arr, _ := seedData[seedKey].([]any)
	idx := selectorIndex(b.Selector["index"])
	if idx < 0 || idx >= len(arr) {
		return nil, resolveErr(http.StatusUnprocessableEntity, "value_unavailable", "no such item in "+group)
	}
	item, _ := arr[idx].(map[string]any)
	if item == nil {
		return nil, resolveErr(http.StatusUnprocessableEntity, "value_unavailable", "no such item in "+group)
	}

	if group == "events" {
		date := asString(item["date"])
		return &resolvedEvidence{
			Ticker:      b.Ticker,
			Kind:        "catalyst",
			Title:       asString(item["title"]),
			Excerpt:     asString(item["note"]),
			Fact:        map[string]any{"key": "catalyst." + asString(item["type"]), "value": asString(item["impact"]), "unit": "impact", "asOf": date, "timeframe": ""},
			Provider:    "seed",
			SourceTime:  parseEvidenceTime(date),
			DataState:   "seed",
			Attribution: "Verified reference dataset (docs/NVIDIA_RESEARCH.md) — dated catalyst",
		}, nil
	}

	// Roadmap entries are dated by YEAR only. `fact.asOf` therefore records Jan 1 of that year and the
	// title carries the year, rather than implying a precise date the dataset does not have.
	year := asString(item["year"])
	asOf := ""
	if year != "" {
		asOf = year + "-01-01"
	}
	return &resolvedEvidence{
		Ticker:      b.Ticker,
		Kind:        "catalyst",
		Title:       fmt.Sprintf("%s roadmap · %s (%s, %s)", b.Ticker, asString(item["gen"]), year, asString(item["status"])),
		Excerpt:     strings.TrimSpace(asString(item["products"]) + " — " + asString(item["note"])),
		Fact:        map[string]any{"key": "roadmap." + asString(item["gen"]), "value": asString(item["status"]), "unit": "", "asOf": asOf, "timeframe": ""},
		Provider:    "seed",
		SourceTime:  0, // year-only: no defensible instant, so it stays unknown and renders as such
		DataState:   "seed",
		Attribution: "Verified reference dataset (docs/NVIDIA_RESEARCH.md) — product roadmap, year-level dating only",
	}, nil
}

// ---- user note ----

// resolveUserNote is the one kind whose CONTENT is the user's. Provenance is still server-stamped:
// the note is captured against a company at a moment, and dataState records whether the prices on
// screen at the time were real — a note written while looking at synthetic demo bars must not later
// read as if it were written against the market.
func (s *Server) resolveUserNote(ctx context.Context, b fromFactBody) (*resolvedEvidence, *evidenceResolveError) {
	if strings.TrimSpace(b.Note) == "" {
		return nil, resolveErr(http.StatusBadRequest, "validation_failed", "a note is required")
	}
	dataState := "live"
	if snap, rerr := s.analysisSnapshotFor(ctx, b.Ticker, b.Timeframe); rerr == nil {
		dataState = snap.dataState
	}
	title := strings.TrimSpace(asString(b.Selector["title"]))
	if title == "" {
		title = b.Ticker + " — your observation"
	}
	return &resolvedEvidence{
		Ticker:      b.Ticker,
		Kind:        "user_note",
		Title:       title,
		Provider:    "user",
		DataState:   dataState,
		Attribution: "Your own observation",
	}, nil
}

// ---- shared helpers ----

func selectorIndex(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		if i, err := strconv.Atoi(n); err == nil {
			return i
		}
	case nil:
		return 0
	}
	return -1
}

// parseEvidenceTime turns the several date shapes the providers use into a unix second. 0 on failure,
// and 0 MEANS UNKNOWN: it is never replaced with time.Now(), because a capture stamped with today's
// date when the real publication time is unknown is a fabricated fact (§2.3).
func parseEvidenceTime(raw string) int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	layouts := []string{
		time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02", time.RFC1123, time.RFC1123Z,
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, raw); err == nil {
			return t.UTC().Unix()
		}
	}
	return 0
}

// flattenValue renders a scalar or a list of scalars as one readable line for an excerpt.
func flattenValue(v any) string {
	if arr, ok := v.([]any); ok {
		parts := make([]string, 0, len(arr))
		for _, x := range arr {
			parts = append(parts, asString(x))
		}
		if len(parts) == 0 {
			return "none"
		}
		return strings.Join(parts, "; ")
	}
	return asString(v)
}
