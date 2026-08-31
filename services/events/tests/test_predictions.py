"""The point-in-time prediction store (contract §5, §3.11, §9.25, §9.26, §9.46).

Everything here runs against a tmp_path database with no keys and no network. Two tests carry more
weight than the rest:

* `test_backfill_never_calls_a_second_provider` — every outbound HTTP client is patched to raise
  EXCEPT the mandated `{ANALYSIS_URL}/candles` call, and a full backfill still completes. That is
  §1.5's leakage test applied to this lane.
* `test_synthetic_bar_refuses_to_resolve_and_records_the_refusal` — §9.46. A synthetic bar produces
  a REFUSAL row, never a number, and the refusal is discoverable by a local query.
"""
from __future__ import annotations

import json
from pathlib import Path

import pytest
from fastapi.testclient import TestClient

from app import predictions as predictions_mod
from app.db import connect, migrate

SERVICE_ROOT = Path(__file__).resolve().parent.parent


# ---- fixtures ------------------------------------------------------------------------------------


@pytest.fixture()
def conn(tmp_path, monkeypatch):
    monkeypatch.setenv("ANALYSIS_URL", "http://analysis.test:8001")
    c = connect()
    migrate(c)
    yield c
    c.close()


@pytest.fixture()
def client(conn):
    from app.main import app

    with TestClient(app) as c:
        yield c


def record(**overrides) -> dict:
    """A complete §5 record. Every test that wants an invalid one removes exactly one thing."""
    body = {
        "ticker": "NVDA",
        "asOf": "2023-06-15T20:00:00Z",           # dev window (< 2024-01-01)
        "createdAt": "2023-06-15T20:00:41Z",
        "experiment": "D",
        "split": "dev",
        "identity": {
            "modelUsed": "qwen3-14b",
            "quantization": "q8_0",
            "reasoningMode": "thinking",
            "promptVersion": "final-analyst@3",
            "schemaVersion": "thesis@2",
            "toolSchemaVersion": "tools@1",
            "generationSettings": {"temperature": 0.2, "topP": 0.9, "seed": 20230615},
        },
        "evidence": {
            "barsRef": {"ticker": "NVDA", "timeframe": "1D", "lastTs": "2023-06-15T20:00:00Z"},
            "technicalStateHash": "a" * 64,
            "eventIds": ["evt_1a2b3c4d5e6f7081"],
            "macroEventIds": ["mac_1a2b3c4d5e6f7081"],
            "earningsSnapshotId": "earn_2023q1",
            "toolCalls": [{"tool": "get_ticker_events", "args": {"ticker": "NVDA"}}],
        },
        "forecast": {
            "horizons": {
                "1d": {"direction": "neutral", "score": 0.04, "abstain": True},
                "5d": {"direction": "bullish", "score": 0.31, "abstain": False},
                "20d": {"direction": "bullish", "score": 0.44, "abstain": False},
                "60d": {"direction": "neutral", "score": 0.08, "abstain": True},
            },
            "confidenceBucket": "medium",
            "thesis": "Data-centre demand is the driver; the technical state is confirming.",
            "invalidation": "daily close below 173.80 on above-average volume",
        },
    }
    body.update(overrides)
    return body


# ---- a fake analysis service ----------------------------------------------------------------------


BUSINESS_DAYS = [
    # A deliberately holiday-bearing stretch: 2023-07-04 (US Independence Day) is ABSENT, so a
    # calendar-arithmetic horizon would land on a bar that does not exist.
    "2023-06-15", "2023-06-16", "2023-06-20", "2023-06-21", "2023-06-22", "2023-06-23",
    "2023-06-26", "2023-06-27", "2023-06-28", "2023-06-29", "2023-06-30",
    "2023-07-03", "2023-07-05", "2023-07-06", "2023-07-07", "2023-07-10", "2023-07-11",
    "2023-07-12", "2023-07-13", "2023-07-14", "2023-07-17", "2023-07-18", "2023-07-19",
    "2023-07-20", "2023-07-21", "2023-07-24", "2023-07-25", "2023-07-26", "2023-07-27",
    "2023-07-28", "2023-07-31",
]


