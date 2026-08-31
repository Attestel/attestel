# Convenience targets. The app needs no Makefile to run (see README) — these just wrap the commands
# people type most. Everything here is research/analysis; nothing places an order or moves money.

.PHONY: help evaluate evaluate-quick evaluate-local evaluate-events evaluate-events-quick evaluate-events-local collect-estimates collect-estimates-local test-prediction ingest ingest-once ablate enrich-once smoke-model automate-once automate-lane production-scheduler automation-status

help:
	@echo "Targets:"
	@echo "  make evaluate               Stage-1 edge check via docker (EDGE / NO EDGE / INCONCLUSIVE)."
	@echo "  make evaluate-quick         Fast smoke pass (small universe / few permutations)."
	@echo "  make evaluate-local         Run the price harness locally against a running analysis :8001."
	@echo "  make evaluate-events        Event study (post-earnings drift) via docker. Needs ALPHAVANTAGE_API_KEY."
	@echo "  make evaluate-events-quick  Fast event-study pass (small universe / few permutations)."
	@echo "  make evaluate-events-local  Run the event study locally against a running analysis :8001."
	@echo "  make collect-estimates      ONE bounded T-7/T-1 estimate snapshot pass via docker."
	@echo "  make collect-estimates-local  The same bounded collector in the local prediction env."
	@echo "  make test-prediction        Run the prediction service test suite."
	@echo "  make ingest                 One manual ingestion run against a RUNNING events :8004."
	@echo "  make ingest-once            The same, narrowed by INGEST_PROVIDERS / INGEST_TICKERS."
	@echo "  make ablate                 The A-F point-in-time ablation ladder (long-running)."
	@echo "  make enrich-once            ONE bounded enrichment pass, then exit. The worker is OFF"
	@echo "                              unless EVENT_ENRICH_ENABLED=true. There is no recurring"
	@echo "                              target; model work is explicit and never scheduled."
	@echo "  make smoke-model            The prod smoke: prove a real model served us, THEN report"
	@echo "                              the seven measures. Refuses on stub:offline. Needs"
	@echo "                              MODEL_RUNTIME_URL + MODEL_RUNTIME_MODEL (no defaults)."
	@echo "  make automate-once          ONE bounded automation dispatch pass over every DUE lane,"
	@echo "                              then exit. OFF unless AUTOMATION_ENABLED=true AND the"
	@echo "                              lane's own flag is true. Run explicitly; do not schedule"
	@echo "                              this target because it includes model-owned lanes."
	@echo "  make automate-lane LANE=x   The same, for one lane, ignoring its cadence (--force)."
	@echo "  make production-scheduler   Run the production clock for model-free events lanes,"
	@echo "                              including Scout and the Opportunity Radar."
	@echo "  make automation-status      Lane health: enabled, last success, last failure,"
	@echo "                              freshness, queue depth. Reads only; no secret values."

# The Stage-1 go/no-go: does the model have a REAL edge? Long-running (universe x horizons x
# walk-forward folds x permutations); expect minutes, not seconds. Writes data/eval/report-*.{md,json}.
# REFUSES a verdict on synthetic/no-network data. Brings analysis up as a dependency.
evaluate:
	docker compose run --rm prediction python -m app.evaluate

# A quick pass to prove the wiring end-to-end without waiting for the full universe.
evaluate-quick:
	docker compose run --rm \
	  -e EVAL_UNIVERSE=NVDA,AAPL,MSFT,GOOGL,AMD,META \
	  -e EVAL_HORIZONS=5 -e EVAL_PERMUTATIONS=100 -e EVAL_HISTORY_DAYS=1825 \
	  prediction python -m app.evaluate

# Local (no docker): analysis must already be serving REAL prices on :8001. Writes services/prediction/data/eval/.
evaluate-local:
	cd services/prediction && python -m app.evaluate

# Event study — post-earnings drift (PEAD). Needs ALPHAVANTAGE_API_KEY (in .env) for earnings history;
# it caches each ticker's history to data/eval/earnings/ (25 req/day cap, so a big universe fills over
# a couple of days). Refuses on synthetic/no-key. Writes data/eval/event-report-*.{md,json}.
evaluate-events:
	docker compose run --rm prediction python -m app.evaluate_events

# A quick event-study pass (still needs a key or a warm earnings cache).
evaluate-events-quick:
	docker compose run --rm \
	  -e EVENT_UNIVERSE=NVDA,AAPL,MSFT,GOOGL,AMD,META \
	  -e EVENT_HORIZONS=10 -e EVENT_PERMUTATIONS=100 -e EVENT_MIN=20 \
	  prediction python -m app.evaluate_events

