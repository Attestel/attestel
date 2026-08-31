"""Golden payloads — `DOCKER_GATE.md` check 2.5, "nothing that shipped before has changed".

Lane 1B's handoff said "golden diff shows one change: the additive `barStore` block on `/health`".
The comparison was made and nothing was committed, so the gate had nothing to run and recorded the
check unrun. **A verification that cannot be repeated is not a verification.** This file is the
repeatable form of it.

WHY THE GOLDEN IS CAPTURED THROUGH THE POINT-IN-TIME PATH
---------------------------------------------------------
An exact golden needs an exact input. Two things make a live `/analysis/NVDA` unpinnable:

1. real bars change every session; and
2. the offline fallback is not deterministic either — `data._synthetic` seeds its RNG from
   `hash((ticker, timeframe))`, and Python salts `hash()` of a `str` per process. `test_pit.py`
   already documents this. "Deterministic pseudo-random walk" in that docstring is true within one
   process and false across restarts.

So the golden is captured the one way the values are genuinely fixed: a store seeded with
`test_pit.known_bars` (a `default_rng(seed)` series, identical in any process) and a request at a
fixed `as_of`. Every provider is patched to raise, so nothing can reach the network to blur it. The
payload is then EXACT — every indicator, every regime string, every rounded price — and a change to
an indicator formula shows up as a numeric diff rather than a shrug.

What that cannot cover is the live path, whose numbers are real. For that the golden records the
payload's SHAPE (every scalar leaf collapsed to its type), and `scripts/check-golden.py` compares a
running container's real response against it. Shape is the strongest claim available about output
whose values legitimately change; it still catches a renamed, removed, added or retyped field, which
is what check 2.5 is asking about.

RUN
---
    cd services/analysis && python -m pytest -q tests/test_golden.py

REGENERATE (only with a reason, and read the diff before committing it)
-----------------------------------------------------------------------
    cd services/analysis && GOLDEN_REGEN=1 python -m pytest -q tests/test_golden.py

A failure here is a FINDING, not a stale file. Regenerating to make the suite pass is the one thing
this file exists to prevent.
"""
import json
import os
import sys
from datetime import datetime, timedelta, timezone
from pathlib import Path

import pytest
from fastapi.testclient import TestClient

import app.store as store

sys.path.insert(0, str(Path(__file__).parent / "golden"))
from normalize import normalize, shape, shape_accepts  # noqa: E402

from test_pit import known_bars  # noqa: E402  — one deterministic bar generator, not two

GOLDEN_DIR = Path(__file__).parent / "golden"
REGEN = os.getenv("GOLDEN_REGEN") == "1"

#: The captured request. Both halves are part of the golden's identity: change either and the
#: payload legitimately changes, so the README records them next to the capture commit.
AS_OF = "2026-03-02T00:00:00Z"
#: `n` bounds only how many points the response EMITS. Indicators are always computed over
#: `max(n, 260) + INDICATOR_WARMUP` bars, so 60 pins exactly the same numbers a default request
#: would and keeps the committed payload at ~55 KB instead of ~230 KB.
N = 60
SEED_1D = ("NVDA", "1D", 400, "2025-01-01", "B", 11)
SEED_1H = ("NVDA", "1H", 400, "2025-11-03 09:30", "h", 12)


# ------------------------------------------------------------------------------------- harness

@pytest.fixture
def seeded_client(monkeypatch):
    """A private store holding the same bars `test_pit.py` seeds, with the network sealed off."""
    monkeypatch.delenv("INTRADAY_RETENTION_DAYS", raising=False)
    store.close()

    import app.data as data
    import app.expected as expected_mod

    def boom(*a, **k):
        raise AssertionError("the golden capture reached a provider — the payload is not pinned")

    monkeypatch.setattr(data.requests, "get", boom)
    monkeypatch.setattr(data.requests, "post", boom)
    for name in ("_from_alpaca", "_from_twelvedata", "_from_yfinance", "_from_tiingo",
                 "_synthetic", "fetch_ohlcv"):
        monkeypatch.setattr(data, name, boom)
    monkeypatch.setattr(expected_mod, "fetch_ohlcv", boom)

    for ticker, tf, n, start, freq, seed in (SEED_1D, SEED_1H):
        store.write_bars(ticker, tf, known_bars(n, start, freq, seed), "yfinance")

    import app.main as main

    yield TestClient(main.app)
    store.close()


def _load(name):
    path = GOLDEN_DIR / name
    if not path.exists():
        pytest.fail(f"golden {name} is missing — regenerate with GOLDEN_REGEN=1 and read the diff")
    return json.loads(path.read_text())