class FakeAnalysis:
    """Serves `/candles` from an in-memory bar table, applying the SAME rules the real endpoint
    applies: `ts <= as_of`, the newest `limit` bars, and a `source` label that carries every distinct
    source in the window joined by '+' (`data.py::_label_sources`)."""

    def __init__(self, series: dict[str, list[tuple[str, float, str]]]):
        self.series = series
        self.calls: list[tuple[str, dict]] = []

    def __call__(self, url: str, params: dict | None = None, timeout: float | None = None, **kw):
        self.calls.append((url, dict(params or {})))
        ticker = url.rstrip("/").rsplit("/", 1)[-1]
        bars = self.series.get(ticker, [])
        as_of = str((params or {}).get("as_of") or "9999-12-31")
        limit = int((params or {}).get("limit") or 60)
        visible = [b for b in bars if f"{b[0]}T00:00:00Z" <= as_of]
        window = visible[-limit:] if limit else visible
        sources = sorted({b[2] for b in window})
        label = "+".join(sources) if sources else "unknown"
        return _Response(
            200,
            {
                "ticker": ticker,
                "timeframe": "1D",
                "asOf": as_of,
                "source": label,
                "sourceIsSynthetic": "synthetic" in sources,
                "bars": [
                    {"time": d, "open": c, "high": c, "low": c, "close": c, "volume": 1000}
                    for d, c, _src in window
                ],
                "coverage": "ok" if window else "insufficient",
                "coverageFrom": window[0][0] if window else None,
                "coverageTo": window[-1][0] if window else None,
            },
        )


class _Response:
    def __init__(self, status_code: int, payload: dict):
        self.status_code = status_code
        self._payload = payload

    def json(self):
        return self._payload


def ramp(source: str = "yfinance", start: float = 100.0, step: float = 1.0):
    return [(d, start + i * step, source) for i, d in enumerate(BUSINESS_DAYS)]


def flat(source: str = "yfinance", value: float = 100.0):
    return [(d, value, source) for d in BUSINESS_DAYS]


@pytest.fixture()
def analysis(monkeypatch):
    """Patch `requests.get` in the module under test and refuse anything that is not ANALYSIS_URL.

    This is the leakage guard every test inherits: any call to a provider, a quote endpoint or a
    second service raises immediately, so a leak fails the whole file rather than one test.
    """
    fake = FakeAnalysis({"NVDA": ramp(), "SPY": flat(), "SMH": flat()})

    def guarded(url, params=None, timeout=None, **kw):
        if not url.startswith(predictions_mod.analysis_url()):
            raise AssertionError(f"outbound call to a non-ANALYSIS_URL endpoint: {url}")
        if "/quote/" in url:
            raise AssertionError("/quote/{ticker} is a live path and is forbidden here (§9.25)")
        return fake(url, params=params, timeout=timeout, **kw)

    monkeypatch.setattr(predictions_mod.requests, "get", guarded)
    return fake


# ---- 1. round-trip -------------------------------------------------------------------------------


def test_round_trip_preserves_every_identity_field(client, analysis):
    posted = client.post("/predictions", json=record())
    assert posted.status_code == 200, posted.text
    written = posted.json()["prediction"]

    listed = client.get("/predictions", params={"ticker": "NVDA", "as_of": "2023-06-16T00:00:00Z"})
    assert listed.status_code == 200
    body = listed.json()
    assert body["asOf"] == "2023-06-16T00:00:00Z"
    assert body["degraded"] == []
    assert len(body["predictions"]) == 1
    read_back = body["predictions"][0]

    assert read_back == written
    source = record()
    assert read_back["identity"] == source["identity"]
    assert read_back["evidence"] == source["evidence"]
    assert read_back["forecast"]["horizons"] == source["forecast"]["horizons"]
    assert read_back["forecast"]["thesis"] == source["forecast"]["thesis"]
    assert read_back["experiment"] == "D" and read_back["split"] == "dev"


