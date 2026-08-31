#!/usr/bin/env python3
"""One-time, read-only import of legacy SQLite event/bar stores into PostgreSQL.

SQLite is intentionally imported only by this migration utility. Neither runtime service imports
it or accepts a file-backed configuration. Target tables must already exist (start the PostgreSQL-
backed analysis and events services once so their migrations run). Inserts are idempotent: existing
primary/unique keys are left untouched.
"""
from __future__ import annotations

import argparse
import json
import os
import sqlite3
from datetime import date, datetime
from pathlib import Path

import psycopg
from psycopg import sql
from psycopg.types.json import Jsonb

EVENT_TABLES = (
    "source_documents",
    "ingest_runs",
    "events",
    "event_state_history",
    "event_documents",
    "event_tickers",
    "macro_events",
    "provider_budget",
    "scheduled_events",
    "scheduled_event_history",
    "predictions",
    "prediction_evidence",
    "outcomes",
)


def _database_url(explicit: str | None) -> str:
    value = (
        (explicit or "").strip()
        or os.getenv("DATABASE_URL", "").strip()
        or os.getenv("EVENTS_DATABASE_URL", "").strip()
        or os.getenv("BARS_DATABASE_URL", "").strip()
    )
    if not value.startswith(("postgresql://", "postgres://")):
        raise SystemExit("provide --database-url (postgresql://...) or DATABASE_URL")
    return value


def _sqlite_tables(conn: sqlite3.Connection) -> set[str]:
    return {
        row[0]
        for row in conn.execute("SELECT name FROM sqlite_master WHERE type = 'table'").fetchall()
    }


def _target_columns(conn: psycopg.Connection, schema: str, table: str) -> list[tuple[str, str]]:
    rows = conn.execute(
        """
        SELECT column_name, data_type
        FROM information_schema.columns
        WHERE table_schema = %s AND table_name = %s AND is_identity = 'NO'
        ORDER BY ordinal_position
        """,
        (schema, table),
    ).fetchall()
    if not rows:
        raise RuntimeError(
            f"target {schema}.{table} is missing; start the owning service to run migrations"
        )
    return [(row[0], row[1]) for row in rows]


def _source_columns(conn: sqlite3.Connection, table: str) -> set[str]:
    return {row[1] for row in conn.execute(f'PRAGMA table_info("{table}")').fetchall()}


def _timestamp(value):
    if value is None or isinstance(value, datetime):
        return value
    text = str(value).strip()
    if text.endswith("Z"):
        text = text[:-1] + "+00:00"
    return datetime.fromisoformat(text)


def _date(value):
    if value is None or isinstance(value, date):
        return value
    return date.fromisoformat(str(value))


def _convert(value, data_type: str, *, table: str, column: str):
    if value is None:
        return None
    if table == "bars" and column == "source" and str(value).lower() == "alpaca:iex":
        return "alpaca"
    if data_type == "jsonb":
        parsed = json.loads(value) if isinstance(value, str) else value
        return Jsonb(parsed)
    if data_type == "timestamp with time zone":
        return _timestamp(value)
    if data_type == "date":
        return _date(value)
    return value


def _copy_table(
    source: sqlite3.Connection,
    target: psycopg.Connection,
    *,
    schema: str,
    table: str,
    dry_run: bool,
) -> int:
    target_columns = _target_columns(target, schema, table)
    available = _source_columns(source, table)
    columns = [(name, kind) for name, kind in target_columns if name in available]
    if not columns:
        return 0

    names = [name for name, _kind in columns]
    select_query = "SELECT " + ", ".join(f'"{name}"' for name in names) + f' FROM "{table}"'
    if table == "bars":
        # The PostgreSQL baseline enforces the current reproducibility contract even if a legacy
        # file predates it.
        select_query += " WHERE timeframe <> '1W' AND source <> 'synthetic'"

    insert_query = sql.SQL("INSERT INTO {}.{} ({}) VALUES ({}) ON CONFLICT DO NOTHING").format(
        sql.Identifier(schema),
        sql.Identifier(table),
        sql.SQL(", ").join(map(sql.Identifier, names)),
        sql.SQL(", ").join(sql.Placeholder() for _ in names),
    )

    attempted = 0
    cursor = source.execute(select_query)
    while True:
        batch = cursor.fetchmany(1000)
        if not batch:
            break
        attempted += len(batch)
        if not dry_run:
            converted = [
                tuple(
                    _convert(value, kind, table=table, column=name)
                    for value, (name, kind) in zip(row, columns)
                )
                for row in batch
            ]
            with target.cursor() as target_cursor:
                target_cursor.executemany(insert_query, converted)
    return attempted


def _copy_database(
    target: psycopg.Connection,
    path: Path,
    *,
    schema: str,
    tables: tuple[str, ...],
    dry_run: bool,
) -> list[tuple[str, int]]:
    if not path.is_file():
        raise FileNotFoundError(path)
    source = sqlite3.connect(f"file:{path.resolve()}?mode=ro", uri=True)
    try:
        available = _sqlite_tables(source)
        copied = []
        for table in tables:
            if table not in available:
                continue
            copied.append(
                (table, _copy_table(source, target, schema=schema, table=table, dry_run=dry_run))
            )
        return copied
    finally:
        source.close()


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--database-url", help="target PostgreSQL URL; defaults to DATABASE_URL")
    parser.add_argument("--events-schema", default="public")
    parser.add_argument("--bars-schema", default="analysis")
    parser.add_argument("--events-db", type=Path, help="legacy events SQLite file")
    parser.add_argument(
        "--bars-db", type=Path, action="append", default=[], help="legacy bars file (repeatable)"
    )
    parser.add_argument("--dry-run", action="store_true", help="validate/count without inserting")
    args = parser.parse_args()
    if args.events_db is None and not args.bars_db:
        parser.error("provide --events-db and/or --bars-db")

    url = _database_url(args.database_url)
    results: list[tuple[str, str, int]] = []
    with psycopg.connect(url) as target:
        try:
            if args.events_db is not None:
                for table, count in _copy_database(
                    target,
                    args.events_db,
                    schema=args.events_schema,
                    tables=EVENT_TABLES,
                    dry_run=args.dry_run,
                ):
                    results.append((str(args.events_db), f"{args.events_schema}.{table}", count))
            for path in args.bars_db:
                for table, count in _copy_database(
                    target,
                    path,
                    schema=args.bars_schema,
                    tables=("bars",),
                    dry_run=args.dry_run,
                ):
                    results.append((str(path), f"{args.bars_schema}.{table}", count))
            if args.dry_run:
                target.rollback()
            else:
                target.commit()
        except Exception:
            target.rollback()
            raise

    mode = "validated" if args.dry_run else "imported/idempotently examined"
    for source, table, count in results:
        print(f"{mode}: {count} row(s) from {source} -> {table}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
