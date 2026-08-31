package main

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// changed.go — `GET /api/changed`, contract §8 and §9.1. Wave 5 Lane 5B.
//
// THIS ROUTE'S ABSENCE WAS RENDERED THREE TIMES BEFORE IT EXISTED. All three Wave 4 frontend lanes
// hit it and each said so honestly on screen: `WhatChangedPanel`'s "Change tracking is not
// available yet … **It is not a statement that nothing changed.**" Those states disappear now, and
// the panel's defensive two-vocabulary reads (`researchLink.ticker || ticker || subject`,
// `bearing || direction`) can finally be checked against a real payload instead of guessed at.
// This file is written so the FIRST spelling in each of those pairs is the one that arrives.
//
// WHAT IT REPORTS: CHANGE, NOT VOLUME (doc §16.5)
// -----------------------------------------------
//
//	87 source documents processed        ← what it READ.       Metadata. Small, mono, dim.
//	 4 material events detected          ← what it CONCLUDED.  Heading scale.
//	 2 companies changed meaningfully    ← what it CONCLUDED.  Heading scale.
//
// "87 new articles" creates work for the user; "4 material events" says the work is already done.
// The two conclusions are computed here, from the same event corpus `/api/following` reads, and
// `materiality` is SERVED alongside them so the client never keeps a second copy of the threshold.
// That is the §AD-8 lesson from Wave 4 integration: two lanes hand-copied `importanceHighMin` into
// JavaScript and one copy had already drifted.
//
// TWO HALVES, AND THEY ANSWER DIFFERENT QUESTIONS
// ------------------------------------------------
//   - The three COUNTS come from the event corpus — what happened in the world to the companies
//     this user follows.
//   - The ITEMS come from the same deterministic collectors `changes.go` already owns, run across
//     the user's whole portfolio instead of one thesis: market context per followed ticker, their
//     triggered alert events, their own thesis edits, transcript comparisons. §9.1 is explicit that
//     this route returns `changes.go`'s `ChangeItem` — no eighth kind, no parallel struct — and
//     that is satisfied by REUSING the collectors, not by inventing items that resemble them.
//
// Nothing here asks a model for anything. `summary` is composed in code from typed fields, exactly
// as `changes.go` requires, and the scheduler never comes near this route (§4.6, invariant #4).
//
// THE EMPTY ANSWER IS A FEATURE. Zero material change is rendered calmly and truthfully. It is also
// why `materialMovePct` exists in `changes.go`: without a stated threshold, `market_context` would
// emit on every call and "nothing changed" could never be true.

func init() {
	// Attached through the Wave 0 event seam, like `feeds.go` and `context.go`. Do NOT also add a
	// line to main.go — that would register the pattern twice and panic the mux at startup, which
	// is the exact hazard §9.1 exists to prevent for this specific route.
	registerEventRoute(func(s *Server, mux *http.ServeMux) {
		s.registerChangedRoutes(mux)
	})
}

func (s *Server) registerChangedRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/changed", s.handleChanged)
}

const (
	// changedWindowSeconds is the DEFAULT window when the caller sends no `since`: 24 hours.
	//
	// The copy rule that travels with it (Wave 4 Lane 4A's locked decision 6) is enforced on the
	// client and stated here so it cannot be lost: a default 24-hour window may NEVER be labelled
	// "since your last check". `since.basis` in the response is how the client tells them apart
	// without re-deriving the rule.
	changedWindowSeconds = 24 * 60 * 60

	// changedTimeout bounds the whole fan-out. Shorter than `changesTimeout` (25 s) because this
	// route is on a landing surface: a Today page that hangs for 25 s has already failed, and every
	// source here degrades rather than erroring.
	changedTimeout = 12 * time.Second

	// changedMaxTickers bounds the per-ticker fan-out. A user following 200 companies would
	// otherwise open 400 upstream requests on one page load. Truncation is REPORTED
	// (`truncated`), never silent — a quietly shortened list reads as "nothing else changed".
	changedMaxTickers = 25

	// changedFanOut caps how many of those requests are in flight AT ONCE. The two bounds do
	// different jobs: `changedMaxTickers` limits the total work, this limits the burst. Without it,
	// 25 tickers means 50 simultaneous requests to the analysis service per visitor.
	changedFanOut = 6
)

// changedSince is the boundary and WHERE IT CAME FROM. `basis` is the field the copy rule turns on.
type changedSince struct {
	At    int64  `json:"at"`
	Basis string `json:"basis"` // "requested" | "default24h"
}

