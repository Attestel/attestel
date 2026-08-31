"""PostgreSQL-backed events service (Python · FastAPI, :8004)."""
from __future__ import annotations

import importlib
import logging
import os
import time
from contextlib import asynccontextmanager

from fastapi import FastAPI
from fastapi.middleware.cors import CORSMiddleware

from . import budget
from .db import applied_versions, connect, database_schema, database_target, migrate
from .repairs import apply_data_repairs, data_repair_status

log = logging.getLogger("events")

# ── boot resilience ───────────────────────────────────────────────────────────────────────────
#
# This module used to call `connect()` at import-of-lifespan time with no retry and no handler. A
# PostgreSQL that was not yet accepting connections therefore killed the process outright, and
# `deploy/supervisord.conf` sets `startretries=10` — so after ten fast failures supervisord marked
# the program FATAL and stopped trying. `autorestart=true` does not apply to a program that never
# started successfully. The observed consequence (2026-08-22): the database came back and the
# events service stayed dead until the whole container was restarted, while every other service
# reported green.
#
# Two changes fix that. Boot now RETRIES with backoff, and if the budget runs out it comes up
# ANYWAY in a schema-not-ready state rather than exiting — `/health` says so, and `ensure_schema()`
# completes the migration on the first poll that finds PostgreSQL back. Recovery no longer needs a
# human.
#
# This adds no timer and no background thread: `ensure_schema()` runs inside the boot hook and
# inside `/health`, both of which are caller-driven. Invariant #4 is untouched — nothing here can
# cause a model call, and there is no loop that runs on its own.

EVENTS_DB_BOOT_RETRY_SECONDS = int(os.getenv("EVENTS_DB_BOOT_RETRY_SECONDS", "").strip() or 120)
_BOOT_BACKOFF_CAP_SECONDS = 15

_SCHEMA_READY = False
_SCHEMA_ERROR: str | None = None


def ensure_schema() -> bool:
    """Migrate once, on the first attempt that reaches PostgreSQL. Idempotent and cheap when ready.

    Returns True when the schema is up to date. On failure it records the reason for `/health` and
    returns False — it never raises, because both callers are diagnostics paths and a diagnostic
    that can crash is the thing this file is fixing.
    """
    global _SCHEMA_READY, _SCHEMA_ERROR
    if _SCHEMA_READY:
        return True
    try:
        conn = connect()
        try:
            migrate(conn)
            apply_data_repairs(conn)
        finally:
            conn.close()
    except Exception as exc:  # noqa: BLE001 — reported, never raised
        _SCHEMA_ERROR = f"{type(exc).__name__}: {exc}"
        return False
    _SCHEMA_READY = True
    _SCHEMA_ERROR = None
    return True


@asynccontextmanager
async def lifespan(_app: FastAPI):
    """Bring the schema up to date on boot, retrying a database that is not up yet.

    Idempotent — applying twice is a no-op. Serving with an unmigrated schema is a real degradation
    and every query will fail until `ensure_schema()` succeeds, but it is strictly better than the
    alternative it replaces: a process that exits, exhausts its supervisor's start budget, and then
    cannot recover without a container restart even after the database returns.
    """
    deadline = time.monotonic() + max(0, EVENTS_DB_BOOT_RETRY_SECONDS)
    delay = 1.0
    attempt = 0
    while True:
        attempt += 1
        if ensure_schema():
            if attempt > 1:
                log.warning("events: database reached on attempt %d; schema up to date", attempt)
            break
        if time.monotonic() >= deadline:
            log.error(
                "events: giving up on the boot connect after %ds (%d attempts) — serving in a "
                "SCHEMA-NOT-READY state; /health reports status=degraded and every query will "
                "fail until PostgreSQL returns. Last error: %s",
                EVENTS_DB_BOOT_RETRY_SECONDS,
                attempt,
                _SCHEMA_ERROR,
            )
            break
        log.warning(
            "events: database unreachable on boot attempt %d, retrying in %.0fs — %s",
            attempt,
            delay,
            _SCHEMA_ERROR,
        )
        time.sleep(delay)
        delay = min(delay * 2, _BOOT_BACKOFF_CAP_SECONDS)
    yield


app = FastAPI(title="NVDA Platform — Events Service", version="0.1.0", lifespan=lifespan)
app.add_middleware(
    CORSMiddleware, allow_origins=["*"], allow_methods=["*"], allow_headers=["*"]
)