def test_prediction_id_format(client, analysis):
    pid = client.post("/predictions", json=record()).json()["prediction"]["id"]
    assert pid.startswith("prd_")
    hex_part = pid[len("prd_"):]
    assert len(hex_part) == 16
    assert hex_part == hex_part.lower()
    int(hex_part, 16)


# ---- 2/3. rejections -----------------------------------------------------------------------------


@pytest.mark.parametrize("field", ["experiment", "split", "ticker", "asOf", "createdAt"])
def test_missing_top_level_field_is_422_naming_the_field(client, analysis, field):
    body = record()
    body.pop(field)
    response = client.post("/predictions", json=body)
    assert response.status_code == 422
    assert field in response.json()["detail"]


@pytest.mark.parametrize("field", list(predictions_mod.IDENTITY_KEYS))
def test_missing_identity_field_is_422_naming_the_field(client, analysis, field):
    body = record()
    body["identity"].pop(field)
    response = client.post("/predictions", json=body)
    assert response.status_code == 422
    assert f"identity.{field}" in response.json()["detail"]


def test_top_level_temperature_is_rejected(client, analysis):
    """§9.40: temperature lives in generationSettings and nowhere else."""
    body = record()
    body["identity"]["temperature"] = 0.2
    response = client.post("/predictions", json=body)
    assert response.status_code == 422
    assert "identity.temperature" in response.json()["detail"]


def test_created_at_before_as_of_is_rejected(client, analysis):
    body = record(createdAt="2023-06-15T19:59:00Z")
    response = client.post("/predictions", json=body)
    assert response.status_code == 422
    assert "createdAt" in response.json()["detail"]


def test_split_disagreeing_with_the_cutoff_is_rejected(client, analysis):
    """`split` is derived from `as_of`, never chosen (locked decision 3)."""
    response = client.post("/predictions", json=record(split="test"))
    assert response.status_code == 422
    detail = response.json()["detail"]
    assert "split" in detail and "dev" in detail


def test_missing_evidence_hash_is_rejected(client, analysis):
    body = record()
    body["evidence"].pop("technicalStateHash")
    response = client.post("/predictions", json=body)
    assert response.status_code == 422
    assert "technicalStateHash" in response.json()["detail"]


# ---- 4. split determinism ---------------------------------------------------------------------------


def test_split_windows_are_deterministic_and_disjoint():
    assert predictions_mod.split_for("2019-01-01T00:00:00Z") == "dev"
    assert predictions_mod.split_for("2023-12-31T23:59:59Z") == "dev"
    assert predictions_mod.split_for("2024-01-01T00:00:00Z") == "validation"
    assert predictions_mod.split_for("2025-06-30T23:59:59Z") == "validation"
    assert predictions_mod.split_for("2025-07-01T00:00:00Z") == "test"
    assert predictions_mod.split_for("2026-08-20T00:00:00Z") == "test"
    # Deterministic: the same cutoff always lands in the same window.
    assert all(
        predictions_mod.split_for("2024-05-05T12:00:00Z") == "validation" for _ in range(5)
    )


# ---- 5. the aggregate guard (locked decision 2) -------------------------------------------------


def test_aggregate_query_without_experiment_or_split_raises():
    with pytest.raises(ValueError, match="experiment"):
        predictions_mod.build_prediction_query(aggregate=True, split="dev")
    with pytest.raises(ValueError, match="split"):
        predictions_mod.build_prediction_query(aggregate=True, experiment="D")
    with pytest.raises(ValueError):
        predictions_mod.build_prediction_query(aggregate=True)
    sql, params = predictions_mod.build_prediction_query(
        aggregate=True, experiment="D", split="dev"
    )
    assert "p.experiment = :experiment" in sql and "p.split = :split" in sql
    assert params["experiment"] == "D" and params["split"] == "dev"


