"""Wave 2 Lane 2B — the enrichment worker: ONE bounded pass over the raw-event queue, then exit.

    python -m app.enrich_worker          # or: make enrich-once

This is the single background component D-25 permits, and everything about it is shaped by the
clauses that permit it:

* **Off by default.** `EVENT_ENRICH_ENABLED` is `false` unless an operator sets it. Nothing else —
  no key, no reachable model, no populated queue — can switch it on.
* **One pass, no interval (contract §9.22).** `run_once()` drains a bounded batch and RETURNS. It
  does not loop, sleep or reschedule itself; the interval variable §9.22 deletes is deleted, not
  defaulted. Repetition is the operator's cron, OUTSIDE this codebase — a timer in here would be a
  model call caused by a scheduler tick, which invariant #4 forbids without qualification. An
  unbounded loop or a sleep in this file is a defect, and `test_enrich_worker.py` asserts on the
  source: the whole file must satisfy §9.22's grep, prose included, so the two forbidden spellings
  do not appear here even as examples.
* **Not reachable from the service.** No FastAPI route, no startup hook, no import from `main.py`.
  It is a process, invoked deliberately.
* **One model at a time.** Every generation runs inside `lease.acquire_background()` — a
  cross-process `fcntl.flock` (contract §9.21) that interactive work always wins. The lease is
  released BETWEEN items, so an interactive request waits for at most one in-flight generation.
* **Point in time (contract §9.12).** For each event, `as_of = publishedAt + ENRICH_LAG_HOURS` is
  computed ONCE and used for the prior-events novelty read AND as the posted `enrichmentAsOf`.
  Never "the last 30 days from now": a backlog event enriched today must not see events that
  happened after it, and no leakage test that patches the HTTP client can catch it, because no
  network call reveals it.
* **Two endpoints, no providers.** It reads `services/events` and it talks to the model. That is
  the whole surface. It never fetches a provider.

ENVIRONMENT — all read at CALL time, never at import, so a container that configures late and a
test that monkeypatches are both honoured:

| Variable | Default | Meaning |
|---|---|---|
| `EVENT_ENRICH_ENABLED` | `false` | the only door |
| `EVENTS_URL` | `http://localhost:8004` | the events service |
| `ENRICH_BATCH_SIZE` | `5` | queue page size |
| `ENRICH_LAG_HOURS` | `24` | §9.12's `ENRICH_LAG` |
| `ENRICH_MAX_PER_RUN` | `25` | hard cap on events processed in one pass |
| `ENRICH_IDLE_SECONDS` | `30` | read by `lease.py`: cooldown after an interactive release |

TWO THINGS THIS WORKER CANNOT DO, MEASURED AGAINST LANE 2A'S ENDPOINT
----------------------------------------------------------------------
1. **It cannot record `enrichment_state = "failed"`.** `POST /events/{id}/enrichment` sets
   `enrichment_state = 'enriched'` unconditionally, and there is no other write path. Posting an
   audit-only body on the stub path would therefore mark the event *enriched* with no hypotheses —
   a lie in the feed, and a permanent one, since the event would leave the raw queue. So the stub
   and skip paths post NOTHING and the event stays `raw`, which is the fail-safe direction: it is
   re-attempted on the operator's next pass. The endpoint change this needs is in the handoff.
2. **It cannot ask the queue for importance order.** `GET /events` orders by
   `published_at DESC, id DESC` only (§3.11), with no `orderBy`. The worker therefore sorts the
   fetched window itself. The consequence is real and is in the handoff's known gaps: with a large
   backlog, an old high-importance event outside the fetched window is never reached.
"""
from __future__ import annotations

import json
import logging
import os
from datetime import datetime, timezone

import requests

from . import event_agent, lease, llm

log = logging.getLogger("enrich_worker")

DEFAULT_EVENTS_URL = "http://localhost:8004"
DEFAULT_BATCH_SIZE = 5
DEFAULT_LAG_HOURS = 24.0
DEFAULT_MAX_PER_RUN = 25
HTTP_TIMEOUT = 15

#: Stop reasons. `drained` is the only success.
DRAINED = "drained"
DISABLED = "disabled"
PREEMPTED = "preempted"
LEASE_UNAVAILABLE = "lease_unavailable"
EVENTS_UNREACHABLE = "events_unreachable"


class _EventsUnreachable(RuntimeError):
    """The events service is down or erroring. Stop the batch — an outage is not a reason to
    hammer it, and a half-read queue is not a queue."""


