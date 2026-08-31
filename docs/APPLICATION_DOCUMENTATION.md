# Attestel / attestel application documentation

**Document status:** current-state product and technical guide  
**Verified against the repository:** 2026-08-23  
**Audience:** users, product owners, operators, developers, QA, and future AI agents working in this repository

This is the consolidated guide to what the application does today: the customer experience, every page, the research workflow, automation, AI and quantitative systems, service ownership, data provenance, persistence, safety controls, and operations.

The codebase still uses **attestel** in several internal names, while the current product UI and customer-facing language use **Attestel**. This document uses **Attestel** for the product and **attestel** when referring to repository or legacy implementation names.

> Attestel is an investment-research and decision-review system. It helps a user collect evidence, form and challenge a thesis, monitor what changes, record decisions, and review outcomes. It is not a brokerage, does not custody money, and never places an order.

---

## 1. Executive summary

Attestel turns a collection of market data, filings, scheduled events, deterministic technical analysis, user research, and AI-assisted interpretation into a repeatable research loop:

1. **Orient** — see what changed and what is coming.
2. **Discover** — find market-moving or related events outside the current watchlist.
3. **Understand** — open a company file and inspect technical, fundamental, event, and transcript evidence.
4. **Form** — write a falsifiable investment thesis with assumptions, invalidations, catalysts, questions, and review commitments.
5. **Challenge** — apply research lenses, run scenarios, ask the copilot, or request a multi-specialist analyst review.
6. **Monitor** — follow companies, evaluate deterministic alert rules, and identify stale theses.
7. **Decide** — record an action or deliberate non-action with its contemporaneous evidence.
8. **Review** — compare outcomes with the original reasoning and separate process quality from market luck.

The primary product object is the **user-owned thesis**, not a chart and not an AI answer. AI outputs are evidence-bearing research artifacts. They do not replace the user’s thesis or decision.

### What the platform automates

The platform can automate:

- price, indicator, regime, confluence, volatility-band, and portfolio-risk calculations;
- event and economic-calendar ingestion from configured providers;
- event normalization, deduplication, entity linking, clustering, and optional AI enrichment;
- following feeds and discovery ranking over the stored event corpus;
- deterministic monitoring rules and notification delivery;
- stale-thesis detection and a durable re-synthesis queue;
- post-event reaction measurement after each horizon matures;
- walk-forward model training, evaluation, probability calibration, and signal gating;
- fail-closed paper validation of an already-qualified quantitative strategy;
- explicit immutable challenger training and evidence-gated model promotion/rollback;
- periodic operational dispatch of four model-free lanes, plus three explicit worker lanes, when
  explicitly enabled by an operator.

It deliberately does **not** automate:

- order execution, broker connectivity, deposits, withdrawals, or money movement;
- a user’s investment decision;
- silently adding evidence to a thesis unless the user has already established the relevant relationship and the contract permits it;
- provider ingestion from a browser page load;
- an AI call from quote polling, alert polling, or the Overview page's secondary event/calendar reads;
- a model-generated price target or a free-form buy/sell/hold recommendation.

---

## 2. Product principles and non-negotiable controls

### 2.1 Analysis, not execution

The deterministic analysis, AI read, copilot, lenses, committee, briefs, scenarios, and analyst pipeline are research tools. They cannot place orders. Manual trades in Journal are records of actions the user took elsewhere. Paper positions are simulations owned by the platform validation engine.

### 2.2 The model does not own market numbers

Technical indicators, support/resistance, expected-close ranges, portfolio weights, risk statistics, consensus arithmetic, and quantitative signal scores are calculated in code. When a model is allowed to mention computed support or resistance, the server re-imposes the original values after generation so the response cannot drift them.

### 2.3 Advice-shaped language is rejected

Model outputs are checked against banned advice and position-management phrases. Malformed or prohibited output is retried once where the workflow supports a retry, then replaced by a deterministic fallback. The specialist analyst has an additional banned list covering phrases such as accumulating, trimming, averaging down, and overweighting.

### 2.4 Provenance travels with the claim

The UI distinguishes live provider data, stored events, verified seed data, synthetic demonstration bars, deterministic calculations, model-authored interpretation, and offline stubs. A missing source is represented as unavailable or degraded; it is not silently converted to a real-looking value.

### 2.5 Point-in-time discipline

Historical analysis and model evaluation use an `asOf` cutoff. Analyst tools receive the cutoff from the runtime; the model cannot choose or override it. Stored-bar consumers cannot read future bars. Event-reaction windows remain pending until the required later bars actually exist.

### 2.6 Honest abstention

The system supports neutral, unclear, unavailable, insufficient-history, not-yet-validated, and offline-stub states as first-class results. It does not force a directional answer.

### 2.7 Offline and fail-soft operation

Once PostgreSQL is available, the application remains demonstrable without API keys or internet access:

- analysis can fall back to clearly labelled synthetic bars;
- the AI service can return deterministic `stub:offline` artifacts;
- verified NVIDIA seed data can populate relevant areas;
- unavailable providers degrade their own sections rather than taking down the application.

---

## 3. Information architecture and routing

The React application is a hash-routed single-page app. The route grammar is:

```text
#destination
#destination/subview
#destination/subview/tab
```

Some routes also accept `event` and `ticker` query parameters inside the hash. Unknown routes fall back to Today. Authentication screens (`#signin`, `#signup`) render outside the main shell.

The nine primary destinations, in customer-visible order, are:

| Destination | Research-loop stage | Default subview | Main purpose |
|---|---|---|---|
| Today | Orient | News | Answer what matters now and what needs the user’s attention |
| Following | Monitor | Changes | Show canonical events affecting followed companies |
| Explore | Orient | — | Discover relevant events beyond the follow set |
| Calendar | Orient · Monitor | — | Show upcoming, current, and historical catalysts |
| Research | Understand · Form · Challenge | Overview | Maintain one complete company research file |
| Watchlist | Monitor | Companies | Manage companies and deterministic monitoring rules |
| Journal | Decide · Review | Decisions | Record reasoning, outcomes, manual trades, and experiments |
| Library | Understand · Challenge | Saved transcripts/research | Retrieve durable research artifacts and reusable lenses |
| Settings | — | Preferences | Manage the experience and operational preferences |

Experience level controls density and the visibility of specialist research sections. Both **Beginner** and **Standard** users can reach all nine destinations; Standard reveals more advanced research depth. This gate changes presentation and access to research tools, not permission to trade.

---

## 4. Page-by-page guide

## 4.1 Today

Today answers two separate questions through two subviews.

### News

News is the market-wide daily orientation page.

It contains:

- a compact “what changed” digest built from stored platform facts;
- an AI market brief that summarizes collected market context;
- market movers and key developments across the configured universe;
- provenance, grounded-input labels, model/stub status, and disclosures;
- a manual refresh action for a new cached synthesis.