@app.get("/health")
def health() -> dict:
    """Contract §3.11.

    `providers` is wired from `budget.state()` (Wave 1 integration). It is the answer to §9.44: the
    gate must judge whether a provider ran from PROVENANCE, never from grepping logs for a provider
    name. Docker gate Pass 1 grepped every container log for `alphavantage|marketaux|tiingo|sec.gov|
    yfinance|news.google`, found nothing, and was wrong — those calls demonstrably happened and are
    simply not logged. This block cannot be wrong in that direction: `calls` is incremented by
    `budget.reserve` BEFORE the socket is opened, so a provider that ran is counted whether or not
    it succeeded, logged or crashed.

    `budget.state()` lists the FULL cascade, including providers with no row yet, so an operator can
    tell "configured and idle" from "not in the build". Fail-soft: /health degrades, it does not
    500 — a service whose health endpoint can fail cannot be used to diagnose the service.

    THAT LAST SENTENCE WAS A PROMISE THIS FUNCTION DID NOT KEEP. `connect()` sat outside the try,
    so a refused PostgreSQL connection propagated and `/health` answered 500 — unusable for exactly
    the outage it most needed to describe. Every connection-owning call is now guarded and the
    failure is reported as `status: "degraded"` with a `dbError`, which is what `gateway/health.go`
    reads to mark the dependency down.

    This is also the recovery point. `ensure_schema()` migrates on the first poll that reaches a
    database which was down at boot, so a gateway/platform probe walks the service back to healthy
    with no restart and no operator.
    """
    schema_ready = ensure_schema()

    versions: list = []
    repairs: list = []
    providers: list = []
    quota_summary: dict | None = None
    provider_error: str | None = None
    db_error: str | None = None

    if schema_ready:
        try:
            conn = connect()
            try:
                versions = applied_versions(conn)
                repairs = data_repair_status(conn)
                try:
                    providers = budget.state(conn)
                    quota_summary = budget.quota_summary(providers)
                except Exception as exc:  # noqa: BLE001 — /health reports, it never raises
                    provider_error = f"{type(exc).__name__}: {exc}"
            finally:
                conn.close()
        except Exception as exc:  # noqa: BLE001 — the store is the thing being reported on
            db_error = f"{type(exc).__name__}: {exc}"
    else:
        db_error = _SCHEMA_ERROR or "schema not ready"

    # `database_target()` resolves the URL, so it raises when none is configured. An unconfigured
    # service is a state to REPORT, not a second way for this endpoint to fail.
    try:
        target = database_target()
    except Exception as exc:  # noqa: BLE001
        target = None
        db_error = db_error or f"{type(exc).__name__}: {exc}"

    body = {
        "status": "ok" if db_error is None else "degraded",
        "service": "events",
        "storage": "postgresql",
        "db": target,
        "schema": database_schema(),
        "schemaReady": schema_ready,
        "migrations": versions,
        "dataRepairs": repairs,
        "providers": providers,
    }
    if quota_summary is not None:
        body["quotaSummary"] = quota_summary
    if db_error:
        # The one field that would have ended the 2026-08-22 outage in a minute instead of an hour.
        body["dbError"] = db_error
    if provider_error:
        # Named rather than swallowed: an empty `providers` for an unknown reason is exactly the
        # kind of silence §9.44 forbids treating as evidence.
        body["providersError"] = provider_error
    return body


# Optional routers. Each later lane adds its OWN module exporting `router`; this loop picks it up.
# A module that is simply absent is skipped; any other ModuleNotFoundError is a real error.
#
# Binding rule (EVENT_CONTRACTS.md §9.28): every module in this service exports its router under the
# literal name `router` — not documents_router, not events_router — and hands the integrator NO
# manual include_router line. A lane that does both gets its routes registered twice.
OPTIONAL_ROUTERS = (
    "documents", "ingest", "events", "macro", "predictions", "automation", "relationships",
    "reactions", "scout", "opportunities",
)

for _name in OPTIONAL_ROUTERS:
    try:
        _mod = importlib.import_module(f".{_name}", __package__)
    except ModuleNotFoundError as exc:
        if exc.name != f"{__package__}.{_name}":
            raise
        continue
    _router = getattr(_mod, "router", None)
    if _router is not None:
        app.include_router(_router)
