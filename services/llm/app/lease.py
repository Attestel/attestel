"""The D-25 model lease: one model at a time, across processes, with background always yielding.

The machine has a 16 GB budget and runs one model at a time (`PROJECT_PLAN.md` §6); two concurrent
generations against a single local 14B either exhaust memory or serialise anyway while destroying
interactive latency. This file is therefore not over-engineering — it is the only thing that makes
D-25's permission (a background enrichment worker) safe to grant at all.

WHY `fcntl.flock` AND NOT `threading.Lock` (contract §9.21)
-----------------------------------------------------------
The enrichment worker is a SEPARATE OS PROCESS (`make enrich-once` runs it as its own `python -m`
invocation) from the `uvicorn` server that answers interactive requests. A `threading`
lock inside the worker's own address space gives **zero** exclusion against that server, so D-25's
permission would be inert and the one-model-at-a-time budget unenforced. The lock must be an
OS-level one on a file both processes can see: `{READS_DIR}/.model.lock`, on the shared `reads:`
volume. For the same reason `services/llm` runs `uvicorn --workers 1` — a multi-worker container
multiplies every in-process guarantee by the worker count.

THE TWO ACQUIRERS
-----------------
* `acquire_interactive()` — a user is waiting. Announces its intent BEFORE blocking on the lock,
  writes a heartbeat while it holds it, and blocks for at most one in-flight background generation.
  Nothing in Wave 2 calls it; it is exported so Wave 3's `analyst.py` and any future interactive
  path can take it (§9.21 requires exactly that).
* `acquire_background()` — the enrichment worker, and only it. **Refuses immediately** — it does
  not queue — if an interactive holder is active or waiting, or if an interactive release is more
  recent than `ENRICH_IDLE_SECONDS`. Background work comes back later; interactive latency, not
  background throughput, is the thing being protected.

PREEMPTION IS "NEVER START A NEW ONE", NOT "KILL THE CURRENT ONE"
-----------------------------------------------------------------
`BackgroundLease.preempted` flips the moment an interactive acquirer announces itself. The worker
checks it between items and before every generation and stops the batch when it is set. It does
**not** abort a generation already in flight: an HTTP call running against `llm.py`'s 120 s timeout
cannot be cancelled safely from here — `requests` has no cancellation, and tearing the socket down
mid-stream would leave the server generating tokens nobody reads while the audit trail records a
call that "did not happen". A future reader who wants real cancellation needs a streaming transport
and a cooperative abort flag inside `llm.py`, which is a change to a file this lane may not touch.

The marker file is politeness, not safety: mutual exclusion is guaranteed by the `flock` alone, so
a missing, truncated or corrupt marker degrades to "background may run", never to a double
generation and never to a deadlock.
"""
from __future__ import annotations

import errno
import fcntl
import json
import os
import threading
import time
from contextlib import contextmanager
from typing import Iterator

__all__ = [
    "LeaseUnavailable", "Lease", "BackgroundLease",
    "acquire_interactive", "acquire_background",
    "lock_path", "marker_path", "interactive_active", "interactive_cooldown_remaining",
    "announce_interactive_waiting", "clear_marker",
    "background_lease_is_held", "interactive_lease_is_held",
    "HEARTBEAT_INTERVAL_SECONDS", "HEARTBEAT_STALE_SECONDS", "DEFAULT_IDLE_SECONDS",
]

LOCK_FILENAME = ".model.lock"
MARKER_FILENAME = ".model.lock.interactive"

#: How often the interactive holder refreshes its heartbeat while it holds the lease. This is a
#: LOCK heartbeat, not a scheduler: it causes no model call, and invariant #4 is about model calls.
HEARTBEAT_INTERVAL_SECONDS = 5.0
#: A heartbeat older than this is treated as a dead holder (crashed process, SIGKILL). Comfortably
#: more than three intervals, comfortably less than `llm.py`'s 120 s call timeout is irrelevant
#: here — a live holder refreshes every 5 s regardless of how long its generation runs.
HEARTBEAT_STALE_SECONDS = 30.0
#: After the last interactive RELEASE, background work waits this long before acquiring again.
DEFAULT_IDLE_SECONDS = 30.0

WAITING = "waiting"
HELD = "held"
RELEASED = "released"


class LeaseUnavailable(RuntimeError):
    """Raised by `acquire_background()` when it must come back later. Never raised for interactive
    work: an interactive acquirer waits, it does not get turned away."""


# --- paths, read at call time so a test or a late-configured container is honoured ---------------

