package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// decisions.go — the decision + outcome surface. Contract: PARALLEL_CONTRACTS.md §5.
//
// The journal owns the records and all per-user scoping; almost everything here is a verbatim
// pass-through that forwards the browser's session and returns the journal's own status and `code`.
// The gateway adds exactly two things it is uniquely able to add:
//
//  1. MODEL CONTEXT AT DECISION TIME. `POST /api/decisions` asks the prediction service whether a
//     WALK-FORWARD-VALIDATED signal existed for this company, and forwards presence + reason — never a
//     direction, never a probability. The journal has no prediction client and should not grow one;
//     the gateway is the aggregator. The call is fail-soft: an unreachable prediction service records
//     "not recorded", which is honest, rather than a default that implies something was checked.
//  2. A CITED RECAP. `POST /api/decisions/summary` reads back records the caller already owns and
//     assembles a recap FROM TYPED FIELDS, IN CODE — the same rule §4.4 applies to change-set
//     summaries. It cites dec_/out_/ev_/rev_ ids, refuses an id the caller does not own, marks what is
//     inference (nothing is), and writes nothing. Because it is not a model call it cannot grade the
//     user, cannot invent a citation, and cannot drift; a model-authored variant would belong in the
//     llm service, behind that lane's retry/stub contract.
//
// Nothing here produces a buy/sell verdict, a price target, or a recommendation, and nothing executes.

// registerDecisionRoutes mounts the surface. One registration line in main.go keeps this lane's
// footprint in the shared file minimal (PARALLEL_EXECUTION.md § shared-file rule).
func (s *Server) registerDecisionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/decisions", s.handleDecisionsList)
	mux.HandleFunc("POST /api/decisions", s.handleDecisionCreate)
	// The literal /summary pattern is more specific than /{id}, so it wins.
	mux.HandleFunc("POST /api/decisions/summary", s.handleDecisionSummary)
	mux.HandleFunc("GET /api/decisions/{id}", s.handleDecisionGet)
	mux.HandleFunc("PATCH /api/decisions/{id}", s.handleDecisionPatch)
	mux.HandleFunc("DELETE /api/decisions/{id}", s.handleDecisionDelete)

	mux.HandleFunc("GET /api/outcomes", s.handleOutcomesList)
	mux.HandleFunc("POST /api/outcomes", s.handleOutcomeCreate)
	mux.HandleFunc("GET /api/outcomes/{id}", s.handleOutcomeGet)
	mux.HandleFunc("PATCH /api/outcomes/{id}", s.handleOutcomePatch)
}

func withQuery(path string, r *http.Request) string {
	if q := r.URL.RawQuery; q != "" {
		return path + "?" + q
	}
	return path
}

func (s *Server) handleDecisionsList(w http.ResponseWriter, r *http.Request) {
	s.proxyJournal(w, r, http.MethodGet, withQuery("/decisions", r))
}

func (s *Server) handleDecisionGet(w http.ResponseWriter, r *http.Request) {
	s.proxyJournal(w, r, http.MethodGet, "/decisions/"+url.PathEscape(r.PathValue("id")))
}

func (s *Server) handleDecisionPatch(w http.ResponseWriter, r *http.Request) {
	s.proxyJournal(w, r, http.MethodPatch, "/decisions/"+url.PathEscape(r.PathValue("id")))
}

func (s *Server) handleDecisionDelete(w http.ResponseWriter, r *http.Request) {
	s.proxyJournal(w, r, http.MethodDelete, "/decisions/"+url.PathEscape(r.PathValue("id")))
}

func (s *Server) handleOutcomesList(w http.ResponseWriter, r *http.Request) {
	s.proxyJournal(w, r, http.MethodGet, withQuery("/outcomes", r))
}

func (s *Server) handleOutcomeCreate(w http.ResponseWriter, r *http.Request) {
	s.proxyJournal(w, r, http.MethodPost, "/outcomes")
}

func (s *Server) handleOutcomeGet(w http.ResponseWriter, r *http.Request) {
	s.proxyJournal(w, r, http.MethodGet, "/outcomes/"+url.PathEscape(r.PathValue("id")))
}

func (s *Server) handleOutcomePatch(w http.ResponseWriter, r *http.Request) {
	s.proxyJournal(w, r, http.MethodPatch, "/outcomes/"+url.PathEscape(r.PathValue("id")))
}

// ---- create: attach the validated-signal context --------------------------------------------------