Opening News can request the cached market brief. Switching away and back does not repeatedly run the model. The brief consumes already-collected facts; it does not fetch providers itself and does not feed the prediction model.

### For You

For You is the signed-in user’s private attention queue.

It combines independently loaded sources:

- active theses;
- due review commitments;
- fired alerts;
- stale-thesis monitoring state;
- recent relevant events;
- direct navigation to a company or thesis that needs review.

If one source is down, the other blocks still render. Guests receive sign-in and first-action guidance instead of another user’s data.

## 4.2 Following

Following contains **Changes** and **Portfolio**.

### Changes

Changes answers: “What happened to the companies I follow?”

Key behavior:

- one canonical event appears once, even when it touches several followed companies;
- ticker, event type, importance, and time-window filters are applied server-side;
- the feed is cursor-paginated;
- read/unread state is stored per user;
- an event is marked read only when the user deliberately opens it, not when it scrolls into view;
- the event detail appears beside the feed on wide screens so scroll position is preserved;
- users can search for and follow a company in one action;
- guests receive a clearly labelled default-universe view rather than a pretend personal feed.

Each event detail can expose its sources, affected companies, key facts, possible read-throughs, historical changes, related thesis context, and research actions. Read-throughs are hypotheses about how an event may reach a company, not recommendations.

### Portfolio

Portfolio is a manual, account-scoped portfolio-intelligence workspace.

The user can maintain:

- one or more named portfolios;
- holdings, quantities, average costs, manual valuations, sectors, and industries;
- cash balances and a base currency;
- target weights and min/max review bands for tickers, sectors, and cash;
- an objective, investment horizon, temporary-loss tolerance, maximum position weight, minimum cash, and excluded sectors.

The deterministic intelligence layer calculates:

- total and known value;
- cash weight;
- largest-position and top-two concentration;
- target drift and policy findings;
- sector and industry exposure;
- annualized volatility;
- beta versus SPY;
- maximum drawdown;
- pairwise historical correlations;
- active-thesis and latest-thesis-check context for each holding.

Users can save explicit portfolio snapshots and compare what changed in holdings, weights, cash, concentration, targets, and thesis checks. AI portfolio review and portfolio scenarios run only when the user asks for them; they are never called by page load or polling.

## 4.3 Explore

Explore discovers events beyond the current watchlist. The gateway ranks and explains the results in six ordered sections:

1. **For You** — ranked for followed companies and the user’s research context.
2. **Market Moving** — high-importance events across the corpus.
3. **Related to Your Companies** — events about other entities with a possible connection to followed companies.
4. **Earnings** — results and guidance in the current window.
5. **Macro** — economic releases and central-bank events.
6. **Trending** — events covered by several sources.

Every visible item must contain both:

- **Why you are seeing this**; and
- **Possible read-through**.

The page drops items that lack either explanation. If the event store is unavailable, Explore refuses to fabricate a ranked feed from raw headlines and shows a degraded state. Users can follow a company, open the shared event-detail panel, open the company research file, or ask Attestel about the event.

Explore also begins with **Discovery Scout**, a company-level queue materialized in PostgreSQL by
the events service. Scout ranks canonical events, upcoming catalysts, real completed-bar technical
salience, and evidence breadth under the versioned `scout@2` formula. Version 2 keeps the original
weights but only admits Marketaux company entities that are explicitly named in the headline;
pre-fix snapshots are withheld. Its gateway rerank can use subscriptions, portfolio holdings,
verified event relationships, and the existing sector table. Every lead states why it surfaced and
why now. The scheduled paths never call Qwen or prediction; **Open research** is an explicit
transition into the existing company research workflow.

`scout@2` weights event attention 35%, catalyst proximity 25%, technical salience 20%, evidence
breadth 10%, and source quality 10%. The result is an attention ordering, not a probability,
expected return, or directional signal. Changing those weights requires a score-version bump so
stored runs remain interpretable. Snapshots older than 12 hours are explicitly marked stale and
their candidates are withheld.

Above Scout, **Early Opportunity Radar** separates developing setups from moves the application
found late. It uses real completed daily bars, deterministic trend, breakout proximity, relative
strength, volume, and compression evidence, plus stored Scout event/catalyst context. Its four
states are `emerging`, `confirmed`, `extended` (explicit no-chase), and `invalidated`. The setup
score is not a probability; paper eligibility remains unassessed and independent prediction and
evaluator gates are never bypassed. The versioned contract is
`docs/EARLY_OPPORTUNITY_RADAR.md`.

## 4.4 Calendar

Calendar is a read-only agenda over the stored catalyst corpus. Browser reads never call providers; ingestion happens separately.

Views:

- **My Calendar** — events relevant to followed companies plus the user’s review commitments;
- **All Events** — the visible global calendar;
- **History** — occurred, released, rescheduled, cancelled, and revised entries with reaction evidence where available.

Calendar entries can show:

- earnings, macro, corporate, and other scheduled-event types;
- official vs. professional/aggregator source tier;
- source links;
- local time and timezone;
- confirmed, tentative, scheduled, released, occurred, or cancelled status;
- previous, expected, actual, surprise, and units when stored;
- revision history such as rescheduling, cancellation, status changes, source upgrades, and release updates;
- post-event market reactions after their windows have matured.

Calendar dates are informational and are not trade triggers. Deterministic alert rules can separately monitor proximity to a calendar event.

## 4.5 Research

Research is the company file. The visible primary strip contains Overview, Events, Technical, Fundamentals, and Earnings. Contextual or advanced sections include Investigations, My Thesis, Evidence, Perspectives, Scenarios, History, and Decision log.

### Overview

Overview opens with the answer rather than a wall of metrics.

It contains:

- company identity, quote, price provenance, and follow toggle;
- the **Attestel analyst view**;
- what changed in the latest analyst read;
- upcoming earnings and company-relevant calendar items;
- latest material events;
- navigation into deeper company sections.

Overview's own event, calendar, earnings-date, and subscription reads are deterministic. Separately, the app-wide dashboard request made when a ticker or timeframe is opened can produce the legacy structured technical read; the newer specialist analyst runs only behind an explicit button.

The Attestel analyst view renders:

- one assessment per requested horizon;
- evidence by technical, company-news, macro, market-context, and fundamentals layers;
- confidence bucket;
- risk flags;
- invalidation text;
- changes since the prior cutoff;
- runtime, prompt/schema identity, degraded-state, and disclosure metadata.

If the model-ablation gate has not validated directional language, the UI shows **Not yet validated** and withholds the direction word. This is the expected safe state, not an error.

### Events

Events shows the material canonical-event history for the selected company. It uses the same event vocabulary and detail surface as Following and Explore, keeping provenance, importance bands, read-throughs, and sources consistent.