# Local (no docker): analysis on :8001, and ALPHAVANTAGE_API_KEY exported (or a warm earnings cache).
evaluate-events-local:
	cd services/prediction && python -m app.evaluate_events

# ONE bounded point-in-time estimate collection pass. Repetition belongs to an external scheduler;
# the application contains no timer. PostgreSQL and ALPHAVANTAGE_API_KEY are required. Existing
# ticker/fiscal-period/stage snapshots are skipped before spending a provider call.
collect-estimates:
	docker compose run --rm prediction python -m app.estimate_snapshots

collect-estimates-local:
	cd services/prediction && python -m app.estimate_snapshots

test-prediction:
	cd services/prediction && python -m pytest -q

# `services/llm`'s suite. Needs `requirements-dev.txt` in the interpreter (pytest + httpx, the
# latter only because starlette's TestClient imports it) and `analysis`/`prediction`'s per-service
# .venv problem does not apply here — `llm` runs on the named interpreter.
test-llm:
	cd services/llm && python -m pytest tests -q

# Wave 4 integration gate (§9.56 binding 3, §9.58). No server-side-only field may reach the built
# bundle. Cheap, and it catches the copy-paste a visual check cannot — a field rendered inside a
# collapsed panel looks like nothing until someone expands it.
check-web-bundle:
	@./scripts/check-web-bundle.sh web/dist

# The mirror-image gate (Wave 4 integration, §AD-8). check-web-bundle asks "did a server-only FIELD
# reach the screen"; this asks "did a server-owned RULE get copied into the client". Two lanes
# independently hand-copied `importanceHighMin`/`importanceMediumMin` into JavaScript and one of the
# copies had already drifted. The band is served now; this keeps the constants deleted. Source-level,
# so it needs no build.
check-band-parity:
	@./scripts/check-band-parity.sh web/src gateway/feeds.go

# One manual ingestion run against a RUNNING events service (contract §3.11 POST /ingest). Never
# called by the UI — ingestion is a batch job, not a page load. Available from Wave 1C; until then
# the route does not exist and this prints a 404, honestly.
ingest:
	curl -s -X POST http://localhost:8004/ingest -H 'Content-Type: application/json' -d '{}' ; echo

# The same, narrowed to one provider set / ticker set from .env, for a cheap smoke pass. Posts {}
# — every configured provider, configured TICKERS plus followed tickers — when both filters are
# empty, rather than the malformed {"providers":[""],"tickers":[""]}.
ingest-once:
	@if [ -z "$(INGEST_PROVIDERS)" ] && [ -z "$(INGEST_TICKERS)" ]; then \
	  curl -s -X POST http://localhost:8004/ingest -H 'Content-Type: application/json' -d '{}' ; echo ; \
	else \
	  curl -s -X POST http://localhost:8004/ingest -H 'Content-Type: application/json' \
	    -d "{\"providers\":[\"$(INGEST_PROVIDERS)\"],\"tickers\":[\"$(INGEST_TICKERS)\"]}" ; echo ; \
	fi

# The A–F point-in-time ablation ladder (contract §5.1). Long-running, real-data-only, refuses a
# verdict on synthetic data exactly as `make evaluate` does. Driver lands in Wave 3C.
ablate:
	docker compose up -d analysis events llm
	docker compose run --rm prediction python -m app.ablation

# ONE bounded enrichment pass over the raw-event queue, then exit (D-25, contract §9.22). This is
# the explicit operator command D-25 permits: it POSTs nothing and polls nothing, it runs the
# worker directly, and the worker holds the cross-process model lease that interactive requests
# always win. The worker refuses to do anything unless EVENT_ENRICH_ENABLED=true, which is NOT the
# shipped default.
#
# There is deliberately NO recurring variant of this target. A timer here would be a model call
# caused by a scheduler tick, which invariant #4 forbids without qualification.
enrich-once:
	docker compose run --rm llm python -m app.enrich_worker

# ONE bounded stale-thesis re-synthesis pass. Alerts owns deterministic detection + durable leases;
# journal owns the thesis; this process alone owns model generation and takes the background lease.
# Off by default and never scheduled. An operator may invoke this target explicitly only after
# setting THESIS_RESYNTH_ENABLED=true.
resynth-once:
	docker compose up -d analysis events gateway alerts journal
	docker compose run --rm llm python -m app.thesis_resynth