def _reads_dir() -> str:
    return os.getenv("READS_DIR", os.path.join(os.getcwd(), "data", "reads"))


def lock_path() -> str:
    return os.path.join(_reads_dir(), LOCK_FILENAME)


def marker_path() -> str:
    return os.path.join(_reads_dir(), MARKER_FILENAME)


def idle_seconds() -> float:
    raw = (os.getenv("ENRICH_IDLE_SECONDS") or "").strip()
    try:
        return max(0.0, float(raw)) if raw else DEFAULT_IDLE_SECONDS
    except ValueError:
        return DEFAULT_IDLE_SECONDS


def _ensure_dir() -> None:
    os.makedirs(_reads_dir(), exist_ok=True)


# --- the interactive marker ----------------------------------------------------------------------
# One small JSON file, written atomically (temp + os.replace) so a reader never sees a torn write.
# Contents: {"state": waiting|held|released, "pid": int, "heartbeatAt": float, "releasedAt": float}


def _read_marker() -> dict:
    try:
        with open(marker_path(), "r", encoding="utf-8") as fh:
            data = json.load(fh)
        return data if isinstance(data, dict) else {}
    except (OSError, ValueError):
        # Missing or corrupt: no evidence of interactive activity. Fail OPEN for background, which
        # is safe because the flock — not this file — is what enforces exclusion.
        return {}


def _write_marker(state: str, *, released_at: float | None = None) -> None:
    _ensure_dir()
    payload = {
        "state": state,
        "pid": os.getpid(),
        "heartbeatAt": time.time(),
        "releasedAt": released_at,
    }
    tmp = f"{marker_path()}.{os.getpid()}.tmp"
    try:
        with open(tmp, "w", encoding="utf-8") as fh:
            json.dump(payload, fh)
        os.replace(tmp, marker_path())
    except OSError:
        try:
            os.unlink(tmp)
        except OSError:
            pass


def clear_marker() -> None:
    """Remove the marker entirely. Used by tests; never by production code, which transitions the
    marker to `released` so the idle delay can be honoured."""
    try:
        os.unlink(marker_path())
    except OSError:
        pass


def announce_interactive_waiting() -> None:
    """What `acquire_interactive()` does FIRST, before it blocks on the lock.

    Exported because it is the observable half of preemption: a background holder sees the marker
    go `waiting` and stops at its next item boundary, rather than the interactive request
    discovering the lock is busy and waiting silently for a whole generation.
    """
    _write_marker(WAITING)


def interactive_active() -> bool:
    """True when an interactive acquirer is waiting for, or holding, the lease right now."""
    marker = _read_marker()
    if marker.get("state") not in (WAITING, HELD):
        return False
    beat = marker.get("heartbeatAt")
    if not isinstance(beat, (int, float)):
        return False
    return (time.time() - float(beat)) <= HEARTBEAT_STALE_SECONDS


def interactive_cooldown_remaining() -> float:
    """Seconds of `ENRICH_IDLE_SECONDS` still owed after the last interactive release.

    Background work does not sleep this off — `run_once()` is a single bounded pass (§9.22) — it
    declines the run and the operator's next invocation finds the cooldown expired.
    """
    marker = _read_marker()
    if marker.get("state") != RELEASED:
        return 0.0
    released = marker.get("releasedAt")
    if not isinstance(released, (int, float)):
        return 0.0
    remaining = idle_seconds() - (time.time() - float(released))
    return max(0.0, remaining)


# --- the lease objects ---------------------------------------------------------------------------

# Whether THIS process currently holds each kind of lease. Used by the invariant-#4 tests to assert
# that every generation happened inside a held lease — a positive measurement, not a log grep
# (contract §9.44).
_held = threading.local()


def background_lease_is_held() -> bool:
    return bool(getattr(_held, "background", False))


def interactive_lease_is_held() -> bool:
    return bool(getattr(_held, "interactive", False))


class Lease:
    """A held lease. `kind` is `interactive` or `background`."""

    def __init__(self, kind: str, fd: int):
        self.kind = kind
        self._fd = fd
        self.acquired_at = time.time()

    def __repr__(self) -> str:  # pragma: no cover - diagnostics only
        return f"<Lease {self.kind} fd={self._fd}>"