# ---- 6. backfill ---------------------------------------------------------------------------------


def test_backfill_resolves_via_candles_and_records_source_and_bars_ts(client, analysis, conn):
    client.post("/predictions", json=record())
    response = client.post("/outcomes/backfill", json={"asOf": "2023-07-31T23:59:59Z"})
    assert response.status_code == 200
    body = response.json()
    assert body["updated"] >= 3        # 1d, 5d, 20d matured; 60d has not
    assert body["refused"] == 0

    rows = conn.execute("SELECT * FROM outcomes ORDER BY horizon").fetchall()
    by_horizon = {r["horizon"]: r for r in rows}

    one_day = by_horizon["1d"]
    assert one_day["resolved_from_bars_ts"] == "2023-06-16"   # the next TRADING day
    assert one_day["resolved_from_source"] == "yfinance"
    assert one_day["realized_return"] == pytest.approx(1.0 / 100.0)
    assert one_day["resolved_at"] is not None

    # 5 trading days from 2023-06-15 is 2023-06-23 — the weekend and the 06-19 holiday gap are
    # skipped because the count is over ROWS THAT EXIST.
    assert by_horizon["5d"]["resolved_from_bars_ts"] == "2023-06-23"
    # 20 trading days lands past 2023-07-04, which is absent from the series entirely.
    assert by_horizon["20d"]["resolved_from_bars_ts"] == "2023-07-17"

    # Every /candles call went to ANALYSIS_URL with timeframe=1D. No other endpoint exists here.
    assert analysis.calls
    for url, params in analysis.calls:
        assert "/candles/" in url
        assert params["timeframe"] == "1D"


def test_horizon_that_has_not_matured_stays_pending(client, analysis, conn):
    client.post("/predictions", json=record())
    body = client.post("/outcomes/backfill", json={"asOf": "2023-06-23T23:59:59Z"}).json()
    assert body["pending"] >= 2                      # 20d and 60d cannot be resolved yet
    resolved = conn.execute(
        "SELECT horizon FROM outcomes WHERE resolved_at IS NOT NULL ORDER BY horizon"
    ).fetchall()
    assert {r["horizon"] for r in resolved} == {"1d", "5d"}
    assert conn.execute(
        "SELECT COUNT(*) c FROM outcomes WHERE horizon IN ('20d','60d')"
    ).fetchone()["c"] == 0


def test_trading_day_counting_skips_a_market_holiday(client, analysis, conn):
    """A 20-day horizon from 2023-06-15 must not land on 2023-07-04, which has no bar."""
    client.post("/predictions", json=record())
    client.post("/outcomes/backfill", json={"asOf": "2023-07-31T23:59:59Z"})
    ts = conn.execute(
        "SELECT resolved_from_bars_ts FROM outcomes WHERE horizon = '20d'"
    ).fetchone()["resolved_from_bars_ts"]
    assert ts in BUSINESS_DAYS
    assert ts != "2023-07-04"
    # Exactly 20 rows forward from the baseline row, counted over rows that exist.
    assert BUSINESS_DAYS.index(ts) - BUSINESS_DAYS.index("2023-06-15") == 20


def test_missing_benchmark_is_null_never_zero(client, monkeypatch, conn):
    """§9.26: a missing benchmark is not a zero benchmark."""
    fake = FakeAnalysis({"NVDA": ramp()})          # no SPY, no SMH bars at all

    def guarded(url, params=None, timeout=None, **kw):
        assert url.startswith(predictions_mod.analysis_url())
        return fake(url, params=params, timeout=timeout, **kw)

    monkeypatch.setattr(predictions_mod.requests, "get", guarded)
    from app.main import app

    with TestClient(app) as client_:
        client_.post("/predictions", json=record())
        body = client_.post("/outcomes/backfill", json={"asOf": "2023-07-31T23:59:59Z"}).json()

    row = conn.execute("SELECT * FROM outcomes WHERE horizon = '5d'").fetchone()
    assert row["realized_return"] is not None
    assert row["benchmark_return"] is None
    assert row["sector_return"] is None
    assert row["excess_return"] is None            # NULL, not the realised return, not 0
    assert "SPY:no_coverage" in body["degraded"]