// changedResponse is §8's object plus the provenance the frontend needs to render it honestly.
//
// Field names are chosen to match what `WhatChangedPanel` already reads FIRST: `items[].bearing`,
// `items[].researchLink.ticker`, `items[].summary`, and the three counts. The panel's fallbacks
// (`ticker`, `subject`, `direction`) exist because this route did not; they are now dead spellings
// and the handoff says so rather than leaving a reader to assume the guess was right.
type changedResponse struct {
	DocumentsProcessed int          `json:"documentsProcessed"`
	MaterialEvents     int          `json:"materialEvents"`
	CompaniesChanged   int          `json:"companiesChanged"`
	Items              []ChangeItem `json:"items"`

	Since     changedSince   `json:"since"`
	Companies []string       `json:"companies"`
	Counts    map[string]int `json:"counts"`
	Empty     bool           `json:"empty"`
	Truncated bool           `json:"truncated"`
	AsOf      string         `json:"asOf"`
	Degraded  []string       `json:"degraded"`

	// Materiality is the SERVED definition of "material", so the word in the payload has a stated
	// meaning and the client never keeps its own copy of the threshold (§AD-8).
	Materiality changedMateriality `json:"materiality"`
}

type changedMateriality struct {
	ImportanceBand string  `json:"importanceBand"` // the band that counts as material
	ImportanceMin  float64 `json:"importanceMin"`  // ...and the number behind it
	MovePct        float64 `json:"movePct"`        // the price move that makes a company "changed"
	Note           string  `json:"note"`
}

// handleChanged answers "what changed across my companies since <boundary>".
//
// GUESTS ARE SERVED. `resolveFollowSet` already yields the seed ticker for a signed-out visitor,
// and the panel renders for them. A 401 here would turn the product's opening screen into a login
// wall for a page that has something honest to say without one.
func (s *Server) handleChanged(w http.ResponseWriter, r *http.Request) {
	id := s.identityFrom(r)

	since := changedSince{At: time.Now().UTC().Unix() - changedWindowSeconds, Basis: "default24h"}
	if raw := strings.TrimSpace(r.URL.Query().Get("since")); raw != "" {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil && n > 0 {
			since = changedSince{At: n, Basis: "requested"}
		}
		// An unparseable `since` falls back to the window rather than 400ing. The boundary is a
		// convenience, and a stale bookmark must not break the landing surface — but the basis then
		// says `default24h`, so the client's heading stays truthful.
	}

	ctx, cancel := context.WithTimeout(r.Context(), changedTimeout)
	defer cancel()

	writeJSON(w, http.StatusOK, s.changedFor(ctx, id, since))
}

// changedFor is the whole computation, separated from the handler so it is testable without a
// server and so the two halves below stay visible as two halves.
func (s *Server) changedFor(ctx context.Context, id identity, since changedSince) changedResponse {
	now := time.Now().UTC()
	follow := s.resolveFollowSet(ctx, id)
	degraded := append([]string(nil), follow.degraded...)

	tickers := append([]string(nil), follow.tickers...)
	sort.Strings(tickers)
	truncated := false
	if len(tickers) > changedMaxTickers {
		tickers = tickers[:changedMaxTickers]
		truncated = true
	}

	// ── half one: the counts, from the event corpus ──────────────────────────────────────────
	feed := s.buildFollowing(ctx, id, followingQuery{since: since.At, limit: followingMaxLimit})
	degraded = append(degraded, feed.Degraded...)
	documents, material, materialCompanies := summariseChange(feed.Items)
	if feed.Truncated {
		// THE COUNTS ARE COMPUTED FROM A PAGE, so a truncated feed under-counts all three of them.
		// Saying so is not a nicety: "87 source documents processed" that is really 300 understates
		// the work, and an under-counted `materialEvents` understates the change — which is the one
		// direction this surface must never err in. `truncated` is what lets the client say "at
		// least".
		truncated = true
	}

	// ── half two: the items, from changes.go's own collectors ────────────────────────────────
	items, itemDegraded := s.portfolioChangeItems(ctx, id, tickers, since.At)
	degraded = append(degraded, itemDegraded...)
	if items == nil {
		// `[]`, never `null`. A client that has to distinguish "no items" from "the field was
		// absent" will eventually get it wrong, and the state it gets wrong is the empty one this
		// whole surface exists to render honestly.
		items = []ChangeItem{}
	}

	// A company counts as CHANGED if a material event named it or its price moved materially.
	// Both are stated thresholds; neither is a model's opinion.
	changedCompanies := map[string]bool{}
	for _, sym := range materialCompanies {
		changedCompanies[strings.ToUpper(sym)] = true
	}
	for _, item := range items {
		if item.Kind == changeMarketContext && item.ResearchLink != nil {
			changedCompanies[strings.ToUpper(item.ResearchLink.Ticker)] = true
		}
	}

	sort.SliceStable(items, func(i, j int) bool {
		if items[i].At != items[j].At {
			return items[i].At > items[j].At
		}
		return items[i].ID < items[j].ID
	})
	counts := map[string]int{}
	for _, item := range items {
		counts[item.Kind]++
	}
	if len(items) > maxChangeItems {
		items = items[:maxChangeItems]
		truncated = true
	}

	companies := make([]string, 0, len(changedCompanies))
	for sym := range changedCompanies {
		companies = append(companies, sym)
	}
	sort.Strings(companies)

	return changedResponse{
		DocumentsProcessed: documents,
		MaterialEvents:     material,
		CompaniesChanged:   len(companies),
		Items:              items,
		Since:              since,
		Companies:          companies,
		Counts:             counts,
		// `empty` is about CHANGE, not about the item list. A user with no material events and no
		// recorded changes gets the calm "nothing material changed" state; one with items but no
		// material events does not, because something did change.
		Empty:     material == 0 && len(companies) == 0 && len(items) == 0,
		Truncated: truncated,
		AsOf:      now.Format(time.RFC3339),
		Degraded:  dedupeStrings(degraded),
		Materiality: changedMateriality{
			ImportanceBand: "high",
			ImportanceMin:  importanceHighMin,
			MovePct:        materialMovePct,
			Note: "A `material event` is one whose served importanceBand is high. A company " +
				"`changed meaningfully` when a material event named it or its close moved by at " +
				"least movePct since the boundary. Both thresholds are served so no client keeps " +
				"a second copy of them.",
		},
	}
}

