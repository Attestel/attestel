package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

// reconcile.go — THE THREE STORES, AND WHAT HAPPENS WHEN THEY DISAGREE.
//
// One simulated position is recorded in three places, for three different reasons:
//
//   - the JOURNAL (`mode="paper"` trades) is the D-20 trade record — what was decided, and when;
//   - the LEDGER (`ledger.go`, contract §5) is the fake-money book — the fill, its fee, its lot;
//   - the ENGINE's own state (`store.go`) is the bar cursor and the side it believes it holds.
//
// They can come apart. `openPosition` writes the journal trade FIRST and books the ledger fill
// second, precisely so a fill always carries the trade id it belongs to — which means a ledger
// failure leaves a position the journal holds and the book does not. The old code logged that and
// said "the book and the engine now disagree and /paper/status says so". **/paper/status did not
// say so**: nothing compared the three stores, no code path retried the lost fill, and the engine
// kept trading a config whose book was already wrong. A comment that promises a surface which does
// not exist is part of the defect, not a mitigation of it.
//
// This file is that surface, and the repair behind it:
//
//  1. DURABLE PENDING BOOKINGS (`store.PendingBooking`). A fill the ledger refused is written down
//     with everything needed to book it later, and retried at the start of every tick. Booking is
//     idempotent by (trade id, kind), so a crash between the ledger accepting a retry and the
//     pending record being cleared cannot double-book.
//  2. A RECONCILIATION CHECK, at startup and once per tick, comparing all three.
//  3. A DESYNCED CONFIG REFUSES TO TRADE — surfaced exactly like a gate refusal, with the mismatch
//     named. Marking and snapshots continue: a book that stopped measuring because its bookkeeping
//     disagreed would lose the very days somebody will want to read afterwards.
//
// Fail-closed, like everything else here. What is NOT checkable is reported as not checked
// (`journalChecked`), never as agreement.

// syncState is one config's three-store verdict, served under `/paper/status` as `sync`.
type syncState struct {
	// Consistent is false when the stores disagree in a way nothing explains, AND when a fill is
	// still owed to the book after this tick's retry. Both are reasons not to move a position.
	Consistent      bool     `json:"consistent"`
	PendingBookings int      `json:"pendingBookings"`
	Detail          string   `json:"detail"`
	Mismatches      []string `json:"mismatches,omitempty"`
	// What was actually compared. An unreachable journal is not agreement.
	JournalChecked bool `json:"journalChecked"`
	LedgerChecked  bool `json:"ledgerChecked"`
}

// blocked reports whether this config may change its position.
func (s syncState) blocked() bool { return !s.Consistent }

// --- the check ---------------------------------------------------------------------------------

// reconcileStores compares engine state, ledger lots and the journal's open paper trades for every
// configured config, caches the answer for this tick, and returns it.
//
// It never mutates a store. Repair is the pending-booking retry's job and the operator's; this only
// reports, because a reconciler that silently "fixed" a book by inventing or deleting a lot would
// be doing the one thing this whole service exists to avoid.
func (e *Engine) reconcileStores(ctx context.Context) map[string]syncState {
	out := e.compareAll(ctx)
	e.setSyncAll(out)
	return out
}

// compareAll is the same comparison WITHOUT touching the cache, for readers.
func (e *Engine) compareAll(ctx context.Context) map[string]syncState {
	journalOpen, journalChecked := e.openTradeIDs(ctx)

	byKey := map[string]ConfigState{}
	for _, st := range e.store.States() {
		byKey[st.ConfigKey] = st
	}

	out := map[string]syncState{}
	for _, cfg := range e.store.Configs() {
		key := cfg.Key()
		out[key] = e.syncStateFor(cfg, byKey[key], journalOpen, journalChecked)
	}
	return out
}

// compareOne re-runs the comparison for a SINGLE config and caches just that entry. Used after a
// repair, so the decision that follows is judged on the stores as they are now — without replacing
// the whole map, which would drop the entry for a config that is being evaluated but is not in the
// enabled list, and blocking it for a reason that is not true.
func (e *Engine) compareOne(ctx context.Context, cfg PaperCfg) syncState {
	journalOpen, journalChecked := e.openTradeIDs(ctx)
	s := e.syncStateFor(cfg, *e.store.StateFor(cfg), journalOpen, journalChecked)
	e.setSync(cfg.Key(), s)
	return s
}

// openTradeIDs is the set of journal trade ids that are currently open and papered. The bool is
// whether the journal could be asked at all.
func (e *Engine) openTradeIDs(ctx context.Context) (map[string]bool, bool) {
	trades, err := e.clients.journalPaperTrades(ctx, "open")
	if err != nil {
		return nil, false
	}
	ids := map[string]bool{}
	for _, t := range trades {
		if t.ID != "" {
			ids[t.ID] = true
		}
	}
	return ids, true
}