def test_regime_and_vol_are_computed_and_versioned(client, analysis, conn):
    client.post("/predictions", json=record())
    body = client.post("/outcomes/backfill", json={"asOf": "2023-07-31T23:59:59Z"}).json()
    assert body["regimeVersion"] == predictions_mod.REGIME_VERSION
    assert body["outcomeVersion"] == predictions_mod.OUTCOME_VERSION
    row = conn.execute("SELECT * FROM outcomes WHERE horizon = '20d'").fetchone()
    assert row["regime_label"] in ("bull", "bear", "sideways", "high_vol")
    # A 1-day horizon has ONE return, so dispersion is undefined and must be NULL, never 0.0.
    assert conn.execute(
        "SELECT realized_vol FROM outcomes WHERE horizon = '1d'"
    ).fetchone()["realized_vol"] is None


# ---- 7. §9.46 — the synthetic refusal ---------------------------------------------------------


def test_synthetic_bar_refuses_to_resolve_and_records_the_refusal(client, monkeypatch, conn):
    fake = FakeAnalysis({"NVDA": ramp(source="synthetic"), "SPY": flat(), "SMH": flat()})
    monkeypatch.setattr(predictions_mod.requests, "get", lambda url, **kw: fake(url, **kw))

    from app.main import app

    with TestClient(app) as client_:
        client_.post("/predictions", json=record())
        body = client_.post("/outcomes/backfill", json={"asOf": "2023-07-31T23:59:59Z"}).json()

    assert body["updated"] == 0
    assert body["refused"] >= 3
    assert body["refused"] <= body["pending"]

    rows = conn.execute("SELECT * FROM outcomes").fetchall()
    assert rows, "the refusal must be RECORDED, not merely skipped"
    for row in rows:
        # Not a number anywhere. A synthetic-resolved outcome is indistinguishable from a real one
        # once it carries a value, so it never carries one.
        assert row["realized_return"] is None
        assert row["benchmark_return"] is None
        assert row["sector_return"] is None
        assert row["excess_return"] is None
        assert row["realized_vol"] is None
        assert row["regime_label"] is None
        assert row["resolved_at"] is None
        assert row["resolved_from_bars_ts"] is None
        # …and the refusal is DISCOVERABLE by the local query the ladder runs.
        assert "synthetic" in row["resolved_from_source"]

    local = conn.execute(
        "SELECT COUNT(*) c FROM outcomes WHERE resolved_from_source LIKE '%synthetic%'"
    ).fetchone()["c"]
    assert local == len(rows)


def test_a_mixed_window_containing_one_synthetic_bar_still_refuses(client, monkeypatch, conn):
    """`/candles` labels a mixed window 'yfinance+synthetic' — an equality test would miss it."""
    bars = ramp()
    bars[3] = (bars[3][0], bars[3][1], "synthetic")
    fake = FakeAnalysis({"NVDA": bars, "SPY": flat(), "SMH": flat()})
    monkeypatch.setattr(predictions_mod.requests, "get", lambda url, **kw: fake(url, **kw))

    from app.main import app

    with TestClient(app) as client_:
        client_.post("/predictions", json=record())
        body = client_.post("/outcomes/backfill", json={"asOf": "2023-07-31T23:59:59Z"}).json()

    assert body["updated"] == 0 and body["refused"] >= 1
    label = conn.execute("SELECT resolved_from_source FROM outcomes LIMIT 1").fetchone()[0]
    assert "+" in label and "synthetic" in label


