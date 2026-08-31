# nvdactl — v1 Technical Documentation

**A local, agent-based stock chart analysis tool.**
Single-user. Runs on M2 Pro / 16GB. No cloud model dependency.

---

## 0. What this is (and what it is not)

**Is:** A tool that automates the manual work of pulling a stock's price data, computing a standard indicator suite, and producing a structured, repeatable analytical read (bull case / bear case / key levels / invalidation) using a locally-run LLM.

**Is not:** A predictor. It does not tell you to buy or sell. It assembles and structures the same evidence you'd gather by hand, faster and consistently. **Every forecasting claim is directional context, not a signal. The decision stays with you.**

This distinction is load-bearing. It determines scope, sets expectations, and keeps the tool honest. If v1 starts emitting "BUY" verdicts, it has failed its own design.

---

## 1. Goals & non-goals for v1

### Goals
- G1. Fetch daily + hourly OHLCV for a single ticker (NVDA as the working example).
- G2. Compute a fixed indicator suite deterministically (no ML).
- G3. Detect the current technical regime from those indicators (rules, not a model).
- G4. Feed a structured summary to a local LLM and get back a consistent, formatted analytical read.
- G5. Run entirely offline after data fetch, on 16GB RAM, one model at a time.
- G6. Be runnable end-to-end from a single command.

### Non-goals (explicitly deferred)
- N1. Multi-ticker portfolios or watchlists.
- N2. Backtesting or strategy evaluation.
- N3. Live/streaming intraday data.
- N4. News sentiment (deferred to v2 — see §9).
- N5. Chronos or any time-series forecasting model (deferred — see §9).
- N6. Web UI / dashboard (CLI only for v1).
- N7. Tool-calling agent loop (v1 is a *linear pipeline*; the agent loop is v1.5 — see §7).
- N8. Persistence / historical run storage.

---

## 2. Architecture

Two layers, deliberately decoupled.

```
┌─────────────────────────────────────────────────────────┐
│  ORCHESTRATOR  (main.py — linear pipeline for v1)       │
│                                                          │
│   1. fetch ──▶ 2. indicators ──▶ 3. regime ──▶          │
│                4. build prompt ──▶ 5. LLM ──▶ 6. render  │
└───────┬───────────────────────┬─────────────────────────┘
        │                       │
        ▼                       ▼
┌───────────────────┐   ┌──────────────────────────┐
│  DATA/ANALYSIS    │   │  MODEL LAYER             │
│  LAYER            │   │                          │
│  (deterministic)  │   │  Ollama  /api/chat       │
│                   │   │  qwen2.5:7b              │
│  yfinance         │   │  localhost:11434         │
│  pandas-ta        │   │                          │
└───────────────────┘   └──────────────────────────┘
```

**Why decoupled:** the data layer is deterministic and testable in isolation; the model layer is swappable (change one function to try llama3.1:8b or, later, a hosted model). You can develop and debug each independently — which, per the sequencing rule, is what keeps the project from stalling.

---

## 3. Component breakdown

### 3.1 Data layer — `data.py`

**Responsibility:** ticker string in, clean indicator-enriched DataFrame out.

| Function | Signature | Returns |
|---|---|---|
| `fetch_ohlcv` | `(ticker: str, period: str, interval: str)` | `pd.DataFrame` (OHLCV) |
| `add_indicators` | `(df: pd.DataFrame)` | `pd.DataFrame` (+ indicator cols) |

**Indicator suite (fixed for v1):**
- RSI (14)
- MACD (12/26/9) — line, signal, histogram
- SMA 50, SMA 200
- EMA 20
- Bollinger Bands (20, 2σ)
- ATR (14) — for volatility / stop context
- Volume + 20-period volume SMA

**Data source:** `yfinance` primary, **Finnhub free tier as fallback** (resolved — see §8, GAP-1). yfinance is an unofficial scraper and breaks without warning; on an empty/malformed frame, `fetch_ohlcv` falls back to Finnhub's candle endpoint, returning the same DataFrame shape so downstream code doesn't care which source served the data.

### 3.2 Regime detection — `regime.py`

**Responsibility:** collapse the latest indicator row into a set of plain-language boolean/categorical facts. **Pure rules, no ML.** This is what makes the LLM's job tractable — it reasons over facts, not raw numbers.

