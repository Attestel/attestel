package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// evidence.go — user-curated evidence and thesis↔evidence links. Contract: PARALLEL_CONTRACTS.md §2.
//
// An evidence record is a CAPTURE: "on this date, from this source, this was true / this was said".
// That framing decides most of the design here:
//
//  1. IDENTITY IS OPAQUE AND RANDOM. `id` is ev_ + 16 random hex, never derived from content, so a
//     re-worded headline or a corrected excerpt cannot change what a decision cited (§2.5). A separate
//     `dedupeKey` — a hash of the UPSTREAM identity — answers the different question "have I already
//     saved this thing?".
//  2. ONLY `note` IS MUTABLE. A changed upstream number is a NEW capture, not an edit of the old one;
//     otherwise yesterday's cited fact silently becomes today's and the citation is a lie.
//  3. COMPUTED FACTS CARRY THEIR COMPUTATION TIME AND TIMEFRAME. `fact.asOf` + `fact.timeframe` are in
//     the dedupe identity precisely because yesterday's RSI and today's RSI are different evidence,
//     and neither is a timeless truth.
//  4. PROVENANCE IS NEVER FAKED. A required provenance field that is missing is a 400 with the field
//     named — never a default, and never today's date standing in for an unknown publication time.
//  5. MODEL OUTPUT IS NOT EXTERNAL EVIDENCE (§2.7). A model finding is a review object. The single
//     exception is an explicitly `inference: true` record sourced from a saved review.
//
// Content policy is D-07 (c) + D-08 (a)+(c), approved as a PRODUCT position and still awaiting
// external counsel: link + bounded excerpt + normalized fact, gateway-fetched page text never
// persisted, one user's pasted text never served to another. Because counsel may narrow the caps, each
// cap is one named constant per kind (§2.4) so a narrowing is a one-line change.

// ---- limits ----
//
// Excerpt caps, one named constant per kind. `filing` is larger on purpose: SEC filings are US
// government works, treated separately from vendor transcripts and licensed news (D-08).
const (
	excerptCapDeterministicFact   = 0 // a computed number needs no prose
	excerptCapFundamentalFact     = 300
	excerptCapChartContext        = 300
	excerptCapNews                = 1200
	excerptCapFiling              = 4000
	excerptCapTranscriptExcerpt   = 1200
	excerptCapManagementStatement = 1200
	excerptCapCatalyst            = 600
	excerptCapUserNote            = 0 // the content of a user note lives in `note`
)

const (
	maxEvidenceTitleLen = 300
	maxEvidenceNoteLen  = 1000
	maxLinkNoteLen      = 500
	maxFactKeyLen       = 120
	maxFactUnitLen      = 40

	// Page size for GET /evidence and GET /evidence/links.
	defaultEvidenceLimit = 100
	maxEvidenceLimit     = 500

	evidenceSchemaVersion = 1
)

// ---- enums (closed sets; adding a member is a contract amendment, not a judgement call) ----

var validEvidenceBearing = map[string]bool{"supports": true, "contradicts": true, "context": true}

var validDataState = map[string]bool{"live": true, "seed": true, "synthetic": true}

// validEvidenceProvider is the closed set from §2.3 — exactly the sources this code can actually
// produce. A provider outside it is a 400, not a free-text label, because provenance that nobody
// validated is provenance nobody can trust.
var validEvidenceProvider = map[string]bool{
	"analysis": true, "alphavantage": true, "marketaux": true, "sec-edgar": true,
	"tiingo": true, "yfinance": true, "fmp": true, "fred": true, "twelvedata": true,
	"alpaca": true, "stocktwits": true, "google-news-rss": true, "synthetic": true,
	"seed": true, "llm": true, "user": true,
}

// thesisItemPrefixes are the item kinds a link may point at (§2.6). `qst_` (next question) is
// deliberately absent: an open question is not something evidence bears on — answering it edits the
// thesis, which is the thesis lane's job.
var thesisItemPrefixes = []string{"asm_", "cat_", "inv_", "rsk_"}

// ---- kind specification ----

// kindSpec is the per-kind provenance contract from §2.3. `required` holds field paths so a failed
// save can name exactly what is missing instead of returning a generic validation error. `sourceTime|
// fact.asOf` means "either one".
type kindSpec struct {
	excerptCap int
	required   []string
	// sourceRefStore, when set, pins which store a sourceRef may point at. This is what enforces
	// D-08's hard rule that a transcript excerpt can only come from a STORED transcript snapshot —
	// never from the gateway's URL-fetch-and-strip path.
	sourceRefStore string
}