def _compare(name, actual):
    """Write on regen, diff otherwise. The failure message names the first differing path rather
    than dumping two 300-line payloads."""
    path = GOLDEN_DIR / name
    if REGEN:
        path.write_text(json.dumps(actual, indent=2, sort_keys=True) + "\n")
        return
    expected = _load(name)
    if actual != expected:
        pytest.fail(f"{name} differs from the golden at: {_first_diff(expected, actual)}\n"
                    f"This is a FINDING. Do not regenerate to make it pass.")


def _first_diff(a, b, path="$"):
    if isinstance(a, dict) and isinstance(b, dict):
        for k in sorted(set(a) | set(b)):
            if k not in a:
                return f"{path}.{k} (new in the response)"
            if k not in b:
                return f"{path}.{k} (missing from the response)"
            if a[k] != b[k]:
                return _first_diff(a[k], b[k], f"{path}.{k}")
    if isinstance(a, list) and isinstance(b, list):
        if len(a) != len(b):
            return f"{path}: length {len(a)} -> {len(b)}"
        for i, (x, y) in enumerate(zip(a, b)):
            if x != y:
                return _first_diff(x, y, f"{path}[{i}]")
    return f"{path}: {a!r} -> {b!r}"


# --------------------------------------------------------------------------------- exact values

def test_analysis_nvda_1d_matches_the_golden(seeded_client):
    """`GET /analysis/NVDA?as_of=…` byte for byte. Nothing here is normalised: at a fixed cutoff
    over fixed bars the payload has no volatile field, `price.asOf` included — it is the timestamp
    of the last stored bar at or before the cutoff."""
    r = seeded_client.get(f"/analysis/NVDA?as_of={AS_OF}&n={N}")
    assert r.status_code == 200, r.text
    _compare("analysis_NVDA_1D.json", r.json())


def test_health_matches_the_golden(seeded_client):
    """`GET /health`, with the PostgreSQL target/schema normalised. Row and ticker counts are exact:
    under this seed they are exact, and a store that silently stopped writing must fail this."""
    r = seeded_client.get("/health")
    assert r.status_code == 200, r.text
    _compare("health.json", normalize(r.json(), "health"))


def test_the_capture_is_reproducible(seeded_client):
    """Two identical requests in the same process return identical payloads. Cheap, and it is the
    property the whole file rests on — if this ever fails, the golden is noise."""
    a = seeded_client.get(f"/analysis/NVDA?as_of={AS_OF}&n={N}").json()
    b = seeded_client.get(f"/analysis/NVDA?as_of={AS_OF}&n={N}").json()
    assert a == b


# ---------------------------------------------------------------------------------------- shape

def test_analysis_shape_matches_the_golden(seeded_client):
    """The shape golden `scripts/check-golden.py` points at a running container."""
    got = shape(seeded_client.get(f"/analysis/NVDA?as_of={AS_OF}&n={N}").json())
    if REGEN:
        (GOLDEN_DIR / "analysis_NVDA_1D.shape.json").write_text(
            json.dumps(got, indent=2, sort_keys=True) + "\n")
        return
    problems = shape_accepts(_load("analysis_NVDA_1D.shape.json"), got)
    assert not problems, "shape drift:\n  " + "\n  ".join(problems)


def test_health_shape_matches_the_golden(seeded_client):
    got = shape(seeded_client.get("/health").json())
    if REGEN:
        (GOLDEN_DIR / "health.shape.json").write_text(
            json.dumps(got, indent=2, sort_keys=True) + "\n")
        return
    problems = shape_accepts(_load("health.shape.json"), got)
    assert not problems, "shape drift:\n  " + "\n  ".join(problems)