| Fact | Rule |
|---|---|
| Trend | `close > SMA200` → uptrend; `<` → downtrend |
| Short-term trend | `close vs SMA50`, `SMA50 vs SMA200` (golden/death cross proximity) |
| Momentum | RSI band: <30 oversold, 30–70 neutral, >70 overbought |
| MACD state | histogram sign + recent cross (last N bars) |
| Volatility | ATR vs its own 20-bar average (expanding/contracting) |
| Price vs bands | inside / riding upper / riding lower / squeeze |
| Volume | latest vs 20-SMA (confirmation or divergence) |

Output: a small dict of strings. Deterministic, unit-testable, no API needed.

### 3.3 Prompt builder — `prompt.py`

**Responsibility:** turn the regime dict + a compact recent-price table into a single well-structured prompt that *forces* consistent output.

**This is where most of the quality lives.** The model is only as good as the structure you impose. The prompt must:
- State the role (technical analysis assistant, not advisor).
- Provide the regime facts as a clean list.
- Provide the last ~10 bars of key values.
- **Demand a fixed output schema:** Bull Case / Bear Case / Key Levels (support & resistance) / What Would Invalidate Each / Summary read.
- Explicitly forbid a buy/sell recommendation.

Output schema is enforced in the prompt text, and validated loosely on the way out (see GAP-4).

### 3.4 Model layer — `llm.py`

**Responsibility:** one function, `analyze(prompt: str) -> str`. Calls Ollama `/api/chat`, `stream: false`, returns the text.

- Model: `qwen2.5:7b` (~5GB, fits budget with headroom).
- Endpoint: `http://localhost:11434/api/chat`.
- Alternate (one-line swap): `llama3.1:8b`.
- **Constraint:** one model loaded at a time. If Chronos is ever added, it runs sequentially, never concurrently (§9, memory budget).

### 3.5 Orchestrator — `main.py`

Wires the six steps linearly. Hardcoded ticker via CLI arg for v1. No branching, no agent decisions yet — that's deliberate (§7).

```
python main.py NVDA
```

### 3.6 Renderer — inline in `main.py`

Prints the model's structured read to the terminal, plus a header block showing the raw regime facts (so you can see what the model was reasoning over — critical for trust and debugging).

---

## 4. Data flow (concrete)

```
"NVDA"
  │
  ▼  fetch_ohlcv("NVDA", "6mo", "1d")
DataFrame[date, O,H,L,C,V]           ~126 rows
  │
  ▼  add_indicators(df)
DataFrame[... + RSI, MACD×3, SMA50, SMA200, EMA20, BB×3, ATR, VolSMA]
  │
  ▼  detect_regime(df.iloc[-1], df.tail(N))
{trend:"uptrend", momentum:"neutral", macd:"bullish cross 2 bars ago", ...}
  │
  ▼  build_prompt(regime, df.tail(10))
"<role> ... <facts> ... <last 10 bars> ... <required schema>"
  │
  ▼  analyze(prompt)   → Ollama → qwen2.5:7b
"## Bull Case ... ## Bear Case ... ## Key Levels ... ## Invalidation ..."
  │
  ▼  render(regime_header + model_output)
[terminal]
```

---

## 5. Tech stack

| Layer | Choice | Why |
|---|---|---|
| Language (v1) | Python 3.11+ | Fastest to prototype; `pandas-ta` + `yfinance` ecosystem |
| Data | yfinance + Finnhub | yfinance primary (free, no key); Finnhub free tier as fallback (60 req/min, key in env var) |
| Indicators | pandas-ta | Comprehensive, one-line appends |
| Model runtime | Ollama | Simplest local serving on Apple Silicon |
| Model | qwen2.5:7b | Strong tool-calling & reasoning, fits 16GB with room |
| Interface | CLI (argparse) | v1 scope |

**Note on Go:** your standard stack is Go. v1 is Python because the data-science libraries make prototyping the indicator/prompt logic dramatically faster, and this is a personal single-user tool where iteration speed dominates. Porting the data layer to Go (Chi/pgx-style service) is a reasonable v2 move once the logic is proven — but do not front-load that cost. Prove the pipeline in Python first.

---

## 6. Repository layout