// summariseChange turns the feed into the three headline numbers.
//
// `documentsProcessed` counts SOURCE DOCUMENTS, not items: one event can cluster a dozen articles,
// and the whole point of the inversion is that the big numbers are conclusions while this one is
// the reading behind them. An item with no `documentCount` (the degraded news/seed paths, where
// there is no cluster) counts as the one document it is — never as zero, which would understate the
// work, and never as a guess.
func summariseChange(items []feedItem) (documents int, material int, companies []string) {
	seen := map[string]bool{}
	for _, item := range items {
		if item.DocumentCount != nil && *item.DocumentCount > 0 {
			documents += *item.DocumentCount
		} else {
			documents++
		}
		if item.ImportanceBand != "high" {
			continue
		}
		material++
		for _, t := range item.Tickers {
			if !t.Followed {
				continue
			}
			sym := strings.ToUpper(t.Ticker)
			if sym != "" && !seen[sym] {
				seen[sym] = true
				companies = append(companies, sym)
			}
		}
	}
	sort.Strings(companies)
	return documents, material, companies
}

// portfolioChangeItems runs `changes.go`'s collectors across the whole portfolio.
//
// Per ticker: market context and transcript comparisons — both deterministic, both already written.
// Once for the user: their triggered alert events and their own thesis edits, neither of which is
// per-ticker.
//
// A source that cannot be read names itself in `degraded` and the rest of the answer still arrives.
// A partial honest list beats an error page on the surface a user opens first.
func (s *Server) portfolioChangeItems(ctx context.Context, id identity, tickers []string,
	since int64) ([]ChangeItem, []string) {

	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		items    []ChangeItem
		degraded []string
	)
	collect := func(got []ChangeItem, ok bool, label string) {
		mu.Lock()
		defer mu.Unlock()
		if !ok {
			degraded = append(degraded, label)
			return
		}
		items = append(items, got...)
	}

	// BOUNDED CONCURRENCY, and it is not a micro-optimisation. Unbounded, 25 followed tickers is 50
	// simultaneous upstream requests on one page load — the analysis service would see a burst per
	// visitor, which is the same unbudgeted fan-out §9.24 forbids the thesis monitor. The gate is a
	// counting semaphore rather than a worker pool because the work is I/O-bound and heterogeneous.
	gate := make(chan struct{}, changedFanOut)
	run := func(fn func()) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gate <- struct{}{}
			defer func() { <-gate }()
			fn()
		}()
	}

	for _, ticker := range tickers {
		ticker := ticker
		run(func() {
			// thesisID is "" — this is the PORTFOLIO feed, not a thesis review. The link is
			// rewritten below to point at the company rather than at a thesis that does not exist.
			got, ok := s.changesFromMarketContext(ctx, ticker, "", since)
			collect(retargetToCompany(got, ticker), ok, "market_context")
		})
		run(func() {
			got, ok := s.changesFromTranscripts(ctx, ticker, "", since)
			collect(retargetToCompany(got, ticker), ok, "transcripts")
		})
	}

	if id.userID != "" {
		run(func() {
			got, ok := s.portfolioAlertEvents(ctx, id.cookie, since)
			collect(got, ok, "alert_events")
		})
		run(func() {
			got, ok := s.portfolioThesisVersions(ctx, id.cookie, since)
			collect(got, ok, "thesis_versions")
		})
	}

	wg.Wait()
	return items, dedupeStrings(degraded)
}

