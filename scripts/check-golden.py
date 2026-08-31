#!/usr/bin/env python3
"""Diff a RUNNING service's response against the committed shape goldens — `DOCKER_GATE.md` 2.5.

The pytest in `services/analysis/tests/test_golden.py` pins exact values, but it runs against a
seeded store inside the test process. This script is the other half: it points at a real container
and answers "does what this thing actually serves still have the shape that shipped?".

It compares SHAPE, not values, and that is deliberate rather than a weakening. A live container
returns real market data; its numbers change every session by design. Shape still catches a renamed,
removed, added or retyped field, which is the whole of what check 2.5 asks.

    python3 scripts/check-golden.py                                   # localhost:8001
    python3 scripts/check-golden.py --base http://localhost:8001
    python3 scripts/check-golden.py --file /tmp/analysis-after.json --golden analysis_NVDA_1D.shape.json

Exit 0 = match. Exit 1 = drift, with every differing path printed. Exit 2 = could not reach the
service, which is a FAILED check and never a pass — a gate that cannot observe the thing it checks
reports nothing, not success (contract §9.44).

Standard library only: it has to run wherever the gate runs.
"""
from __future__ import annotations

import argparse
import json
import sys
import urllib.error
import urllib.request
from pathlib import Path

GOLDEN_DIR = Path(__file__).resolve().parent.parent / "services" / "analysis" / "tests" / "golden"
sys.path.insert(0, str(GOLDEN_DIR))
from normalize import shape, shape_accepts  # noqa: E402 — one implementation, shared with the pytest

#: endpoint -> golden file. Both are live-path requests, exactly what the gate curls.
ENDPOINTS = {
    "/health": "health.shape.json",
    "/analysis/NVDA": "analysis_NVDA_1D.shape.json",
}


def fetch(url: str) -> object:
    with urllib.request.urlopen(url, timeout=20) as r:
        if r.status != 200:
            raise RuntimeError(f"HTTP {r.status}")
        return json.loads(r.read().decode())


def check(label: str, payload: object, golden_name: str) -> bool:
    golden = json.loads((GOLDEN_DIR / golden_name).read_text())
    problems = shape_accepts(golden, shape(payload))
    if problems:
        print(f"FAIL  {label}  vs {golden_name}")
        for p in problems:
            print(f"        {p}")
        return False
    print(f"ok    {label}  vs {golden_name}")
    return True


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--base", default="http://localhost:8001", help="analysis service base URL")
    ap.add_argument("--file", help="a saved JSON response instead of a live call")
    ap.add_argument("--golden", help="golden filename, required with --file")
    args = ap.parse_args()

    if args.file:
        if not args.golden:
            print("--file requires --golden", file=sys.stderr)
            return 2
        payload = json.loads(Path(args.file).read_text())
        return 0 if check(args.file, payload, args.golden) else 1

    ok = True
    for path, golden_name in ENDPOINTS.items():
        url = args.base.rstrip("/") + path
        try:
            payload = fetch(url)
        except (urllib.error.URLError, OSError, RuntimeError, ValueError) as e:
            # Unreachable is a failed check, not a skipped one.
            print(f"FAIL  {url}  unreachable: {e}")
            return 2
        ok = check(url, payload, golden_name) and ok
    return 0 if ok else 1


if __name__ == "__main__":
    raise SystemExit(main())