class BackgroundLease(Lease):
    """The background half, with the preemption flag D-25 requires.

    `preempted` is a property, not a cached bool: the interactive acquirer runs in a DIFFERENT
    PROCESS, so the only way to observe it is to look at the marker file at the moment of the
    check. The worker checks it between items and before every generation.
    """

    def __init__(self, fd: int):
        super().__init__("background", fd)

    @property
    def preempted(self) -> bool:
        return interactive_active()

    def check(self) -> None:
        """Raise `LeaseUnavailable` if an interactive acquirer has arrived. The worker calls this
        immediately before starting a generation — 'never start a new one' is the rule."""
        if self.preempted:
            raise LeaseUnavailable("an interactive acquirer arrived; the batch stops here")


def _open_lock() -> int:
    _ensure_dir()
    return os.open(lock_path(), os.O_RDWR | os.O_CREAT, 0o644)


@contextmanager
def acquire_interactive(*, timeout: float | None = None) -> Iterator[Lease]:
    """Take the model lease on behalf of a user who is waiting.

    Announces intent first (so a background holder stops at its next item boundary), then blocks on
    the `flock`. The wait is therefore bounded by ONE in-flight background generation, never by a
    whole batch. `timeout` is a safety valve for a pathological holder; `None` blocks indefinitely,
    which is correct for a request already inside a 120 s model timeout.

    Wave 3's `analyst.py` wraps every generation in this (contract §9.21).
    """
    announce_interactive_waiting()
    fd = _open_lock()
    stop = threading.Event()
    beat: threading.Thread | None = None
    try:
        _flock_blocking(fd, timeout)
        _write_marker(HELD)
        os.ftruncate(fd, 0)
        os.write(fd, json.dumps({"pid": os.getpid(), "kind": "interactive",
                                 "at": time.time()}).encode("utf-8"))

        def _heartbeat() -> None:
            # A lock heartbeat, refreshed while held. It makes no model call, so invariant #4 —
            # which is about model calls — is untouched. `Event.wait` is the sleep primitive on
            # purpose: it wakes immediately on release instead of lingering for a full interval.
            while not stop.wait(HEARTBEAT_INTERVAL_SECONDS):
                _write_marker(HELD)

        beat = threading.Thread(target=_heartbeat, name="model-lease-heartbeat", daemon=True)
        beat.start()
        _held.interactive = True
        yield Lease("interactive", fd)
    finally:
        _held.interactive = False
        stop.set()
        if beat is not None:
            beat.join(timeout=1.0)
        _write_marker(RELEASED, released_at=time.time())
        _unlock(fd)


@contextmanager
def acquire_background() -> Iterator[BackgroundLease]:
    """Take the model lease on behalf of the enrichment worker.

    Refuses immediately — `LeaseUnavailable` — when an interactive acquirer is active or waiting,
    when the post-release cooldown has not elapsed, or when the lock is simply busy. It never
    queues: queueing behind interactive work is how a background job ends up holding the model the
    instant a user stops typing.
    """
    if interactive_active():
        raise LeaseUnavailable("an interactive acquirer holds or is waiting for the model lease")
    cooldown = interactive_cooldown_remaining()
    if cooldown > 0:
        raise LeaseUnavailable(
            f"interactive work released the lease recently; {cooldown:.1f}s of "
            f"ENRICH_IDLE_SECONDS remain"
        )

    fd = _open_lock()
    try:
        fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except OSError as exc:
        os.close(fd)
        if exc.errno not in (errno.EACCES, errno.EAGAIN):
            raise
        raise LeaseUnavailable("the model lease is held by another process") from None

    try:
        # Re-check AFTER taking the lock: an interactive acquirer may have announced itself in the
        # window between the first check and the flock. It is waiting on the lock right now, so the
        # honest answer is to hand it straight back.
        if interactive_active():
            raise LeaseUnavailable("an interactive acquirer arrived while the lock was being taken")
        os.ftruncate(fd, 0)
        os.write(fd, json.dumps({"pid": os.getpid(), "kind": "background",
                                 "at": time.time()}).encode("utf-8"))
        _held.background = True
        yield BackgroundLease(fd)
    finally:
        _held.background = False
        _unlock(fd)


def _flock_blocking(fd: int, timeout: float | None) -> None:
    if timeout is None:
        fcntl.flock(fd, fcntl.LOCK_EX)
        return
    deadline = time.monotonic() + timeout
    while True:
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            return
        except OSError as exc:
            if exc.errno not in (errno.EACCES, errno.EAGAIN):
                raise
            if time.monotonic() >= deadline:
                raise TimeoutError(
                    f"the model lease was not free within {timeout}s"
                ) from None
            time.sleep(0.05)


def _unlock(fd: int) -> None:
    try:
        fcntl.flock(fd, fcntl.LOCK_UN)
    finally:
        try:
            os.close(fd)
        except OSError:
            pass
