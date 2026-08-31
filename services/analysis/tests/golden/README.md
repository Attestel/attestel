# Golden payloads — `DOCKER_GATE.md` check 2.5

Check 2.5 asks one question: **has anything that shipped before changed?** Lane 1B answered it
("golden diff shows one change: the additive `barStore` block on `/health`") and committed nothing,
so the gate had no artifact and recorded the check unrun. A verification that cannot be repeated is
not a verification. These files are that answer, in re-runnable form.

## Capture provenance

| | |
|---|---|
| **Commit** | `2fef3b3` — *Wave 2 Lane 2A: event normalization (landed out of order)*, i.e. `master` at capture time |
| **Environment** | Python 3.11.15, pandas 2.2.3, numpy 2.2.1, fastapi 0.115.6 (`services/analysis/.venv`), macOS |
| **Network** | none — every provider is patched to raise; a capture that reaches one fails loudly |
| **Store** | an isolated PostgreSQL schema seeded with `test_pit.known_bars`, 400×`1D` (`2025-01-01`, `B`, seed 11) and 400×`1H` (`2025-11-03 09:30`, `h`, seed 12) |
| **Request** | `GET /analysis/NVDA?as_of=2026-03-02T00:00:00Z&n=60` and `GET /health` |

Change any row of that table and the payloads legitimately change. That is why the table is here and
not in a commit message.

## Why the exact golden is captured at a past `as_of`

An exact golden needs an exact input, and a live `/analysis/NVDA` has neither:

1. real bars change every session; and
2. **the offline fallback is not deterministic either.** `data._synthetic` seeds its RNG from
   `hash((ticker, timeframe))`, and Python salts `hash()` of a `str` per process. Its docstring says
   "deterministic pseudo-random walk", which is true inside one process and false across restarts —
   `test_pit.py` already documents this. It also anchors its date range on `now`.

So the golden is captured the one way the numbers are genuinely fixed: a store seeded with a
`default_rng(seed)` series and a request at a fixed cutoff. Every indicator, regime string and
rounded price is then pinned, and an indicator formula change shows up as a numeric diff.

`n=60` bounds only how many points the response *emits*; indicators are always computed over
`max(n, 260) + INDICATOR_WARMUP` bars, so the numbers are identical to a default request and the
committed file is 55 KB instead of 230 KB.

## The files

| File | What it pins |
|---|---|
| `analysis_NVDA_1D.json` | the **exact** payload of the captured request — every value |
| `health.json` | the exact `/health` payload, one field normalised (below) |
| `analysis_NVDA_1D.shape.json` | its **shape**: every scalar leaf collapsed to its type. What a *live* service is compared against |
| `health.shape.json` | the same for `/health` |
| `pre_wave1_1b.shape.json` | the shapes of `/health` and `/analysis/NVDA` at **`36a47e8`**, the commit immediately before Lane 1B. This is 1B's claim, frozen |
| `normalize.py` | the one implementation of normalisation, shape and shape-diff, shared by the pytest and `scripts/check-golden.py` |

## Normalised fields — the complete list

There is no wildcard, no regex and no fuzzy match. Each rule is a named field at a named path, and a
rule naming a field that no longer exists **raises** rather than silently skipping — a stale rule
that quietly stops normalising is how a golden stops checking.

| Ruleset | Path | Placeholder | Why |
|---|---|---|---|
| `health` | `barStore.target`, `barStore.schema` | `<POSTGRES>`, `<SCHEMA>` | credentials are never exposed; pytest schemas are isolated |
| `health_live` | `barStore.target`, `barStore.schema` | `<POSTGRES>`, `<SCHEMA>` | as above |
| `health_live` | `barStore.rows` | `<ROWS>` | a container's row count depends on how much it has ingested |
| `health_live` | `barStore.tickers` | `<TICKERS>` | as above |
| `health_live` | `barStore.timeframes` | `<TIMEFRAMES>` | as above |

**`/analysis/{ticker}` at a fixed `as_of` has no volatile field at all** — `price.asOf` is the
timestamp of the last stored bar at or before the cutoff, so it is pinned by the request rather than
by the clock. Nothing in that payload is normalised. Under pytest, `barStore.rows` / `tickers` /
`timeframes` are *not* normalised either: the seed makes them exact (800 / 1 / `["1D","1H"]`), and a
store that silently stopped writing must fail.

## Running it

```bash
# the pytest — exact values, shapes, and the pre-1B comparison
cd services/analysis && python -m pytest -q tests/test_golden.py

# a RUNNING container or service — shape only, because its values are real market data
python3 scripts/check-golden.py --base http://localhost:8001
```

`scripts/check-golden.py` exits 0 on a match, 1 on drift with every differing path printed, and
**2 when it cannot reach the service** — an unreachable service is a failed check, never a pass
(contract §9.44).

## Do not regenerate to make a failure go away

`GOLDEN_REGEN=1 python -m pytest -q tests/test_golden.py` rewrites every file here. A failure is a
**finding**: read the diff, work out what changed and why, and only then decide whether the new
output is correct. Regenerating first and looking second is the one failure mode this directory
exists to prevent.