var evidenceKinds = map[string]kindSpec{
	"deterministic_fact": {
		excerptCap: excerptCapDeterministicFact,
		required:   []string{"fact", "provider", "dataState", "fact.asOf", "fact.timeframe"},
	},
	"fundamental_fact": {
		excerptCap: excerptCapFundamentalFact,
		required:   []string{"fact", "provider", "dataState"},
	},
	"chart_context": {
		excerptCap: excerptCapChartContext,
		required:   []string{"fact", "provider", "dataState", "fact.timeframe"},
	},
	"news": {
		excerptCap: excerptCapNews,
		required:   []string{"sourceUrl", "provider", "attribution", "sourceTime", "dataState"},
	},
	"filing": {
		excerptCap: excerptCapFiling,
		required:   []string{"sourceUrl", "provider", "sourceRef.id", "dataState"},
	},
	"transcript_excerpt": {
		excerptCap:     excerptCapTranscriptExcerpt,
		required:       []string{"sourceRef", "attribution", "dataState"},
		sourceRefStore: "llm-transcript",
	},
	"management_statement": {
		excerptCap:     excerptCapManagementStatement,
		required:       []string{"sourceRef", "attribution", "title", "dataState"},
		sourceRefStore: "llm-transcript",
	},
	"catalyst": {
		excerptCap: excerptCapCatalyst,
		required:   []string{"title", "provider", "sourceTime|fact.asOf", "dataState"},
	},
	"user_note": {
		excerptCap: excerptCapUserNote,
		required:   []string{"note", "dataState"},
	},
}

// ---- types ----

// EvidenceFact is the normalized fact: one named value with the time and timeframe it was computed
// for. Value is `any` because a fact is legitimately numeric ("rsi14": 71.2) or categorical
// ("trend": "uptrend"); it is stored as given and never coerced.
type EvidenceFact struct {
	Key       string `json:"key"`
	Value     any    `json:"value"`
	Unit      string `json:"unit"`
	AsOf      string `json:"asOf"`      // YYYY-MM-DD (or an ISO instant for intraday)
	Timeframe string `json:"timeframe"` // 1D | 1H | 15m | 5m — "" for non-chart facts
}

// SourceRef points at an artefact in a store that is NOT this one. Model artefacts (reads,
// transcripts, committee snapshots) stay exactly where they are and are referenced, never copied
// (§2.1) — so a transcript analysis has one home, and evidence cites it.
type SourceRef struct {
	Store   string `json:"store"`   // llm-transcript | llm-read | llm-committee | journal-review | sec-edgar
	ID      string `json:"id"`      // e.g. "NVDA/Q1-FY2027", or an SEC accession number
	Locator string `json:"locator"` // e.g. "evasiveMoments:0:answer"
}

// EvidenceRecord is one capture. Field order mirrors §2.2.
type EvidenceRecord struct {
	ID            string `json:"id"` // ev_ + 16 hex, random — NEVER derived from content
	SchemaVersion int    `json:"schemaVersion"`
	Ticker        string `json:"ticker"`
	Kind          string `json:"kind"`

	Title            string `json:"title"`   // display only; never part of identity
	Excerpt          string `json:"excerpt"` // bounded quotation, may be ""
	ExcerptTruncated bool   `json:"excerptTruncated"`

	Fact *EvidenceFact `json:"fact"` // null for non-fact kinds
	Note string        `json:"note"` // user-authored; the ONLY mutable field

	Provider   string     `json:"provider"`
	SourceURL  string     `json:"sourceUrl"`
	SourceRef  *SourceRef `json:"sourceRef"`
	SourceTime int64      `json:"sourceTime"` // 0 = unknown, and the UI must say "unknown"
	CapturedAt int64      `json:"capturedAt"` // server-set

	DataState   string `json:"dataState"`   // live | seed | synthetic
	Attribution string `json:"attribution"` // required display string (D-09)

	Inference bool   `json:"inference"` // true => model-derived content, not observed
	ModelUsed string `json:"modelUsed"` // set only when Inference

	DedupeKey string `json:"dedupeKey"`
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
}

// EvidenceLink attaches one record to one thesis with a BEARING. `contradicts` is first-class: a
// thesis holding both supporting and contradicting evidence is the desired state, not an error.
type EvidenceLink struct {
	ID           string  `json:"id"` // lnk_ + 16 hex
	ThesisID     string  `json:"thesisId"`
	EvidenceID   string  `json:"evidenceId"`
	Bearing      string  `json:"bearing"`
	ThesisItemID *string `json:"thesisItemId"` // optional asm_|cat_|inv_|rsk_ this link is about
	Note         string  `json:"note"`

	// AuthoredBy is "user" for everything this lane writes. "system" is reserved for the one
	// D-10 case (an alert firing on a condition the user themselves named) — which the monitoring
	// lane builds. A system link is revoked, never deleted, so the audit trail survives.
	AuthoredBy string `json:"authoredBy"`
	Revoked    bool   `json:"revoked"`
	RevokedAt  *int64 `json:"revokedAt"`

	LinkedAt  int64 `json:"linkedAt"`
	CreatedAt int64 `json:"createdAt"`
}