# --- environment, read at call time --------------------------------------------------------------

def enabled() -> bool:
    """`EVENT_ENRICH_ENABLED`. Strictly `true`/`1`/`yes`/`on`; anything else, including absent, is
    off. Deliberately narrow — a typo must fail CLOSED."""
    return (os.getenv("EVENT_ENRICH_ENABLED") or "").strip().lower() in ("true", "1", "yes", "on")


def events_url() -> str:
    return (os.getenv("EVENTS_URL") or DEFAULT_EVENTS_URL).strip().rstrip("/")


def _positive_int(name: str, default: int) -> int:
    raw = (os.getenv(name) or "").strip()
    try:
        value = int(raw) if raw else default
    except ValueError:
        return default
    return value if value > 0 else default


def batch_size() -> int:
    return _positive_int("ENRICH_BATCH_SIZE", DEFAULT_BATCH_SIZE)


def max_per_run() -> int:
    return _positive_int("ENRICH_MAX_PER_RUN", DEFAULT_MAX_PER_RUN)


def lag_hours() -> float:
    raw = (os.getenv("ENRICH_LAG_HOURS") or "").strip()
    try:
        value = float(raw) if raw else DEFAULT_LAG_HOURS
    except ValueError:
        return DEFAULT_LAG_HOURS
    return value if value >= 0 else DEFAULT_LAG_HOURS


# --- transport ------------------------------------------------------------------------------------

def _http(method: str, url: str, *, params=None, json_body=None, timeout=HTTP_TIMEOUT):
    """The ONE outbound call site, so a test can replace it wholesale and a reader can see that the
    worker talks to exactly one service. Returns `(status, body)`; raises on a transport failure."""
    if method == "GET":
        response = requests.get(url, params=params, timeout=timeout)
    else:
        response = requests.post(url, json=json_body, timeout=timeout)
    try:
        body = response.json()
    except ValueError:
        body = {}
    return response.status_code, body


def _get_status(path: str, params: dict) -> tuple[int, dict]:
    """A GET that reports its status. Only a TRANSPORT failure raises — a 404 is an answer."""
    try:
        status, body = _http("GET", f"{events_url()}{path}", params=params)
    except (requests.RequestException, OSError) as exc:
        raise _EventsUnreachable(f"GET {path}: {type(exc).__name__}: {exc}") from exc
    return status, (body if isinstance(body, dict) else {})


def _get(path: str, params: dict) -> dict:
    status, body = _get_status(path, params)
    if status != 200:
        raise _EventsUnreachable(f"GET {path}: HTTP {status}")
    return body


# --- the queue --------------------------------------------------------------------------------------