// handleDecisionCreate stamps the model context onto the request and forwards it.
//
// Only two fields are added, and both are about whether the user HAD a validated signal to lean on —
// not what it said. Everything the client sent is otherwise passed through untouched, so the journal
// stays the single validator, and the snapshot it builds stays server-owned.
func (s *Server) handleDecisionCreate(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "could not read request body", "code": "validation_failed"})
		return
	}

	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil || body == nil {
		// Not our error to interpret — let the journal answer with its own validation shape.
		s.forwardJournalBytes(w, r, http.MethodPost, "/decisions", raw)
		return
	}

	ticker := strings.ToUpper(strings.TrimSpace(stringField(body, "ticker")))
	present, reason := s.validatedSignalContext(r.Context(), ticker)
	body["modelSignalPresent"] = present
	body["modelSignalReason"] = reason

	enriched, err := json.Marshal(body)
	if err != nil {
		s.forwardJournalBytes(w, r, http.MethodPost, "/decisions", raw)
		return
	}
	s.forwardJournalBytes(w, r, http.MethodPost, "/decisions", enriched)
}

// validatedSignalContext reports whether a backtest-gated signal exists for a ticker, and why not
// when it does not.
//
// It reads the SAME /predict endpoint the SignalPanel uses, and reduces it to a boolean plus a
// sentence. The direction, probability and confidence are deliberately dropped on the floor: storing
// them in a decision record would turn an audit trail into a stored recommendation, which invariant #2
// forbids everywhere except the SignalPanel's live, track-record-bearing display.
func (s *Server) validatedSignalContext(ctx context.Context, ticker string) (bool, string) {
	if ticker == "" {
		return false, "not recorded"
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	res, err := s.getJSON(ctx, s.cfg.PredictionURL+"/predict/"+url.PathEscape(ticker))
	if err != nil {
		// Fail-soft, and say so: "we did not check" is different from "there was no signal".
		return false, "not recorded (prediction service unavailable)"
	}

	passed := false
	if bt, ok := res["backtest"].(map[string]any); ok {
		passed, _ = bt["passed"].(bool)
	}
	signal, hasSignal := res["signal"].(map[string]any)
	reason := strings.TrimSpace(stringField(res, "reason"))

	switch {
	case hasSignal && signal != nil && passed:
		return true, "a walk-forward-validated signal was available"
	case reason != "":
		return false, reason
	default:
		return false, "no validated signal (backtest gate not passed)"
	}
}

// forwardJournalBytes is proxyJournal with a body the gateway composed rather than the caller's raw
// stream. Same rules: forward the session, copy the journal's status and body back verbatim.
func (s *Server) forwardJournalBytes(w http.ResponseWriter, r *http.Request, method, path string, payload []byte) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, s.cfg.JournalURL+path, bytes.NewReader(payload))
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
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": "journal unreachable: " + err.Error(), "code": "journal_unreachable"})
		return
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(out)
}

// ---- cited recap ----------------------------------------------------------------------------------

type summaryRequest struct {
	DecisionIds []string `json:"decisionIds"`
}

// SummaryLine is one sentence about one record, with the ids it was built from. `inference` is on
// every line so a consumer never has to guess which parts are observed and which are derived; here
// it is always false, because every line is assembled from stored typed fields.
type SummaryLine struct {
	DecisionID string   `json:"decisionId"`
	Text       string   `json:"text"`
	CitedIds   []string `json:"citedIds"`
	Inference  bool     `json:"inference"`
}

const maxSummaryRecords = 25

// handleDecisionSummary assembles a recap of decisions the caller already owns.
//
// Every id is resolved through the journal WITH the caller's session, so "records you own" is
// enforced by the same partition that enforces it everywhere else — an id belonging to someone else
// is indistinguishable from one that does not exist, and both are refused rather than silently
// dropped. A dropped citation would produce a recap that looks complete and is not.
func (s *Server) handleDecisionSummary(w http.ResponseWriter, r *http.Request) {
	var req summaryRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "invalid JSON: " + err.Error(), "code": "validation_failed"})
		return
	}

	ids := make([]string, 0, len(req.DecisionIds))
	seen := map[string]bool{}
	for _, raw := range req.DecisionIds {
		id := strings.TrimSpace(raw)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "decisionIds is required", "code": "validation_failed", "field": "decisionIds"})
		return
	}
	if len(ids) > maxSummaryRecords {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": fmt.Sprintf("at most %d decisions can be summarised at once", maxSummaryRecords),
			"code":  "validation_failed", "field": "decisionIds"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	cookie := r.Header.Get("Cookie")

	lines := make([]SummaryLine, 0, len(ids))
	var unknown []string
	for _, id := range ids {
		res, err := s.journalGet(ctx, s.cfg.JournalURL+"/decisions/"+url.PathEscape(id), cookie)
		if err != nil {
			unknown = append(unknown, id)
			continue
		}
		decision, _ := res["decision"].(map[string]any)
		if decision == nil {
			unknown = append(unknown, id)
			continue
		}
		outcome, _ := res["outcome"].(map[string]any)
		lines = append(lines, summariseDecision(decision, outcome))
	}

	if len(unknown) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "one or more referenced records do not exist in your account",
			"code":  "unknown_reference", "field": "decisionIds", "unknown": unknown,
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"summary":     lines,
		"generatedBy": "code", // not a model: no retry/stub path, and nothing to drift
		"modelUsed":   nil,
		"reviewed":    countReviewed(lines),
		"total":       len(lines),
		"disclaimer": "Assembled from your stored records. It restates what you wrote; it does not " +
			"grade your reasoning and it is not investment advice.",
	})
}

