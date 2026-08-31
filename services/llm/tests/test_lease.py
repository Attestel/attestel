"""Wave 2 Lane 2B — the D-25 model lease (contract §9.21).

The load-bearing test in this file is `test_the_flock_excludes_a_real_second_os_process`. Everything
else could be satisfied by a `threading.Lock`, and a `threading.Lock` gives ZERO exclusion between
the `uvicorn` server and `python -m app.enrich_worker` — which is the entire situation the lease
exists for. So one test spawns a genuine second interpreter.

Deterministic and fast: the only real sleeps are the sub-second synchronisation waits the
second-process tests need.

Run:  cd services/llm && python -m pytest -q tests/test_lease.py
"""
from __future__ import annotations

import json
import os
import subprocess
import sys
import textwrap
import time

import pytest

from app import lease


@pytest.fixture(autouse=True)
def reads_dir(tmp_path, monkeypatch):
    monkeypatch.setenv("READS_DIR", str(tmp_path))
    monkeypatch.delenv("ENRICH_IDLE_SECONDS", raising=False)
    lease.clear_marker()
    yield tmp_path
    lease.clear_marker()


def _wait_for(predicate, timeout=10.0, interval=0.02):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            return True
        time.sleep(interval)
    return False


# =================================================================================================
# The cross-process proof
# =================================================================================================

CHILD = textwrap.dedent(
    """
    import os, sys, time
    sys.path.insert(0, {app_root!r})
    os.environ["READS_DIR"] = {reads!r}
    os.environ["ENRICH_IDLE_SECONDS"] = "0"
    from app import lease

    kind, ready_file, release_file = sys.argv[1], sys.argv[2], sys.argv[3]
    acquire = lease.acquire_interactive if kind == "interactive" else lease.acquire_background
    with acquire() as held:
        open(ready_file, "w").write(str(os.getpid()))
        deadline = time.monotonic() + 30
        while not os.path.exists(release_file) and time.monotonic() < deadline:
            time.sleep(0.02)
    """
)


def _spawn(kind, tmp_path, name):
    app_root = str(os.path.dirname(os.path.dirname(os.path.abspath(lease.__file__))))
    ready = tmp_path / f"{name}.ready"
    release = tmp_path / f"{name}.release"
    proc = subprocess.Popen(
        [sys.executable, "-c",
         CHILD.format(app_root=app_root, reads=str(tmp_path)), kind, str(ready), str(release)],
        stdout=subprocess.PIPE, stderr=subprocess.PIPE,
    )
    if not _wait_for(ready.exists):
        proc.kill()
        out, err = proc.communicate(timeout=5)
        raise AssertionError(f"child never acquired the lease: {err.decode()[-2000:]}")
    return proc, release


def _reap(proc, release):
    release.write_text("go")
    try:
        proc.wait(timeout=10)
    except subprocess.TimeoutExpired:  # pragma: no cover
        proc.kill()
        proc.wait(timeout=5)
    assert proc.returncode == 0, proc.stderr.read().decode()[-2000:]


def test_the_flock_excludes_a_real_second_os_process(tmp_path):
    """A separate interpreter holds the BACKGROUND lease; this process cannot take it.

    §9.21: a `threading` lease inside the worker's address space gives zero exclusion against the
    uvicorn server process, so D-25's permission would be inert. This is the test that would fail
    if someone "simplified" `lease.py` to a `threading.Lock`.
    """
    proc, release = _spawn("background", tmp_path, "bg")
    try:
        with pytest.raises(lease.LeaseUnavailable):
            with lease.acquire_background():
                pass  # pragma: no cover
    finally:
        _reap(proc, release)

    # and once the child exits, the lease is free again
    with lease.acquire_background() as held:
        assert held.kind == "background"


def test_background_refuses_while_a_second_process_holds_the_interactive_lease(tmp_path):
    proc, release = _spawn("interactive", tmp_path, "ia")
    try:
        assert lease.interactive_active() is True
        with pytest.raises(lease.LeaseUnavailable) as exc:
            with lease.acquire_background():
                pass  # pragma: no cover
        assert "interactive" in str(exc.value)
    finally:
        _reap(proc, release)


def test_the_interactive_holder_writes_a_heartbeat_that_a_second_process_can_read(tmp_path):
    """§9.21's "with the interactive holder writing a heartbeat". Observed from THIS process while
    a different one holds the lease — the heartbeat has to cross the process boundary to be worth
    anything."""
    proc, release = _spawn("interactive", tmp_path, "hb")
    try:
        marker = json.loads((tmp_path / lease.MARKER_FILENAME).read_text())
        assert marker["state"] == lease.HELD
        assert marker["pid"] == proc.pid
        first = marker["heartbeatAt"]
        assert _wait_for(
            lambda: json.loads((tmp_path / lease.MARKER_FILENAME).read_text())["heartbeatAt"] > first,
            timeout=HEARTBEAT_WAIT,
        ), "the heartbeat never advanced"
    finally:
        _reap(proc, release)


HEARTBEAT_WAIT = lease.HEARTBEAT_INTERVAL_SECONDS * 3 + 2


