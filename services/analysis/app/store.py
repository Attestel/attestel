"""Point-in-time bar store backed exclusively by PostgreSQL.

Every historical read is served from this store and applies ``ts <= as_of`` in SQL. Live provider
responses are persisted before being read back, except for weekly bars (derived from daily bars)
and synthetic fallback data (never history). PostgreSQL is required; there is no file or SQLite
fallback.
"""
from __future__ import annotations

import json
import os
import re
import threading
import time
from contextlib import contextmanager
from datetime import date, datetime, timedelta, timezone
from pathlib import Path
from urllib.parse import urlsplit

import pandas as pd
import psycopg
from psycopg import sql

TS_FMT = "%Y-%m-%dT%H:%M:%SZ"
WEEKLY_TIMEFRAME = "1W"

SOURCE_ALIASES = {"alpaca:iex": "alpaca"}
KNOWN_SOURCES = ("alpaca", "twelvedata", "yfinance", "tiingo", "synthetic")
SYNTHETIC_SOURCE = "synthetic"

DEFAULT_INTRADAY_RETENTION_DAYS = 90
DEFAULT_SCHEMA = "analysis"
MIGRATIONS_DIR = Path(__file__).resolve().parent.parent / "migrations"

_NAMED_PLACEHOLDER = re.compile(r"(?<!:):([A-Za-z_][A-Za-z0-9_]*)")
_LOCK = threading.RLock()
_CONN: "Connection | None" = None
_CONN_TARGET: tuple[str, str] | None = None


class StoreError(Exception):
    """Raised on a store configuration error or a write forbidden by the bar contract."""


class Row(dict):
    """DB row supporting both mapping and legacy positional access."""

    def __getitem__(self, key):
        if isinstance(key, int):
            return tuple(self.values())[key]
        return super().__getitem__(key)

    def __iter__(self):
        return iter(self.values())


def _normalise_value(value):
    if isinstance(value, datetime):
        moment = value if value.tzinfo else value.replace(tzinfo=timezone.utc)
        return moment.astimezone(timezone.utc).strftime(TS_FMT)
    if isinstance(value, date):
        return value.isoformat()
    if isinstance(value, (dict, list)):
        return json.dumps(value, separators=(",", ":"), sort_keys=True)
    return value


def _row_factory(cursor):
    columns = [column.name for column in cursor.description] if cursor.description else []

    def make_row(values) -> Row:
        return Row((name, _normalise_value(value)) for name, value in zip(columns, values))

    return make_row


def _postgres_query(query: str, params) -> str:
    if params is None:
        return query
    if isinstance(params, dict):
        return _NAMED_PLACEHOLDER.sub(r"%(\1)s", query)
    return query.replace("?", "%s")


def _is_read_only(query: str) -> bool:
    """True for statements that are safe to run twice.

    Deliberately conservative: only a leading SELECT or WITH counts, and anything containing a
    data-modifying CTE keyword is refused. A false negative costs one failed request; a false
    positive would re-run a write.
    """
    head = query.lstrip().lstrip("(").upper()
    if not head.startswith(("SELECT", "WITH")):
        return False
    body = query.upper()
    return not any(word in body for word in ("INSERT", "UPDATE", "DELETE", "MERGE"))


