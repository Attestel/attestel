"""Invariant #1's OFFLINE half, and §9.46 — the guarantees nothing had ever executed.

`EVENT_CONTRACTS.md` §9.43 split invariant #1 into two modes that `CLAUDE.md` had conflated. The
keyless mode (empty `.env`, network available) has been measured repeatedly: Docker gate Pass 1 got
`priceSource: yfinance`, 260 real bars, from inside a container with nothing configured. The
**offline** mode — no network at all — is the one that promises synthetic bars, and **it had never
been run**. This file runs it in-process; Docker gate check 2.7 runs it in containers.

The load-bearing claim is §9.46: **a synthetic bar is never written to the PostgreSQL bar store.** One synthetic row
makes the point-in-time store non-reproducible forever, because `data._synthetic` seeds its RNG from
`abs(hash((ticker, timeframe)))` and Python salts string hashing per process — two processes produce
two different series for the same input — on top of anchoring its date range on the wall clock.
Serve it live and label it; persist it and every §1/§4 replay guarantee is quietly void.

WHY THIS IS NOT `test_pit.py` AGAIN. That file patches `_synthetic` to RAISE, which proves the
leakage fixture rather than production behaviour — §9.38.2 says so in as many words. Here `_synthetic`
runs for real and only the network is cut, which is the condition the invariant actually describes.

§9.38.2 ALSO REQUIRES AN INSPECTION, NOT A TEST — because the property is "no path exists", and a
test can only show that the paths it happened to walk did not. Traced at Wave 1 integration:

    _synthetic has exactly ONE production caller: fetch_ohlcv (data.py:362), the last resort of the
    provider cascade.

    fetch_ohlcv has exactly THREE production callers, and every one of them is live-only:
      1. data._refresh_store (data.py:499)  <- load_bars calls it only under `if not historical`
                                               (data.py:539). A past as_of skips the fetch entirely
                                               and reads the store.
      2. expected.expected_close (expected.py:90) <- /expected-close calls it only under
                                               `if as_of is None` (main.py). The historical branch
                                               goes through the store-derived row instead.
      3. data.latest_quote (data.py:375)    <- /quote/{ticker} accepts no as_of parameter at all,
                                               so it is structurally live-only.

    Both guards are explicit branches on the historical flag, not incidental ordering. Re-run this
    trace with `grep -rn '_synthetic\|fetch_ohlcv' services/analysis/app/*.py` if either file moves.

Run: cd services/analysis && python -m pytest -q tests/test_offline_invariants.py
"""
import pytest
from fastapi.testclient import TestClient

import app.data as data
import app.store as store


@pytest.fixture
def offline(monkeypatch):
    """A private bar store with the network genuinely cut at the transport.

    Every provider helper is left INTACT — they are allowed to try and fail, exactly as they would
    with no route to the internet. Only `requests` is replaced, so the cascade's own error handling
    is what produces the synthetic fallback rather than a patch that hands it over directly.
    """
    store.close()

    class NoNetwork:
        """Every outbound call fails the way an unreachable network fails."""

        class exceptions:  # noqa: N801 — mirrors the requests module surface the cascade catches
            RequestException = Exception

        def get(self, *a, **k):
            raise OSError("network is unreachable")

        def post(self, *a, **k):
            raise OSError("network is unreachable")

    monkeypatch.setattr(data, "requests", NoNetwork())
    # yfinance is not a `requests` caller — it has its own transport, so cutting `requests` alone
    # would leave a live path open. This is the kind of gap §9.44 warns about: the check would pass
    # while the call still happened.
    monkeypatch.setattr(data, "_from_yfinance", lambda *a, **k: (_ for _ in ()).throw(
        OSError("network is unreachable")))

    import app.main as main

    yield TestClient(main.app)
    store.close()


def rows_in_store(kind: str | None = None) -> int:
    conn = store.connect()
    sql = "SELECT COUNT(*) FROM bars"
    args: tuple = ()
    if kind is not None:
        sql += " WHERE source = ?"
        args = (kind,)
    return int(conn.execute(sql, args).fetchone()[0])


# ------------------------------------------------------------------ §9.46: nothing synthetic lands

