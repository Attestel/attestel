package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// handlers.go — the HTTP surface. Everything is SIMULATION: no order execution, no broker, no money
// movement, and the ledger below is simulated bookkeeping rather than an account.
//
// CORS is an EXACT ALLOW-LIST that is EMPTY BY DEFAULT (`CORS_ORIGINS`, mirroring the feedback
// service). It used to answer `Access-Control-Allow-Origin: *`, which is the worst of both: browsers
// forbid `*` alongside credentials, so the two authenticated POSTs were un-callable cross-origin
// while the header advertised that anything could call them. Empty means no CORS headers at all,
// which is correct for a service every shipped deployment reaches same-origin.

type API struct {
	cfg     Config
	store   *Store
	clients *Clients
	// The engine, for the two things only it knows: how it is sizing positions, and the book it
	// keeps. The API never drives the engine — it reads it.
	engine *Engine
	now    func() time.Time
}

func newAPI(cfg Config, store *Store, clients *Clients, engine *Engine) *API {
	return &API{cfg: cfg, store: store, clients: clients, engine: engine, now: time.Now}
}

func (a *API) currentTime() time.Time {
	if a.now == nil {
		return time.Now().UTC()
	}
	return a.now().UTC()
}

// ledger returns the engine's book, or nil when there is none.
func (a *API) ledger() *Ledger {
	if a.engine == nil {
		return nil
	}
	return a.engine.ledger
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (a *API) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", a.handleHealth)
	mux.HandleFunc("GET /paper/positions", a.handlePositions)
	mux.HandleFunc("GET /paper/status", a.handleStatus)
	mux.HandleFunc("GET /paper/comparison", a.handleComparison)
	mux.HandleFunc("GET /paper/ledger", a.handleLedger)
	mux.HandleFunc("GET /paper/readiness", a.handleReadiness)
	mux.HandleFunc("GET /paper/dashboard", a.handleDashboard)
	mux.HandleFunc("GET /paper/experiments", a.handleExperiments)
	mux.HandleFunc("GET /paper/shadow", a.handleShadow)
	// READS are public (the frontend fails silent if this is down). The two MUTATING routes are
	// not: /paper/reset deletes every paper trade in the journal and /paper/config rewrites what
	// the engine trades. `confirm=true` is a typo guard, not authentication.
	mux.HandleFunc("POST /paper/reset", a.requireSession(a.handleReset))
	mux.HandleFunc("GET /paper/config", a.handleGetConfig)
	mux.HandleFunc("POST /paper/config", a.requireSession(a.handleSetConfig))
	return a.withCORS(mux)
}

func (a *API) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := a.store.Check(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "unavailable", "service": "paper", "simulation": true,
			"storage": "postgresql",
		})
		return
	}
	storage := "files"
	if a.store.db != nil {
		storage = "postgresql"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "service": "paper", "simulation": true,
		"configs": len(a.store.Configs()), "sizing": a.sizing(),
		"ledger":          a.ledger() != nil,
		"shadowRecording": a.store.ShadowError() == nil,
		"book":            a.bookIdentity(),
		"storage":         storage,
	})
}

func (a *API) handleShadow(w http.ResponseWriter, r *http.Request) {
	report, err := a.store.ShadowReport(dashboardDecisionTail)
	if err != nil {
		report.Recording = false
		if report.Error == "" {
			report.Error = err.Error()
		}
	}
	writeJSON(w, http.StatusOK, report)
}