```
nvdactl/
├── main.py            # orchestrator + renderer
├── data.py            # fetch_ohlcv, add_indicators
├── regime.py          # detect_regime (pure rules)
├── prompt.py          # build_prompt (schema enforcement)
├── llm.py             # analyze() → Ollama
├── config.py          # ticker defaults, indicator params, model name, FINNHUB_API_KEY (from env)
├── requirements.txt   # yfinance, pandas, pandas-ta, finnhub-python, requests
├── .env               # FINNHUB_API_KEY=...  (gitignored, never committed)
├── .gitignore         # must include .env
├── tests/
│   ├── test_regime.py     # deterministic — no network, no model
│   └── fixtures/          # saved DataFrames for offline testing
└── README.md
```

**Secrets:** the Finnhub key lives in `.env`, loaded via `python-dotenv` in `config.py` (`os.getenv("FINNHUB_API_KEY")`). `.env` is gitignored from day one. Even for a personal repo, don't hardcode — you'll forget it's there the day you push the repo public or share it.

---

## 7. The linear-vs-agent decision (important)

v1 is a **linear pipeline**, not an agent. The steps run in fixed order every time.

**Why not start with the agent loop?** Because "agent" means the LLM decides *which* tools to call and *when* — and debugging a non-deterministic control flow on top of an unproven data layer and an unproven prompt is where these projects die. You'd never know if a bad output came from the data, the prompt, or the model's tool-choice.

**The path:**
- **v1** — linear. Fetch → compute → regime → prompt → LLM → render. Every run identical structure.
- **v1.5** — introduce Ollama tool-calling. Expose `fetch_ohlcv`, `add_indicators`, maybe `get_news` as tools; let qwen decide what it needs. Only attempt this once v1's outputs are trustworthy, because now you have a known-good baseline to compare against.

This staging is a hard recommendation, not a preference. Get the deterministic version producing reads you trust, *then* hand the model the steering wheel.

---

## 8. Gaps, risks & open questions

These are the things that are unresolved, fragile, or will bite you. Ordered by how likely they are to cause pain.

### GAP-1 — yfinance fragility (HIGH) — ✅ RESOLVED
Unofficial scraper; Yahoo changes their endpoints and it breaks silently or returns empty frames. **Mitigation:** wrap `fetch_ohlcv` so empty/malformed results raise loudly rather than flowing garbage downstream.

**Decision (resolved):** Finnhub free tier registered as the fallback data source. 60 requests/minute, free for personal use — vastly more than a single-ticker tool needs (a run is a handful of calls). Covers real-time US quotes, company news, and SEC filings on the free plan.

**Implementation notes:**
- Store the key in an env var (`FINNHUB_API_KEY`), never hardcoded. Load via `config.py`.
- Architecture: yfinance primary, Finnhub fallback. `fetch_ohlcv` tries yfinance; on empty/malformed frame, falls back to Finnhub's candle endpoint. Both return the same DataFrame shape so downstream code is source-agnostic.
- **For v2 news (GAP-4/§9):** use Finnhub's **REST** news endpoint, *not* the WebSocket news feed — the WebSocket news stream has documented issues delivering only stale/historical items during market hours.
- Free-tier limits that don't affect us: deep fundamentals (full financial statements) and international/non-US market data require paid. Neither is on the roadmap. If that ever changes, Premium is $11.99–$99.99/mo.

### GAP-2 — Hallucinated numbers (HIGH)
The LLM may invent price levels or misread the indicator data in the prompt. It's a language model, not a calculator. **Mitigation:** compute *all* key levels (support/resistance from recent swing highs/lows, the actual MA values) deterministically in `regime.py` and pass them in explicitly, so the model quotes given numbers rather than inventing them. Never let the model derive a number you could have computed. This is partly a §3.2 responsibility — the regime layer should do more numeric work precisely to keep the model on rails.

### GAP-3 — Model output consistency (MEDIUM)
Even with a demanded schema, a 7B model occasionally drifts (skips a section, editorializes, sneaks in a recommendation). **Mitigation:** loose validation on the output — check the required section headers are present; if not, one retry with a firmer prompt. **Open question:** how strict? For a personal tool, a warning line ("⚠ model omitted Invalidation section") may be enough vs. hard re-prompting.