// ---- errors ----

var (
	errEvidenceUnreadable  = errors.New("stored evidence could not be read")
	errEvidenceReservedUID = errors.New("reserved user partition")
)

// writeEvidenceError emits {error, code, ...extra}. `code` is what the frontend switches on: a cap
// breach, a missing provenance field, and a linked-delete conflict need three different UI
// responses, and none of them is a generic toast.
func writeEvidenceError(w http.ResponseWriter, status int, code, msg string, extra map[string]any) {
	body := map[string]any{"error": msg, "code": code}
	for k, v := range extra {
		body[k] = v
	}
	writeJSON(w, status, body)
}

// writeStoreError maps the two store-level failures onto responses. store_unreadable is a 503, not a
// 500: the data is intact, we are refusing to overwrite it, and a retry after the file is fixed will
// work.
func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errEvidenceUnreadable):
		writeEvidenceError(w, http.StatusServiceUnavailable, "store_unreadable",
			"stored evidence could not be read; refusing to overwrite it", nil)
	default:
		log.Printf("journal: evidence store error: %v", err)
		writeEvidenceError(w, http.StatusInternalServerError, "store_error", "evidence store unavailable", nil)
	}
}

// ---- ids, dedupe ----

func newEvidenceID() string { return "ev_" + newID() }
func newLinkID() string     { return "lnk_" + newID() }

// canonicalSourceURL normalizes a URL for dedupe: lowercase scheme+host, tracking parameters
// dropped. Two links to the same article that differ only by a utm_source are the same article.
func canonicalSourceURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return strings.TrimSpace(raw)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	q := u.Query()
	for key := range q {
		lk := strings.ToLower(key)
		if strings.HasPrefix(lk, "utm_") || lk == "fbclid" || lk == "gclid" {
			q.Del(key)
		}
	}
	u.RawQuery = q.Encode()
	u.Fragment = ""
	return u.String()
}

// normalizeTitleForDedupe collapses case and whitespace. Used ONLY for the catalyst dedupe identity,
// where no URL or accession number exists — never for record identity.
func normalizeTitleForDedupe(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(s)), " ")
}

// evidenceIdentity is the UPSTREAM identity of a capture, per the §2.5 table. It answers "which
// thing in the world is this?" — never "which record is this?". `user_note` returns the record's own
// id: two notes with identical words are two observations, so notes never dedupe.
func evidenceIdentity(e *EvidenceRecord) string {
	switch e.Kind {
	case "news":
		return canonicalSourceURL(e.SourceURL)
	case "filing":
		if e.SourceRef != nil {
			return e.SourceRef.ID
		}
		return canonicalSourceURL(e.SourceURL)
	case "transcript_excerpt", "management_statement":
		if e.SourceRef != nil {
			return e.SourceRef.ID + "#" + e.SourceRef.Locator
		}
		return e.ID
	case "deterministic_fact", "chart_context":
		if e.Fact != nil {
			return e.Fact.Key + "@" + e.Fact.Timeframe + "@" + e.Fact.AsOf
		}
		return e.ID
	case "fundamental_fact":
		if e.Fact != nil {
			return e.Fact.Key + "@" + e.Fact.AsOf
		}
		return e.ID
	case "catalyst":
		return e.Provider + "#" + normalizeTitleForDedupe(e.Title)
	default: // user_note
		return e.ID
	}
}

// evidenceDedupeKey = sha256(kind \0 identity \0 TICKER).
func evidenceDedupeKey(e *EvidenceRecord) string {
	sum := sha256.Sum256([]byte(e.Kind + "\x00" + evidenceIdentity(e) + "\x00" + e.Ticker))
	return hex.EncodeToString(sum[:])
}

// ---- validation ----

// truncateRunes cuts to n runes (not bytes) so a multi-byte character is never split into mojibake.
// Returns the text and whether anything was removed.
func truncateRunes(s string, n int) (string, bool) {
	r := []rune(s)
	if len(r) <= n {
		return s, false
	}
	return string(r[:n]), true
}

// hasField answers one entry of a kindSpec.required list against a decoded record.
func hasField(e *EvidenceRecord, path string) bool {
	// "a|b" — either satisfies the requirement (catalyst: a date from the event or from the fact).
	if strings.Contains(path, "|") {
		for _, alt := range strings.Split(path, "|") {
			if hasField(e, alt) {
				return true
			}
		}
		return false
	}
	switch path {
	case "fact":
		return e.Fact != nil && strings.TrimSpace(e.Fact.Key) != ""
	case "fact.asOf":
		return e.Fact != nil && strings.TrimSpace(e.Fact.AsOf) != ""
	case "fact.timeframe":
		return e.Fact != nil && strings.TrimSpace(e.Fact.Timeframe) != ""
	case "provider":
		return e.Provider != ""
	case "dataState":
		return e.DataState != ""
	case "attribution":
		return strings.TrimSpace(e.Attribution) != ""
	case "sourceUrl":
		return strings.TrimSpace(e.SourceURL) != ""
	case "sourceTime":
		return e.SourceTime > 0
	case "sourceRef":
		return e.SourceRef != nil && strings.TrimSpace(e.SourceRef.ID) != ""
	case "sourceRef.id":
		return e.SourceRef != nil && strings.TrimSpace(e.SourceRef.ID) != ""
	case "title":
		return strings.TrimSpace(e.Title) != ""
	case "note":
		return strings.TrimSpace(e.Note) != ""
	}
	return false
}