func countReviewed(lines []SummaryLine) int {
	n := 0
	for _, l := range lines {
		for _, id := range l.CitedIds {
			if strings.HasPrefix(id, "out_") {
				n++
				break
			}
		}
	}
	return n
}

// summariseDecision builds one line from typed fields only.
//
// It states the intent, the horizon, what was cited, and — when a review exists — what the user
// concluded. It never combines `reasoningQuality` with P&L, never ranks decisions, and never adds a
// judgement of its own: those are the user's to make, and §5.5 keeps "good result" and "good
// reasoning" apart in the presentation as well as in the store.
func summariseDecision(decision, outcome map[string]any) SummaryLine {
	id := stringField(decision, "id")
	cited := []string{id}

	intent := stringField(decision, "intent")
	verb := map[string]string{
		"act":        "acted",
		"do_not_act": "deliberately did not act",
		"wait":       "chose to wait",
	}[intent]
	if verb == "" {
		verb = "recorded a decision"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "On %s you %s", stringField(decision, "ticker"), verb)
	if action, ok := decision["action"].(map[string]any); ok {
		if kind := stringField(action, "kind"); kind != "" && kind != "none" {
			fmt.Fprintf(&b, " (%s", kind)
			if side := stringField(action, "side"); side != "" {
				fmt.Fprintf(&b, " %s", side)
			}
			b.WriteString(")")
		}
	}
	if h := stringField(decision, "horizon"); h != "" {
		fmt.Fprintf(&b, " over %s", h)
	}
	b.WriteString(".")

	evidenceIds := stringList(decision["evidenceIds"])
	reviewIds := stringList(decision["reviewIds"])
	cited = append(cited, evidenceIds...)
	cited = append(cited, reviewIds...)
	if n := len(evidenceIds); n > 0 {
		fmt.Fprintf(&b, " Cited %d evidence record%s", n, plural(n))
		if m := len(reviewIds); m > 0 {
			fmt.Fprintf(&b, " and %d saved review%s", m, plural(m))
		}
		b.WriteString(".")
	} else if m := len(reviewIds); m > 0 {
		fmt.Fprintf(&b, " Cited %d saved review%s.", m, plural(m))
	} else {
		b.WriteString(" No evidence was cited.")
	}

	if snap, ok := decision["snapshot"].(map[string]any); ok {
		if ds := stringField(snap, "dataState"); ds == "synthetic" {
			b.WriteString(" Taken on SYNTHETIC price data.")
		}
	}

	if outcome != nil {
		outID := stringField(outcome, "id")
		if outID != "" {
			cited = append(cited, outID)
		}
		b.WriteString(" Reviewed: ")
		b.WriteString(oneSentence(stringField(outcome, "observedOutcome")))
		if rq, ok := outcome["reasoningQuality"].(float64); ok {
			// Stated on its own, never alongside or divided by a P&L number.
			fmt.Fprintf(&b, " You rated the reasoning %d of 5.", int(rq))
		}
	} else {
		b.WriteString(" No outcome review yet.")
	}

	sort.Strings(cited[1:])
	return SummaryLine{DecisionID: id, Text: b.String(), CitedIds: cited, Inference: false}
}

// `plural` lives in changes.go. Steps 08 and 09 each added a byte-identical copy to this flat
// package without being able to see the other; the Wave 4 integrator kept the one from the lane that
// merged first and deleted this one. Nothing about either behaviour changed.

// oneSentence bounds a quoted user string so a recap line stays a line.
func oneSentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(no note)"
	}
	r := []rune(s)
	if len(r) > 180 {
		return string(r[:180]) + "…"
	}
	return s
}

func stringField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func stringList(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, item := range list {
		if s, ok := item.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}