def test_an_offline_request_cycle_writes_no_synthetic_bar(offline):
    """The clause, asserted the way §9.46.1 words it.

    A full offline cycle across every endpoint that loads bars, then: zero rows with
    `source = 'synthetic'`. Not "zero rows" — the store may legitimately be empty here — but zero
    SYNTHETIC rows, which is the claim that has to hold even once a real store exists.
    """
    for path in ("/analysis/NVDA", "/candles/NVDA?limit=30", "/technical-context/NVDA",
                 "/expected-close/NVDA", "/quote/NVDA"):
        r = offline.get(path)
        assert r.status_code == 200, f"{path} -> {r.status_code}: {r.text[:200]}"

    assert rows_in_store("synthetic") == 0, (
        "a synthetic bar reached PostgreSQL — §9.46: the point-in-time store is now unreproducible, "
        "because _synthetic's series is not stable across processes")
    assert rows_in_store() == 0, (
        "the offline cycle persisted bars from somewhere; nothing offline is entitled to write "
        "history")


def test_the_offline_response_is_labelled_synthetic(offline):
    """Serving synthetic data is fine. Serving it unlabelled is not — that is what puts a number
    nobody can source in front of someone about to risk money."""
    body = offline.get("/analysis/NVDA").json()
    assert body["sourceIsSynthetic"] is True
    assert body["source"] == "synthetic"


def test_the_store_writer_refuses_synthetic_directly():
    """The guard itself, addressed rather than inferred from a cycle that happened not to write.

    Two independent guards exist — `_refresh_store` skips the write, and `store.write_bars` raises —
    and this pins the second, so removing the first cannot silently open the path.
    """
    store.close()
    frame = data._synthetic("NVDA", "1D", n=40)
    with pytest.raises(store.StoreError, match="never stored"):
        store.write_bars("NVDA", "1D", frame, "synthetic")
    assert rows_in_store() == 0
    store.close()


# ------------------------------------------- §9.46.2: offline + a past as_of is a refusal, not a lie

def test_a_historical_request_offline_is_409_not_fabricated_history(offline):
    """§9.46.2. There is no history offline, and inventing some would be worse than refusing.

    A 200 carrying `sourceIsSynthetic: true` for a 2019 cutoff is the failure this forbids: Wave 5A
    would reference invented bars in thousands of prediction records before anyone noticed.
    """
    for path in ("/analysis/NVDA?as_of=2019-03-04T00:00:00Z",
                 "/expected-close/NVDA?as_of=2019-03-04T00:00:00Z"):
        r = offline.get(path)
        assert r.status_code == 409, f"{path} -> {r.status_code}: {r.text[:200]}"
        assert r.json()["error"] == "insufficient_coverage"


# ----------------------------------------------------- coverage bounds are real once bars do exist

def test_a_409_carries_real_bounds_once_the_store_has_bars():
    """`coverageFrom`/`coverageTo` are `null` only when the store is genuinely empty.

    Null on an empty store is honest. Null on a POPULATED one would make the 409 useless: a caller
    that asks for 2019, is refused, and is told nothing about what IS held cannot do anything but
    guess. This is asserted because `read_coverage` derives the bounds from SQL `MIN`/`MAX` under
    the same cutoff predicate as the read — so they are real bounds or no rows, never a placeholder.
    """
    store.close()

    from test_pit import known_bars

    store.write_bars("NVDA", "1D", known_bars(400, "2025-01-01", "B", 11), "yfinance")

    import app.main as main

    client = TestClient(main.app)
    body = client.get("/analysis/NVDA?as_of=2019-03-04T00:00:00Z").json()
    assert body["error"] == "insufficient_coverage"
    assert body["coverageFrom"] is None and body["coverageTo"] is None, (
        "a cutoff BEFORE every stored bar has no coverage under the as-of predicate, so null is "
        "the truthful answer here")

    # A cutoff inside the stored range, but too early to satisfy MIN_BARS, is the case that must
    # carry bounds: rows exist under the predicate, so the caller can be told what is held.
    body = client.get("/analysis/NVDA?as_of=2025-01-06T00:00:00Z").json()
    assert body["error"] == "insufficient_coverage"
    assert body["coverageFrom"] and body["coverageTo"], (
        "a 409 with bars in range returned null bounds — the caller cannot tell how far back the "
        "data goes")
    assert body["coverageFrom"] <= body["coverageTo"]
    # Bounded BY THE CUTOFF, compared at full width: `read_coverage` runs `MIN/MAX(ts)` under the
    # same `ts <= :as_of` predicate as the read, so a bound after the cutoff would mean the 409 was
    # describing bars the caller was not entitled to see.
    assert body["coverageTo"] <= "2025-01-06T00:00:00Z"
    store.close()
