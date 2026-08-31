"""Prediction service config (12-factor). Sensible localhost defaults for zero-config dev."""
from __future__ import annotations

import os

PORT = int(os.getenv("PORT", "8003"))
ANALYSIS_URL = os.getenv("ANALYSIS_URL", "http://localhost:8001").rstrip("/")
PREDICTION_DATABASE_URL = os.getenv("PREDICTION_DATABASE_URL", "").strip() or os.getenv(
    "DATABASE_URL", ""
).strip()
PREDICTION_DATABASE_SCHEMA = os.getenv("PREDICTION_DATABASE_SCHEMA", "prediction")
MODELS_DIR = os.getenv("MODELS_DIR", os.path.join(os.getcwd(), "data", "models"))
# Chronos is an OPTIONAL secondary "expected path" overlay, off by default. The core LightGBM
# signal never depends on it, and it is never loaded alongside qwen (16 GB budget).
ENABLE_CHRONOS = os.getenv("ENABLE_CHRONOS", "false").lower() in ("1", "true", "yes")

# Backtest cost assumptions (basis points, per position change). Conservative retail defaults.
COMMISSION_BPS = float(os.getenv("COMMISSION_BPS", "1.0"))
SLIPPAGE_BPS = float(os.getenv("SLIPPAGE_BPS", "5.0"))

# Offline edge-evaluation harness output (evaluate.py). The LIVE services read one thing from it:
# PostgreSQL stores production verdict/report evidence. EVAL_OUT_DIR remains the transient runner
# directory and the explicit no-database file fallback.
EVAL_OUT_DIR = os.getenv("EVAL_OUT_DIR", os.path.join(os.getcwd(), "data", "eval"))

# Earnings features (context.py): cache-first history shared with the event-study harness
# (evaluate_events uses the same directory). With no key set, reads are cache-ONLY — the zero-keys
# path falls back to neutral features and never touches the network.
EARNINGS_CACHE_DIR = os.getenv(
    "EVENT_CACHE_DIR", os.path.join(os.getcwd(), "data", "eval", "earnings")
)
ALPHAVANTAGE_API_KEY = os.getenv("ALPHAVANTAGE_API_KEY", "")

# Committee features (committee_features.py): the llm service's persisted analyst-committee
# snapshots as CANDIDATE features. Read-only; the llm service owns writes. Production reads the
# `llm` PostgreSQL schema; COMMITTEE_READS_DIR is the explicit no-database fallback. LLM stances
# cannot be backfilled without hindsight, so the features stay NEUTRAL until at least
# MIN_COMMITTEE_SNAPSHOTS real snapshot days exist — the signal is unchanged until the committee
# has earned a track record.
COMMITTEE_READS_DIR = os.getenv(
    "COMMITTEE_READS_DIR",
    os.path.join(os.path.dirname(os.getcwd()), "llm", "data", "reads"),
)
MIN_COMMITTEE_SNAPSHOTS = int(os.getenv("MIN_COMMITTEE_SNAPSHOTS", "60"))
