package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// changes.go — "what changed since your last review" (PARALLEL_CONTRACTS.md §4.4, Step 08).
//
// THE CHANGE SET IS DETERMINISTIC AND IT IS THE AUTHORITY. That is the whole design of this file.
// Every item is read from a store that ALREADY holds it — thesis versions, evidence links, saved
// lens reviews, alert events, dated analysis series, persisted transcript snapshots. Nothing is
// invented to pad the list, and `summary` is composed IN CODE from typed fields, never by a model.
//
// Two consequences that are deliberate:
//
//   · `id` is sha256(kind|sourceRef|at) truncated to 16 hex. Recomputing the same boundary always
//     yields the same ids, which is what makes the set testable AND what lets Step 09 reference a
//     change item from a decision record without this lane persisting anything.
//   · When nothing changed, `empty` is true and `items` is []. An honest empty answer is a feature:
//     a filler narrative would teach the user to distrust the ones that aren't empty.
//
// The boundary is the Step 04 review checkpoint (§1.8). This lane READS `thesis.lastReview` and
// CALLS `POST /api/theses/{id}/review-checkpoint` to advance it. It creates no checkpoint store of
// its own and writes no other thesis field.
//
// The optional LLM summary lives at the bottom of this file. It receives the NORMALIZED CHANGE SET
// AND NOTHING ELSE, it cannot change a thesis field, and it is reachable only from a POST. The
// scheduler never comes near it (§4.6) — alerts/eval.go has no client for this route or any other.

const (
	// changeKind* — the seven kinds §4.4 allows. There is no eighth, and no free-text kind.
	changeThesisVersion    = "thesis_version"
	changeEvidenceLinked   = "evidence_linked"
	changeEvidenceUnlinked = "evidence_unlinked"
	changeLensReview       = "lens_review"
	changeAlertEvent       = "alert_event"
	changeMarketContext    = "market_context"
	changeTranscriptCompar = "transcript_comparison"

	// maxChangeItems bounds the response. A user reviewing after a year should get a usable list,
	// not a megabyte. Truncation is REPORTED (`truncated`), never silent.
	maxChangeItems = 200

	// materialMovePct is the stated threshold at which a price move since the checkpoint becomes a
	// change item. A threshold is required: without one, `market_context` would emit on every call
	// and "nothing changed" could never be true, which would make the empty state a lie.
	materialMovePct = 5.0

	changesTimeout = 25 * time.Second
)

// ChangeItem is one deterministic change since the boundary (§4.4).
type ChangeItem struct {
	ID           string          `json:"id"`
	Kind         string          `json:"kind"`
	At           int64           `json:"at"`
	Summary      string          `json:"summary"`
	SourceRef    changeSourceRef `json:"sourceRef"`
	ThesisItemID string          `json:"thesisItemId,omitempty"`
	Bearing      *string         `json:"bearing"`
	DataState    string          `json:"dataState"`
	ResearchLink *changeLink     `json:"researchLink"`
}

// changeSourceRef names the store and record this item was read from, so every line in the change
// set can be traced back to something durable.
type changeSourceRef struct {
	Store string `json:"store"`
	ID    string `json:"id"`
}

// changeLink mirrors alerts' ResearchLink (§4.5). It is duplicated rather than imported because the
// two services are independent stdlib-only Go modules — the same reason the session verifier is
// copy-pasted across auth/gateway/journal/alerts.
type changeLink struct {
	View     string  `json:"view"`
	Subview  string  `json:"subview"`
	Tab      *string `json:"tab"`
	Ticker   string  `json:"ticker"`
	ThesisID string  `json:"thesisId"`
	Hash     string  `json:"hash"`
}

func changeLinkFor(ticker, thesisID string) *changeLink {
	return &changeLink{
		View: "research", Subview: "thesis", Tab: nil,
		Ticker: ticker, ThesisID: thesisID, Hash: "#research/thesis",
	}
}

// changeID is deterministic from (kind, sourceRef, at) — §4.4. Same boundary, same ids, always.
func changeID(kind string, ref changeSourceRef, at int64) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + ref.Store + "\x00" + ref.ID + "\x00" + fmt.Sprint(at)))
	return "chg_" + hex.EncodeToString(sum[:])[:16]
}

// registerChangeRoutes mounts this lane's surface. One line in main.go, matching how evidence.go and
// lens.go register theirs.
func (s *Server) registerChangeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/theses/{id}/changes", s.handleThesisChanges)
	mux.HandleFunc("POST /api/theses/{id}/changes/summary", s.handleThesisChangesSummary)
}

// ---------------------------------------------------------------- the change set