### Technical

Technical shows price and deterministic market structure:

- candlestick chart;
- supported timeframes (`1D`, `1H`, `15m`, `5m`, with weekly context available to the backend analyst tools);
- RSI, MACD, SMA/EMA, Bollinger Bands, ATR, stochastic, OBV, ADX, volume, and intraday VWAP where applicable;
- trend, momentum, volatility, volume, and regime facts;
- computed support and resistance;
- multi-timeframe confluence and conflicts;
- source and synthetic-data labels.

### Fundamentals

Fundamentals presents available company reference and earnings evidence, including actual versus estimate and beat/miss history. NVIDIA has a rich verified embedded seed covering quarterly results, roadmap, suppliers, and known comparability caveats. Other tickers depend more heavily on live or stored providers and may show honest empty states.

### Earnings

Earnings combines catalyst history with earnings-call research. Users can inspect earnings events and analyze transcript text. Transcript analysis identifies critical points, management tone, potentially evasive answers, and tone change versus a stored prior quarter. Quotes claimed as evasive are checked against the supplied source text; unverifiable quotes are removed. The raw transcript is not stored as a Library artifact—the analysis record is.

### Investigations

Investigations is a focused question-to-research workspace. A user asks a company-specific research question and explicitly launches the run. The output is structured around an answer, findings, sources, limits, and suggested research follow-ups. It is not invoked on mount.

### My Thesis

My Thesis is the user’s durable claim workspace.

A thesis can contain:

- a claim and lifecycle status;
- assumptions;
- invalidation conditions;
- catalysts;
- open questions;
- evidence links;
- review cadence and checkpoints;
- version history and last AI check;
- closure or archive metadata.

AI checks classify the **user’s thesis** as confirming, neutral, or challenged against supplied evidence. The verdict is about whether the claim is supported, not whether the stock should be bought or sold.

### Evidence

Evidence is divided into:

- Fundamentals;
- Chart context;
- News & filings;
- Transcripts;
- Catalysts & risks;
- Model evidence.

Evidence captures are durable, account-scoped records with source references and provenance. The gateway can turn a current fact into a normalized evidence record. Links between evidence and a thesis are separate records, preserving the difference between “this artifact exists” and “the user says this artifact supports or challenges this thesis.”

### Perspectives

Perspectives applies reusable research lenses to a thesis or evidence item. Built-in lenses offer stable research methods; signed-in users can also manage custom specialists/personas. A saved review records the method, inputs, output, provenance, disclosure, and links rather than overwriting the thesis.

The legacy analyst committee can also provide technical, fundamental, and news/sentiment perspectives followed by bull/bear debate and a deterministic consensus.

### Scenarios

Scenarios performs qualitative what-if analysis grounded in supplied company context. It produces first-order, second-order, confirming, and offsetting effects, along with what to monitor. It does not invent a price target or modify the prediction signal.

### History

History retrieves stored research artifacts:

- daily analytical reads;
- transcript analyses;
- committee snapshots;
- available thesis/version history.

One daily read is kept per ticker per calendar day for the daily timeframe. Intraday reads are not persisted as daily history.

### Decision log

The company decision log shows decisions and executions related to the selected ticker. It connects the company file to the broader Journal without treating a trade as the only possible outcome of research.

## 4.6 Watchlist

Watchlist contains **Companies** and **Monitoring rules**.

### Companies

Companies manages the configured/followed ticker set and opens a selected company’s Research file. Following is a durable user record and is distinct from the currently active ticker preference.

### Monitoring rules

Monitoring rules are descriptive, deterministic conditions. Supported rule families include:

- price crossing a level;
- absolute daily percentage move;
- RSI threshold;
- MACD cross;
- trend flip;
- multi-timeframe confluence conflict;
- VWAP cross;
- new 8-K filing;
- calendar-event proximity;
- one-time or recurring thesis review due date.

Rules can carry a thesis, thesis item, intent, cooldown, timeframe, and active state. They edge-trigger: a condition that remains true does not fire on every evaluation. When upstream data is missing, the evaluator skips rather than inventing a result.

Fired events appear in the in-app notification feed and can optionally be delivered through browser notifications, a generic/Slack/Discord-compatible webhook, or email. Messages describe what happened and never place an order.

## 4.7 Journal

Journal records what the user decided, why, what they executed elsewhere, and what they learned.

### Decisions

Decision records can represent an action or a deliberate non-action. They preserve the contemporaneous thesis and evidence context so a future review does not reconstruct the rationale with hindsight.

### Outcomes

Outcome reviews separate:

- what happened in the market; and
- whether the reasoning process was sound.

This prevents a profitable but poorly reasoned decision from being mistaken for a good process, or a well-reasoned decision with an adverse outcome from being dismissed as useless.

### Trades

Trades is a manual ledger for activity executed outside Attestel. It tracks entries, exits, P&L, R multiples, notes, mode, and the analysis snapshot available at entry. It does not connect to a broker.

### Experiments

Experiments displays the platform’s paper validation book and compares it with the matching backtest. The paper engine:

- operates as a per-bar allocation rule;
- asks the validated prediction service for the target state on each new bar;
- can open, close, flip, or maintain a simulated position;
- sizes each new position at `equity_at_entry / N` across the enabled configs and then keeps its
  share count fixed until the target changes;
- records simulated trades separately with `mode="paper"`;
- refuses to open or flip when data is synthetic or stale, the model is stale, the walk-forward gate fails, or the offline strategy verdict is not `EDGE`.

The model’s horizon labels the prediction target; it is **not** a fixed holding period. The current engine is designed to fail closed. If no qualifying offline edge verdict exists, it trades nothing and reports the reason.

The summary-first paper dashboard reads one generation-scoped `GET /paper/dashboard` payload. It
shows the official clock, integrity, sample maturity and live-versus-reference state separately;
dated equity/drawdown and explicit gap points; the current config/gate matrix; and append-only
settled decision history with model lineage. Polling retries are not counted as decisions. The
surface never labels an experiment as safe for real money and cannot train, promote or tune a model.

## 4.8 Library

Library contains **Saved research** (the route still carries the older “Saved transcripts” label) and **Research lenses**.

### Saved research

The Library loads durable artifacts from their owning stores into one searchable, filterable list. It does not create duplicate copies.

Object types include:

- evidence captures;
- lens reviews;
- committee/perspective reviews;
- transcript analyses;
- daily reads;
- lens definitions.

Filters can narrow by text, object type, ticker, date, provenance, and relationships. Coverage problems are shown explicitly when a store is unreachable or a result set is truncated.

### Research lenses

The lens catalogue shows built-in methods and custom specialists. Users can create a reusable specialist once, then apply it in Research or the Copilot. Lens execution remains user-triggered and produces a review artifact rather than silently changing a thesis.

## 4.9 Settings