// validateEvidence normalizes and checks a record. Two distinct failure modes, deliberately:
//
//   - a missing PROVENANCE field is `provenance_incomplete` with the missing paths listed, because
//     the honest fix is for the caller to supply the provenance, not for the server to guess it;
//   - an over-long excerpt is NOT a failure. It is truncated and flagged (§2.4), so a save never
//     silently fails on length — the user keeps their capture and can see it was shortened.
func validateEvidence(e *EvidenceRecord) *evidenceValidationError {
	e.Ticker = strings.ToUpper(strings.TrimSpace(e.Ticker))
	if e.Ticker == "" {
		return badEvidence("validation_failed", "ticker is required", nil)
	}
	e.Kind = strings.TrimSpace(e.Kind)
	spec, ok := evidenceKinds[e.Kind]
	if !ok {
		return badEvidence("unknown_kind", "unknown evidence kind: "+e.Kind, map[string]any{"kinds": evidenceKindNames()})
	}

	if !validDataState[e.DataState] {
		return badEvidence("validation_failed", "dataState must be live, seed, or synthetic", nil)
	}
	if e.Provider != "" && !validEvidenceProvider[e.Provider] {
		return badEvidence("unknown_provider", "unknown provider: "+e.Provider, nil)
	}

	// Model attribution: `inference` and `modelUsed` travel together, and an inference record must
	// cite the saved review it came from (§2.7). Anything else would let model prose enter the
	// evidence store as if it had been observed in the world.
	if e.Inference {
		if e.Provider != "llm" {
			return badEvidence("validation_failed", "an inference record must have provider \"llm\"", nil)
		}
		if strings.TrimSpace(e.ModelUsed) == "" {
			return badEvidence("validation_failed", "modelUsed is required when inference is true", nil)
		}
		if e.SourceRef == nil || e.SourceRef.Store != "journal-review" {
			return badEvidence("validation_failed",
				"an inference record must reference a saved review (sourceRef.store = \"journal-review\")", nil)
		}
	} else {
		e.ModelUsed = ""
	}

	if spec.sourceRefStore != "" {
		if e.SourceRef == nil || e.SourceRef.Store != spec.sourceRefStore {
			return badEvidence("validation_failed",
				e.Kind+" requires sourceRef.store = \""+spec.sourceRefStore+"\"", nil)
		}
	}

	var missing []string
	for _, path := range spec.required {
		if !hasField(e, path) {
			missing = append(missing, path)
		}
	}
	if len(missing) > 0 {
		return badEvidence("provenance_incomplete",
			"missing required provenance for "+e.Kind, map[string]any{"missing": missing})
	}

	e.Title, _ = truncateRunes(strings.TrimSpace(e.Title), maxEvidenceTitleLen)
	e.Note, _ = truncateRunes(strings.TrimSpace(e.Note), maxEvidenceNoteLen)
	e.Excerpt, e.ExcerptTruncated = truncateRunes(strings.TrimSpace(e.Excerpt), spec.excerptCap)
	e.SourceURL = strings.TrimSpace(e.SourceURL)

	if e.Fact != nil {
		e.Fact.Key, _ = truncateRunes(strings.TrimSpace(e.Fact.Key), maxFactKeyLen)
		e.Fact.Unit, _ = truncateRunes(strings.TrimSpace(e.Fact.Unit), maxFactUnitLen)
		e.Fact.AsOf = strings.TrimSpace(e.Fact.AsOf)
		e.Fact.Timeframe = normalizeTimeframeOrEmpty(e.Fact.Timeframe)
	}
	if e.SourceRef != nil {
		e.SourceRef.Store = strings.TrimSpace(e.SourceRef.Store)
		e.SourceRef.ID = strings.TrimSpace(e.SourceRef.ID)
		e.SourceRef.Locator = strings.TrimSpace(e.SourceRef.Locator)
	}
	if e.SourceTime < 0 {
		e.SourceTime = 0
	}
	return nil
}

// normalizeTimeframeOrEmpty normalizes a timeframe but keeps "" as "" — an EPS datum has no chart
// timeframe, and defaulting it to 1D would invent a claim about the data.
func normalizeTimeframeOrEmpty(tf string) string {
	if strings.TrimSpace(tf) == "" {
		return ""
	}
	return normalizeTimeframe(tf)
}