def test_the_shape_golden_also_describes_the_live_path(seeded_client, monkeypatch):
    """The bridge that makes the shape golden usable against a container.

    The exact golden is captured historically; the gate curls the LIVE endpoint. This proves the two
    have the same shape, so a shape match against a running service means something.

    THIS TEST WAS NOT HERMETIC AND IS NOW. Fixed at Wave 4 integration, where it had been carried
    across three waves as "pre-existing". `monkeypatch.undo()` restores the real `_synthetic` and
    `fetch_ohlcv` — which is what this leg wants — but historically it also reverted the fixture's
    store selection. The request therefore fell through to whatever ambient store the environment
    pointed at, borrowing data outside the test and warming every indicator. In a clean environment
    that history does not exist, the series is short,
    the leading indicator values are `null`, and the shape comes back `float|null` against a golden
    that says `float` — measured, five paths, at Wave 4 integration.

    The fix is to give this leg its OWN store rather than to borrow one: a fresh empty path, seeded
    with the same `known_bars` the fixture uses, so the input is pinned by construction and the
    result cannot depend on what any machine happens to have lying around.
    """
    import app.data as data

    monkeypatch.undo()  # restore the real _synthetic and fetch_ohlcv

    # ...and immediately re-seal everything `undo()` also reverted. Empty this test's isolated
    # PostgreSQL schema so the live leg cannot borrow the historical seed from the fixture.
    monkeypatch.delenv("INTRADAY_RETENTION_DAYS", raising=False)
    store.close()
    conn = store.connect()
    conn.execute("TRUNCATE TABLE bars")
    conn.commit()

    # DEEPER, AND ANCHORED TO TODAY. Two things the fixture's seed does not give this leg:
    #
    #   * DEPTH. A live request (no `as_of`) computes over `max(n, 260) + INDICATOR_WARMUP` = 510
    #     bars and emits 260. `sma200` needs 200 of warmup before the first emitted point, so a
    #     window shallower than 510 leaves leading `sma200` values null and the shape reads
    #     `float|null` against a golden that says `float`.
    #   * A START THAT MOVES. The fixture's series begins at a fixed 2025 date. A live request ends
    #     at TODAY, so the number of seeded bars lying before today shrinks by one every session and
    #     the test would go red on an arbitrary future morning for a reason having nothing to do
    #     with the code. Measured: the same 700 bars pass from a 2024 start and fail from a 2025 one.
    #
    # So the series is anchored relative to now and made comfortably deeper than 510. This is what
    # "hermetic" has to mean for a test whose subject is a window ending at the current date.
    live_bars = 760
    start_1d = (datetime.now(timezone.utc) - timedelta(days=int(live_bars * 7 / 5) + 30)).strftime("%Y-%m-%d")
    store.write_bars("NVDA", "1D", known_bars(live_bars, start_1d, "B", 11), "yfinance")
    start_1h = (datetime.now(timezone.utc) - timedelta(days=90)).strftime("%Y-%m-%d %H:%M")
    store.write_bars("NVDA", "1H", known_bars(live_bars, start_1h, "h", 12), "yfinance")

    monkeypatch.setattr(data, "_from_alpaca", lambda *a, **k: (_ for _ in ()).throw(RuntimeError()))
    monkeypatch.setattr(data, "_from_twelvedata", lambda *a, **k: (_ for _ in ()).throw(RuntimeError()))
    monkeypatch.setattr(data, "_from_yfinance", lambda *a, **k: (_ for _ in ()).throw(RuntimeError()))
    monkeypatch.setattr(data, "_from_tiingo", lambda *a, **k: (_ for _ in ()).throw(RuntimeError()))

    r = seeded_client.get("/analysis/NVDA")
    assert r.status_code == 200, r.text
    body = r.json()
    # No provider can have answered — all four raise — so the response came from the seeded store or
    # from the synthetic fallback, and either is a real run of the live code path.
    assert body["source"] in ("yfinance", "synthetic"), body["source"]

    problems = shape_accepts(_load("analysis_NVDA_1D.shape.json"), shape(body))
    assert not problems, (
        "the live response has a different shape from the historical golden — check 2.5 cannot "
        "compare a container against it:\n  " + "\n  ".join(problems))


# ------------------------------------------------------------- the claim 1B made and never proved

def test_the_only_change_since_before_lane_1b_is_the_barstore_block(seeded_client, monkeypatch):
    """Lane 1B's handoff: "golden diff shows one change: the additive `barStore` block on
    `/health`". The comparison was made and nothing was committed, so nobody could re-check it.

    `golden/pre_wave1_1b.shape.json` is that baseline, captured from `36a47e8` — the commit
    immediately before 1B — through the same offline path this test uses. The assertion is exact:
    `/health` gains `barStore` and nothing else, and `/analysis/NVDA` is unchanged. A later wave that
    quietly adds a field to either endpoint fails here, which is what check 2.5 is for.
    """
    import app.data as data

    monkeypatch.undo()
    for name in ("_from_alpaca", "_from_twelvedata", "_from_yfinance", "_from_tiingo"):
        monkeypatch.setattr(data, name, lambda *a, **k: (_ for _ in ()).throw(RuntimeError()))

    baseline = _load("pre_wave1_1b.shape.json")

    health = shape(seeded_client.get("/health").json())
    assert shape_accepts(baseline["health"], health) == ["$.barStore: NEW field, not in the golden"], \
        ("/health changed in some way other than gaining barStore:\n  "
         + "\n  ".join(shape_accepts(baseline["health"], health)))

    analysis = shape(seeded_client.get("/analysis/NVDA").json())
    problems = shape_accepts(baseline["analysis"], analysis)
    assert not problems, ("/analysis/NVDA is no longer shape-identical to its pre-1B form:\n  "
                          + "\n  ".join(problems))


def test_every_volatile_rule_names_a_field_that_exists(seeded_client):
    """A normalisation rule for a field that is gone would silently stop normalising anything.
    `normalize` raises rather than skipping; this proves the rules are still live."""
    from normalize import VOLATILE

    body = seeded_client.get("/health").json()
    for ruleset in VOLATILE:
        normalize(body, ruleset)  # raises MissingField if a rule has gone stale