// bookIdentity says WHOSE simulated book this is, so the UI never has to guess — and never labels a
// shared engine book as the reader's own experiment (D-20).
//
// Today the answer is always "system": the engine runs one book against a dedicated service user
// (auth.go). When per-user engines land — D-20 option (b), the product-correct target — this is the
// field that changes, and every surface reading it follows without another copy-edit.
//
// `recording` reports whether the engine can actually write its trades. Without an AUTH_SECRET the
// journal refuses them, and a book that silently records nothing must say so rather than presenting
// an empty table as "no signals fired".
func (a *API) bookIdentity() map[string]any {
	journal := a.cfg.AuthSecret != ""
	out := map[string]any{
		"owner":     "system",
		"label":     systemBookLabel,
		"perUser":   false,
		"recording": journal,
		"note": "Simulated positions opened by the platform's validation engine — not your own " +
			"trades, and not a portfolio. Nothing here executes an order or touches a broker.",
		// The two records are separate and can be alive independently. The LEDGER is this service's
		// own book (contract §5) and needs no credential; the JOURNAL is the D-20 trade record and
		// needs AUTH_SECRET. A payload that reported only one of them would let a dead recorder read
		// as "no signals fired".
		"records": map[string]any{
			"journal": journal,
			"ledger":  a.ledger() != nil,
		},
	}
	switch {
	case !journal && a.ledger() != nil:
		out["recordingNote"] = "JOURNAL RECORDING IS DEAD (no AUTH_SECRET): the journal refuses every " +
			"write, so the engine records no trade — and because a position it cannot record is a " +
			"position it does not take, it opens nothing new. The LEDGER is running and needs no " +
			"credential: it marks the book to every real bar close and serves its equity at " +
			"GET /paper/ledger."
	case a.ledger() == nil:
		out["recordingNote"] = "THE LEDGER IS NOT RUNNING. Every position change is blocked; no fixed-size " +
			"substitute is permitted, and no equity, fee or daily mark is being recorded. " + a.sizing()
	}
	return out
}

// sizing is the engine's one-line description of how it sizes a position, or the honest answer when
// there is no engine to ask (tests that build an API alone).
func (a *API) sizing() string {
	if a.engine == nil {
		return "unknown: no engine is attached to this API"
	}
	return a.engine.sizing()
}

// handlePositions returns OPEN paper positions with live unrealized P&L (from the journal, which
// marks them to /quote), annotated with the bar each was decided on.
//
// There is deliberately NO `dueAt` / `barsRemaining` any more: a position has no scheduled exit. It
// is re-decided every bar and closes when the target changes (contract §1.1), so a countdown would
// be a number about a rule that does not exist.
func (a *API) handlePositions(w http.ResponseWriter, r *http.Request) {
	trades, err := a.clients.journalPaperTrades(r.Context(), "open")
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error(), "positions": []any{}})
		return
	}
	stateBy := map[string]ConfigState{}
	anySynthetic := false
	for _, p := range a.store.OpenPositions() {
		stateBy[p.TradeID] = p
		if p.EntrySynthetic {
			anySynthetic = true // legacy only: gate 1 rejects synthetic inputs for new opens
		}
	}
	out := make([]map[string]any, 0, len(trades))
	for _, t := range trades {
		item := map[string]any{
			"tradeId": t.ID, "ticker": t.Ticker, "side": t.Side, "timeframe": t.Entry.Timeframe,
			"entryPrice": t.Entry.Price, "size": t.Entry.Size,
			"markPrice": t.MarkPrice, "pnlAbs": t.PnlAbs, "pnlPct": t.PnlPct,
			"markIsSynthetic": t.MarkSynthetic,
		}
		if st, ok := stateBy[t.ID]; ok {
			item["horizon"] = st.Horizon // the model's LABEL horizon, not a holding period
			item["entryBar"] = st.EntryBar
			item["lastBarActedOn"] = st.LastBarActedOn
			item["exitRule"] = "re-decided every bar; closes when the target changes"
			item["modelVersion"] = st.EntryModelVersion
			item["strategyVersion"] = st.EntryStrategyVersion
		}
		if t.MarkSynthetic {
			anySynthetic = true
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"positions": out, "paper": true, "synthetic": anySynthetic, "book": a.bookIdentity(),
	})
}

// handleStatus reports, per config, WHAT THE ENGINE LAST DID AND WHY: the bar it decided on, the
// target it derived, the action it took, and — when it took none — which gate refused and why.
//
// This exists because a fail-closed engine can correctly trade NOTHING. That behaviour must be
// VISIBLE rather than silent: an empty book with no explanation is indistinguishable from a broken
// service.
func (a *API) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.statusPayload(r.Context(), a.currentTime()))
}