Settings contains **Preferences** and **Help & feedback**.

Preferences include:

- Beginner or Standard experience level;
- active ticker;
- UI and notification preferences;
- browser-notification permission and opt-in;
- manual “train challenger” control and champion/challenger gate review;
- Reality Check/onboarding review;
- service and automation health, including per-lane state.

Help & feedback allows beta testers to submit feedback with page, ticker, timeframe, and diagnostic context. Owner/admin capabilities can list, triage, update, summarize, or delete submissions; ordinary users only see the submission experience.

## 4.10 Global surfaces

### Copilot drawer

The Copilot is available throughout the shell and can be toggled with the `C` key when focus is not inside a form field. It supports grounded chat, personas/custom specialists, scenario presets, and contextual prompts from company or event surfaces. Chat is not cached; the normal chat request is user-triggered.

### Event detail

Following and Explore render the same detail component beside the feed. Research can open it as an overlay. This preserves one event contract across the product.

### Notifications

The bell shows unread deterministic alert events. Read state is user-scoped. Browser notifications are optional and seed their last-seen marker before emitting anything, preventing a backlog burst when enabled.

### Authentication and onboarding

Email/password and Google OAuth are supported. Guests can browse read-only surfaces; durable mutations require sign-in. First-run Reality Check and later graduation prompts explain the analysis-only posture and allow a user to move from Beginner to Standard density.

---

## 5. How the AI systems work

Attestel has several AI workflows. They share a model transport and safety infrastructure but solve different tasks. “The AI” is not one autonomous agent with unrestricted access.

## 5.1 Model runtime and fallback chain

The AI service supports:

- a declared, keyless self-hosted OpenAI-compatible or Ollama runtime via `MODEL_RUNTIME_*`;
- a managed OpenAI-compatible endpoint via `AI_*`;
- an explicitly configured legacy `LLM_*` OpenAI-compatible or Ollama endpoint;
- a deterministic structured stub when no provider succeeds.

No endpoint is invented by default. When no provider is configured, the service stays online and reports the offline-stub state. HTTP 429 can be identified separately as `stub:quota`.

All model calls:

- use bounded HTTP timeouts;
- strip hidden `<think>...</think>` content before downstream validation;
- support explicit thinking/non-thinking modes where the transport permits it;
- record whether requested reasoning-mode control was supported or degraded;
- expose provider/runtime status and last error through health metadata without exposing credentials.

## 5.2 Cross-process model lease

Interactive and background generations share a file-based cross-process lease. This protects a resource-constrained local model from concurrent generations across web requests and workers.

The specialist analyst takes the interactive lease per generation, not for an entire multi-call run. Background enrichment and thesis re-synthesis take the background lease. Interactive work has priority, and generations remain sequential.

## 5.3 Specialist analyst pipeline

The specialist analyst is the most agentic workflow in the platform. It runs only after an explicit `POST /api/analyst/{ticker}` initiated by the user.