class Connection:
    """Small DB-API facade retained for existing store consumers and tests.

    SELF-HEALING (2026-08-23). `connect()` caches ONE connection for the life of the process, and
    this facade used to hand its dead socket back forever: when PostgreSQL restarted — which it was
    demonstrably doing, "server closed the connection unexpectedly" one minute and "Connection
    refused" the next — every subsequent read raised `OperationalError` until uvicorn's
    `--limit-max-requests 2000` recycle happened to come around. The database recovering did not
    recover the service.

    So a broken connection now invalidates the module cache instead of persisting, and a READ is
    retried once on a fresh connection. Writes are never auto-retried: an `OperationalError` means
    the server-side transaction is gone, but "gone" is not something this layer can prove about a
    statement it already put on the wire, and a silently duplicated bar write is worse than a 500.
    """

    def __init__(self, raw: psycopg.Connection):
        self._raw = raw

    def _reopen(self) -> bool:
        """Swap in a fresh raw connection after a transport failure. False if PostgreSQL is still
        down — the caller then re-raises the original error, which is the honest answer."""
        try:
            self._raw.close()
        except Exception:  # noqa: BLE001 — it is already broken; closing is best-effort
            pass
        try:
            self._raw = _open_raw()
        except Exception:  # noqa: BLE001 — reported by the caller's re-raise
            return False
        return True

    def execute(self, query: str, params=None):
        text = _postgres_query(query, params)
        try:
            return self._raw.execute(text, params)
        except psycopg.OperationalError:
            # The cached connection is dead. Drop it so the NEXT caller reconnects even if this
            # one cannot be salvaged.
            _invalidate_cache(self)
            if not _is_read_only(query) or not self._reopen():
                raise
            _adopt_cache(self)
            return self._raw.execute(text, params)

    def executemany(self, query: str, params_seq):
        try:
            with self._raw.cursor() as cursor:
                cursor.executemany(_postgres_query(query, ()), params_seq)
                return cursor.rowcount
        except psycopg.OperationalError:
            # Always a write path here. Invalidate and re-raise — never replay a batch.
            _invalidate_cache(self)
            raise

    def commit(self) -> None:
        self._raw.commit()

    def rollback(self) -> None:
        self._raw.rollback()

    def close(self) -> None:
        self._raw.close()

    @contextmanager
    def transaction(self):
        with self._raw.transaction():
            yield self


def _positive_int(name: str, default: int) -> int:
    try:
        return max(1, int(os.getenv(name, str(default))))
    except (TypeError, ValueError):
        return default


def database_url() -> str:
    value = os.getenv("BARS_DATABASE_URL", "").strip() or os.getenv("DATABASE_URL", "").strip()
    if not value:
        raise StoreError(
            "DATABASE_URL (or BARS_DATABASE_URL) is required: "
            "the analysis bar store is PostgreSQL-only"
        )
    if not value.startswith(("postgresql://", "postgres://")):
        raise StoreError("DATABASE_URL must use postgresql:// or postgres://")
    return value


def database_schema() -> str:
    return os.getenv("BARS_DATABASE_SCHEMA", DEFAULT_SCHEMA).strip() or DEFAULT_SCHEMA


def database_target() -> str:
    parsed = urlsplit(database_url())
    host = parsed.hostname or "unknown"
    port = parsed.port or 5432
    name = parsed.path.lstrip("/") or "unknown"
    return f"{host}:{port}/{name}"


def intraday_retention_days() -> int:
    raw = os.getenv("INTRADAY_RETENTION_DAYS", "")
    try:
        return max(0, int(raw))
    except (TypeError, ValueError):
        return DEFAULT_INTRADAY_RETENTION_DAYS


def _ensure_schema_and_migrations(raw: psycopg.Connection, schema: str) -> None:
    raw.execute(sql.SQL("CREATE SCHEMA IF NOT EXISTS {}").format(sql.Identifier(schema)))
    raw.execute(sql.SQL("SET search_path TO {}, public").format(sql.Identifier(schema)))
    raw.execute(
        "SELECT set_config('statement_timeout', %s, false)",
        (str(_positive_int("BARS_DB_STATEMENT_TIMEOUT_MS", 15000)),),
    )
    raw.execute(
        "CREATE TABLE IF NOT EXISTS schema_migrations ("
        "version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL)"
    )
    raw.commit()

    for path in sorted(MIGRATIONS_DIR.glob("*.sql")):
        version = path.stem
        with raw.transaction():
            raw.execute(
                "SELECT pg_advisory_xact_lock(hashtext(%s))",
                (f"attestel.analysis.{schema}.schema_migrations",),
            )
            exists = raw.execute(
                "SELECT 1 FROM schema_migrations WHERE version = %s", (version,)
            ).fetchone()
            if exists:
                continue
            raw.execute(path.read_text(encoding="utf-8"))
            raw.execute(
                "INSERT INTO schema_migrations (version, applied_at) VALUES (%s, %s)",
                (version, now_ts()),
            )


# The shared PostgreSQL behind this store has been observed cycling ("server closed the connection
# unexpectedly" / "Connection refused", seconds apart, windows lasting a few seconds). A request
# that dies on the FIRST failed connect becomes a user-visible 500 for an outage already over by
# the time anyone retries — so a failed connect is retried on a short, bounded schedule. Both fault
# shapes fail in milliseconds, so the worst case adds ~1s of sleep; a database that is DOWN rather
# than flapping still fails, just ~1s later. Mitigation for someone else's instability, not the fix.
_FLAP_RETRY_DELAYS = (0.3, 0.7)


