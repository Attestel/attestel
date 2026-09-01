# Attestel

[![CI](https://github.com/Attestel/attestel/actions/workflows/ci.yml/badge.svg)](https://github.com/Attestel/attestel/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Status: beta](https://img.shields.io/badge/status-beta-orange.svg)](#honest-beta-status)

**Self-hosted investment research that turns filings, market data, news, and local-AI analysis into falsifiable, provenance-backed theses.**

Attestel helps you collect evidence, form an investment thesis, challenge it, record a decision, and
review what actually happened. It runs on your hardware, works without paid data keys, supports your
own local model, and never connects to a broker or executes a trade.

[Quickstart](#quickstart) · [Product tour](docs/APPLICATION_DOCUMENTATION.md) ·
[Beginner guide](docs/BEGINNER.md) · [Contributing](CONTRIBUTING.md)

> **Beta:** Attestel runs, is tested, and is useful today, but it is not finished. Expect rough
> edges and breaking changes between commits. Read the [honest beta status](#honest-beta-status)
> before relying on model or prediction features.

![Attestel Today view showing a model-authored morning brief beside a deterministic market pulse](assets/screenshot-today.png)

## Why Attestel?

Most investing tools optimize for more charts, more alerts, or a faster answer. Attestel optimizes
for a better research process:

```text
Discover → Collect evidence → Form a thesis → Challenge it → Decide → Review the outcome
```

- **Research with memory.** Evidence, assumptions, invalidation conditions, decisions, and outcomes
  stay connected instead of disappearing across browser tabs and chat histories.
- **Know where every number came from.** Figures carry their provider, source, fetch time, and
  live/seed/synthetic state.
- **Keep judgment human.** Language models can organize and challenge evidence, but they do not
  issue buy/sell verdicts.
- **Keep private research private.** Run the application and model on hardware you control. Local
  product measurements stay inside your deployment, and a local model has no per-token bill.
- **Start without subscriptions.** The default path uses keyless providers and verified seed data;
  paid API keys are optional extensions.

## What you get

- A daily brief that separates model-written context from deterministic market facts.
- Company research files combining events, technicals, fundamentals, earnings, transcripts, and
  saved evidence.
- Thesis and challenge workspaces for assumptions, counterarguments, and invalidation conditions.
- Market-wide discovery where every surfaced item includes a reason and possible read-through.
- Watchlists, catalysts, alerts, a research library, and a decision/outcome journal.
- Optional quantitative predictions that remain hidden unless a walk-forward evaluation passes.
- Simulated paper validation with no order execution, broker integration, or money movement.

Attestel ships with NVDA, GOOGL, and TSLA configured by default. A verified NVIDIA reference file is
included so the application is useful before you connect a model or data account.

## See it in action

### Research with provenance

![Attestel Research Fundamentals view showing verified quarterly figures and comparability warnings](assets/screenshot-research.png)

Figures are labelled with their provenance. Changes that make year-over-year comparisons unsafe are
shown before the table rather than buried in a footnote.

### Save evidence, not screenshots

![Attestel Save as evidence dialog showing the value, fiscal period, source, and provenance that will be stored](assets/screenshot-evidence.png)

Save a number directly into the research record with its source, provider, timestamp, data state,
and your note about why it matters.

### Discover with a reason

![Attestel Explore view showing companies ranked by stored events and their relationship to existing coverage](assets/screenshot-explore.png)

Discovery results explain why they surfaced. Ranking is deterministic and opening a result does not
silently start a model call or paper trade.

For every screen and workflow, see the [complete product tour](docs/APPLICATION_DOCUMENTATION.md).

## Quickstart

### Requirements

- Docker
- Docker Compose v2
- Git

### Start Attestel

```bash
git clone https://github.com/Attestel/attestel.git
cd attestel
cp .env.example .env
docker compose up --build
```

Open **http://localhost:5173/app/**.

The first build downloads and compiles several services, so it takes longer than later starts. All
configuration keys are optional: an empty `.env` works, verified seed data is included, and the
language-model service clearly identifies itself as `stub:offline` until you configure a runtime.

The defaults are for local evaluation. Before exposing Attestel to a network, replace the example
`AUTH_SECRET` and follow the [security policy](SECURITY.md).

Useful operator commands:

```bash
make health   # verify every service
make logs     # follow service logs
make down     # stop the stack
```

### Add a local model

Attestel supports Ollama and OpenAI-compatible servers such as vLLM, SGLang, and LM Studio. For
Ollama:

```bash
ollama pull qwen3:14b
```

Add this to `.env`, then restart the stack:

```dotenv
MODEL_RUNTIME_URL=http://host.docker.internal:11434
MODEL_RUNTIME_MODEL=qwen3:14b
MODEL_RUNTIME_KIND=ollama
```

```bash
make restart
```

There is deliberately no API-key variable for the local runtime. When no runtime is configured,
Attestel uses an explicit deterministic stub instead of silently connecting to localhost.

## Data modes

| Mode | What you configure | What happens |
|---|---|---|
| **Seeded/offline** | Nothing | Verified reference data, synthetic bars labelled as synthetic, and the deterministic model stub |
| **Keyless** | Network access | Real prices through yfinance, news through Google News RSS, and filings through SEC EDGAR |
| **Extended providers** | Optional API keys | Wider intraday, fundamentals, news, macro, and calendar coverage |
| **Local model** | Ollama or another compatible runtime | On-demand model reads using infrastructure you control |

Synthetic price data is never written to the historical store. Provider failures degrade visibly,
and every response identifies the source that actually served it. See the commented
[`.env.example`](.env.example) for the complete configuration reference.

## Safety and research discipline

These are enforced product rules, not marketing preferences:

1. **The language model does not issue buy/sell verdicts.** A banned-phrase guard rejects
   prescriptive output from model reads, committee views, and thesis checks.
2. **The model does not invent market numbers.** Levels and indicators are computed
   deterministically, passed to the model as evidence, and re-imposed after its response.
3. **No scheduler tick calls a model.** Model work is on-demand or an explicitly invoked, bounded
   worker pass.
4. **Directional predictions fail closed.** A signal appears only with a passing walk-forward
   evaluation, confidence, and its measured track record.
5. **Nothing executes a trade.** The paper service simulates positions for validation; Attestel has
   no broker integration and moves no money.

## Honest beta status

- The paper engine opens no simulated position until real-data freshness, passing-backtest, and
  persisted-edge gates all pass. No passing `EDGE` verdict ships with the repository. See the
  [paper execution contract](docs/PAPER_EXECUTION_CONTRACT.md).
- No repository claim has yet been measured against a real model runtime. The offline stub tests
  plumbing and schemas; it is not evidence of model quality.
- Committee-derived prediction features remain neutral until at least 60 genuine snapshots exist.
- Provider coverage varies by company and source. Missing and synthetic data are labelled rather
  than presented as live facts.
- Before trusting a directional signal, complete the
  [validation and go-live checklist](docs/VALIDATION_AND_GO_LIVE.md).

## Architecture

<details>
<summary>Show the service architecture</summary>

```text
React web
  ├── Go gateway ─────── Python analysis     prices, indicators, regimes
  │                 ├── Python events       documents, catalysts, macro
  │                 ├── Python LLM          reads, committee, transcripts
  │                 └── Python prediction   models and evaluation evidence
  ├── Go auth             local accounts and signed sessions
  ├── Go alerts           scheduled descriptive notifications
  ├── Go journal          decisions, outcomes, and manual trade records
  ├── Go paper            simulated positions only
  └── Go feedback         local feedback records

Durable state ─────────── PostgreSQL with service-owned schemas
```

Python owns data and ML work. Go owns the API gateway and durable user-facing records. React stays
thin, and the gateway uses only the Go standard library.

</details>

For deeper implementation detail, see the
[application documentation](docs/APPLICATION_DOCUMENTATION.md),
[prediction model documentation](docs/PREDICTION_MODELS.md), and
[automation operations guide](docs/AUTOMATION_OPERATIONS.md).

## Contributing

Contributions are welcome. Read [CONTRIBUTING.md](CONTRIBUTING.md) for setup, tests, and the
project's data and model discipline.

The easiest high-value contribution does not require code: expand company investor-relations feed
coverage or event relationships through the JSON registries. Every submitted relationship or feed
must include a source.

- [Browse good first issues](https://github.com/Attestel/attestel/issues?q=is%3Aissue+is%3Aopen+label%3A%22good+first+issue%22)
- [Report a bug or request a feature](https://github.com/Attestel/attestel/issues/new/choose)
- [Read the security policy](SECURITY.md)

Questions and general feedback are welcome in
[GitHub Issues](https://github.com/Attestel/attestel/issues).

## License

Attestel is available under the [MIT License](LICENSE).

---

**Not investment advice.** Attestel is an analysis tool. It produces no recommendation, executes
no order, and holds no money. Every decision is yours.

If Attestel improves your research process, consider starring the repository so other self-hosters
and researchers can find it.