func (s *Server) handleThesisChanges(w http.ResponseWriter, r *http.Request) {
	id := s.identityFrom(r)
	if id.userID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": "sign in required", "code": "auth_required"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), changesTimeout)
	defer cancel()

	set, apiErr := s.changeSetFor(ctx, r.PathValue("id"), id)
	if apiErr != nil {
		writeJSON(w, apiErr.status, map[string]any{"error": apiErr.msg, "code": apiErr.code})
		return
	}
	writeJSON(w, http.StatusOK, set)
}

type changesError struct {
	status int
	code   string
	msg    string
}

// changeSet is the response body (§4.4).
type changeSet struct {
	ThesisID  string         `json:"thesisId"`
	Ticker    string         `json:"ticker"`
	Since     changeSince    `json:"since"`
	Items     []ChangeItem   `json:"items"`
	Counts    map[string]int `json:"counts"`
	Empty     bool           `json:"empty"`
	Truncated bool           `json:"truncated"`

	// Degraded names the sources that could not be read this call. The set stays USABLE when a
	// service is down, but it never pretends to be complete: a missing source is stated, not hidden.
	Degraded []string `json:"degraded"`
}

type changeSince struct {
	CheckpointID string `json:"checkpointId"`
	At           int64  `json:"at"`
	Basis        string `json:"basis"` // "checkpoint" | "created"
}

// changeSetFor computes the whole set. Every source is read WITH THE CALLER'S COOKIE, so a thesis
// that is not theirs is a 404 before anything else happens and no cross-user data can enter the set.
func (s *Server) changeSetFor(ctx context.Context, thesisID string, id identity) (*changeSet, *changesError) {
	thesis, status, err := s.loadThesisWithStatus(ctx, thesisID, id.cookie)
	if err != nil {
		return nil, &changesError{http.StatusBadGateway, "journal_unreachable", "journal unreachable: " + err.Error()}
	}
	if thesis == nil {
		if status >= 500 {
			// The journal answered, but with a server error. That is not "your thesis doesn't
			// exist" — saying so would be a lie the user could act on.
			return nil, &changesError{http.StatusBadGateway, "journal_unreachable", "journal error"}
		}
		// Foreign or missing is 404 either way — never confirm that another user's thesis exists.
		return nil, &changesError{http.StatusNotFound, "not_found", "thesis not found"}
	}

	ticker := strings.ToUpper(asString(thesis["ticker"]))
	since := boundaryFor(thesis)

	set := &changeSet{
		ThesisID: thesisID, Ticker: ticker, Since: since,
		Items: []ChangeItem{}, Counts: map[string]int{}, Degraded: []string{},
	}

	// Each collector appends what it can and names itself in `degraded` when it cannot. A source
	// being down degrades the answer; it never fails the request, because a partial honest list is
	// more useful than an error page when the user is trying to review.
	set.Items = append(set.Items, changesFromVersions(thesis, ticker, thesisID, since.At)...)

	if items, ok := s.changesFromEvidenceLinks(ctx, thesisID, ticker, since.At, id.cookie); ok {
		set.Items = append(set.Items, items...)
	} else {
		set.Degraded = append(set.Degraded, "evidence_links")
	}

	if items, ok := s.changesFromReviews(ctx, thesisID, ticker, since.At, id.cookie); ok {
		set.Items = append(set.Items, items...)
	} else {
		set.Degraded = append(set.Degraded, "lens_reviews")
	}

	if items, ok := s.changesFromAlertEvents(ctx, thesisID, ticker, since.At, id.cookie); ok {
		set.Items = append(set.Items, items...)
	} else {
		set.Degraded = append(set.Degraded, "alert_events")
	}

	if items, ok := s.changesFromMarketContext(ctx, ticker, thesisID, since.At); ok {
		set.Items = append(set.Items, items...)
	} else {
		set.Degraded = append(set.Degraded, "market_context")
	}

	if items, ok := s.changesFromTranscripts(ctx, ticker, thesisID, since.At); ok {
		set.Items = append(set.Items, items...)
	} else {
		set.Degraded = append(set.Degraded, "transcripts")
	}

	// Newest first, with the deterministic id as the tie-break so equal timestamps never reorder
	// between two calls — a set whose order flickered could not be diffed or tested.
	sort.SliceStable(set.Items, func(i, j int) bool {
		if set.Items[i].At != set.Items[j].At {
			return set.Items[i].At > set.Items[j].At
		}
		return set.Items[i].ID < set.Items[j].ID
	})

	for _, it := range set.Items {
		set.Counts[it.Kind]++
	}
	if len(set.Items) > maxChangeItems {
		set.Items = set.Items[:maxChangeItems]
		set.Truncated = true
	}
	set.Empty = len(set.Items) == 0
	return set, nil
}