// retargetToCompany points an item's `researchLink` at the company overview instead of at a thesis.
//
// `changeLinkFor(ticker, "")` would produce `subview: "thesis"` with an empty `thesisId` — a link
// to a thesis that does not exist. On this feed the subject is the COMPANY, and `researchLink.ticker`
// is exactly the field `WhatChangedPanel` reads first to label the row.
func retargetToCompany(items []ChangeItem, ticker string) []ChangeItem {
	out := make([]ChangeItem, 0, len(items))
	for _, item := range items {
		item.ResearchLink = &changeLink{
			View: "research", Subview: "overview", Tab: nil,
			Ticker: strings.ToUpper(ticker), ThesisID: "", Hash: "#research/overview",
		}
		out = append(out, item)
	}
	return out
}

// portfolioAlertEvents is `changesFromAlertEvents` without the thesis predicate: every alert event
// this user's rules produced since the boundary, whatever it was attached to.
func (s *Server) portfolioAlertEvents(ctx context.Context, cookie string, since int64) ([]ChangeItem, bool) {
	res, err := s.journalGet(ctx, s.cfg.AlertsURL+"/events?limit=500&since="+
		strconv.FormatInt(since, 10), cookie)
	if err != nil {
		return nil, false
	}
	raw, _ := res["events"].([]any)
	out := []ChangeItem{}
	seen := map[string]bool{}
	for _, e := range raw {
		ev, ok := e.(map[string]any)
		if !ok {
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
		ticker := strings.ToUpper(asString(ev["ticker"]))
		out = append(out, ChangeItem{
			ID: changeID(changeAlertEvent, ref, at), Kind: changeAlertEvent, At: at,
			Summary:      asString(ev["message"]),
			SourceRef:    ref,
			ThesisItemID: asString(ev["thesisItemId"]),
			// Carried through from the event, never computed here and never asked of a model.
			Bearing:      optString(asString(ev["bearing"])),
			DataState:    dataStateOr(asString(ev["dataState"])),
			ResearchLink: companyOrThesisLink(ticker, asString(ev["thesisId"])),
		})
	}
	return out, true
}

// portfolioThesisVersions collects edits across ALL of the user's theses since the boundary. It
// reuses `changesFromVersions` per thesis rather than reimplementing the diff summary.
func (s *Server) portfolioThesisVersions(ctx context.Context, cookie string, since int64) ([]ChangeItem, bool) {
	res, err := s.journalGet(ctx, s.cfg.JournalURL+"/theses?limit=200", cookie)
	if err != nil {
		return nil, false
	}
	raw, _ := res["theses"].([]any)
	out := []ChangeItem{}
	for _, t := range raw {
		thesis, ok := t.(map[string]any)
		if !ok {
			continue
		}
		id := asString(thesis["id"])
		ticker := strings.ToUpper(asString(thesis["ticker"]))
		if id == "" {
			continue
		}
		out = append(out, changesFromVersions(thesis, ticker, id, since)...)
	}
	return out, true
}

// companyOrThesisLink keeps a thesis link when the source names a thesis, and points at the company
// otherwise. Both spellings put a ticker in `researchLink.ticker`, which is the field the row label
// reads — so a row is never labelled "MACRO" merely because its item happened to lack a thesis.
func companyOrThesisLink(ticker, thesisID string) *changeLink {
	if thesisID != "" {
		return changeLinkFor(ticker, thesisID)
	}
	if ticker == "" {
		// A genuinely portfolio-wide item (a macro alert with no company) has no company page to
		// open. `nil` is honest; the client renders the row without a destination and labels the
		// subject MACRO, which is a legitimate subject on this feed (doc §15.8).
		return nil
	}
	return &changeLink{
		View: "research", Subview: "overview", Tab: nil,
		Ticker: ticker, ThesisID: "", Hash: "#research/overview",
	}
}