func evidenceKindNames() []string {
	out := make([]string, 0, len(evidenceKinds))
	for k := range evidenceKinds {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

type evidenceValidationError struct {
	Code  string
	Msg   string
	Extra map[string]any
}

func badEvidence(code, msg string, extra map[string]any) *evidenceValidationError {
	return &evidenceValidationError{Code: code, Msg: msg, Extra: extra}
}

// ---- HTTP surface ----

// evidenceAPI owns the evidence routes. It is constructed inside registerEvidenceRoutes rather than
// hung off Server, which keeps this lane's footprint in shared files down to a single registration
// line (PARALLEL_EXECUTION.md § shared-file rule).
type evidenceAPI struct {
	srv   *Server
	store *evidenceStore
}

// registerEvidenceRoutes mounts the evidence surface. Reads are optionalAuth (a guest sees an empty
// list, exactly like the journal's other reads); every write is requireAuth.
func (s *Server) registerEvidenceRoutes(mux *http.ServeMux) {
	store, err := openEvidenceStoreWithRepository(s.cfg.TradesDir, s.documents)
	if err != nil {
		// Evidence is additive: if its directory cannot be opened the rest of the journal must still
		// serve trades and theses. The routes simply are not mounted and the frontend degrades.
		log.Printf("journal: evidence store unavailable in %q: %v (evidence routes not mounted)", s.cfg.TradesDir, err)
		return
	}
	api := &evidenceAPI{srv: s, store: store}

	mux.HandleFunc("GET /evidence", s.optionalAuth(api.handleList))
	mux.HandleFunc("POST /evidence", s.requireAuth(api.handleCreate))
	mux.HandleFunc("PATCH /evidence/{id}", s.requireAuth(api.handlePatch))
	mux.HandleFunc("DELETE /evidence/{id}", s.requireAuth(api.handleDelete))
	// The literal /evidence/links patterns are more specific than /evidence/{id}, so they win.
	mux.HandleFunc("GET /evidence/links", s.optionalAuth(api.handleListLinks))
	mux.HandleFunc("POST /evidence/links", s.requireAuth(api.handleCreateLink))
	mux.HandleFunc("PATCH /evidence/links/{id}", s.requireAuth(api.handlePatchLink))
	mux.HandleFunc("DELETE /evidence/links/{id}", s.requireAuth(api.handleDeleteLink))
}

// ---- records ----

// handleList returns the caller's evidence, newest capture first, with cursor pagination. The
// `thesisId` filter resolves through the LINK store, so "evidence on this thesis" is one call.
func (a *evidenceAPI) handleList(w http.ResponseWriter, r *http.Request) {
	uid := userID(r)
	q := r.URL.Query()
	ticker := strings.ToUpper(strings.TrimSpace(q.Get("ticker")))
	kind := strings.TrimSpace(q.Get("kind"))
	thesisID := strings.TrimSpace(q.Get("thesisId"))
	limit := parseLimit(q.Get("limit"), defaultEvidenceLimit, maxEvidenceLimit)

	// A guest has no partition to read. Empty, not 401 — reads stay open so the UI can render the
	// section and its sign-in prompt without a failed request.
	if uid == "" {
		writeJSON(w, http.StatusOK, map[string]any{"evidence": []EvidenceRecord{}, "nextCursor": ""})
		return
	}

	records := a.store.listRecords(uid)

	var allow map[string]bool
	if thesisID != "" {
		allow = map[string]bool{}
		for _, l := range a.store.listLinks(uid) {
			if l.ThesisID == thesisID && !l.Revoked {
				allow[l.EvidenceID] = true
			}
		}
	}

	out := make([]EvidenceRecord, 0, limit)
	cursor := strings.TrimSpace(q.Get("cursor"))
	past := cursor == ""
	next := ""
	for _, rec := range records {
		if !past {
			if rec.ID == cursor {
				past = true
			}
			continue
		}
		if ticker != "" && rec.Ticker != ticker {
			continue
		}
		if kind != "" && rec.Kind != kind {
			continue
		}
		if allow != nil && !allow[rec.ID] {
			continue
		}
		if len(out) == limit {
			next = out[len(out)-1].ID
			break
		}
		out = append(out, rec)
	}
	writeJSON(w, http.StatusOK, map[string]any{"evidence": out, "nextCursor": next})
}

// evidenceCreateBody is a record plus two request-only flags. Server-owned fields on the embedded
// record (id, capturedAt, dedupeKey, createdAt, updatedAt, schemaVersion) are overwritten
// unconditionally below — a client cannot choose its own identity or backdate a capture.
type evidenceCreateBody struct {
	EvidenceRecord
	CaptureNew bool `json:"captureNew"`
	// Link, when present, saves and links in one request so the "save to thesis" flow is one
	// round-trip and cannot half-succeed into an orphaned record the user never sees.
	Link *struct {
		ThesisID     string  `json:"thesisId"`
		Bearing      string  `json:"bearing"`
		ThesisItemID *string `json:"thesisItemId"`
		Note         string  `json:"note"`
	} `json:"link"`
}

func (a *evidenceAPI) handleCreate(w http.ResponseWriter, r *http.Request) {
	uid := userID(r)
	var body evidenceCreateBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeEvidenceError(w, http.StatusBadRequest, "invalid_json", "invalid JSON: "+err.Error(), nil)
		return
	}
	rec := body.EvidenceRecord
	if verr := validateEvidence(&rec); verr != nil {
		writeEvidenceError(w, http.StatusBadRequest, verr.Code, verr.Msg, verr.Extra)
		return
	}

	// A link supplied inline is validated BEFORE the record is written, so a bad bearing or a
	// foreign thesis id cannot leave a saved record with no link behind it.
	if body.Link != nil {
		if !validEvidenceBearing[body.Link.Bearing] {
			writeEvidenceError(w, http.StatusBadRequest, "validation_failed",
				"bearing must be supports, contradicts, or context", nil)
			return
		}
		if err := a.checkThesisItemID(body.Link.ThesisItemID); err != nil {
			writeEvidenceError(w, http.StatusBadRequest, "validation_failed", err.Error(), nil)
			return
		}
		if _, ok := a.srv.theses.Get(uid, body.Link.ThesisID); !ok {
			// 404, never 403: a foreign id must not be distinguishable from a missing one.
			writeEvidenceError(w, http.StatusNotFound, "not_found", "thesis not found", nil)
			return
		}
	}

	now := time.Now().Unix()
	rec.ID = newEvidenceID()
	rec.SchemaVersion = evidenceSchemaVersion
	rec.CapturedAt = now
	rec.CreatedAt = now
	rec.UpdatedAt = now
	rec.DedupeKey = evidenceDedupeKey(&rec)
	if body.CaptureNew {
		// An explicit re-capture: same upstream thing, deliberately saved again at a later time.
		// Suffixing keeps the key unique so it does not collide with the earlier capture.
		rec.DedupeKey += "#" + strconv.FormatInt(now, 10)
	}

	stored, deduped, err := a.store.addRecord(uid, rec)
	if err != nil {
		writeStoreError(w, err)
		return
	}

	resp := map[string]any{"evidence": stored, "deduplicated": deduped}
	status := http.StatusCreated
	if deduped {
		status = http.StatusOK
	}
	// §6.2: evidence_saved fires on a 201 only. A dedupe hit wrote no new record, so counting it would
	// inflate research depth with the same source item saved twice.
	if !deduped {
		emitEvidenceEvent(evEvidenceSaved, uid, stored, "created", "")
	}

	if body.Link != nil {
		link, updated, lerr := a.store.upsertLink(uid, EvidenceLink{
			ID:           newLinkID(),
			ThesisID:     body.Link.ThesisID,
			EvidenceID:   stored.ID,
			Bearing:      body.Link.Bearing,
			ThesisItemID: body.Link.ThesisItemID,
			Note:         trimTo(body.Link.Note, maxLinkNoteLen),
			AuthoredBy:   "user",
			LinkedAt:     now,
			CreatedAt:    now,
		})
		if lerr != nil {
			writeStoreError(w, lerr)
			return
		}
		resp["link"] = link
		resp["linkUpdated"] = updated
		emitEvidenceLinkEvent(uid, link, stored)
	}
	writeJSON(w, status, resp)
}

// emitEvidenceEvent carries the capture's PROVENANCE — kind, provider, data state — and never its
// title, excerpt, note, or source URL (§6.4). Those four are the whole reason the forbidden-field list
// exists: they are where a user's research actually lives.
func emitEvidenceEvent(event, uid string, rec EvidenceRecord, transitionKey, bearing string) {
	analytics.emit(eventInput{
		Event:         event,
		UID:           uid,
		TransitionKey: transitionKey,
		Ticker:        rec.Ticker,
		ObjectID:      rec.ID,
		Kind:          rec.Kind,
		Provider:      rec.Provider,
		DataState:     rec.DataState,
		Bearing:       bearing,
	})
}

// emitEvidenceLinkEvent records that a capture was attached to a thesis with a bearing. The bearing is
// the transition key, so re-saving the same link is idempotent while changing supports -> contradicts
// is a genuine second event.
func emitEvidenceLinkEvent(uid string, link EvidenceLink, rec EvidenceRecord) {
	if link.Revoked {
		return
	}
	analytics.emit(eventInput{
		Event:         evEvidenceLinked,
		UID:           uid,
		TransitionKey: link.Bearing,
		Ticker:        rec.Ticker,
		ObjectID:      link.ID,
		ThesisID:      link.ThesisID,
		Bearing:       link.Bearing,
		Kind:          rec.Kind,
		Provider:      rec.Provider,
		DataState:     rec.DataState,
	})
}

// evidencePatch carries the one mutable field. Any other field in the body is rejected rather than
// ignored: silently dropping an attempted edit to `excerpt` would let a caller believe capture state
// is editable.
type evidencePatch struct {
	Note *string `json:"note"`
}

var immutableEvidenceFields = []string{
	"ticker", "kind", "title", "excerpt", "excerptTruncated", "fact", "provider", "sourceUrl",
	"sourceRef", "sourceTime", "capturedAt", "dataState", "attribution", "inference", "modelUsed",
	"dedupeKey", "id", "schemaVersion", "createdAt", "updatedAt",
}

func (a *evidenceAPI) handlePatch(w http.ResponseWriter, r *http.Request) {
	uid := userID(r)
	raw := map[string]json.RawMessage{}
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		writeEvidenceError(w, http.StatusBadRequest, "invalid_json", "invalid JSON: "+err.Error(), nil)
		return
	}
	for _, f := range immutableEvidenceFields {
		if _, present := raw[f]; present {
			writeEvidenceError(w, http.StatusBadRequest, "immutable_field",
				f+" cannot be edited; a changed upstream fact is a new capture", map[string]any{"field": f})
			return
		}
	}
	var p evidencePatch
	if n, ok := raw["note"]; ok {
		if err := json.Unmarshal(n, &p.Note); err != nil {
			writeEvidenceError(w, http.StatusBadRequest, "invalid_json", "note must be a string", nil)
			return
		}
	}
	if p.Note == nil {
		writeEvidenceError(w, http.StatusBadRequest, "validation_failed", "nothing to update (note only)", nil)
		return
	}
	rec, ok, err := a.store.patchNote(uid, r.PathValue("id"), trimTo(*p.Note, maxEvidenceNoteLen))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if !ok {
		writeEvidenceError(w, http.StatusNotFound, "not_found", "evidence not found", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"evidence": rec})
}