// boundaryFor resolves the comparison boundary from the thesis's own checkpoint (§1.8). A thesis
// that was never reviewed uses its creation time and says so via basis:"created", which the UI
// renders as "since you created this thesis" rather than implying a review that never happened.
func boundaryFor(thesis map[string]any) changeSince {
	if lr, ok := thesis["lastReview"].(map[string]any); ok && lr != nil {
		at := asInt64(lr["at"])
		if at > 0 {
			return changeSince{CheckpointID: asString(lr["checkpointId"]), At: at, Basis: "checkpoint"}
		}
	}
	return changeSince{CheckpointID: "", At: asInt64(thesis["createdAt"]), Basis: "created"}
}

// loadThesisWithStatus fetches the thesis and KEEPS THE UPSTREAM STATUS, which the shared
// journalGet helper discards (it collapses every non-2xx into one error).
//
// The distinction matters here: "the journal said this thesis is not yours" must surface as 404,
// while "the journal is broken" must surface as 502. Collapsing them would either hide an outage
// behind a reassuring 404 or tell a user their own thesis is missing during one.
func (s *Server) loadThesisWithStatus(ctx context.Context, id, cookie string) (map[string]any, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		s.cfg.JournalURL+"/theses/"+url.PathEscape(id), nil)
	if err != nil {
		return nil, 0, err
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, 0, err // transport failure: the journal was not reached at all
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, resp.StatusCode, nil
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, resp.StatusCode, err
	}
	t, _ := out["thesis"].(map[string]any)
	return t, resp.StatusCode, nil
}

// ---------------------------------------------------------------- optional on-demand summary

// handleThesisChangesSummary compresses an ALREADY-COMPUTED change set into prose (§4.4).
//
// Read the constraints as a list of things this endpoint CANNOT do, because that is what makes it
// safe to have at all:
//
//	· It cannot see anything but the normalized change set. The model is handed `chg_` ids and the
//	  code-generated summaries — no thesis text, no evidence excerpts, no raw prices.
//	· It cannot introduce a change. Every `chg_` id in the reply is checked against the set that was
//	  just computed; an unknown id fails validation, and a second failure falls back to the stub.
//	· It cannot write. There is no journal write anywhere in this handler, so no model output can
//	  reach a thesis field.
//	· It cannot be reached by a poll or the scheduler. It is a POST, and alerts/eval.go has no
//	  client for it (§4.6).
//
// The change set stays the authority; this is a reading aid over it.
func (s *Server) handleThesisChangesSummary(w http.ResponseWriter, r *http.Request) {
	id := s.identityFrom(r)
	if id.userID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": "sign in required", "code": "auth_required"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), changesTimeout+30*time.Second)
	defer cancel()

	// Recomputed server-side rather than accepted from the body: a client that could POST its own
	// "change set" could have the model summarize changes that never happened.
	set, apiErr := s.changeSetFor(ctx, r.PathValue("id"), id)
	if apiErr != nil {
		writeJSON(w, apiErr.status, map[string]any{"error": apiErr.msg, "code": apiErr.code})
		return
	}
	if set.Empty {
		// Nothing changed: say so, and never spend a model call inventing a narrative about it.
		writeJSON(w, http.StatusOK, map[string]any{
			"thesisId": set.ThesisID, "since": set.Since, "empty": true,
			"summary": "Nothing has changed since your last review.", "citedIds": []string{},
			"inference": false, "modelUsed": "none", "warnings": []string{},
		})
		return
	}

	known := map[string]bool{}
	for _, it := range set.Items {
		known[it.ID] = true
	}

	prompt := changeSummaryPrompt(set, false)
	text, modelUsed := s.callChangeSummary(ctx, set.Ticker, prompt, id.userID)
	cited, bad := citedChangeIDs(text, known)
	warnings := []string{}

	// ONE firmer retry on a structural failure, then the stub — the same contract every other model
	// path in this codebase follows (services/llm/app/main.py).
	if text == "" || len(bad) > 0 {
		warnings = append(warnings, "first attempt cited unknown change ids; retried")
		prompt = changeSummaryPrompt(set, true)
		text, modelUsed = s.callChangeSummary(ctx, set.Ticker, prompt, id.userID)
		cited, bad = citedChangeIDs(text, known)
	}
	if text == "" || len(bad) > 0 {
		text = stubChangeSummary(set)
		cited, _ = citedChangeIDs(text, known)
		modelUsed = "stub:validation"
		warnings = append(warnings, "model output cited change ids that do not exist; deterministic summary used instead")
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"thesisId": set.ThesisID, "since": set.Since, "empty": false,
		"summary":  text,
		"citedIds": cited,
		// Always true: this is a model reading OF the change set, never an additional observation.
		"inference":  true,
		"modelUsed":  modelUsed,
		"warnings":   warnings,
		"counts":     set.Counts,
		"disclaimer": changeSummaryDisclaimer,
	})
}