The run is a **background job**, not a request. A run is up to eight sequential local-model generations and does not fit inside an HTTP request: nginx closes the connection at `proxy_read_timeout 120s`, which used to return 504 while the run carried on unobserved. So `POST /api/analyst/{ticker}` registers the run and answers `202 {runId, status: "running", pollAfterMs}` immediately, and the client polls `GET /api/analyst/runs/{runId}` for `running | done | failed` (the finished envelope travels on `done`; a reason travels on `failed`). The status read is a map lookup in the gateway, reaches no upstream, and can never start a run — which is why it may be polled while the run route may not (invariant #4). A second POST for the same `(ticker, horizon, asOf, uid)` while a run is in flight attaches to it and returns the same `runId` rather than starting a rival run against the model lease. A cache hit answers `200` with the envelope and `status: "done"`.

```text
User clicks Run analysis
        |
        v
Gateway builds a point-in-time context envelope
        |
        v
Orchestrator selects bounded tool work (non-thinking mode)
        |
        +--> Technical specialist (thinking)
        |      tools: multi-timeframe context, candles
        |
        +--> News/Event specialist
        |      tools: ticker events, event details
        |
        +--> Macro/Market specialist (thinking)
               tools: macro context, market context
        |
        v
Final analyst synthesizes one object per horizon
        |
        v
Server validates schema and language, resolves conflicts,
re-imposes computed levels, computes ordinal score, stamps
identity/runtime/provenance, and applies the validation gate
```

The available typed tools are:

1. `get_user_subscriptions`
2. `get_ticker_context`
3. `get_multi_timeframe_context`
4. `get_candles`
5. `get_ticker_events`
6. `get_event_details`
7. `get_macro_context`
8. `get_market_context`

Tool controls:

- each specialist sees only its registered subset;
- the runtime injects `asOf`; it is not present as a model parameter;
- the model cannot select a provider;
- ticker, timeframe, horizon, event ID, date, limit, and unknown arguments are validated;
- each specialist and the overall run have hard tool-call limits;
- coverage gaps are returned as evidence states rather than filled by another untracked source.

Output controls:

- allowed directions are bullish, neutral, bearish, and unclear;
- confidence is an ordinal low/medium/high bucket, not a probability;
- the numeric score is calculated in code from ordinal output;
- technical evidence is required for a directional price read; without it the result becomes unclear;
- mixed or missing specialist stances can cause a server-owned override;
- if the server overrides direction, it also rewrites thesis/invalidation prose so the narrative cannot contradict the badge;
- advice-shaped language is blocked;
- support and resistance are re-imposed from deterministic analysis;
- directional display remains gated by the stored ablation verdict.

## 5.4 Structured technical read

The original read turns a deterministic regime object into structured bull case, bear case, key levels, invalidation, and summary fields. The model sees precomputed facts, returns JSON, receives one firmer retry on structural failure, and falls back to a deterministic stub. Daily reads are persisted and diffable.

## 5.5 Market brief

The brief compresses already-collected facts such as price movement, technical context, headlines, filings, earnings, calendar items, and sentiment into a structured daily summary. It is cached, grounded, non-prescriptive, and never becomes a prediction feature.

## 5.6 Committee and deterministic consensus

The committee runs sequentially:

1. Technical analyst
2. Fundamentals analyst
3. News & Sentiment analyst
4. Bull researcher
5. Bear researcher
6. Judge

Each persona receives only its fact slice. Stances are analytical leans. The final agreement percentage and consensus are calculated in code from weighted stances, not authored by the judge.

Daily committee snapshots include a numeric feature vector. The prediction service treats these as candidate features only after at least `MIN_COMMITTEE_SNAPSHOTS` genuine prior snapshot days exist (default 60). Before then, committee features are neutral. This prevents hindsight backfilling.

## 5.7 Copilot and situational assistant

The copilot receives a compact gateway-assembled context for the selected ticker and conversation. It can explain what changed, answer questions, use a chosen persona, or run scenario mode. It must use uncertainty language and avoid advice. Chat is user-triggered and not cached; the older situational-note endpoint is cached briefly but is not the primary interactive UI.

## 5.8 Transcript, digest, thesis, investigation, and lens workflows

- **Transcript:** chunked map/reduce over supplied text; verifies quoted evasive moments; compares tone only with a stored prior record.
- **Digest:** clusters a supplied news list, validates model-selected item indexes, assigns qualitative impact, and generates a short grounded digest.
- **Thesis check:** judges the user’s claim as confirming, neutral, or challenged using supplied facts.
- **Investigation:** answers a user-authored research question with structured findings and source context.
- **Lens:** applies a stable or custom research method to supplied thesis/evidence context and saves a review when requested.
- **Portfolio review/scenario:** interprets deterministic portfolio context only on explicit user action.

## 5.9 AI event enrichment and thesis re-synthesis

These are bounded background workers, disabled by default:

- event enrichment leases raw events, calls the model under the background lease, validates the structured enrichment, and writes through the events service;
- thesis re-synthesis leases stale-thesis jobs from alerts, loads the current thesis and deterministic trigger context, generates an updated synthesis, and completes the job through token-bound internal APIs.

They are not started by a browser read and do not run unless the master automation flag and their own feature flag allow them.

---

## 6. Deterministic analysis and quantitative prediction

## 6.1 Analysis pipeline

For each ticker/timeframe, the analysis service:

1. fetches or loads OHLCV bars;
2. normalizes the frame;
3. calculates indicators;
4. derives market structure and key levels;
5. collapses the latest row into regime facts;
6. exposes price, features, confluence, expected-close, candle, and multi-timeframe context endpoints;
7. persists eligible real point-in-time bars to PostgreSQL.

Provider fallback is fail-soft. Depending on configuration and timeframe, the chain can include Alpaca, Twelve Data, yfinance, Tiingo, stored bars, and synthetic fallback. Weekly and synthetic bars are not persisted.

The expected-close output is an ATR/time-remaining volatility band, not a model price forecast.

## 6.2 Prediction model

The prediction service is separate from the LLM. It uses deterministic feature frames and LightGBM to estimate direction over a configured label horizon.

Its safeguards include:

- chronological walk-forward validation;
- feature lagging and point-in-time joins;
- probability calibration;
- ablation and leakage tests;
- explicit performance reports;
- no signal without a passing backtest gate;
- committee features held neutral until sufficient real history exists;
- refusal to treat synthetic data as empirical validation.
- immutable model versions: training creates a challenger and cannot replace the active model;
- admin-only, audited promotion/rollback, with the pooled evaluator verdict checked again at
  promotion time.

An optional deterministic controller automates candidate qualification after a fixed number of
newer completed real bars. It creates immutable challengers, invokes the existing fixed evaluator,
and then compares the frozen challenger and champion prospectively on identical completed bars.
Every trial and paired observation is durable in PostgreSQL. The controller has a fixed per-config
trial budget, does not tune thresholds or call Qwen, and cannot promote, reset paper trading, or
place an order. **Settings → Preferences → Challenger automation** is a read-only view; promotion
remains an authenticated human decision for the initial production cycles.

The Buy/Hold/Sell vocabulary is permitted only in this backtest-gated quantitative surface. It is displayed with confidence and track record, never as an LLM verdict.

## 6.3 Event studies and reaction evidence

The prediction service owns a separate PEAD research harness. Historical provider `estimatedEPS`
rows remain descriptive because their point-in-time vintage is not proven. A bounded operator job
(`python -m app.estimate_snapshots`, or `make collect-estimates`) captures the exact fiscal-period
consensus at T−7 and T−1 into immutable PostgreSQL rows, then refreshes the reported actual after
the event. Production operators start one pass from **Settings → Preferences → Forward estimate
snapshots**; the authenticated API returns immediately and exposes durable status and result counts.
The job has no internal timer, so it never opens a provider connection merely because the service
or a browser started. Provider requests are paced at 1.1 seconds to stay below Alpha Vantage's
free-tier burst limit.

PEAD provenance is decided per event from ticker, fiscal period, provider, payload hash, stage and
capture time. Report-day, cross-ticker and incomplete snapshots are rejected. The primary study
switches automatically to the forward-only corpus once enough verified prior events exist to
calculate prior-only SUE. Until then, the historical study is visible but permanently
`INCONCLUSIVE`; no environment trust flag can promote it. PEAD is research-only and neither writes
the prediction verdict used by paper trading nor opens a simulated position.

The events service can record post-event reactions at 1-day, 5-day, and 20-day horizons using stored bars only. It classifies whether the event occurred before, during, after, or outside a market session and picks a deterministic reference close.

Rules include:

- a window stays pending until enough later bars exist;
- missing benchmark data is null, never zero;
- synthetic observations are recorded for diagnosis but disqualified from empirical claims;
- results describe association, not causation;
- sensitivity statistics are withheld until the minimum real matured sample is reached (default 12).

---

## 7. Automation in detail

Automation is divided into four classes.

## 7.1 Browser polling and app-open automation

| Automation | Cadence | Trigger | Model involved? | Behavior |
|---|---:|---|---|---|
| Header quote refresh | 20 seconds | App open | No | Calls the cheap quote endpoint for the active ticker |
| Unread bell count | 30 seconds | App open, except while notification/monitor surfaces own the count | No | Reads alert events and updates the badge |
| Browser desktop notifier | 30 seconds | Signed in, opted in, OS permission granted | No | Reads newest events, de-duplicates per user, emits desktop notifications |
| Model target publisher (legacy `AutoTrainer` component name) | On company/timeframe change | App open | No | Publishes the config used by explicit lifecycle actions; it runs no timer and never trains |

Changing ticker or timeframe reloads the dashboard. This may produce the structured technical read as part of that explicit company analysis request, but the cheap 20-second quote loop never calls the LLM.

## 7.2 Always-on deterministic service loops

### Alert evaluator

- enabled as part of the alerts service;
- runs once at service start and every `EVAL_INTERVAL` (default 60 seconds);
- batches upstream reads per ticker, not per rule;
- evaluates only active deterministic rules;
- edge-triggers and respects cooldowns;
- appends an in-app event and optionally sends webhook/email;
- never calls the LLM.

### Thesis monitor

- separately controlled by `THESIS_MONITOR_ENABLED` (default false);
- interval defaults to 15 minutes with a one-minute minimum;
- reads active theses and one last-stored-bar point per distinct ticker;
- detects material staleness deterministically;
- writes stale markers and re-synthesis queue jobs;
- never calls the LLM directly.

### Paper engine

- evaluates configured ticker/timeframe/horizon strategies every `EVAL_INTERVAL` (default 5 minutes);
- acts only once per genuinely new bar unless demo fast-forward is explicitly enabled;
- reconciles the current simulated allocation with the latest gated target;
- fails closed on data, model, validation, or freshness problems;
- records simulation-only journal entries;
- never calls the LLM and never sends an order.

### Prediction challenger controller

- off unless `PREDICTION_AUTOMATION_ENABLED=true`;
- polls for a fixed number of newer completed real bars, not wall-clock permission to fit;
- creates at most `PREDICTION_AUTOMATION_MAX_TRIALS` durable trials per config (default 3);
- runs the fixed price evaluator and records honest negative or inconclusive outcomes without
  retrying alternate thresholds;
- shadows frozen challenger/champion versions on identical future bars until the fixed paired-bar
  floor (default 20);
- never calls an LLM, changes the active model, or writes the official paper ledger.

## 7.3 Automation lanes

Every lane remains a bounded one-shot entrypoint. The all-in-one production image can run a
separate, opt-in clock for the five events-owned model-free lanes only. Model lanes remain explicit
operator or user actions and cannot be caused by that clock.

| Lane | Work performed | Model? | Default minimum interval | Own enable flag |
|---|---|---:|---:|---|
| `ingest` | Bounded provider ingestion pass | No | 1 hour | `INGEST_ENABLED` |
| `enrich` | Bounded raw-event enrichment pass | Yes | 30 minutes | `EVENT_ENRICH_ENABLED` |
| `resynth` | Bounded stale-thesis re-synthesis pass | Yes | 1 hour | `THESIS_RESYNTH_ENABLED` |
| `thesis-monitor` | One deterministic stored-bar thesis sweep | No | 15 minutes | `THESIS_MONITOR_ENABLED` |
| `reactions` | Bounded matured post-event reaction capture | No | 6 hours | `REACTION_CAPTURE_ENABLED` |
| `scout-intake` | Rotate a bounded discovery batch through the keyless provider allowlist | No | 4 hours | `SCOUT_INGEST_ENABLED` |
| `scout` | Materialize the company-level research queue from stored evidence | No | 4 hours | `SCOUT_ENABLED` |
| `opportunity-radar` | Materialize completed-bar early/confirmed/extended/invalidated setup research | No | 4 hours | `OPPORTUNITY_RADAR_ENABLED` |

Every lane also requires `AUTOMATION_ENABLED=true`. `AUTOMATION_LANES` can narrow the allowed set. Defaults are off.

Commands:

```bash
make automate-once
make automate-lane LANE=ingest
make automate-lane LANE=enrich
make automation-status
```

The dispatcher uses one PostgreSQL lease per lane. Completion requires the exact lease token. Expired abandoned runs are reconciled as failures, and an old worker cannot overwrite a newer result. Each job remains independently idempotent.

Operational status distinguishes:

- **success** — all attempted work completed cleanly;
- **degraded** — a pass ran but a provider, item, or batch was unavailable/skipped;
- **failure** — the pass raised an error.

Only success updates the last-clean-success timestamp.

## 7.4 Manual, user-triggered automation

These actions automate complex work but run only after a click or submit:

- specialist analyst;
- copilot chat;
- investigation;
- scenario;
- lens review;
- committee refresh;
- transcript analysis;
- thesis check;
- portfolio review and portfolio scenario;
- quant challenger training and evidence-gated promotion/rollback;
- evidence capture and thesis linking;
- explicit portfolio snapshot;
- decision and outcome creation.

## 7.5 What a page load is allowed to do

Ordinary reads may load stored or cached data. They may not:

- start provider ingestion;
- acquire an automation lane;
- enable a disabled job;
- call the specialist analyst;
- start an investigation, scenario, lens, committee refresh, portfolio AI review, or thesis re-synthesis;
- turn an alert or AI output into a trade.

---

## 8. Event and catalyst data pipeline

The event system separates collection from browser consumption.

```text
Configured providers / IR feeds / macro schedules
                  |
                  v
         bounded ingestion pass
                  |
                  v
 documents -> canonical events -> entities/relationships
                  |
                  +--> scheduled-event calendar + revisions
                  |
                  +--> optional AI enrichment
                  |
                  +--> Following / Explore / Research / Calendar
                  |
                  +--> matured reaction capture and sensitivity
```

Important behaviors:

- documents deduplicate by content hash;
- canonical events cluster related documents rather than showing duplicate headlines;
- scheduled events use stable occurrence identities and preserve first-seen time;
- stronger official sources can upgrade weaker tentative records under conservative matching rules;
- ambiguous occurrences remain separate rather than risking a destructive merge;
- provider calls pass through stored budget reservations;
- every source can degrade independently;
- browser reads use the stored corpus and never fetch a provider.

---

## 9. Service architecture

| Unit | Port | Technology | Responsibility | Persistence |
|---|---:|---|---|---|
| `postgres` | 55432 host / 5432 container | PostgreSQL 16 | Shared durable database | Docker volume |
| `analysis` | 8001 | Python / FastAPI | OHLCV, indicators, regime, levels, confluence, risk, features | PostgreSQL `analysis` schema |
| `llm` | 8002 | Python / FastAPI | All model-backed research workflows and offline stubs | `READS_DIR`, plus event writes through APIs |
| `prediction` | 8003 | Python / FastAPI | LightGBM training, backtests, calibration, forecasts, PEAD evidence | PostgreSQL `prediction` schema; local files are fallback/import only |
| `events` | 8004 | Python / FastAPI | Documents, events, calendar, automation ledger, predictions/outcomes, reactions | PostgreSQL `public` schema |
| `gateway` | 8080 | Go standard library | Frontend API, aggregation, context assembly, provider enrichment, cache, auth forwarding | in-memory TTL cache |
| `alerts` | 8095 | Go standard library | Rules, evaluation, notifications, stale-thesis markers/queue | alerts volume / JSON stores |
| `journal` | 8096 | Go standard library | Theses, evidence, reviews, decisions, outcomes, trades, subscriptions, portfolios | journal volume / per-user JSON stores |
| `paper` | 8097 | Go standard library | Fail-closed strategy simulation and comparison | paper volume plus journal writes |
| `feedback` | 8098 | Go standard library | Tester submissions and triage | feedback volume |
| `auth` | 8099 | Go standard library | Accounts, sessions, settings, Google OAuth | users volume |
| `web` | 5173 | React / Vite | Customer UI | browser state plus server-owned records |

### Core request paths

```text
React web
   |
   +--> Gateway --> Analysis
   |       |------> LLM
   |       |------> Prediction
   |       |------> Events
   |       |------> Journal / Alerts / Auth (selected proxy/context reads)
   |       `------> live providers + embedded NVIDIA seed
   |
   +--> Auth, Journal, Alerts, Paper, Feedback dev proxies
```

The gateway is the only general frontend aggregator and remains standard-library-only Go. Analysis and LLM do not know how the frontend is laid out.

### API surface by owner

This is the public/operational route map by responsibility. Query parameters and internal request schemas remain defined by the handlers and tests.

| Owner | Route families |
|---|---|
| Gateway core | `/health`, `/ready`, `/api/tickers`, `/api/tickers/search`, `/api/quote/{ticker}`, `/api/dashboard/{ticker}`, `/api/context/{ticker}` |
| Gateway market data | `/api/confluence/{ticker}`, `/api/fundamentals/{ticker}`, `/api/catalysts/{ticker}`, `/api/news/{ticker}`, `/api/earnings/{ticker}`, `/api/sentiment/{ticker}`, `/api/calendar`, `/api/next-earnings/{ticker}` |
| Gateway AI/quant | `/api/predict/{ticker}`, `/api/train/{ticker}`, `/api/models/{ticker}/*`, `/api/prediction-automation/status`, `/api/evaluate/*`, `/api/evaluate-events/*`, `/api/estimate-snapshots/*`, `/api/brief/*`, `/api/assistant/{ticker}`, `/api/chat/{ticker}`, `/api/analyst/{ticker}`, `/api/committee/{ticker}`, `/api/digest/{ticker}`, `/api/scenario/{ticker}`, `/api/investigate/{ticker}`, `/api/transcript/{ticker}`, `/api/reads/{ticker}` |
| Gateway research records | `/api/theses/*`, `/api/evidence/*`, `/api/lens`, `/api/lenses`, `/api/reviews/*`, `/api/decisions/*`, `/api/outcomes/*` |
| Gateway event experience | `/api/following`, `/api/explore`, `/api/scout`, `/api/opportunities`, `/api/events/{id}`, `/api/changed`, `/api/reactions`, `/api/sensitivity`, `/api/subscriptions`, `/api/event-state`, `/api/monitor/theses` |
| Analysis | `/health`, `/quote/{ticker}`, `/analysis/{ticker}`, `/analysis/{ticker}/features`, `/analysis/{ticker}/confluence`, `/candles/{ticker}`, `/technical-context/{ticker}`, `/multi-timeframe-context/{ticker}`, `/expected-close/{ticker}`, `/portfolio-risk` |
| Events | `/health`, `/documents`, `/events`, `/events/{id}`, `/calendar`, `/macro`, `/relationships`, `/predictions`, `/reactions`, `/sensitivity`, `/scout`, `/opportunities`, `/ingest`, `/ir/coverage`, `/automation/status`, `/automation/runs` plus protected lease/completion routes |
| AI service | `/health`, `/read`, `/reads/{ticker}`, `/brief`, `/assistant`, `/chat`, `/analyst`, `/committee`, `/transcript`, `/thesis-check`, `/lens`, `/lenses`, `/digest`, `/scenario`, `/investigation`, `/portfolio-review`, `/portfolio-scenario`, `/personas` |
| Prediction | `/health`, `/automation/status`, `/train/{ticker}`, `/models/{ticker}`, `/models/{ticker}/{version}/promote`, `/models/{ticker}/{version}/rollback`, `/predict/{ticker}`, `/backtest/{ticker}`, `/forecast/{ticker}`, `/evaluate/run`, `/evaluate/status`, `/evaluate-events/run`, `/evaluate-events/status`, `/estimate-snapshots/run`, `/estimate-snapshots/status` |
| Alerts | `/health`, `/rules`, `/events`, `/monitoring/due`, `/monitoring/theses` plus protected monitor/re-synthesis routes |
| Journal | `/health`, `/trades`, `/stats`, `/theses`, `/evidence`, `/reviews`, `/decisions`, `/outcomes`, `/subscriptions`, `/event-state`, `/onboarding`, `/portfolios` and portfolio snapshot/review/scenario/intelligence routes |
| Paper | `/health`, `/paper/positions`, `/paper/status`, `/paper/ledger`, `/paper/comparison`, `/paper/dashboard`, `/paper/experiments`, `/paper/readiness`, `/paper/config`, `/paper/reset` |
| Auth | `/health`, `/auth/signup`, `/auth/login`, `/auth/logout`, `/auth/me`, `/auth/settings`, `/auth/google/*` |
| Feedback | `/health`, `/feedback`, `/feedback/summary`, `/feedback/{id}` |

---

## 10. Data ownership and persistence

### PostgreSQL

- `analysis` schema: point-in-time real OHLCV bars and migrations;
- `prediction` schema: immutable model versions, active deployment pointers, promotion/rollback
  audit, autonomous trial/shadow ledgers, evaluation artifacts/verdicts, raw earnings payloads,
  immutable forward estimate snapshots, and event-time earnings text;
- `public` schema: source documents, canonical events, scheduled events and revision history,
  entities, relationships, provider budgets, automation lane state/runs, Discovery Scout
  runs/candidates, event predictions/outcomes, and post-event reactions.

### Journal volume

Account-partitioned JSON records include:

- trades;
- theses and versions/checkpoints;
- evidence and evidence links;
- saved lens reviews;
- decisions and outcomes;
- subscriptions and per-event read state;
- portfolios, snapshots, reviews, and related records.

### AI read volume

The AI service stores:

- one daily read per ticker/day;
- committee snapshots;
- transcript-analysis records;
- user personas/custom specialists;
- other workflow artifacts where the owning module specifies persistence.

### Model, paper, alerts, auth, and feedback volumes

- model binaries, calibrators, records, and evaluation reports;
- paper engine state;
- alert rules, append-only event log, read state, stale markers, and re-synthesis queue;
- users, password hashes, and settings;
- tester feedback records.

File-backed stores use guarded temporary-write-and-rename patterns where implemented, reducing partial-write risk. They are not a replacement for database replication or backups.

---

## 11. Accounts, authorization, and privacy boundaries

- Auth issues an HMAC-signed, HTTP-only session cookie.
- Gateway, Journal, and Alerts verify the same cookie locally using the shared `AUTH_SECRET`.
- The gateway forwards the resolved `X-User-Id` to the AI service; the AI service does not independently validate the cookie.
- Read-only guest experiences are allowed where safe.
- Durable mutations require authentication. Onboarding progress may also be recorded for a guest; feedback submission requires sign-in and feedback listing/triage requires an administrator allowlist.
- User research records are partitioned by user in the owning stores.
- The paper book belongs to a dedicated platform system user, not to the currently signed-in person.
- CORS uses an explicit credentialed-origin allowlist, never `*` with cookies.
- Google OAuth return destinations are sanitized to a constrained route identifier.

Secrets belong in environment variables and must never be written to status payloads. Automation health reports flag names and booleans, not secret values.

---

## 12. Caching, freshness, and degradation

The gateway uses separate TTLs because sources have different costs and freshness needs:

- quote: seconds;
- dashboard: short composite cache;
- confluence and prediction: minutes;
- news and sentiment: minutes;
- brief, digest, and committee: hard caches because they are expensive or provider/model-backed;
- earnings: hours because the free provider budget is small;
- calendar: short enough to surface lifecycle changes.

Each response can carry source/degraded metadata. Expected behavior during partial failure is:

- retain unaffected sections;
- show the unavailable source or reduced coverage;
- fall back to seed or deterministic stub only where that fallback is a documented data state;
- never label seed, synthetic, cached, or stub output as live;
- avoid repeating expensive model/provider calls from polling loops.

---

## 13. Operations

### Start the complete stack

```bash
docker compose up --build
```

Main URLs:

- Product: `http://localhost:5173/app/`
- Gateway health: `http://localhost:8080/health`
- Gateway readiness: `http://localhost:8080/ready`

### Health and readiness

```bash
make health
make automation-status
```

`/health` means a process can serve. Gateway `/ready` can return 503 when required upstreams—by default the PostgreSQL-backed analysis and events services—are unavailable. The LLM is deliberately not required for readiness because an offline stub is a supported product state.

### Main test commands

```bash
docker compose up -d postgres
(cd services/analysis && ANALYSIS_TEST_DATABASE_URL=postgresql://attestel:attestel@localhost:55432/attestel_events python -m pytest -q)
(cd services/events && EVENTS_TEST_DATABASE_URL=postgresql://attestel:attestel@localhost:55432/attestel_events python -m pytest -q)
(cd services/prediction && python -m pytest -q)
(cd services/llm && python -m pytest -q)
go test ./...                 # run inside each Go service directory as needed
(cd web && npm run build)
```

### Model runtime smoke

```bash
make smoke-model
```

This is an operator check against a real declared runtime. It refuses to report model-quality measurements when the response is a stub, the runtime identity is inconsistent, or the model lease cannot be demonstrated.

### Quantitative evaluation

```bash
make evaluate
make evaluate-events
make collect-estimates
make ablate
```

These are research validation jobs, not production order workflows. They refuse empirical verdicts on synthetic data.
On the shell-less production deployment, use the corresponding Settings panels; `make` targets are
for local Docker/operator environments only.

---

## 14. Current limitations and status-sensitive behavior

- Product naming is not fully reconciled across UI, repository, cookie, logs, and email subjects.
- NVIDIA has the richest embedded fundamental and roadmap reference data; other tickers depend more on live/stored sources.
- Synthetic prices preserve the workflow offline but must not be treated as market evidence.
- Directional analyst language can remain hidden until the ablation verdict is present and passing.
- Committee features do not influence prediction until enough genuine historical snapshots exist.
- Event sensitivity reports no statistic below its minimum real matured sample.
- The paper engine is expected to do nothing until every fail-closed validation gate passes.
- Several advanced AI workflows can take minutes on a local model because generations are deliberately sequential and lease-protected.
- File-backed service records need an external backup strategy for production durability.
- External providers can change terms, quotas, or response formats; the product’s supported response is explicit degradation, not fabricated continuity.

Planned or historical documents under `docs/prompts/`, older project phases, and pre-transformation audits are useful design evidence but are not authoritative descriptions of the current UI. For current navigation, use `web/src/lib/routes.js`; for current service behavior, use the registered routes and implementations.

---

## 15. Glossary

| Term | Meaning |
|---|---|
| Thesis | A user-owned, falsifiable investment claim and its assumptions, invalidations, catalysts, evidence, and review state |
| Evidence | A durable artifact plus provenance; linking it to a thesis is a separate user/system relationship |
| Lens | A reusable research method or specialist perspective |
| Review | A saved application of a lens or interpretive workflow to supplied context |
| Canonical event | One normalized event that can be supported by several source documents |
| Read-through | A disclosed hypothesis about how an event might affect another company or business dimension |
| Regime | Deterministic latest-bar summary of trend, momentum, volatility, volume, bands, and levels |
| Confluence | Agreement or conflict across several deterministic timeframes |
| AI read | Structured model interpretation of supplied regime facts, with computed levels re-imposed |
| Analyst | User-triggered orchestrator + specialist + final-synthesis pipeline |
| Committee | Legacy sequential persona/debate pipeline with deterministic weighted consensus |
| Stub | Deterministic offline or quota fallback, explicitly labelled as not model-authored evidence |
| `asOf` | Point-in-time cutoff shared by a run and injected by the runtime |
| Automation lane | One bounded operator-dispatched background job with its own lease, cadence, and feature flag |
| Paper experiment | Simulation of an already-gated quantitative strategy; no order or money exists |
| EDGE | Offline evaluator verdict required by the fail-closed paper strategy gate |

---

## 16. Authoritative code and companion documents

Use these sources when extending or checking this guide:

- Navigation: [`web/src/lib/routes.js`](../web/src/lib/routes.js)
- Application composition: [`web/src/App.jsx`](../web/src/App.jsx)
- Gateway routes: [`gateway/main.go`](../gateway/main.go)
- Analysis API: [`services/analysis/app/main.py`](../services/analysis/app/main.py)
- AI API and runtime: [`services/llm/app/main.py`](../services/llm/app/main.py), [`services/llm/app/llm.py`](../services/llm/app/llm.py)
- Specialist analyst: [`services/llm/app/analyst.py`](../services/llm/app/analyst.py), [`services/llm/app/tools.py`](../services/llm/app/tools.py)
- Automation: [`docs/AUTOMATION_OPERATIONS.md`](AUTOMATION_OPERATIONS.md), [`services/events/app/automation.py`](../services/events/app/automation.py)
- Prediction: [`docs/PREDICTION_MODELS.md`](PREDICTION_MODELS.md)
- Paper contract: [`docs/PAPER_EXECUTION_CONTRACT.md`](PAPER_EXECUTION_CONTRACT.md)
- Portfolio intelligence: [`docs/PORTFOLIO_INTELLIGENCE_IMPLEMENTATION.md`](PORTFOLIO_INTELLIGENCE_IMPLEMENTATION.md)
- Catalyst calendar: [`docs/CATALYST_CALENDAR_IMPLEMENTATION.md`](CATALYST_CALENDAR_IMPLEMENTATION.md)
- Safety/disclosures: [`docs/DISCLOSURE_CLASSIFICATION.md`](DISCLOSURE_CLASSIFICATION.md)
- Verified NVIDIA data: [`docs/NVIDIA_RESEARCH.md`](NVIDIA_RESEARCH.md)
- Environment and startup: [`README.md`](../README.md), [`docker-compose.yml`](../docker-compose.yml)

When this document and implementation disagree, the current code and tests win. Update this guide in the same change that alters a page, automation lane, AI contract, service boundary, persistence owner, or safety invariant.