// handleDelete refuses to delete evidence that is still attached to a thesis, and says which theses
// (§2.6). Deleting a cited record silently would break the citation behind the user's back; making
// them pass ?force=true means the cascade is a decision, not an accident.
func (a *evidenceAPI) handleDelete(w http.ResponseWriter, r *http.Request) {
	uid := userID(r)
	force := r.URL.Query().Get("force") == "true"
	found, blocking, err := a.store.deleteRecord(uid, r.PathValue("id"), force)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if !found {
		writeEvidenceError(w, http.StatusNotFound, "not_found", "evidence not found", nil)
		return
	}
	if len(blocking) > 0 {
		seen := map[string]bool{}
		ids := make([]string, 0, len(blocking))
		for _, l := range blocking {
			if !seen[l.ThesisID] {
				seen[l.ThesisID] = true
				ids = append(ids, l.ThesisID)
			}
		}
		writeEvidenceError(w, http.StatusConflict, "evidence_linked",
			"this evidence is linked to a thesis; unlink it or delete with force=true",
			map[string]any{"linkCount": len(blocking), "thesisIds": ids})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deletedLinks": force})
}

// ---- links ----

func (a *evidenceAPI) handleListLinks(w http.ResponseWriter, r *http.Request) {
	uid := userID(r)
	thesisID := strings.TrimSpace(r.URL.Query().Get("thesisId"))
	evidenceID := strings.TrimSpace(r.URL.Query().Get("evidenceId"))
	includeRevoked := r.URL.Query().Get("includeRevoked") == "true"

	if uid == "" {
		writeJSON(w, http.StatusOK, map[string]any{"links": []EvidenceLink{}})
		return
	}
	out := make([]EvidenceLink, 0)
	for _, l := range a.store.listLinks(uid) {
		if thesisID != "" && l.ThesisID != thesisID {
			continue
		}
		if evidenceID != "" && l.EvidenceID != evidenceID {
			continue
		}
		if l.Revoked && !includeRevoked {
			continue
		}
		out = append(out, l)
	}
	writeJSON(w, http.StatusOK, map[string]any{"links": out})
}

type linkCreateBody struct {
	ThesisID     string  `json:"thesisId"`
	EvidenceID   string  `json:"evidenceId"`
	Bearing      string  `json:"bearing"`
	ThesisItemID *string `json:"thesisItemId"`
	Note         string  `json:"note"`
}

func (a *evidenceAPI) handleCreateLink(w http.ResponseWriter, r *http.Request) {
	uid := userID(r)
	var body linkCreateBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeEvidenceError(w, http.StatusBadRequest, "invalid_json", "invalid JSON: "+err.Error(), nil)
		return
	}
	if !validEvidenceBearing[body.Bearing] {
		writeEvidenceError(w, http.StatusBadRequest, "validation_failed",
			"bearing must be supports, contradicts, or context", nil)
		return
	}
	if err := a.checkThesisItemID(body.ThesisItemID); err != nil {
		writeEvidenceError(w, http.StatusBadRequest, "validation_failed", err.Error(), nil)
		return
	}
	// Both ends are resolved against the CALLER'S OWN stores. A link is a claim about two records;
	// if either is not the caller's, the link cannot be made and existence is not confirmed.
	if _, ok := a.srv.theses.Get(uid, body.ThesisID); !ok {
		writeEvidenceError(w, http.StatusNotFound, "not_found", "thesis not found", nil)
		return
	}
	if _, ok := a.store.getRecord(uid, body.EvidenceID); !ok {
		writeEvidenceError(w, http.StatusNotFound, "not_found", "evidence not found", nil)
		return
	}

	now := time.Now().Unix()
	link, updated, err := a.store.upsertLink(uid, EvidenceLink{
		ID:           newLinkID(),
		ThesisID:     body.ThesisID,
		EvidenceID:   body.EvidenceID,
		Bearing:      body.Bearing,
		ThesisItemID: body.ThesisItemID,
		Note:         trimTo(body.Note, maxLinkNoteLen),
		AuthoredBy:   "user",
		LinkedAt:     now,
		CreatedAt:    now,
	})
	if err != nil {
		writeStoreError(w, err)
		return
	}
	status := http.StatusCreated
	if updated {
		status = http.StatusOK
	}
	rec, _ := a.store.getRecord(uid, body.EvidenceID)
	emitEvidenceLinkEvent(uid, link, rec)
	writeJSON(w, status, map[string]any{"link": link, "updated": updated})
}

