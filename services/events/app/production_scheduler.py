"""Production clock for the events service's model-free automation lanes.

This process is intentionally narrower than ``make automate-once``. It can dispatch only the
events-owned ``ingest``, ``reactions``, ``scout-intake``, ``scout`` and
``opportunity-radar`` lanes. All five are
model-free. The model-owning worker lanes are absent from its imports and call path, preserving
D-25's rule that no timer may cause a model call.

The clock is off by default. When enabled, it performs one bounded dispatch pass, waits, and checks
again. PostgreSQL lane intervals and leases remain authoritative, so frequent checks neither rerun
a not-due lane nor allow two replicas to enter the same lane.
"""
from __future__ import annotations

import json
import signal
from threading import Event

from .automation import (
    EVENTS_OWNED,
    dispatch,
    production_scheduler_enabled,
    production_scheduler_poll_seconds,
)

TRIGGER = "production-scheduler"


def run_forever(*, stop_event: Event | None = None, emit=print) -> int:
    """Run the model-free production clock until stopped; return immediately when disabled."""
    if not production_scheduler_enabled():
        emit(json.dumps({
            "productionScheduler": False,
            "reason": "disabled",
            "ownedLanes": list(EVENTS_OWNED),
        }, sort_keys=True))
        return 0

    stop = stop_event or Event()
    poll_seconds = production_scheduler_poll_seconds()
    while not stop.is_set():
        try:
            report = dispatch(lanes=EVENTS_OWNED, trigger=TRIGGER)
        except Exception as exc:  # noqa: BLE001 — a transient store outage must not kill the clock
            report = {
                "enabled": True,
                "lanes": [],
                "schedulerError": f"{type(exc).__name__}: {exc}"[:500],
            }
        emit(json.dumps({
            "productionScheduler": True,
            "pollSeconds": poll_seconds,
            **report,
        }, sort_keys=True))
        stop.wait(poll_seconds)
    return 0


def main() -> int:
    stop = Event()

    def request_stop(_signum, _frame) -> None:
        stop.set()

    signal.signal(signal.SIGTERM, request_stop)
    signal.signal(signal.SIGINT, request_stop)
    return run_forever(stop_event=stop, emit=lambda line: print(line, flush=True))


if __name__ == "__main__":
    raise SystemExit(main())