func (a *API) statusPayload(ctx context.Context, asOf time.Time) map[string]any {
	byKey := map[string]ConfigState{}
	for _, st := range a.store.States() {
		byKey[st.ConfigKey] = st
	}
	// THE THREE-STORE COMPARISON, computed FRESH (reconcile.go). The engine's own log line used to
	// claim "/paper/status says so" whenever a ledger booking failed; nothing here compared the
	// stores, so it did not. This is that surface: one `sync` block per config, saying whether the
	// engine, the ledger and the journal agree, how many fills the book is still owed, and — when
	// they disagree — exactly how.
	sync := map[string]syncState{}
	if a.engine != nil {
		sync = a.engine.SyncReport(ctx)
	}
	out := make([]map[string]any, 0)
	totalPending := 0
	desynced := make([]string, 0)
	for _, cfg := range a.store.Configs() {
		st := byKey[cfg.Key()]
		sy, haveSync := sync[cfg.Key()]
		item := map[string]any{
			"config": cfg.Key(), "ticker": cfg.Ticker, "timeframe": cfg.Timeframe,
			"horizon": cfg.Horizon, "position": sideOf(st.Side),
			"lastBarActedOn": st.LastBarActedOn, "lastDecision": st.LastDecision,
		}
		if haveSync {
			item["sync"] = sy
			totalPending += sy.PendingBookings
			if !sy.Consistent {
				desynced = append(desynced, cfg.Key())
			}
		} else {
			item["sync"] = syncState{Detail: "no engine is attached to this API, so nothing was compared"}
		}
		if st.Side != "" {
			item["tradeId"] = st.TradeID
			item["entryPrice"] = st.EntryPrice
			item["entryBar"] = st.EntryBar
			item["modelVersion"] = st.EntryModelVersion
			item["strategyVersion"] = st.EntryStrategyVersion
		}
		out = append(out, item)
	}
	payload := map[string]any{
		"configs": out, "paper": true, "book": a.bookIdentity(),
		"asOf":   asOf.UTC().Format(time.RFC3339),
		"sizing": a.sizing(),
		// The roll-up of the per-config `sync` blocks, so an operator does not have to scan them.
		"reconciliation": map[string]any{
			"desyncedConfigs": desynced,
			"pendingBookings": totalPending,
			"stores":          a.persistenceStores(),
			"note": "A DESYNCED CONFIG REFUSES TO CHANGE ITS POSITION and says which mismatch stopped " +
				"it; it keeps marking and snapshotting. Fills the ledger refused are retried at the " +
				"start of every tick and are idempotent by (trade id, kind).",
		},
		"contract": "docs/PAPER_EXECUTION_CONTRACT.md v1.1.0 — per-bar allocation rule on COMPLETED " +
			"bars only, fail-closed behind gates: no-synthetic-data, fresh-data, backtest-passed, " +
			"evaluator-verdict; fills require a non-synthetic quote whose asOf is not older than the bar",
	}
	// The one-line equity summary (contract §5). Present whenever the book is running — including
	// when it has never traded, which is the state today and is not the same thing as absent.
	if l := a.ledger(); l != nil {
		payload["ledger"] = l.Summary()
	} else {
		payload["ledger"] = map[string]any{
			"available": false,
			"note": "The fake-money ledger is not running, so there is no equity, no fee accounting and " +
				"no daily mark. " + a.sizing(),
		}
	}
	return payload
}

func (a *API) persistenceStores() []string {
	if a.store.db != nil {
		return []string{
			"engine (PostgreSQL paper.documents)",
			"ledger (PostgreSQL paper.fills/paper.snapshots)",
			"journal (PostgreSQL journal.trades, mode=paper)",
		}
	}
	return []string{"engine (state.json)", "ledger (fills.jsonl)", "journal (mode=paper trades)"}
}