type linkPatch struct {
	Bearing *string `json:"bearing"`
	Note    *string `json:"note"`
	Revoked *bool   `json:"revoked"`
}

func (a *evidenceAPI) handlePatchLink(w http.ResponseWriter, r *http.Request) {
	uid := userID(r)
	var p linkPatch
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeEvidenceError(w, http.StatusBadRequest, "invalid_json", "invalid JSON: "+err.Error(), nil)
		return
	}
	if p.Bearing == nil && p.Note == nil && p.Revoked == nil {
		writeEvidenceError(w, http.StatusBadRequest, "validation_failed",
			"nothing to update (bearing, note, revoked)", nil)
		return
	}
	if p.Bearing != nil && !validEvidenceBearing[*p.Bearing] {
		writeEvidenceError(w, http.StatusBadRequest, "validation_failed",
			"bearing must be supports, contradicts, or context", nil)
		return
	}
	if p.Note != nil {
		trimmed := trimTo(*p.Note, maxLinkNoteLen)
		p.Note = &trimmed
	}
	link, ok, err := a.store.patchLink(uid, r.PathValue("id"), p.Bearing, p.Note, p.Revoked)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if !ok {
		writeEvidenceError(w, http.StatusNotFound, "not_found", "link not found", nil)
		return
	}
	// A bearing change is a change of BELIEF about the evidence, and §6.2 counts it. A note edit or a
	// revocation is not a new link, so neither emits.
	if p.Bearing != nil {
		rec, _ := a.store.getRecord(uid, link.EvidenceID)
		emitEvidenceLinkEvent(uid, link, rec)
	}
	writeJSON(w, http.StatusOK, map[string]any{"link": link})
}

