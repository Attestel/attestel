"""PostgreSQL repository for durable LLM research snapshots and custom personas."""
from __future__ import annotations

import json
import os
import re
import threading
from contextlib import contextmanager
from pathlib import Path

_SCHEMA_RE = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")
_lock = threading.Lock()
_prepared: set[tuple[str, str]] = set()


def database_url() -> str:
    return os.getenv("LLM_DATABASE_URL", "").strip() or os.getenv("DATABASE_URL", "").strip()


def enabled() -> bool:
    return bool(database_url())


def schema() -> str:
    value = os.getenv("LLM_DATABASE_SCHEMA", "llm").strip() or "llm"
    if not _SCHEMA_RE.fullmatch(value):
        raise RuntimeError("LLM_DATABASE_SCHEMA must be a PostgreSQL identifier")
    return value


def _driver():
    try:
        import psycopg  # type: ignore
        from psycopg import sql  # type: ignore
    except ImportError as exc:  # pragma: no cover
        raise RuntimeError("LLM PostgreSQL storage is configured but psycopg is not installed") from exc
    return psycopg, sql


def _import_files(conn) -> None:
    if conn.execute("SELECT 1 FROM legacy_imports WHERE name='llm-files-v1'").fetchone():
        return
    reads_dir = Path(os.getenv("READS_DIR", Path.cwd() / "data" / "reads"))
    if reads_dir.is_dir():
        for ticker_dir in reads_dir.iterdir():
            if not ticker_dir.is_dir() or ticker_dir.name.startswith("_"):
                continue
            ticker = ticker_dir.name.upper()
            groups = [
                ("read", ticker_dir.glob("*.json")),
                ("committee", (ticker_dir / "committee").glob("*.json")),
                ("transcript", (ticker_dir / "transcripts").glob("*.json")),
            ]
            for kind, paths in groups:
                for path in paths:
                    try:
                        record = json.loads(path.read_text())
                    except (OSError, json.JSONDecodeError):
                        continue
                    key = str(record.get("date") or record.get("key") or path.stem)
                    conn.execute(
                        "INSERT INTO snapshots(kind,ticker,snapshot_key,user_id,data) "
                        "VALUES(%s,%s,%s,'',%s::jsonb) ON CONFLICT DO NOTHING",
                        (kind, ticker, key, json.dumps(record)),
                    )
    personas_dir = Path(os.getenv("PERSONAS_DIR", reads_dir / "_personas"))
    if personas_dir.is_dir():
        for user_dir in personas_dir.iterdir():
            path = user_dir / "personas.json"
            if not user_dir.is_dir() or not path.is_file():
                continue
            try:
                record = json.loads(path.read_text())
            except (OSError, json.JSONDecodeError):
                continue
            conn.execute(
                "INSERT INTO snapshots(kind,ticker,snapshot_key,user_id,data) "
                "VALUES('personas','','current',%s,%s::jsonb) ON CONFLICT DO NOTHING",
                (user_dir.name, json.dumps(record)),
            )
    conn.execute("INSERT INTO legacy_imports(name) VALUES('llm-files-v1') ON CONFLICT DO NOTHING")


def _prepare(conn) -> None:
    _, sql = _driver()
    name = schema()
    conn.execute(sql.SQL("CREATE SCHEMA IF NOT EXISTS {}").format(sql.Identifier(name)))
    conn.execute(sql.SQL("SET search_path TO {}, public").format(sql.Identifier(name)))
    conn.execute("SELECT set_config('statement_timeout','15000',false)")
    prepared_key = (database_url(), name)
    if prepared_key in _prepared:
        conn.commit()
        return
    with _lock:
        if prepared_key not in _prepared:
            conn.execute(
                "CREATE TABLE IF NOT EXISTS schema_migrations("
                "version TEXT PRIMARY KEY,applied_at TIMESTAMPTZ NOT NULL DEFAULT now())"
            )
            conn.execute(
                "CREATE TABLE IF NOT EXISTS snapshots("
                "kind TEXT NOT NULL,ticker TEXT NOT NULL DEFAULT '',snapshot_key TEXT NOT NULL,"
                "user_id TEXT NOT NULL DEFAULT '',data JSONB NOT NULL,"
                "updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),"
                "PRIMARY KEY(kind,ticker,snapshot_key,user_id))"
            )
            conn.execute(
                "CREATE INDEX IF NOT EXISTS snapshots_lookup_idx "
                "ON snapshots(kind,ticker,updated_at DESC)"
            )
            conn.execute(
                "CREATE TABLE IF NOT EXISTS legacy_imports("
                "name TEXT PRIMARY KEY,imported_at TIMESTAMPTZ NOT NULL DEFAULT now())"
            )
            conn.execute(
                "INSERT INTO schema_migrations(version) VALUES('001_snapshots_personas') "
                "ON CONFLICT DO NOTHING"
            )
            _import_files(conn)
            _prepared.add(prepared_key)
    conn.commit()


@contextmanager
def connection():
    psycopg, _ = _driver()
    url = database_url()
    if not url.startswith(("postgresql://", "postgres://")):
        raise RuntimeError("LLM_DATABASE_URL must use postgresql:// or postgres://")
    conn = psycopg.connect(url, connect_timeout=5)
    try:
        _prepare(conn)
        yield conn
        conn.commit()
    except Exception:
        conn.rollback()
        raise
    finally:
        conn.close()


def check() -> None:
    with connection() as conn:
        conn.execute("SELECT 1")


def save(kind: str, ticker: str, key: str, record: dict, user_id: str = "") -> None:
    with connection() as conn:
        conn.execute(
            "INSERT INTO snapshots(kind,ticker,snapshot_key,user_id,data,updated_at) "
            "VALUES(%s,%s,%s,%s,%s::jsonb,now()) ON CONFLICT(kind,ticker,snapshot_key,user_id) "
            "DO UPDATE SET data=excluded.data,updated_at=now()",
            (kind, ticker.upper(), key, user_id, json.dumps(record, default=str)),
        )


def list_records(kind: str, ticker: str = "", limit: int = 10, user_id: str = "") -> list[dict]:
    with connection() as conn:
        rows = conn.execute(
            "SELECT data FROM snapshots WHERE kind=%s AND ticker=%s AND user_id=%s "
            "ORDER BY snapshot_key DESC,updated_at DESC LIMIT %s",
            (kind, ticker.upper(), user_id, limit),
        ).fetchall()
    return [row[0] if isinstance(row[0], dict) else json.loads(row[0]) for row in rows]


def load_prediction_artifact(name: str) -> bytes | None:
    """Read one cross-service evaluation artifact from prediction's service-owned schema."""
    _, sql = _driver()
    prediction_schema = os.getenv("PREDICTION_DATABASE_SCHEMA", "prediction").strip() or "prediction"
    if not _SCHEMA_RE.fullmatch(prediction_schema):
        return None
    with connection() as conn:
        row = conn.execute(
            sql.SQL("SELECT payload FROM {}.artifacts WHERE name=%s").format(sql.Identifier(prediction_schema)),
            (os.path.basename(name),),
        ).fetchone()
    return None if row is None else bytes(row[0])