// syncStateFor is the comparison itself, for one config.
func (e *Engine) syncStateFor(cfg PaperCfg, st ConfigState, journalOpen map[string]bool,
	journalChecked bool) syncState {

	key := cfg.Key()
	pending := e.store.PendingBookingsFor(key)
	s := syncState{
		PendingBookings: len(pending),
		JournalChecked:  journalChecked,
		LedgerChecked:   e.ledger != nil,
	}
	if !journalChecked {
		s.Mismatches = append(s.Mismatches,
			"the journal could not be reached, so open paper trades could not be verified")
	}
	if e.ledger == nil {
		reason := "the ledger is unavailable"
		if e.ledgerErr != nil {
			reason += ": " + e.ledgerErr.Error()
		}
		s.Mismatches = append(s.Mismatches, reason)
	}
	if err := e.store.PersistenceError(); err != nil {
		s.Mismatches = append(s.Mismatches, "the engine state store is not durable: "+err.Error())
	}

	var lot *Lot
	if e.ledger != nil {
		lot = e.ledger.LotFor(key)
	}

	// ENGINE vs LEDGER. A missing book was already added as a fail-closed mismatch above.
	if e.ledger != nil {
		switch {
		case st.Side != "" && lot == nil:
			s.Mismatches = append(s.Mismatches, fmt.Sprintf(
				"the engine holds a %s position (trade %s) but the ledger holds no lot for %s",
				st.Side, orNone(st.TradeID), key))
		case st.Side == "" && lot != nil:
			s.Mismatches = append(s.Mismatches, fmt.Sprintf(
				"the ledger holds a %s lot (trade %s) but the engine is flat on %s",
				lot.Side, orNone(lot.TradeID), key))
		case st.Side != "" && lot != nil && lot.TradeID != st.TradeID:
			s.Mismatches = append(s.Mismatches, fmt.Sprintf(
				"the engine holds trade %s but the ledger's lot is trade %s",
				orNone(st.TradeID), orNone(lot.TradeID)))
		case st.Side != "" && lot != nil && lot.Side != st.Side:
			s.Mismatches = append(s.Mismatches, fmt.Sprintf(
				"the engine is %s but the ledger's lot is %s (trade %s)",
				st.Side, lot.Side, orNone(lot.TradeID)))
		}
	}

	// ENGINE vs JOURNAL. Only one direction is a desync. "The engine holds a trade the journal does
	// not have open" is a real disagreement. The MIRROR — the journal holds an open paper trade this
	// flat config could own — is the ORPHAN case, and `adoptOrphan` is the designed repair for it
	// (contract §3.6); flagging it here would block the very decision that adopts it.
	if journalChecked && st.Side != "" && st.TradeID != "" && !journalOpen[st.TradeID] {
		s.Mismatches = append(s.Mismatches, fmt.Sprintf(
			"the engine holds trade %s but the journal has no OPEN paper trade with that id",
			st.TradeID))
	}

	// A FILL THE BOOK STILL OWES is not agreement either. It is retried at the start of every tick,
	// so anything still queued here has already failed at least once and the book is behind by a
	// known amount.
	for _, p := range pending {
		s.Mismatches = append(s.Mismatches, fmt.Sprintf(
			"a %s fill for trade %s @ %.2f (bar %s) is still owed to the ledger after %d attempt(s): %s",
			p.Kind, orNone(p.TradeID), p.Price, orNone(p.Bar), p.Attempts, orNone(p.LastError)))
	}

	s.Consistent = len(s.Mismatches) == 0
	switch {
	case !s.Consistent:
		s.Detail = "DESYNCED — " + strings.Join(s.Mismatches, "; ") +
			". Position changes are blocked until the stores agree; marking and snapshots continue."
	default:
		s.Detail = "engine, ledger and journal agree"
	}
	return s
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none)"
	}
	return s
}

// --- the cache ---------------------------------------------------------------------------------
//
// The tick computes the map once and every config's decision reads it, so one journal round trip
// covers the whole pass rather than one per config.

func (e *Engine) setSyncAll(m map[string]syncState) {
	e.syncMu.Lock()
	defer e.syncMu.Unlock()
	e.sync = m
}

func (e *Engine) setSync(key string, s syncState) {
	e.syncMu.Lock()
	defer e.syncMu.Unlock()
	if e.sync == nil {
		e.sync = map[string]syncState{}
	}
	e.sync[key] = s
}

// syncFor reads this tick's verdict for one config. An UNRECONCILED config — one the current tick
// has not compared — is reported as such and treated as blocked: "we have not checked" is not
// "they agree", and this service fails closed everywhere else too.
func (e *Engine) syncFor(key string) syncState {
	e.syncMu.Lock()
	defer e.syncMu.Unlock()
	if s, ok := e.sync[key]; ok {
		return s
	}
	return syncState{Detail: "the three stores have not been reconciled yet for " + key}
}

// --- the repair --------------------------------------------------------------------------------