// handleDeleteLink removes the LINK ONLY. The evidence record survives and stays listable — an
// unlink is "this does not bear on my thesis", not "this never happened".
func (a *evidenceAPI) handleDeleteLink(w http.ResponseWriter, r *http.Request) {
	ok, err := a.store.deleteLink(userID(r), r.PathValue("id"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if !ok {
		writeEvidenceError(w, http.StatusNotFound, "not_found", "link not found", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- helpers ----

// checkThesisItemID validates the PREFIX of an optional item id.
//
// Deliberate limit: it does not yet verify that the item EXISTS on the thesis. The structured item
// lists (asm_/cat_/inv_/rsk_) are built by the thesis lane, which is not integrated into this
// branch's baseline, and validating against a schema that is not here would either fail every link
// or fake a check. Once the thesis lane lands, this is the one place to add the existence check.
func (a *evidenceAPI) checkThesisItemID(id *string) error {
	if id == nil || strings.TrimSpace(*id) == "" {
		return nil
	}
	for _, p := range thesisItemPrefixes {
		if strings.HasPrefix(*id, p) {
			return nil
		}
	}
	return errors.New("thesisItemId must be an assumption, catalyst, invalidation condition, or risk id (" +
		strings.Join(thesisItemPrefixes, ", ") + ")")
}

func trimTo(s string, n int) string {
	out, _ := truncateRunes(strings.TrimSpace(s), n)
	return out
}

func parseLimit(raw string, def, max int) int {
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}