// handleLedger serves the simulated book: cash, equity, open lots with their entry fees, the daily
// snapshot series and the statistics computed from it, the dates it could not mark, and a tail of
// the append-only fills journal.
//
// SIMULATED BOOKKEEPING, NOT AN ACCOUNT. Nothing here is withdrawable, nothing reached a broker, and
// no order was placed. It exists so a live number and a backtest number can disagree meaningfully.
func (a *API) handleLedger(w http.ResponseWriter, r *http.Request) {
	l := a.ledger()
	if l == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error":     "the fake-money ledger is not running",
			"available": false,
			"sizing":    a.sizing(),
			"note": "Position changes are blocked until the evaluator-comparable ledger is available; " +
				"there is no fixed-notional substitute.",
		})
		return
	}
	tail := 50
	if v := strings.TrimSpace(r.URL.Query().Get("fills")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 1000 {
			tail = n
		}
	}
	payload := l.Payload(tail)
	payload["book"] = a.bookIdentity()
	payload["sizing"] = a.sizing()
	payload["nConfigs"] = len(a.store.Configs())
	payload["asOf"] = time.Now().UTC().Format(time.RFC3339)
	writeJSON(w, http.StatusOK, payload)
}

// handleComparison builds the per-config live-paper vs. backtest picture.
func (a *API) handleComparison(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.comparisonPayload(r.Context(), a.currentTime()))
}

func (a *API) comparisonPayload(ctx context.Context, asOf time.Time) map[string]any {
	closed, err := a.clients.journalPaperTrades(ctx, "closed")
	if err != nil {
		closed = nil // still return backtest-side info; live just shows 0 closed
	}
	// Group closed paper trades by the FULL config key — ticker:timeframe:horizon.
	//
	// It used to group by ticker+timeframe, on the comment "one config per pair". That is false the
	// moment two horizons of one ticker are configured (NVDA:1D:5 and NVDA:1D:10 are two different
	// models), and it silently credited both configs with the union of their trades. The horizon now
	// travels on each trade in `attachedSignal` (contract §3.2) and is read back here.
	//
	// A LEGACY trade — written before that block existed — carries no horizon. It is attributed only
	// when exactly one configured (ticker, timeframe) could have produced it; otherwise it is
	// EXCLUDED and counted, because attributing it to a guess is worse than leaving it out of a
	// validation number.
	configsPerPair := map[string]int{}
	soleConfig := map[string]string{}
	for _, cfg := range a.store.Configs() {
		pair := cfg.Ticker + ":" + cfg.Timeframe
		configsPerPair[pair]++
		soleConfig[pair] = cfg.Key()
	}
	byConfig := map[string][]journalTrade{}
	unattributed := 0
	for _, t := range closed {
		pair := t.Ticker + ":" + t.Entry.Timeframe
		switch {
		case t.AttachedSignal != nil && t.AttachedSignal.Horizon != nil:
			byConfig[pair+":"+strconv.Itoa(*t.AttachedSignal.Horizon)] = append(
				byConfig[pair+":"+strconv.Itoa(*t.AttachedSignal.Horizon)], t)
		case configsPerPair[pair] == 1:
			byConfig[soleConfig[pair]] = append(byConfig[soleConfig[pair]], t)
		default:
			unattributed++
		}
	}
	anySynthetic := false
	// ONE book, read once: every row's `portfolio.live` is the same simulated book (contract §5.2),
	// paired with that row's own reference.
	live := livePortfolioFrom(a.ledger())
	comparisons := make([]comparison, 0)
	for _, cfg := range a.store.Configs() {
		pred, perr := a.clients.predict(ctx, cfg)
		if perr != nil {
			pred = nil
		}
		st := a.store.StateFor(cfg)
		nOpen := 0
		if st.Side != "" {
			nOpen = 1
		}
		cmp := buildComparison(cfg, byConfig[cfg.Key()], pred, nOpen, st.LastDecision, live)
		if cmp.TrainedOnSynthetic {
			anySynthetic = true
		}
		comparisons = append(comparisons, cmp)
	}
	return map[string]any{
		"comparisons": comparisons, "paper": true, "synthetic": anySynthetic,
		"minMeaningful": minMeaningful, "asOf": asOf.UTC().Format(time.RFC3339),
		// Closed paper trades that could not be attributed to exactly one config (a legacy trade
		// with no horizon, on a ticker+timeframe more than one config trades). Reported rather than
		// distributed by guesswork — a validation number built on a guess is not a validation.
		"unattributed": unattributed,
		"book":         a.bookIdentity(),
		// Which number is in which unit, as DATA rather than as prose nobody reads (contract §5.4).
		// The `portfolio` block on each row is the like-for-like half; the per-trade columns are
		// counting stats and still say so.
		"units": comparisonUnits,
		"portfolioNote": "`portfolio.live` is ONE simulated book shared by every config — equal-weight " +
			"1/N, fees charged at the cost each model was validated under, marked to real bar closes " +
			"(docs/PAPER_EXECUTION_CONTRACT.md §5). Its daily Sharpe is the same statistic as the " +
			"evaluator's portfolio Sharpe.",
	}
}