def test_interactive_is_never_blocked_by_more_than_one_in_flight_background_generation(tmp_path):
    """The worker releases BETWEEN items, so an interactive acquirer waits for at most one
    generation — not for a whole batch. Modelled here with a background holder that releases when
    it sees the preemption flag, and an interactive acquirer that must then get through."""
    import threading

    got_through = threading.Event()
    started = threading.Event()

    def interactive():
        started.set()
        with lease.acquire_interactive(timeout=10):
            got_through.set()

    with lease.acquire_background() as bg:
        assert bg.preempted is False
        thread = threading.Thread(target=interactive, daemon=True)
        thread.start()
        assert started.wait(5)
        # the interactive acquirer announces itself BEFORE blocking on the lock, so the background
        # holder sees the preemption immediately rather than after its own release.
        assert _wait_for(lambda: bg.preempted, timeout=5), "preemption was never observed"
        with pytest.raises(lease.LeaseUnavailable):
            bg.check()
    assert got_through.wait(10), "interactive work was blocked behind the released background lease"
    thread.join(timeout=5)


# =================================================================================================
# The in-process semantics
# =================================================================================================

def test_background_refuses_immediately_rather_than_queueing():
    lease.announce_interactive_waiting()
    started = time.monotonic()
    with pytest.raises(lease.LeaseUnavailable):
        with lease.acquire_background():
            pass  # pragma: no cover
    assert time.monotonic() - started < 1.0, "acquire_background() queued instead of refusing"


def test_the_idle_delay_after_an_interactive_release_is_honoured(monkeypatch):
    monkeypatch.setenv("ENRICH_IDLE_SECONDS", "3600")
    with lease.acquire_interactive():
        pass
    remaining = lease.interactive_cooldown_remaining()
    assert 3500 < remaining <= 3600
    with pytest.raises(lease.LeaseUnavailable) as exc:
        with lease.acquire_background():
            pass  # pragma: no cover
    assert "ENRICH_IDLE_SECONDS" in str(exc.value)


def test_a_zero_idle_delay_lets_background_straight_back_in(monkeypatch):
    monkeypatch.setenv("ENRICH_IDLE_SECONDS", "0")
    with lease.acquire_interactive():
        pass
    assert lease.interactive_cooldown_remaining() == 0.0
    with lease.acquire_background() as held:
        assert held.kind == "background"


def test_the_idle_default_is_thirty_seconds(monkeypatch):
    monkeypatch.delenv("ENRICH_IDLE_SECONDS", raising=False)
    assert lease.idle_seconds() == 30.0
    monkeypatch.setenv("ENRICH_IDLE_SECONDS", "not-a-number")
    assert lease.idle_seconds() == 30.0


def test_a_stale_heartbeat_does_not_block_background_forever(tmp_path):
    """A holder that was SIGKILLed leaves a `held` marker behind. The OS drops its `flock` on
    process exit, so the lock is free — the marker must not be what keeps background out."""
    marker = tmp_path / lease.MARKER_FILENAME
    marker.write_text(json.dumps({
        "state": lease.HELD, "pid": 999999,
        "heartbeatAt": time.time() - (lease.HEARTBEAT_STALE_SECONDS + 60),
        "releasedAt": None,
    }))
    assert lease.interactive_active() is False
    with lease.acquire_background() as held:
        assert held.kind == "background"


def test_a_corrupt_marker_degrades_to_background_may_run(tmp_path):
    """The marker is politeness; the `flock` is safety. A torn or hand-edited marker must never
    deadlock the worker, and it can never cause a double generation."""
    (tmp_path / lease.MARKER_FILENAME).write_text("{not json")
    assert lease.interactive_active() is False
    assert lease.interactive_cooldown_remaining() == 0.0
    with lease.acquire_background():
        pass


def test_held_flags_are_observable_and_reset(tmp_path):
    assert lease.background_lease_is_held() is False
    assert lease.interactive_lease_is_held() is False
    with lease.acquire_background():
        assert lease.background_lease_is_held() is True
        assert lease.interactive_lease_is_held() is False
    assert lease.background_lease_is_held() is False
    with lease.acquire_interactive():
        assert lease.interactive_lease_is_held() is True
    assert lease.interactive_lease_is_held() is False


def test_the_lock_lives_on_the_shared_reads_volume(tmp_path):
    assert lease.lock_path() == str(tmp_path / ".model.lock")
    with lease.acquire_background():
        pass
    assert (tmp_path / ".model.lock").exists()


def test_the_lock_file_records_who_holds_it(tmp_path):
    with lease.acquire_background():
        payload = json.loads((tmp_path / ".model.lock").read_text())
    assert payload["kind"] == "background"
    assert payload["pid"] == os.getpid()


def test_an_exception_inside_the_lease_still_releases_it():
    with pytest.raises(ValueError):
        with lease.acquire_background():
            raise ValueError("boom")
    assert lease.background_lease_is_held() is False
    with lease.acquire_background():
        pass  # free again


def test_reads_dir_is_read_at_call_time(monkeypatch, tmp_path):
    other = tmp_path / "elsewhere"
    monkeypatch.setenv("READS_DIR", str(other))
    assert lease.lock_path() == str(other / ".model.lock")
