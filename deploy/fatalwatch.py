#!/usr/bin/env python3
"""Bring the container down when supervisord gives up on a program.

WHY. supervisord's job is to keep programs alive; when it cannot, it marks the program FATAL and
moves on. The container stays up, because supervisord and nginx are still running — which is exactly
how this image reported a successful deploy on 2026-08-22 while the events service was dead. From
outside, a pod missing one internal service is indistinguishable from a healthy one.

This listener closes that gap. FATAL means supervisord already burned the program's `startretries`
(10, per supervisord.conf), so by the time we act the process has genuinely failed to start ten
times — this is not a hair trigger on a transient crash. Killing supervisord makes the container
exit non-zero, which is the one signal the orchestrator cannot ignore: the pod restarts on a clean
image instead of serving from a half-dead one.

Experiment-critical storage is the deliberate exception to ordinary dependency degradation. When
DATABASE_URL is configured, paper, journal and auth refuse to start without it: continuing on local
container files would create a second, disposable source of truth. This listener can therefore trip
for either an image/process defect or a persistent PostgreSQL outage; in both cases serving a
half-started application would be less honest than restarting it.

Protocol: supervisord speaks a line-based protocol on stdin/stdout. We must answer every event with
`RESULT 2\nOK` or supervisord blocks waiting for us — so the acknowledgement is written BEFORE the
shutdown, and stdout is flushed, or the last thing the log shows would be an unanswered event.
"""
from __future__ import annotations

import os
import signal
import sys

# Read by entrypoint.sh after supervisord exits, to turn supervisord's graceful 0 into a non-zero
# container exit. Kept in /run so it never survives a restart.
TRIP_FILE = os.getenv("FATALWATCH_TRIP_FILE", "/run/fatalwatch.trip")


def write(line: str) -> None:
    sys.stdout.write(line)
    sys.stdout.flush()


def log(message: str) -> None:
    # stderr is redirected to the container log by supervisord's own stderr_logfile setting.
    sys.stderr.write(f"nvda-platform: fatalwatch: {message}\n")
    sys.stderr.flush()


def parse(header: str) -> dict[str, str]:
    out: dict[str, str] = {}
    for token in header.split():
        key, _, value = token.partition(":")
        out[key] = value
    return out


def main() -> None:
    # supervisord sets SUPERVISOR_ENABLED for its event listeners; PPID is supervisord itself.
    supervisord_pid = os.getppid()
    log(f"watching for FATAL programs (supervisord pid {supervisord_pid})")

    while True:
        write("READY\n")

        header = sys.stdin.readline()
        if not header:
            return  # supervisord closed the pipe: it is shutting down anyway
        meta = parse(header.strip())

        payload_len = int(meta.get("len", 0))
        payload = sys.stdin.read(payload_len) if payload_len else ""
        event = parse(payload)

        # Acknowledge FIRST. An unacknowledged event stalls supervisord's event loop, and a listener
        # that exits mid-shutdown without answering leaves that stall in the logs as the last word.
        write("RESULT 2\nOK")

        if meta.get("eventname") != "PROCESS_STATE_FATAL":
            continue

        program = event.get("processname", "unknown")
        log(
            f"program '{program}' entered FATAL — supervisord exhausted its start retries. "
            f"Taking the container down so the platform restarts it rather than serving from a "
            f"half-started image."
        )

        # supervisord handles SIGTERM as a graceful shutdown and exits 0, which would present this
        # failure to the platform as a clean stop. The trip file is how entrypoint.sh knows to turn
        # that 0 into a non-zero container exit.
        try:
            with open(TRIP_FILE, "w", encoding="utf-8") as handle:
                handle.write(f"program '{program}' entered FATAL\n")
        except OSError as exc:
            log(f"could not write {TRIP_FILE} ({exc}) — the exit code may read as clean")

        try:
            os.kill(supervisord_pid, signal.SIGTERM)
        except OSError as exc:
            log(f"could not signal supervisord ({exc}); exiting non-zero instead")
            sys.exit(1)
        return


if __name__ == "__main__":
    main()
