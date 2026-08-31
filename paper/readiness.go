package main

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// readinessCheck is one factual precondition for starting an official experiment. The reset
// handler consumes the same structure the UI reads; there is no separate, weaker launch path.
type readinessCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

type configReadiness struct {
	Config string           `json:"config"`
	Ready  bool             `json:"ready"`
	Checks []readinessCheck `json:"checks"`
}

type experimentReadiness struct {
	Ready     bool              `json:"ready"`
	CheckedAt string            `json:"checkedAt"`
	Checks    []readinessCheck  `json:"checks"`
	Configs   []configReadiness `json:"configs"`
	Blockers  []string          `json:"blockers"`
	Note      string            `json:"note"`
}

func (r *experimentReadiness) add(check readinessCheck) {
	r.Checks = append(r.Checks, check)
	if !check.OK {
		r.Blockers = append(r.Blockers, check.Name+": "+check.Detail)
	}
}

func (r *experimentReadiness) addConfig(row configReadiness) {
	r.Configs = append(r.Configs, row)
	if row.Ready {
		return
	}
	for _, check := range row.Checks {
		if !check.OK {
			r.Blockers = append(r.Blockers, row.Config+" — "+check.Name+": "+check.Detail)
		}
	}
}

// readinessLocked evaluates the launch checklist while Engine.opMu is held. It reads the same
// upstream payloads and invokes the same four gates as a real decision, plus the quote-integrity,
// durable-store and three-store checks the experiment needs in order to keep honest score.
//
// It never mutates state. A negative evaluator verdict is a successful check result whose gate is
// false; it is never converted into an error and never bypassed.
func (a *API) readinessLocked(ctx context.Context, now time.Time) experimentReadiness {
	r := experimentReadiness{CheckedAt: now.UTC().Format(time.RFC3339)}
	configs := a.store.Configs()

	r.add(readinessCheck{
		Name: "configs", OK: len(configs) > 0,
		Detail: fmt.Sprintf("%d paper configuration(s) are durably enabled", len(configs)),
	})
	r.add(readinessCheck{
		Name: "real-bar-clock", OK: !a.cfg.fastForward(),
		Detail: map[bool]string{
			true:  "PAPER_BAR_SECONDS=0; only provider bar timestamps advance the experiment",
			false: "PAPER_BAR_SECONDS is enabled; fast-forward is for demos/tests and cannot start an official experiment",
		}[!a.cfg.fastForward()],
	})
	storeErr := a.store.Check(ctx)
	if storeErr == nil {
		storeErr = a.store.PersistenceError()
	}
	r.add(readinessCheck{
		Name: "engine-storage", OK: storeErr == nil,
		Detail: detailFor(storeErr, "the engine state store is reachable and has no unresolved commit failure"),
	})
	r.add(readinessCheck{
		Name: "ledger", OK: a.ledger() != nil,
		Detail: map[bool]string{
			true:  "the equal-weight simulated ledger is initialized",
			false: "the equal-weight simulated ledger is unavailable; no official result can be scored",
		}[a.ledger() != nil],
	})
	r.add(readinessCheck{
		Name: "journal-recording", OK: a.cfg.AuthSecret != "",
		Detail: map[bool]string{
			true:  "AUTH_SECRET is configured for the platform validation engine's journal records",
			false: "AUTH_SECRET is missing; the engine cannot record simulated trades in the journal",
		}[a.cfg.AuthSecret != ""],
	})

	sync := map[string]syncState{}
	if a.engine != nil {
		sync = a.engine.compareAll(ctx)
	}
	allStoresReachable := a.engine != nil && len(configs) > 0
	setupMismatches := 0
	for _, cfg := range configs {
		sy, ok := sync[cfg.Key()]
		if !ok || !sy.JournalChecked || !sy.LedgerChecked {
			allStoresReachable = false
		}
		if ok && (!sy.Consistent || sy.PendingBookings != 0) {
			setupMismatches++
		}
	}
	syncDetail := "engine, ledger and journal are reachable for every enabled config"
	if setupMismatches > 0 && allStoresReachable {
		syncDetail = fmt.Sprintf(
			"all three stores are reachable; the confirmed reset will clear %d setup-era mismatch(es) before day 0",
			setupMismatches,
		)
	}
	r.add(readinessCheck{
		Name: "three-store-access", OK: allStoresReachable,
		Detail: map[bool]string{
			true:  syncDetail,
			false: "engine, ledger and journal were not all reachable for every enabled config",
		}[allStoresReachable],
	})

	for _, cfg := range configs {
		row := configReadiness{Config: cfg.Key(), Ready: true}
		add := func(check readinessCheck) {
			row.Checks = append(row.Checks, check)
			row.Ready = row.Ready && check.OK
		}

		bar, barErr := a.clients.latestBar(ctx, cfg.Ticker, cfg.Timeframe)
		add(readinessCheck{
			Name: "latest-bar", OK: barErr == nil,
			Detail: detailFor(barErr, "latest bar loaded with explicit provenance"),
		})
		pred, predErr := a.clients.predict(ctx, cfg)
		add(readinessCheck{
			Name: "prediction", OK: predErr == nil,
			Detail: detailFor(predErr, "prediction record and its validation evidence loaded"),
		})
		quote, quoteErr := a.clients.quote(ctx, cfg.Ticker)
		add(readinessCheck{
			Name: "execution-quote", OK: quoteErr == nil,
			Detail: detailFor(quoteErr, "execution quote loaded with source and timestamp"),
		})

		if barErr == nil && predErr == nil {
			for _, gate := range a.engine.gateInputs(cfg, pred, bar, quote, now).gates() {
				add(readinessCheck{Name: gate.Name, OK: gate.OK, Detail: gate.Detail})
			}
		}
		if predErr == nil {
			ok := pred != nil && pred.Signal != nil
			detail := "the validated prediction service returned a target-bearing signal"
			if !ok {
				detail = "the prediction service returned no validated signal"
				if pred != nil && pred.Reason != "" {
					detail += ": " + pred.Reason
				}
			}
			add(readinessCheck{Name: "validated-signal", OK: ok, Detail: detail})
		}
		if quoteErr == nil && barErr == nil {
			issue := executionQuoteIssue(quote, bar, cfg.Timeframe)
			add(readinessCheck{
				Name: "quote-integrity", OK: issue == "",
				Detail: map[bool]string{
					true:  "the real quote is not older than the bar it would reconcile",
					false: issue,
				}[issue == ""],
			})
		}
		if sy, ok := sync[cfg.Key()]; ok {
			detail := sy.Detail
			if sy.JournalChecked && sy.LedgerChecked && !sy.Consistent {
				detail += " The confirmed reset will clear this setup-era mismatch before day 0."
			}
			add(readinessCheck{Name: "three-store-access", OK: sy.JournalChecked && sy.LedgerChecked, Detail: detail})
		}
		r.addConfig(row)
	}

	r.Ready = len(r.Blockers) == 0
	if r.Ready {
		r.Note = "All launch checks pass. A confirmed reset may establish day 0; this still simulates only and executes no order."
	} else {
		r.Note = "Official start is blocked. Fix the named causes; reset does not repair a failed gate and cannot bypass one."
	}
	return r
}

func detailFor(err error, ok string) string {
	if err == nil {
		return ok
	}
	return err.Error()
}

func (a *API) handleReadiness(w http.ResponseWriter, r *http.Request) {
	if a.engine == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ready": false, "error": "launch readiness is unavailable: no engine is attached",
		})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	a.engine.opMu.Lock()
	readiness := a.readinessLocked(ctx, a.currentTime())
	a.engine.opMu.Unlock()
	writeJSON(w, http.StatusOK, readiness)
}