### GAP-4 — No output validation/parsing yet (MEDIUM)
v1 prints raw model text. There's no structured object you could later store, diff across days, or feed onward. **Deferred to v2** but flagged: if you'll ever want "compare today's read to last week's," you need the output as structured data (JSON schema out of the model), not prose. Design the prompt now so retrofitting JSON later is easy.

### GAP-5 — Timeframe ambiguity (MEDIUM)
"Worth buying this week" is a *swing* horizon, but daily bars over 6mo and hourly bars tell different stories. v1 fetches both but the regime/prompt logic must be explicit about *which timeframe each fact comes from*, or the read blurs horizons. **Decision needed:** is v1's read daily-primary with hourly as texture, or genuinely multi-timeframe? Recommend daily-primary for simplicity; state it in the output.

### GAP-6 — Cold-start memory pressure (LOW–MEDIUM)
qwen2.5:7b + Python + pandas + a browser open can approach the ceiling. First model call after boot is slow (model load into memory). **Mitigation:** none needed structurally — just close heavy apps, and expect a ~10–20s first-call latency. Do *not* try to also load Chronos in v1.

### GAP-7 — No error surface for market-closed / bad ticker (LOW)
Weekends, holidays, delisted/typo'd tickers. **Mitigation:** validate the returned frame is non-empty and recent; fail with a clear message.

### GAP-8 — Overfitting trust to a confident model (LOW, but insidious)
A fluent 7B model *sounds* authoritative regardless of correctness. The single biggest behavioral risk of the whole project is you starting to treat the read as a signal because it's well-written. **Mitigation is discipline, not code:** the regime-facts header (§3.6) exists so you always see the raw evidence next to the prose and judge for yourself.

---

## 9. Deferred features & their entry conditions

Don't build these until the trigger is met.

| Feature | Build it when… | Notes |
|---|---|---|
| News sentiment (v2) | v1 reads are trusted and you want catalyst context | Finnhub news endpoint → summarize → add as regime facts |
| Chronos forecasting | You specifically want a directional numeric prior | `chronos-t5-small`/`-base` only; run **sequentially** with qwen, never concurrent. Expect wide, often-wrong intervals — treat as one input among many |
| Tool-calling agent (v1.5) | v1 baseline outputs are known-good | See §7 |
| Structured JSON output | You want to store/diff runs over time | See GAP-4; design prompt for it now |
| Go port of data layer | Logic is proven & stable in Python | Your native stack; only worth it post-validation |
| Multi-ticker / watchlist | Single-ticker flow is solid | Loop + rate-limit awareness |
| Web dashboard | You outgrow the CLI | Not before |

**Memory budget reminder (16GB):** usable model budget ≈ 8–10GB after OS/apps. qwen2.5:7b ≈ 5GB. That's your v1 headroom. Anything that loads a second model simultaneously breaks the budget → runs sequentially or not at all.

---

## 10. Definition of done for v1

v1 is complete when:
1. `python main.py NVDA` runs end to end with no manual steps.
2. It prints the regime-facts header + a schema-conformant analytical read.
3. `tests/test_regime.py` passes offline (no network, no model) against saved fixtures.
4. A bad/empty ticker fails with a clear message, not a stack trace mid-pipeline.
5. The output never contains a buy/sell recommendation.
6. You've run it across at least a few market days and the reads are *consistent in structure* and *numerically grounded* (no invented levels).

When all six hold, you have a trustworthy baseline — and *only then* is v1.5 (the agent loop) worth starting.

---

## 11. Immediate next actions

1. **Confirm the two environment checks pass** (Ollama returns text; yfinance+pandas-ta prints NVDA with indicator columns). These are independent — do them in parallel.
2. **Decide GAP-5 now** — daily-primary vs multi-timeframe. (GAP-1 is resolved: Finnhub free tier is the fallback; put your key in a `FINNHUB_API_KEY` env var.) The timeframe choice still changes the code you write on day one.
3. **Build `regime.py` first** (after data), because it's pure, testable, and it's the layer that keeps the model numerically honest (GAP-2).
4. **Then `prompt.py`** — spend real effort here; it's most of the quality.
5. **Wire `main.py` linearly.** Resist the agent loop until §10 is met.