def _connect_retries() -> int:
    try:
        n = int(os.getenv("BARS_DB_CONNECT_RETRIES", "").strip() or len(_FLAP_RETRY_DELAYS))
    except ValueError:
        n = len(_FLAP_RETRY_DELAYS)
    return max(0, min(n, len(_FLAP_RETRY_DELAYS)))


def _open_raw() -> psycopg.Connection:
    """Open one raw connection with the schema and migrations ensured. Never touches the cache."""
    retries = _connect_retries()
    raw = None
    for attempt in range(retries + 1):
        try:
            raw = psycopg.connect(
                database_url(),
                connect_timeout=_positive_int("BARS_DB_CONNECT_TIMEOUT", 10),
                row_factory=_row_factory,
            )
            break
        except psycopg.OperationalError:
            if attempt >= retries:
                raise
            time.sleep(_FLAP_RETRY_DELAYS[attempt])
    try:
        _ensure_schema_and_migrations(raw, database_schema())
    except Exception:
        raw.close()
        raise
    return raw


def _invalidate_cache(conn: "Connection") -> None:
    """Forget `conn` if it is the cached one, so the next `connect()` opens a fresh socket."""
    global _CONN, _CONN_TARGET
    with _LOCK:
        if _CONN is conn:
            _CONN, _CONN_TARGET = None, None


def _adopt_cache(conn: "Connection") -> None:
    """Re-cache a facade that just healed itself, so the repair is shared, not per-caller."""
    global _CONN, _CONN_TARGET
    with _LOCK:
        if _CONN is None:
            _CONN, _CONN_TARGET = conn, (database_url(), database_schema())


def connect() -> Connection:
    """Open and cache a thread-safe PostgreSQL connection for the configured database/schema.

    A cached connection that PostgreSQL has closed underneath us is discarded rather than handed
    back. `closed`/`broken` only catch a socket already known to be gone — a server that vanished
    silently is caught by `Connection.execute` instead — but checking here means a restart between
    two requests costs nothing at all.
    """
    global _CONN, _CONN_TARGET
    url = database_url()
    schema = database_schema()
    target = (url, schema)
    with _LOCK:
        if _CONN is not None and _CONN_TARGET == target:
            raw = _CONN._raw
            if not getattr(raw, "closed", False) and not getattr(raw, "broken", False):
                return _CONN
        close()
        _CONN = Connection(_open_raw())
        _CONN_TARGET = target
        return _CONN


def close() -> None:
    """Drop the cached connection (tests and shutdown)."""
    global _CONN, _CONN_TARGET
    with _LOCK:
        if _CONN is not None:
            try:
                _CONN.close()
            except Exception:  # noqa: BLE001
                pass
        _CONN, _CONN_TARGET = None, None


def format_ts(value) -> str:
    """Any timestamp-like value -> fixed-width UTC for the public service boundary."""
    ts = pd.Timestamp(value)
    if ts.tzinfo is not None:
        ts = ts.tz_convert("UTC").tz_localize(None)
    return ts.to_pydatetime().strftime(TS_FMT)


def now_ts() -> str:
    return datetime.now(timezone.utc).strftime(TS_FMT)


def normalize_source(source: str | None) -> str:
    value = (source or "").strip().lower()
    return SOURCE_ALIASES.get(value, value)


def write_bars(ticker: str, timeframe: str, df: pd.DataFrame, source: str) -> int:
    """Upsert a provider window; weekly and synthetic bars are rejected by design."""
    if timeframe == WEEKLY_TIMEFRAME:
        raise StoreError("1W is resampled from stored 1D on read and is never stored")
    src = normalize_source(source)
    if src == SYNTHETIC_SOURCE:
        raise StoreError("synthetic bars are a live-only fallback and are never stored")
    if df is None or df.empty:
        return 0
    stamped = now_ts()
    rows = [
        (
            ticker,
            timeframe,
            format_ts(idx),
            float(row.open),
            float(row.high),
            float(row.low),
            float(row.close),
            float(row.volume),
            src,
            stamped,
        )
        for idx, row in df.iterrows()
    ]
    conn = connect()
    with _LOCK:
        conn.executemany(
            """
            INSERT INTO bars (ticker, timeframe, ts, open, high, low, close, volume, source,
                              ingested_at)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
            ON CONFLICT (ticker, timeframe, ts) DO UPDATE SET
                open=excluded.open, high=excluded.high, low=excluded.low, close=excluded.close,
                volume=excluded.volume, source=excluded.source, ingested_at=excluded.ingested_at
            """,
            rows,
        )
        conn.commit()
    return len(rows)