// retryPendingBookings is the FIRST thing every tick does: it offers the ledger every fill it
// refused earlier. Booking is idempotent by (trade id, kind), so a fill the book already holds
// resolves the pending record instead of double-booking it.
//
// A retry that fails again is KEPT, with its attempt count and its latest error, and leaves the
// config desynced. Giving up on a fill the book owes would make the book quietly wrong, which is
// the failure mode this whole file exists to remove.
func (e *Engine) retryPendingBookings(now time.Time) {
	pending := e.store.PendingBookings()
	if len(pending) == 0 {
		return
	}
	if e.ledger == nil {
		// Nothing to retry into. The records stay: a book that comes back later still owes them.
		log.Printf("PAPER LEDGER: %d fill(s) are owed to a book that is not running", len(pending))
		return
	}
	for _, p := range pending {
		if e.ledger.HasBooked(p.TradeID, p.Kind) {
			if err := e.store.ResolvePendingBooking(p.ident()); err != nil {
				log.Printf("PAPER STATE: could not durably clear already-booked pending fill %s: %v", p.ident(), err)
				continue
			}
			log.Printf("PAPER LEDGER: pending %s fill for trade %s was already booked — cleared",
				p.Kind, p.TradeID)
			continue
		}
		var err error
		switch p.Kind {
		case fillOpen, fillFlipOpen:
			_, err = e.ledger.openExact(p.cfg(), p.Side, p.Bar, p.TradeID, p.Price, p.CostBps,
				p.N, p.Qty, p.Notional, now, p.Kind, p.Note)
		case fillClose, fillFlipClose:
			_, err = e.ledger.close(p.cfg(), p.Bar, p.Price, now, p.Kind, p.Note)
		default:
			err = fmt.Errorf("unknown fill kind %q — this booking cannot be replayed", p.Kind)
		}
		switch {
		case err == nil:
			if clearErr := e.store.ResolvePendingBooking(p.ident()); clearErr != nil {
				log.Printf("PAPER STATE: booked pending fill %s but could not durably clear it: %v",
					p.ident(), clearErr)
				continue
			}
			log.Printf("PAPER LEDGER: booked the pending %s fill for %s trade %s @ %.2f (attempt %d)",
				p.Kind, p.ConfigKey, p.TradeID, p.Price, p.Attempts+1)
		case errors.Is(err, errDuplicateFill):
			if clearErr := e.store.ResolvePendingBooking(p.ident()); clearErr != nil {
				log.Printf("PAPER STATE: could not durably clear duplicate pending fill %s: %v",
					p.ident(), clearErr)
				continue
			}
			log.Printf("PAPER LEDGER: pending %s fill for trade %s was already booked — cleared",
				p.Kind, p.TradeID)
		default:
			if persistErr := e.store.FailPendingBooking(p.ident(), err.Error()); persistErr != nil {
				log.Printf("PAPER STATE: could not durably update failed pending fill %s: %v",
					p.ident(), persistErr)
			}
			log.Printf("PAPER LEDGER: the pending %s fill for %s trade %s is STILL unbooked: %v",
				p.Kind, p.ConfigKey, p.TradeID, err)
		}
	}
}

// owePending records a fill the ledger would not take, so the next tick can offer it again. It is
// the ONE place a lost fill becomes a durable obligation rather than a log line.
func (e *Engine) owePending(p PendingBooking, now time.Time, cause error) error {
	p.At = now.UTC().Format(time.RFC3339)
	if cause != nil {
		p.LastError = cause.Error()
	}
	if err := e.store.AddPendingBooking(p); err != nil {
		return fmt.Errorf("persist pending %s fill for trade %s: %w", p.Kind, p.TradeID, err)
	}
	// The config is desynced from this instant, not from the next tick's reconciliation: it must not
	// take another decision on a book that is already behind.
	s := e.syncFor(p.ConfigKey)
	s.Consistent = false
	s.PendingBookings = len(e.store.PendingBookingsFor(p.ConfigKey))
	s.Mismatches = append(s.Mismatches, fmt.Sprintf(
		"a %s fill for trade %s @ %.2f (bar %s) was refused by the ledger and is owed to it: %v",
		p.Kind, orNone(p.TradeID), p.Price, orNone(p.Bar), cause))
	s.Detail = "DESYNCED — " + strings.Join(s.Mismatches, "; ") +
		". Position changes are blocked until the stores agree; marking and snapshots continue."
	e.setSync(p.ConfigKey, s)
	return nil
}

// SyncReport is the status handler's read: a FRESH comparison, so an operator watching
// /paper/status sees the stores as they are now rather than as the last tick left them.
//
// It deliberately does NOT write the cache. A status request runs on an HTTP goroutine with the
// REQUEST's context, and a client that disconnects mid-call cancels the journal lookup — which would
// stamp `journalChecked: false` over a comparison the tick had already made properly, and a
// comparison that did not happen must never replace one that did.
func (e *Engine) SyncReport(ctx context.Context) map[string]syncState {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	return e.compareAll(ctx)
}