// handleReset clears ALL paper trades + engine state. Confirm-guarded so it can't fire by accident.
func (a *API) handleReset(w http.ResponseWriter, r *http.Request) {
	confirm := r.URL.Query().Get("confirm") == "true"
	if !confirm {
		var body struct {
			Confirm bool `json:"confirm"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		confirm = body.Confirm
	}
	if !confirm {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "reset requires confirm=true"})
		return
	}
	if a.engine == nil || a.ledger() == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error":            "reset refused: the simulated ledger is unavailable, so all stores cannot be reset together",
			"engineStateReset": false, "ledgerReset": false,
		})
		return
	}
	a.engine.opMu.Lock()
	defer a.engine.opMu.Unlock()
	now := a.currentTime()
	readiness := a.readinessLocked(r.Context(), now)
	if !readiness.Ready {
		writeJSON(w, http.StatusConflict, map[string]any{
			"ok": false, "error": "official experiment start refused: launch readiness checks failed",
			"readiness": readiness, "engineStateReset": false, "ledgerReset": false,
		})
		return
	}
	trades, err := a.clients.journalPaperTrades(r.Context(), "all")
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	// THE JOURNAL GOES FIRST, AND A SINGLE FAILURE ABORTS THE WHOLE RESET.
	//
	// It used to count successes, ignore failures, and wipe the engine state and the book anyway.
	// That is the worst possible ordering: a journal that refused one delete keeps a live paper
	// trade with no engine state and no lot behind it — an orphan the engine may later adopt into a
	// freshly-reset book, whose entry belongs to an experiment that no longer exists. Reset is the
	// OFFICIAL EXPERIMENT START (docs/VALIDATION_AND_GO_LIVE.md §2.8); a start that half-happened
	// is worse than one that did not happen, because only one of the two is obvious afterwards.
	deleted := make([]string, 0, len(trades))
	failures := make([]map[string]any, 0)
	for _, t := range trades {
		if derr := a.clients.journalDelete(r.Context(), t.ID); derr != nil {
			failures = append(failures, map[string]any{"tradeId": t.ID, "error": derr.Error()})
			continue
		}
		deleted = append(deleted, t.ID)
	}
	if len(failures) > 0 {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": fmt.Sprintf("%d of %d paper trades could not be deleted — engine and ledger reset aborted",
				len(failures), len(trades)),
			"deletedPaperTrades":   deleted,
			"failedPaperTrades":    failures,
			"engineStateReset":     false,
			"ledgerReset":          false,
			"reconciliationIntact": false,
			"note": "The engine's state and fake-money book are UNTOUCHED, but the listed journal trades " +
				"were already deleted, so the service is intentionally fail-closed until cleanup completes. A reset that deleted " +
				"some journal trades and then wiped the engine and the ledger would leave live paper " +
				"trades no store has a record of. Fix the journal and run the reset again — it is " +
				"idempotent over the trades it already deleted.",
		})
		return
	}
	// The book is reset too — a book whose positions were just deleted is not a book. The
	// append-only files are ARCHIVED with a timestamp rather than removed: a reset is an operator
	// action on a validation book, and the record of what it did before the reset is exactly the
	// thing somebody will want afterwards.
	if err := a.ledger().Reset(now); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":              "ledger reset failed after journal cleanup: " + err.Error(),
			"deletedPaperTrades": len(deleted), "deletedTradeIds": deleted,
			"engineStateReset": false, "ledgerReset": false, "ok": false,
			"note": "The official experiment start was NOT established. Engine state was kept so the " +
				"failure remains visible; repair the ledger and run the confirmed reset again.",
		})
		return
	}
	if err := a.store.Reset(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":              "engine-state reset failed after journal and ledger cleanup: " + err.Error(),
			"deletedPaperTrades": len(deleted), "deletedTradeIds": deleted,
			"engineStateReset": false, "ledgerReset": true, "ok": false,
			"note": "The official experiment start was NOT established. Position changes remain " +
				"fail-closed until engine state is repaired and the reset is run again.",
		})
		return
	}
	if err := a.ledger().StartOfficial(now, a.store.Configs()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error":              "official clock could not be persisted after the stores were reset: " + err.Error(),
			"deletedPaperTrades": len(deleted), "deletedTradeIds": deleted,
			"engineStateReset": true, "ledgerReset": true, "ok": false,
			"note": "The official experiment start was NOT established. Repair the ledger and run the confirmed reset again.",
		})
		return
	}
	out := map[string]any{
		"ok": true, "deletedPaperTrades": len(deleted), "deletedTradeIds": deleted,
		"engineStateReset": true, "ledgerReset": true,
		"officialStartedAt": now.Format(time.RFC3339),
		"ledgerNote":        "the previous ledger generation is archived and remains queryable; reset never deletes its fills or snapshots",
		"note": "This is the OFFICIAL EXPERIMENT START (docs/VALIDATION_AND_GO_LIVE.md §2.8). All " +
			"three stores are now empty and agree; the clock starts from here.",
	}
	writeJSON(w, http.StatusOK, out)
}

// removalsHoldingPositions lists the configs that `next` would drop while they still hold something
// — in the ENGINE's state, in the LEDGER's lots, or with a fill still owed to the book.
func (a *API) removalsHoldingPositions(next []PaperCfg) []map[string]any {
	keep := map[string]bool{}
	for _, c := range next {
		keep[c.Key()] = true
	}
	stateBy := map[string]ConfigState{}
	for _, st := range a.store.States() {
		stateBy[st.ConfigKey] = st
	}
	out := make([]map[string]any, 0)
	for _, c := range a.store.Configs() {
		key := c.Key()
		if keep[key] {
			continue
		}
		item := map[string]any{"config": key}
		holds := false
		if st, ok := stateBy[key]; ok && st.Side != "" {
			holds = true
			item["enginePosition"] = st.Side
			item["tradeId"] = st.TradeID
		}
		if l := a.ledger(); l != nil {
			if lot := l.LotFor(key); lot != nil {
				holds = true
				item["ledgerLot"] = map[string]any{
					"side": lot.Side, "qty": round6(lot.Qty), "tradeId": lot.TradeID,
					"entryPrice": lot.EntryPrice,
				}
			}
		}
		if n := len(a.store.PendingBookingsFor(key)); n > 0 {
			holds = true
			item["pendingBookings"] = n
		}
		if holds {
			out = append(out, item)
		}
	}
	return out
}

func blockerKeys(blockers []map[string]any) []string {
	out := make([]string, 0, len(blockers))
	for _, b := range blockers {
		if k, ok := b["config"].(string); ok {
			out = append(out, k)
		}
	}
	return out
}

func sameConfigs(a, b []PaperCfg) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Key() != b[i].Key() {
			return false
		}
	}
	return true
}

func (a *API) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"configs": a.store.Configs()})
}

// handleSetConfig replaces the auto-papered config list. Accepts either a "NVDA:1D:5,..." string or
// an array of {ticker, timeframe, horizon}.
func (a *API) handleSetConfig(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Configs json.RawMessage `json:"configs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid JSON"})
		return
	}
	var cfgs []PaperCfg
	var asString string
	if json.Unmarshal(body.Configs, &asString) == nil && strings.TrimSpace(asString) != "" {
		cfgs = parseConfigs(asString)
	} else {
		var arr []PaperCfg
		if json.Unmarshal(body.Configs, &arr) == nil {
			for _, c := range arr {
				c.Ticker = strings.ToUpper(strings.TrimSpace(c.Ticker))
				c.Timeframe = normalizeTimeframe(c.Timeframe)
				if c.Ticker != "" && c.Horizon > 0 {
					cfgs = append(cfgs, c)
				}
			}
		}
	}
	if len(cfgs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "no valid configs (use \"TICKER:TF:HORIZON,...\" or [{ticker,timeframe,horizon}])"})
		return
	}
	if a.engine == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "config update refused: no engine is attached"})
		return
	}
	a.engine.opMu.Lock()
	defer a.engine.opMu.Unlock()
	// A CONFIG THAT HOLDS SOMETHING MAY NOT BE REMOVED. Dropping it from the list does not close its
	// position: the journal trade stays open, the ledger keeps marking the lot, and nothing will ever
	// reconcile either — the engine only decides on configs that are enabled. It also silently
	// changes N, so every other position's 1/N slice is now measured against a different portfolio.
	// The operator closes it first, or waits for the signal to close it.
	if blockers := a.removalsHoldingPositions(cfgs); len(blockers) > 0 {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "refusing to remove " + strconv.Itoa(len(blockers)) +
				" config(s) that still hold a position: " + strings.Join(blockerKeys(blockers), ", "),
			"holdingPositions": blockers,
			"configs":          a.store.Configs(),
			"note": "Removing a config does not close what it holds — the journal trade stays open and " +
				"the ledger keeps marking the lot, with nothing left to reconcile either, and N changes " +
				"under every other position's 1/N slice. Close the position (or wait for the signal to " +
				"close it) and re-send this request.",
		})
		return
	}
	changed := !sameConfigs(a.store.Configs(), cfgs)
	clockInvalidated := false
	if changed && a.ledger() != nil {
		var err error
		clockInvalidated, err = a.ledger().InvalidateOfficial()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"error":   "config update refused because the official clock could not be invalidated durably: " + err.Error(),
				"configs": a.store.Configs(),
			})
			return
		}
	}
	if err := a.store.SetConfigs(cfgs); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": "config update was not durable: " + err.Error(), "configs": a.store.Configs(),
			"officialClockInvalidated": clockInvalidated,
		})
		return
	}
	note := "The configured portfolio is unchanged."
	if clockInvalidated {
		note = "The configured portfolio changed, so the previous official clock was stopped. " +
			"Re-check readiness and reset to establish a new day 0."
	} else if changed {
		note = "The configured portfolio was saved. No official clock was active."
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"configs": a.store.Configs(), "officialClockInvalidated": clockInvalidated, "note": note,
	})
}

// withCORS answers an EXACT ALLOW-LIST of origins with credentials, and answers NOTHING when
// `CORS_ORIGINS` is empty — which is the default.
//
// It replaces `Access-Control-Allow-Origin: *`, which was the worst of both worlds: browsers refuse
// `*` alongside `Allow-Credentials`, so the two authenticated POSTs (`/paper/reset`, `/paper/config`)
// could not actually be called cross-origin, while the wildcard advertised that anything could. This
// is byte-for-byte the pattern `feedback/auth.go` already uses.
func (a *API) withCORS(next http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, o := range a.cfg.CORSOrigins {
		allowed[o] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && allowed[origin] {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