def test_absent_bar_never_resolves_and_writes_no_outcome(client, monkeypatch, conn):
    """An absent bar is not a zero bar and not the nearest bar: it is `pending`, with no row."""
    fake = FakeAnalysis({})                            # coverage: insufficient, zero bars
    monkeypatch.setattr(predictions_mod.requests, "get", lambda url, **kw: fake(url, **kw))

    from app.main import app

    with TestClient(app) as client_:
        client_.post("/predictions", json=record())
        body = client_.post("/outcomes/backfill", json={"asOf": "2023-07-31T23:59:59Z"}).json()

    assert body["updated"] == 0
    assert body["pending"] == 4
    assert conn.execute("SELECT COUNT(*) c FROM outcomes").fetchone()["c"] == 0


def test_backfill_survives_an_unreachable_analysis_service(client, monkeypatch, conn):
    def boom(url, **kw):
        raise ConnectionError("analysis is down")

    monkeypatch.setattr(predictions_mod.requests, "get", boom)
    from app.main import app

    with TestClient(app) as client_:
        client_.post("/predictions", json=record())
        body = client_.post("/outcomes/backfill", json={"asOf": "2023-07-31T23:59:59Z"}).json()

    assert body["updated"] == 0 and body["pending"] == 4
    assert any("unavailable" in d for d in body["degraded"])
    assert conn.execute("SELECT COUNT(*) c FROM outcomes").fetchone()["c"] == 0


# ---- 8. leakage and boundary discipline ---------------------------------------------------------


def test_backfill_never_calls_a_second_provider(client, monkeypatch, conn):
    """§1.5 for this lane: EVERY outbound client raises except the mandated /candles call."""
    fake = FakeAnalysis({"NVDA": ramp(), "SPY": flat(), "SMH": flat()})
    seen: list[str] = []

    def only_analysis(url, params=None, timeout=None, **kw):
        seen.append(url)
        if not url.startswith(predictions_mod.analysis_url() + "/candles/"):
            raise AssertionError(f"forbidden outbound call: {url}")
        return fake(url, params=params, timeout=timeout, **kw)

    import requests as requests_lib

    def forbidden(*a, **kw):
        raise AssertionError(f"an outbound HTTP client was used: {a!r}")

    monkeypatch.setattr(predictions_mod.requests, "get", only_analysis)
    for verb in ("post", "put", "patch", "delete", "head", "request"):
        monkeypatch.setattr(requests_lib, verb, forbidden, raising=False)

    from app.main import app

    with TestClient(app) as client_:
        client_.post("/predictions", json=record())
        body = client_.post("/outcomes/backfill", json={"asOf": "2023-07-31T23:59:59Z"}).json()

    assert body["updated"] >= 3
    assert seen and all("/candles/" in u for u in seen)


def test_no_code_path_opens_bars_db_or_calls_quote():
    """Boundary discipline, asserted on the source (§9.25).

    `bars` is another service's private PostgreSQL schema. This lane may not query it directly and
    may not reach the live `/quote/{ticker}` path. The assertion is on the source
    text because the absence of a call cannot be proved by running the happy path (§9.44).
    """
    source = (SERVICE_ROOT / "app" / "predictions.py").read_text(encoding="utf-8")
    code = _executable_source(source)
    lowered = code.lower()
    # The prose in this module discusses the forbidden schema and /quote/{ticker} at length — that is the
    # point of the prose. The CODE must contain none of them.
    assert "bars_db" not in lowered
    assert "attach" not in lowered
    assert "/quote/" not in lowered
    assert "psycopg.connect (" not in lowered        # the event store is opened via db.connect only
    # Exactly one URL is ever constructed, and it is the §9.25 one.
    assert code.count("requests.get") == 1
    assert "/candles/" in code