# THE PROD SMOKE (Wave 5C). One runnable check, executed by whoever brings the prod box up, which
# produces the measurement table this programme has been deferring: resident memory, cold load and
# generation latency, the override-branch distribution, the `proseSuppressions` rate itemised, the
# schema-failure rate, whether `neutral`/`unclear` are reachable without the stub, and the
# `BANNED_ANALYST` hit rate WITH its denominator.
#
# IT REFUSES BEFORE IT MEASURES. No MODEL_RUNTIME_URL, a `stub:*` answer, a server serving a model
# other than the configured one, or a lease that cannot be shown taken and released — any of those
# and it exits non-zero having printed no numbers at all. `stub:offline` never licenses a claim
# about the model, so a smoke that fell back to it would be worse than one that did not run.
#
# It is NOT part of any test suite: it needs a real model, a real endpoint, and minutes. And it is
# not on a timer — this is an operator command, like `enrich-once`, for the same reason (§9.22).
smoke-model:
	docker compose run --rm llm python -m app.smoke

# ---- Docker lifecycle (run the whole app with one command) ----
up: ## build if needed + start all services in the background
	docker compose up -d --build
	@echo "up → web http://localhost:5173 · gateway http://localhost:8080"

down: ## stop and remove all containers
	docker compose down

restart: ## recreate all services (picks up .env changes)
	docker compose up -d --build --force-recreate

logs: ## tail logs from all services (Ctrl-C to stop)
	docker compose logs -f --tail=100

ps: ## show service status
	docker compose ps

# Container state is checked BEFORE ports, and a non-running service is fatal. A port answering 200
# proves only that SOMETHING holds it -- on 2026-08-17 a stray binary from another project squatted
# 8080 and reported a healthy gateway whose container had never started. Ports alone cannot tell the
# difference; `docker compose ps` can.
health: ## verify every service is running, then curl its /health
	@echo "== container state =="
	@docker compose ps -a --format '{{.Service}}\t{{.State}}' | sort | awk -F'\t' \
		'{printf "  %-11s %s\n", $$1, $$2; if ($$2 != "running") bad++} \
		 END {if (bad) {printf "\nFAIL: %d service(s) not running -- ports below are NOT trustworthy\n", bad; exit 1}}'
	@echo
	@echo "== /health =="
	@rc=0; for pair in 8001:analysis 8002:llm 8003:prediction 8004:events 8080:gateway \
	                   8095:alerts 8096:journal 8097:paper 8098:feedback 8099:auth; do \
		p=$${pair%%:*}; s=$${pair##*:}; \
		code=$$(curl -s -o /dev/null -m 5 -w '%{http_code}' http://localhost:$$p/health 2>/dev/null); \
		printf "  %-6s %-11s %s\n" "$$p" "$$s" "$$code"; \
		[ "$$code" = "200" ] || rc=1; \
	done; exit $$rc

# ---- automation (Phase 1) ------------------------------------------------------------------------
#
# `automate-once` is an EXPLICIT operator pass across both halves. Do not put it in cron: two lanes
# call the model, and invariant #4 forbids a scheduler tick from causing a model call. Production
# repetition uses `app.production_scheduler`, which is structurally limited to the five model-free
# events lanes: ingest, reactions, scout-intake, scout, and opportunity-radar.
#
# TWO HALVES, ONE PASS. `services/events` dispatches the lanes it can run against its own database
# (ingest, reactions, scout-intake, scout, opportunity-radar); `services/llm` dispatches the lanes whose executor lives
# elsewhere (enrich and resynth, both under the cross-process model lease, plus the model-free
# thesis-monitor sweep in `alerts`). Both halves take the SAME lane lease out of PostgreSQL, so they
# cannot collide with each other or with a second copy of this target.
automate-once:
	docker compose run --rm events python -m app.automation
	docker compose run --rm llm python -m app.automation

# The production clock is deliberately narrower than automate-once: the imported dispatcher owns
# only the five model-free events lanes. No scheduler tick can reach the model lanes.
production-scheduler:
	docker compose run --rm events python -m app.production_scheduler

# One lane, right now, ignoring its cadence. It still respects the flags and still takes the lease,
# so this cannot stampede a lane another dispatcher is already running.
#   make automate-lane LANE=ingest
automate-lane:
	@if [ -z "$(LANE)" ]; then echo "usage: make automate-lane LANE=<ingest|reactions|scout-intake|scout|opportunity-radar|enrich|resynth|thesis-monitor>"; exit 2; fi
	@case "$(LANE)" in \
	  ingest|reactions|scout-intake|scout|opportunity-radar) docker compose run --rm events python -m app.automation --lane $(LANE) --force ;; \
	  enrich|resynth|thesis-monitor) docker compose run --rm llm python -m app.automation --lane $(LANE) --force ;; \
	  *) echo "unknown lane: $(LANE)"; exit 2 ;; \
	esac

# Operator-safe read. Reports flag NAMES and booleans, never a flag VALUE and never a secret.
automation-status:
	curl -s http://localhost:8004/automation/status ; echo