def read_bars(ticker: str, timeframe: str, as_of: str, limit: int | None = None) -> pd.DataFrame:
    """Return the newest requested bars at or before the cutoff, in ascending order."""
    conn = connect()
    query = (
        "SELECT ts, open, high, low, close, volume, source FROM bars "
        "WHERE ticker = ? AND timeframe = ? AND ts <= ? ORDER BY ts DESC"
    )
    params: list = [ticker, timeframe, as_of]
    if limit is not None:
        query += " LIMIT ?"
        params.append(int(limit))
    with _LOCK:
        rows = conn.execute(query, params).fetchall()
        conn.commit()
    if not rows:
        return pd.DataFrame(
            columns=["open", "high", "low", "close", "volume", "source"],
            index=pd.DatetimeIndex([], name=None),
        )
    rows = list(reversed(rows))
    idx = pd.DatetimeIndex([pd.Timestamp(row["ts"].removesuffix("Z")) for row in rows])
    return pd.DataFrame(
        {
            "open": [float(row["open"]) for row in rows],
            "high": [float(row["high"]) for row in rows],
            "low": [float(row["low"]) for row in rows],
            "close": [float(row["close"]) for row in rows],
            "volume": [float(row["volume"]) for row in rows],
            "source": [row["source"] for row in rows],
        },
        index=idx,
    )


def read_coverage(ticker: str, timeframe: str, as_of: str) -> tuple[int, str | None, str | None]:
    conn = connect()
    with _LOCK:
        row = conn.execute(
            "SELECT COUNT(*) AS n, MIN(ts) AS lo, MAX(ts) AS hi FROM bars "
            "WHERE ticker = ? AND timeframe = ? AND ts <= ?",
            (ticker, timeframe, as_of),
        ).fetchone()
        conn.commit()
    return int(row["n"] or 0), row["lo"], row["hi"]


def sweep_intraday(now: datetime | None = None, retention_days: int | None = None) -> int:
    """Delete intraday bars older than the retention window; daily bars persist."""
    days = intraday_retention_days() if retention_days is None else int(retention_days)
    if days <= 0:
        return 0
    cutoff = (now or datetime.now(timezone.utc)) - timedelta(days=days)
    conn = connect()
    with _LOCK:
        cur = conn.execute(
            "DELETE FROM bars WHERE timeframe NOT IN (?, ?) AND ts < ?",
            ("1D", WEEKLY_TIMEFRAME, cutoff),
        )
        conn.commit()
    return cur.rowcount or 0


def store_health() -> dict:
    """PostgreSQL bar-store diagnostics. Credentials are never exposed."""
    info: dict = {
        "backend": "postgresql",
        "target": None,
        "schema": database_schema(),
        "writable": False,
        "rows": 0,
        "tickers": 0,
        "timeframes": [],
        "intradayRetentionDays": intraday_retention_days(),
    }
    try:
        info["target"] = database_target()
        conn = connect()
        with _LOCK:
            row = conn.execute(
                "SELECT COUNT(*) AS n, COUNT(DISTINCT ticker) AS t FROM bars"
            ).fetchone()
            tfs = conn.execute("SELECT DISTINCT timeframe FROM bars ORDER BY timeframe").fetchall()
            privilege = conn.execute(
                "SELECT has_table_privilege(current_user, 'bars', 'INSERT,UPDATE,DELETE') AS ok"
            ).fetchone()
            conn.commit()
        info["rows"] = int(row["n"] or 0)
        info["tickers"] = int(row["t"] or 0)
        info["timeframes"] = [row["timeframe"] for row in tfs]
        info["writable"] = bool(privilege["ok"])
    except Exception as exc:  # noqa: BLE001 — health reports dependency failure
        info["error"] = str(exc)
    return info