def _executable_source(source: str) -> str:
    """The module's executable AST with comments and docstrings removed.

    Asserting on raw text would be defeated by this module's own docstrings, which name every
    forbidden path in order to forbid it. Runtime string literals must remain because the allowed
    `/candles/` route is itself an f-string. AST unparse behaves consistently on Python 3.11+;
    tokenizing f-strings does not (3.11 emits one STRING token, 3.12 emits f-string parts).
    """
    import ast

    class WithoutDocstrings(ast.NodeTransformer):
        @staticmethod
        def _strip(body):
            if (body and isinstance(body[0], ast.Expr)
                    and isinstance(body[0].value, ast.Constant)
                    and isinstance(body[0].value.value, str)):
                return body[1:]
            return body

        def visit_Module(self, node):  # noqa: N802 — ast visitor API
            node.body = self._strip(node.body)
            return self.generic_visit(node)

        def visit_FunctionDef(self, node):  # noqa: N802
            node.body = self._strip(node.body)
            return self.generic_visit(node)

        def visit_AsyncFunctionDef(self, node):  # noqa: N802
            node.body = self._strip(node.body)
            return self.generic_visit(node)

        def visit_ClassDef(self, node):  # noqa: N802
            node.body = self._strip(node.body)
            return self.generic_visit(node)

    return ast.unparse(WithoutDocstrings().visit(ast.parse(source)))


def test_the_module_declares_only_the_postgres_runtime_dependency():
    """D-26 amendment: psycopg is the only event-store driver."""
    text = (SERVICE_ROOT / "requirements.txt").read_text(encoding="utf-8")
    assert sorted(l.split("==")[0] for l in text.splitlines() if l.strip()) == [
        "fastapi",
        "psycopg[binary]",
        "requests",
        "uvicorn[standard]",
    ]


# ---- 9. §9.26's proxy map ----------------------------------------------------------------------


def test_sector_map_and_benchmark(client, analysis):
    assert predictions_mod.BENCHMARK_TICKER == "SPY"
    for ticker in ("NVDA", "AMD", "AVGO"):
        assert predictions_mod.sector_etf_for(ticker) == "SMH"
    for ticker in ("MSFT", "GOOGL", "META"):
        assert predictions_mod.sector_etf_for(ticker) == "XLK"
    assert predictions_mod.sector_etf_for("JPM") == "SPY"      # named default, not a guess


def test_postgres_baseline_and_as_of_index_applied(conn):
    versions = [
        # ORDER BY, not insertion order: a second migration made this assertion depend on the
        # physical row order, which PostgreSQL does not promise.
        r["version"]
        for r in conn.execute("SELECT version FROM schema_migrations ORDER BY version").fetchall()
    ]
    assert versions == [
        "0001_postgres_baseline", "0002_automation", "0003_scheduled_event_documents",
        "0004_event_relationships", "0005_event_reactions", "0006_discovery_scout",
        "0007_opportunity_radar", "0008_event_data_repairs",
    ]
    indexes = {
        r["indexname"]
        for r in conn.execute(
            "SELECT indexname FROM pg_indexes WHERE schemaname = current_schema() "
            "AND tablename = 'predictions'"
        ).fetchall()
    }
    assert "idx_predictions_as_of" in indexes


def test_evidence_rows_are_local_to_the_events_db(client, analysis, conn):
    client.post("/predictions", json=record())
    rows = conn.execute("SELECT kind, ref_id FROM prediction_evidence ORDER BY kind").fetchall()
    assert [(r["kind"], r["ref_id"]) for r in rows] == [
        ("event", "evt_1a2b3c4d5e6f7081"),
        ("macro_event", "mac_1a2b3c4d5e6f7081"),
    ]


def test_historical_read_cannot_see_a_later_prediction(client, analysis):
    """§1.2 applied to this table: `as_of` is the cutoff, `created_at` is when the store learned it.
    Both must pass, so a record written later is invisible at an earlier cutoff."""
    client.post("/predictions", json=record())
    early = client.get("/predictions", params={"as_of": "2023-06-15T19:00:00Z"}).json()
    assert early["predictions"] == []
    late = client.get("/predictions", params={"as_of": "2023-06-16T00:00:00Z"}).json()
    assert len(late["predictions"]) == 1