def _fetch_queue(*, queue_as_of: str, page: int, cap: int) -> list[dict]:
    """§9.9's `enrichmentState=raw`, paged until `cap` candidates.

    `queue_as_of` is NOW — a live read — and §9.12 rule 1's predicate is then applied per event by
    `_eligible`. MEASURED, not chosen: a global `as_of = now − ENRICH_LAG` looks like the same rule
    but is not, because §1.1's as-of filter is `published_at <= as_of AND first_seen_at <= as_of`.
    A backlog event published months ago and ingested today has `first_seen_at = today`, so the
    global form hides it for a further ENRICH_LAG hours even though its own cutoff passed long ago.
    Verified against a running `services/events`: seeding one document dated 2026-08-01 and reading
    `GET /events?enrichmentState=raw&as_of=now-24h` returned zero events.

    The per-event form has no such side effect and is what rule 1 actually says.
    """
    collected: list[dict] = []
    cursor = None
    # A bounded `for`, never a `while` (§9.22 and this lane's source-level gate): the number of
    # pages needed to reach `cap` is known before the first request, so an unbounded loop would buy
    # nothing but the possibility of never terminating.
    for _ in range(max(1, -(-cap // max(1, page)))):
        params = {"enrichmentState": "raw", "as_of": queue_as_of, "limit": page}
        if cursor:
            params["cursor"] = cursor
        body = _get("/events", params)
        events = [e for e in (body.get("events") or []) if isinstance(e, dict)]
        collected.extend(events)
        cursor = body.get("nextCursor")
        if not events or not cursor or len(collected) >= cap:
            break
    return collected[:cap]


def _order(events: list[dict]) -> list[dict]:
    """`importance DESC, published_at DESC`.

    Enrich the material first: a free-tier machine will never drain the queue, so the ordering IS
    the policy. `GET /events` cannot express it (§3.11 orders by `published_at DESC` only), so it is
    applied here over the fetched window — see the module docstring for what that costs.
    """
    return sorted(
        events,
        key=lambda e: (float(e.get("importance") or 0.0), str(e.get("publishedAt") or ""),
                       str(e.get("id") or "")),
        reverse=True,
    )


def _eligible(event: dict, *, lag: float, now_iso: str) -> bool:
    """§9.12 rule 1, per event: an event is eligible only once its OWN cutoff has arrived.

    Enriching an event whose cutoff is still in the future would stamp an `enrichmentAsOf` the
    enrichment had not actually reached, and `services/events` would then serve those hypotheses at
    a cutoff at which they did not exist.
    """
    published = event.get("publishedAt")
    if not published:
        return False
    try:
        return event_agent.cutoff_for(published, lag) <= now_iso
    except (TypeError, ValueError):
        return False


def _event_at_cutoff(event: dict, as_of: str) -> tuple[dict, bool]:
    """The event AS IT WAS at its own cutoff, if the store can serve it (§9.13).

    §9.13: `as_of` controls row visibility, never field values, so the LIVE row carries the
    `importance`, `documentCount`, `sourceTier` and `officialConfirmed` it has *now* — after
    documents that joined the cluster AFTER the cutoff. Re-reading at the cutoff projects
    `event_state_history` instead, so the model is shown the state that actually existed then.

    A 404 is a real and correct answer, not an error: §1.1's as-of filter is
    `published_at <= as_of AND first_seen_at <= as_of`, so an event first seen AFTER its own cutoff
    genuinely did not exist at that cutoff. Measured against a running service: a document dated
    2026-08-01 ingested today 404s at `as_of = 2026-08-02T13:00:00Z`. Falling back to the live row
    is the only alternative to never enriching backlog at all, and the fallback is COUNTED in the
    report so the ablation can tell the two populations apart.
    """
    status, body = _get_status(f"/events/{event.get('id')}", {"as_of": as_of})
    if status == 404:
        return event, False
    if status != 200:
        raise _EventsUnreachable(f"GET /events/{event.get('id')}: HTTP {status}")
    projected = body.get("event")
    return (projected, True) if isinstance(projected, dict) else (event, False)


def _fetch_prior(event: dict, as_of: str) -> list[dict]:
    """The ticker's prior events AS KNOWN AT THE CUTOFF (§9.12), for the novelty judgement.

    `as_of` is the event's own cutoff, never "now" and never "the last 30 days from now". The event
    under enrichment is removed from its own prior list.
    """
    tickers = sorted({str(t.get("ticker", "")).strip().upper()
                      for t in (event.get("tickers") or []) if t.get("ticker")})
    if not tickers:
        return []
    body = _get("/events", {
        "tickers": ",".join(tickers),
        "as_of": as_of,
        "limit": event_agent.PRIOR_EVENT_LIMIT + 1,
    })
    prior = [e for e in (body.get("events") or [])
             if isinstance(e, dict) and e.get("id") != event.get("id")]
    return prior[:event_agent.PRIOR_EVENT_LIMIT]


def _post_enrichment(event_id: str, payload: dict) -> tuple[int, dict]:
    try:
        return _http("POST", f"{events_url()}/events/{event_id}/enrichment", json_body=payload)
    except (requests.RequestException, OSError) as exc:
        raise _EventsUnreachable(f"POST /events/{event_id}/enrichment: "
                                 f"{type(exc).__name__}: {exc}") from exc


# --- the pass ---------------------------------------------------------------------------------------

def _report(**kw) -> dict:
    base = {
        "enabled": False, "candidates": 0, "generated": 0, "posted": 0,
        "rejected": 0, "failed": 0, "skipped": 0, "liveFallback": 0, "stopped": None,
        "queueAsOf": None, "failures": [],
    }
    base.update(kw)
    return base


def run_once(*, now=None) -> dict:
    """ONE bounded pass. Returns a report; raises nothing a caller must handle.

    `now` is a parameter so a test can pin the wall clock; production passes nothing.
    """
    if not enabled():
        # The flag is checked FIRST, before the queue is even read. A disabled worker makes no
        # outbound call of any kind — not to the model, not to the events service.
        return _report(stopped=DISABLED)

    now_iso = event_agent.iso(now or datetime.now(timezone.utc))
    lag = lag_hours()
    report = _report(enabled=True, queueAsOf=now_iso)

    try:
        fetched = _fetch_queue(queue_as_of=now_iso, page=batch_size(), cap=max_per_run())
        candidates = _order([e for e in fetched if _eligible(e, lag=lag, now_iso=now_iso)])
    except _EventsUnreachable as exc:
        log.warning("enrichment queue unreadable, stopping: %s", exc)
        report["stopped"] = EVENTS_UNREACHABLE
        report["failures"].append({"eventId": None, "reason": EVENTS_UNREACHABLE,
                                   "detail": str(exc)})
        return report

    report["candidates"] = len(candidates)

    for event in candidates:
        event_id = str(event.get("id") or "")
        as_of = event_agent.cutoff_for(event.get("publishedAt"), lag)

        try:
            subject, projected = _event_at_cutoff(event, as_of)
            if not projected:
                report["liveFallback"] += 1
            prior = _fetch_prior(subject, as_of)
        except _EventsUnreachable as exc:
            log.warning("prior-events read failed, stopping: %s", exc)
            report["stopped"] = EVENTS_UNREACHABLE
            report["failures"].append({"eventId": event_id, "reason": EVENTS_UNREACHABLE,
                                       "detail": str(exc)})
            break

        try:
            with lease.acquire_background() as held:
                # "Never start a new one", not "kill the current one": a generation already in
                # flight against llm.py's 120 s timeout cannot be aborted safely from here.
                held.check()
                result = event_agent.enrich_event(
                    subject, prior, as_of=as_of, now=now_iso, call=llm.call_model_meta,
                )
        except lease.LeaseUnavailable as exc:
            reason = PREEMPTED if lease.interactive_active() else LEASE_UNAVAILABLE
            log.info("stopping the batch (%s): %s", reason, exc)
            report["stopped"] = reason
            break
        # The lease is released HERE, between items — not once at the end — so an interactive
        # acquirer waits for at most one generation.

        report["generated"] += 1

        if result.state == event_agent.SKIPPED:
            report["skipped"] += 1
            log.info("event %s skipped: %s", event_id, result.skip_reason)
        elif result.state == event_agent.FAILED or result.payload is None:
            report["failed"] += 1
            report["failures"].append({
                "eventId": event_id, "reason": "enrichment_failed",
                "detail": "; ".join(result.failures) or "no payload",
                "modelUsed": result.model_used, "asOf": as_of,
            })
            log.warning("event %s not enriched (%s): %s",
                        event_id, result.model_used, "; ".join(result.failures))
        else:
            try:
                status, body = _post_enrichment(event_id, result.payload)
            except _EventsUnreachable as exc:
                report["stopped"] = EVENTS_UNREACHABLE
                report["failures"].append({"eventId": event_id, "reason": EVENTS_UNREACHABLE,
                                           "detail": str(exc)})
                break
            if status == 200:
                report["posted"] += 1
            elif status == 400:
                # A rejected payload names the offending field. Logged and left; NOT retried in a
                # loop — the endpoint refused it for a reason that a second identical POST cannot
                # change.
                detail = str((body or {}).get("detail") or f"HTTP {status}")
                report["rejected"] += 1
                report["failures"].append({
                    "eventId": event_id, "reason": "rejected", "detail": detail,
                    "modelUsed": result.model_used, "asOf": as_of,
                })
                log.warning("event %s enrichment rejected: %s", event_id, detail)
            else:
                # 5xx or anything unexpected: the service is unhealthy. Stop.
                report["stopped"] = EVENTS_UNREACHABLE
                report["failures"].append({
                    "eventId": event_id, "reason": EVENTS_UNREACHABLE,
                    "detail": str((body or {}).get("detail") or f"HTTP {status}"),
                })
                log.warning("event %s enrichment failed with HTTP %s, stopping", event_id, status)
                break

        if lease.interactive_active():
            report["stopped"] = PREEMPTED
            log.info("interactive work arrived; stopping the batch after %s events",
                     report["generated"])
            break

    if report["stopped"] is None:
        report["stopped"] = DRAINED
    return report


def main() -> int:
    """The entry point `make enrich-once` runs. One pass, a JSON report on stdout, exit."""
    logging.basicConfig(level=os.getenv("LOG_LEVEL", "INFO").upper())
    report = run_once()
    print(json.dumps(report, indent=2, sort_keys=True))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