const changeSummaryDisclaimer = "This summary is model-generated from the change list above. " +
	"The change list is the authoritative record; the summary may compress or omit. " +
	"No buy/sell verdict, price target, or recommendation is offered."

// changeSummaryPrompt renders the NORMALIZED change set — ids and code-generated summaries only.
func changeSummaryPrompt(set *changeSet, firmer bool) string {
	var b strings.Builder
	b.WriteString("You are summarizing a list of changes to a user's own investment research since ")
	b.WriteString("their last review. Compress it into at most 5 short sentences.\n\n")
	b.WriteString("RULES:\n")
	b.WriteString("- Refer to changes ONLY by the exact ids given below, in square brackets, e.g. [chg_1a2b3c4d5e6f7081].\n")
	b.WriteString("- NEVER invent an id. Every id you write must appear in the list.\n")
	b.WriteString("- Do not add facts. Everything you say must come from the list.\n")
	b.WriteString("- No buy/sell/hold advice, no price targets, no recommendation.\n")
	b.WriteString("- Do not tell the user what their thesis now means; describe what changed.\n")
	if firmer {
		b.WriteString("- YOUR PREVIOUS REPLY USED AN ID THAT DOES NOT EXIST. Use ONLY ids from this list, copied character for character.\n")
	}
	b.WriteString("\nCHANGES (newest first):\n")
	for _, it := range set.Items {
		b.WriteString("[" + it.ID + "] " + it.Kind + " — " + it.Summary)
		if it.Bearing != nil {
			b.WriteString(" (bearing: " + *it.Bearing + ")")
		}
		if it.DataState != dataStateLive {
			b.WriteString(" (" + it.DataState + " data)")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// callChangeSummary asks the llm for free text. Any transport failure returns "" so the caller falls
// through to retry and then to the deterministic stub — the model being down is never an error page.
func (s *Server) callChangeSummary(ctx context.Context, ticker, prompt, userID string) (string, string) {
	payload := map[string]any{
		"ticker":    ticker,
		"timeframe": "1D",
		// Deliberately empty: the change list in the message is the ONLY context this call gets.
		"context":  map[string]any{},
		"messages": []map[string]string{{"role": "user", "content": prompt}},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", "stub:offline"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.LLMURL+"/chat", strings.NewReader(string(body)))
	if err != nil {
		return "", "stub:offline"
	}
	req.Header.Set("Content-Type", "application/json")
	if userID != "" {
		req.Header.Set("X-User-Id", userID)
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return "", "stub:offline"
	}
	defer resp.Body.Close()
	var out map[string]any
	if json.NewDecoder(resp.Body).Decode(&out) != nil || resp.StatusCode >= 400 {
		return "", "stub:offline"
	}
	used := asString(out["modelUsed"])
	if used == "" {
		used = "unknown"
	}
	return strings.TrimSpace(asString(out["reply"])), used
}

// citedChangeIDs extracts every chg_ token in the text and splits it into ids that exist in the set
// and ids that do not. This is the check that makes "cannot introduce unknown change ids" true.
func citedChangeIDs(text string, known map[string]bool) (cited []string, unknown []string) {
	cited, unknown = []string{}, []string{}
	seen := map[string]bool{}
	for i := 0; i+4 <= len(text); i++ {
		if text[i:i+4] != "chg_" {
			continue
		}
		j := i + 4
		for j < len(text) && isHexByte(text[j]) {
			j++
		}
		id := text[i:j]
		if seen[id] {
			continue
		}
		seen[id] = true
		if known[id] {
			cited = append(cited, id)
		} else {
			unknown = append(unknown, id)
		}
	}
	return cited, unknown
}

func isHexByte(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
}

// stubChangeSummary is the deterministic fallback: counts by kind, composed in code. It cites real
// ids, so the UI renders identically whether the model answered or not.
func stubChangeSummary(set *changeSet) string {
	order := []string{changeThesisVersion, changeEvidenceLinked, changeEvidenceUnlinked,
		changeLensReview, changeAlertEvent, changeMarketContext, changeTranscriptCompar}
	labels := map[string]string{
		changeThesisVersion:    "thesis edit",
		changeEvidenceLinked:   "evidence link",
		changeEvidenceUnlinked: "evidence unlink",
		changeLensReview:       "saved lens review",
		changeAlertEvent:       "monitoring alert",
		changeMarketContext:    "market-context change",
		changeTranscriptCompar: "transcript analysis",
	}
	var parts []string
	for _, k := range order {
		if n := set.Counts[k]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s%s", n, labels[k], plural(n)))
		}
	}
	var b strings.Builder
	b.WriteString("Since your last review: " + strings.Join(parts, ", ") + ".")
	limit := 3
	if len(set.Items) < limit {
		limit = len(set.Items)
	}
	for i := 0; i < limit; i++ {
		b.WriteString(" [" + set.Items[i].ID + "] " + set.Items[i].Summary + ".")
	}
	return b.String()
}

// ---------------------------------------------------------------- collectors

// changesFromVersions reads the thesis's own append-only version history (§1.4). No network call:
// the versions travelled with the thesis.
func changesFromVersions(thesis map[string]any, ticker, thesisID string, since int64) []ChangeItem {
	raw, _ := thesis["versions"].([]any)
	out := []ChangeItem{}
	for _, v := range raw {
		ver, ok := v.(map[string]any)
		if !ok {
			continue
		}
		at := asInt64(ver["at"])
		if at <= since {
			continue // strictly after the boundary
		}
		fields := stringsFrom(ver["changedFields"])
		ref := changeSourceRef{Store: "journal-thesis-version", ID: asString(ver["id"])}
		out = append(out, ChangeItem{
			ID: changeID(changeThesisVersion, ref, at), Kind: changeThesisVersion, At: at,
			Summary:   summarizeVersion(asString(ver["actor"]), fields, asString(ver["reason"])),
			SourceRef: ref,
			// A user editing their own thesis is a change, not evidence for or against it.
			Bearing: nil, DataState: dataStateLive, ResearchLink: changeLinkFor(ticker, thesisID),
		})
	}
	return out
}

func summarizeVersion(actor string, fields []string, reason string) string {
	who := "You"
	if actor == "system" {
		who = "The system"
	}
	s := who + " edited the thesis"
	if len(fields) > 0 {
		s += " (" + strings.Join(fields, ", ") + ")"
	}
	if reason != "" {
		s += ": " + truncateString(reason, 160)
	}
	return s
}

// changesFromEvidenceLinks reads journal evidence links for this thesis. A link created after the
// boundary is `evidence_linked`; a link REVOKED after it is `evidence_unlinked` — unlinking is a
// real research event, and §2.6 keeps the record so it stays visible here.
func (s *Server) changesFromEvidenceLinks(ctx context.Context, thesisID, ticker string, since int64, cookie string) ([]ChangeItem, bool) {
	// includeRevoked=true is REQUIRED here: the journal hides revoked links by default, and an unlink
	// is precisely one of the changes this set has to report (§4.4's evidence_unlinked kind).
	res, err := s.journalGet(ctx,
		s.cfg.JournalURL+"/evidence/links?includeRevoked=true&thesisId="+url.QueryEscape(thesisID), cookie)
	if err != nil {
		return nil, false
	}
	raw, _ := res["links"].([]any)
	out := []ChangeItem{}
	for _, l := range raw {
		link, ok := l.(map[string]any)
		if !ok || asString(link["thesisId"]) != thesisID {
			continue
		}
		linkID := asString(link["id"])
		itemID := asString(link["thesisItemId"])
		bearing := asString(link["bearing"])
		authored := asString(link["authoredBy"])

		if at := asInt64(link["linkedAt"]); at > since {
			ref := changeSourceRef{Store: "journal-evidence-link", ID: linkID}
			out = append(out, ChangeItem{
				ID: changeID(changeEvidenceLinked, ref, at), Kind: changeEvidenceLinked, At: at,
				Summary:      summarizeLink(true, bearing, authored, asString(link["note"])),
				SourceRef:    ref,
				ThesisItemID: itemID,
				// The link's own bearing IS deterministic: the user (or, in the D-10 case, a rule the
				// user built from a named condition) recorded it. It is read, never inferred.
				Bearing:      optString(bearing),
				DataState:    dataStateLive,
				ResearchLink: changeLinkFor(ticker, thesisID),
			})
		}
		if revoked, _ := link["revoked"].(bool); revoked {
			if at := asInt64(link["revokedAt"]); at > since {
				ref := changeSourceRef{Store: "journal-evidence-link", ID: linkID}
				out = append(out, ChangeItem{
					ID: changeID(changeEvidenceUnlinked, ref, at), Kind: changeEvidenceUnlinked, At: at,
					Summary:      summarizeLink(false, bearing, authored, ""),
					SourceRef:    ref,
					ThesisItemID: itemID,
					Bearing:      nil, // removing a link asserts nothing about the thesis
					DataState:    dataStateLive,
					ResearchLink: changeLinkFor(ticker, thesisID),
				})
			}
		}
	}
	return out, true
}

func summarizeLink(linked bool, bearing, authoredBy, note string) string {
	verb := "Linked"
	if !linked {
		verb = "Unlinked"
	}
	what := "evidence"
	switch bearing {
	case "supports":
		what = "supporting evidence"
	case "contradicts":
		what = "contradicting evidence"
	case "neutral":
		what = "neutral evidence"
	}
	s := verb + " " + what
	if authoredBy == "system" {
		// System-authored links must be visually and structurally distinct from user-authored ones
		// (D-10's (d) marking). Saying so in the summary keeps that true in plain text too.
		s += " (added automatically by a rule you created)"
	}
	if note != "" {
		s += ": " + truncateString(note, 160)
	}
	return s
}

// changesFromReviews reads saved lens reviews (§3.7) whose target is this thesis.
func (s *Server) changesFromReviews(ctx context.Context, thesisID, ticker string, since int64, cookie string) ([]ChangeItem, bool) {
	res, err := s.journalGet(ctx, s.cfg.JournalURL+"/reviews?limit=200", cookie)
	if err != nil {
		return nil, false
	}
	raw, _ := res["reviews"].([]any)
	out := []ChangeItem{}
	for _, r := range raw {
		rev, ok := r.(map[string]any)
		if !ok {
			continue
		}
		if reviewTargetID(rev) != thesisID {
			continue
		}
		at := asInt64(rev["savedAt"])
		if at <= since {
			continue
		}
		ref := changeSourceRef{Store: "journal-review", ID: asString(rev["id"])}
		out = append(out, ChangeItem{
			ID: changeID(changeLensReview, ref, at), Kind: changeLensReview, At: at,
			Summary:   summarizeReview(rev),
			SourceRef: ref,
			// A saved review is "a clearly labelled saved review" in §4.2's sense, so its own stance
			// may travel — but only when the review itself recorded one. Nothing is inferred here.
			Bearing:      optString(asString(rev["bearing"])),
			DataState:    dataStateOr(asString(rev["dataState"])),
			ResearchLink: changeLinkFor(ticker, thesisID),
		})
	}
	return out, true
}

// reviewTargetID digs the target id out of a saved review, tolerating both the nested target object
// and a flat thesisId, so a review saved by any version of Step 07's writer still resolves.
func reviewTargetID(rev map[string]any) string {
	if tgt, ok := rev["target"].(map[string]any); ok {
		if asString(tgt["type"]) == "thesis" {
			return asString(tgt["id"])
		}
	}
	return asString(rev["thesisId"])
}

func summarizeReview(rev map[string]any) string {
	lens := asString(rev["lensLabel"])
	if lens == "" {
		lens = asString(rev["lensId"])
	}
	if lens == "" {
		lens = "A research lens"
	}
	s := "Saved a " + lens + " review"
	if n := countFindings(rev); n > 0 {
		s += fmt.Sprintf(" (%d finding%s)", n, plural(n))
	}
	return s
}

func countFindings(rev map[string]any) int {
	f, _ := rev["findings"].([]any)
	return len(f)
}

// changesFromAlertEvents reads the caller's own alert events for this thesis, straight from the
// alerts service with the caller's cookie (alerts scopes by that cookie itself).
//
// dedupeKey collapses repeat firings inside one cooldown window so the change set never shows one
// firing twice (§4.2) — the suppression mechanism itself stays in alerts and is untouched.
func (s *Server) changesFromAlertEvents(ctx context.Context, thesisID, ticker string, since int64, cookie string) ([]ChangeItem, bool) {
	u := fmt.Sprintf("%s/events?limit=500&thesisId=%s&since=%d",
		s.cfg.AlertsURL, url.QueryEscape(thesisID), since)
	res, err := s.journalGet(ctx, u, cookie) // plain cookie-forwarding GET; the helper is service-agnostic
	if err != nil {
		return nil, false
	}
	raw, _ := res["events"].([]any)
	out := []ChangeItem{}
	seen := map[string]bool{}
	for _, e := range raw {
		ev, ok := e.(map[string]any)
		if !ok || asString(ev["thesisId"]) != thesisID {
			continue
		}
		at := asInt64(ev["ts"])
		if at <= since {
			continue
		}
		if key := asString(ev["dedupeKey"]); key != "" {
			if seen[key] {
				continue
			}
			seen[key] = true
		}
		ref := changeSourceRef{Store: "alerts-event", ID: asString(ev["id"])}
		out = append(out, ChangeItem{
			ID: changeID(changeAlertEvent, ref, at), Kind: changeAlertEvent, At: at,
			Summary:      asString(ev["message"]),
			SourceRef:    ref,
			ThesisItemID: asString(ev["thesisItemId"]),
			// Carried through from the event, where it was set deterministically or left null. The
			// gateway does not compute a bearing of its own and never asks a model for one.
			Bearing:      optString(asString(ev["bearing"])),
			DataState:    dataStateOr(asString(ev["dataState"])),
			ResearchLink: changeLinkFor(ticker, thesisID),
		})
	}
	return out, true
}

// changesFromMarketContext compares deterministic analysis values AT THE BOUNDARY against the latest
// ones, using the dated series the analysis service already returns. This is a real comparison, not a
// snapshot of "now" dressed up as a change: the value at the checkpoint is read from the same bar
// series, so the same boundary always produces the same answer.
//
// Only material, stated transitions qualify (see materialMovePct) — otherwise this source would emit
// on every call and `empty` could never be true.
func (s *Server) changesFromMarketContext(ctx context.Context, ticker, thesisID string, since int64) ([]ChangeItem, bool) {
	if ticker == "" || since <= 0 {
		return nil, false
	}
	res, err := s.getJSON(ctx, s.cfg.AnalysisURL+"/analysis/"+url.PathEscape(ticker)+"?timeframe=1D")
	if err != nil {
		return nil, false
	}

	state := dataStateLive
	if synth, _ := res["sourceIsSynthetic"].(bool); synth {
		state = dataStateSynthetic
	}

	price, _ := res["price"].(map[string]any)
	bars, _ := price["ohlcv"].([]any)
	thenClose, thenTime, okThen := closeAtOrBefore(bars, since)
	nowClose, nowTime, okNow := lastClose(bars)
	if !okThen || !okNow || thenClose == 0 || nowTime <= thenTime {
		return []ChangeItem{}, true // not enough history to make an honest comparison
	}

	out := []ChangeItem{}
	link := changeLinkFor(ticker, thesisID)

	changePct := (nowClose/thenClose - 1) * 100
	if math.Abs(changePct) >= materialMovePct {
		ref := changeSourceRef{Store: "analysis-1d", ID: fmt.Sprintf("%s:close:%d", ticker, nowTime)}
		out = append(out, ChangeItem{
			ID: changeID(changeMarketContext, ref, nowTime), Kind: changeMarketContext, At: nowTime,
			Summary: fmt.Sprintf("%s moved %+.1f%% since your last review (%.2f → %.2f)",
				ticker, changePct, thenClose, nowClose),
			SourceRef: ref,
			// A price move is not evidence for or against a thesis — what it means is the user's
			// judgement, which is exactly the call this product refuses to make for them.
			Bearing: nil, DataState: state, ResearchLink: link,
		})
	}

	ind, _ := res["indicators"].(map[string]any)
	series, _ := ind["series"].(map[string]any)

	// RSI regime band, not the raw number: "RSI 68.4 → 71.2" is noise; crossing into overbought is
	// a stated, discrete transition.
	if thenRSI, _, ok1 := seriesValueAtOrBefore(series, "rsi", since); ok1 {
		if nowRSI, atRSI, ok2 := seriesLast(series, "rsi"); ok2 {
			if a, b := rsiBand(thenRSI), rsiBand(nowRSI); a != b {
				ref := changeSourceRef{Store: "analysis-1d", ID: fmt.Sprintf("%s:rsiBand:%d", ticker, atRSI)}
				out = append(out, ChangeItem{
					ID: changeID(changeMarketContext, ref, atRSI), Kind: changeMarketContext, At: atRSI,
					Summary: fmt.Sprintf("%s daily RSI moved from %s to %s (%.1f → %.1f)",
						ticker, a, b, thenRSI, nowRSI),
					SourceRef: ref, Bearing: nil, DataState: state, ResearchLink: link,
				})
			}
		}
	}

	// The 50/200 relationship flipping is the one moving-average transition worth a line.
	thenFast, _, f1 := seriesValueAtOrBefore(series, "sma50", since)
	thenSlow, _, f2 := seriesValueAtOrBefore(series, "sma200", since)
	nowFast, atFast, f3 := seriesLast(series, "sma50")
	nowSlow, _, f4 := seriesLast(series, "sma200")
	if f1 && f2 && f3 && f4 {
		was, is := thenFast >= thenSlow, nowFast >= nowSlow
		if was != is {
			rel := "below"
			if is {
				rel = "above"
			}
			ref := changeSourceRef{Store: "analysis-1d", ID: fmt.Sprintf("%s:maCross:%d", ticker, atFast)}
			out = append(out, ChangeItem{
				ID: changeID(changeMarketContext, ref, atFast), Kind: changeMarketContext, At: atFast,
				Summary: fmt.Sprintf("%s 50-day moving average moved %s the 200-day since your last review",
					ticker, rel),
				SourceRef: ref, Bearing: nil, DataState: state, ResearchLink: link,
			})
		}
	}
	return out, true
}

func rsiBand(v float64) string {
	switch {
	case v >= 70:
		return "overbought"
	case v <= 30:
		return "oversold"
	default:
		return "neutral"
	}
}

// changesFromTranscripts reports earnings-call analyses persisted since the boundary. The comparison
// itself (tone shift vs the stored prior quarter) is already computed and stored by
// services/llm/app/transcript.py — this lane reads that record, it does not recompute or re-ask.
func (s *Server) changesFromTranscripts(ctx context.Context, ticker, thesisID string, since int64) ([]ChangeItem, bool) {
	if ticker == "" {
		return nil, false
	}
	res, err := s.getJSON(ctx, s.cfg.LLMURL+"/transcript/"+url.PathEscape(ticker))
	if err != nil {
		return nil, false
	}
	raw, _ := res["analyses"].([]any)
	out := []ChangeItem{}
	for _, t := range raw {
		tr, ok := t.(map[string]any)
		if !ok {
			continue
		}
		// transcript.py stamps an ISO `timestamp` on every stored analysis and sorts by it.
		at := parseEvidenceTime(asString(tr["timestamp"]))
		if at <= since {
			continue
		}
		quarter := asString(tr["label"])
		ref := changeSourceRef{Store: "llm-transcript", ID: ticker + ":" + quarter}
		summary := "New earnings-call analysis stored for " + ticker
		if quarter != "" {
			summary += " (" + quarter + ")"
		}
		if shift := asString(tr["toneShift"]); shift != "" {
			summary += " — tone vs the prior quarter: " + shift
		}
		out = append(out, ChangeItem{
			ID: changeID(changeTranscriptCompar, ref, at), Kind: changeTranscriptCompar, At: at,
			Summary: summary, SourceRef: ref,
			// A stored tone comparison is a model reading of text. It is a change worth surfacing,
			// but it is not a deterministic bearing on the user's thesis.
			Bearing: nil, DataState: dataStateOr(asString(tr["dataState"])),
			ResearchLink: changeLinkFor(ticker, thesisID),
		})
	}
	return out, true
}

// ---------------------------------------------------------------- series helpers

// closeAtOrBefore finds the last daily close at or before `ts`. Bars are ascending by time.
func closeAtOrBefore(bars []any, ts int64) (float64, int64, bool) {
	var val float64
	var at int64
	found := false
	for _, b := range bars {
		bar, ok := b.(map[string]any)
		if !ok {
			continue
		}
		t := barTime(bar["time"])
		if t == 0 || t > ts {
			continue
		}
		if c, ok := finiteFloat(bar["close"]); ok {
			val, at, found = c, t, true
		}
	}
	return val, at, found
}

func lastClose(bars []any) (float64, int64, bool) {
	for i := len(bars) - 1; i >= 0; i-- {
		bar, ok := bars[i].(map[string]any)
		if !ok {
			continue
		}
		if c, ok := finiteFloat(bar["close"]); ok {
			return c, barTime(bar["time"]), true
		}
	}
	return 0, 0, false
}

func seriesValueAtOrBefore(series map[string]any, key string, ts int64) (float64, int64, bool) {
	points, _ := series[key].([]any)
	var val float64
	var at int64
	found := false
	for _, p := range points {
		pt, ok := p.(map[string]any)
		if !ok {
			continue
		}
		t := barTime(pt["time"])
		if t == 0 || t > ts {
			continue
		}
		if v, ok := finiteFloat(pt["value"]); ok {
			val, at, found = v, t, true
		}
	}
	return val, at, found
}

func seriesLast(series map[string]any, key string) (float64, int64, bool) {
	points, _ := series[key].([]any)
	for i := len(points) - 1; i >= 0; i-- {
		pt, ok := points[i].(map[string]any)
		if !ok {
			continue
		}
		if v, ok := finiteFloat(pt["value"]); ok {
			return v, barTime(pt["time"]), true
		}
	}
	return 0, 0, false
}

// barTime accepts both shapes lightweight-charts uses: a unix second (intraday) and a YYYY-MM-DD
// string (daily), which is what the analysis service emits for the 1D timeframe.
func barTime(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case string:
		if d, err := time.Parse("2006-01-02", t); err == nil {
			return d.Unix()
		}
		return parseEvidenceTime(t)
	}
	return 0
}

// finiteFloat is toFloat plus a finiteness check. A NaN close or indicator would otherwise sail
// through every comparison below and produce a change item describing a number that isn't one.
func finiteFloat(v any) (float64, bool) {
	f, ok := toFloat(v)
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// ---------------------------------------------------------------- small shared helpers

const (
	dataStateLive      = "live"
	dataStateSynthetic = "synthetic"
	dataStateSeed      = "seed"
)

func dataStateOr(s string) string {
	switch s {
	case dataStateLive, dataStateSeed, dataStateSynthetic:
		return s
	default:
		return dataStateLive
	}
}

// optString returns nil for "", so an absent bearing serializes as JSON null rather than "".
func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func asInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	}
	return 0
}

func stringsFrom(v any) []string {
	raw, _ := v.([]any)
	out := make([]string, 0, len(raw))
	for _, s := range raw {
		if str, ok := s.(string); ok && str != "" {
			out = append(out, str)
		}
	}
	return out
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
