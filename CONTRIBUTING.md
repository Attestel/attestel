# Contributing to Attestel

Thanks for being here. Attestel is a self-hosted investment research OS, and it improves fastest
through two very different kinds of contribution: **data coverage** (no code, high value) and
**code** (held to the discipline rules below).

## Start here: the two registries

The highest-value, lowest-friction contribution is coverage. Both registries are plain JSON files
loaded at runtime — adding a company needs **no code change and no rebuild**.

### 1. Company investor-relations feeds — `COMPANY_IR_REGISTRY_PATH`

A company's *own* IR events feed is what upgrades a tentative aggregator earnings date to a
confirmed one. A company present in neither the built-ins nor this registry is reported as missing
coverage (`GET /ir/coverage` → `company-ir:no-coverage`) — it is **never** served a guessed date.

```json
[
  { "ticker": "ACME",
    "company": "Acme Industries",
    "feedUrl": "https://investors.example.com/events.ics",
    "feedKind": "ics",
    "timezone": "America/New_York",
    "eventKinds": ["earnings"] }
]
```

An entry replaces a built-in for the same ticker wholesale. Point `COMPANY_IR_REGISTRY_PATH` at your
file, set `COMPANY_IR_ENABLED=true` and `EVENTS_CONTACT_UA`, and check `GET /ir/coverage`.

### 2. Event relationships — `RELATIONSHIP_REGISTRY_PATH`

Which companies a scheduled event bears on, and why. Entries are appended to the built-ins.

```json
[
  { "subject": "ACME",
    "counterparty": "WIDGET",
    "relationship": "supplier",
    "reason": "Widget Co supplies Acme's primary component.",
    "sourceRef": "2026 supplier disclosure",
    "band": "primary" }
]
```

Every relationship carries its own provenance. `relationship` must be one of `direct`, `sector`,
`supplier`, `customer`, `competitor`, `macro`, `factor`. **A malformed entry — missing `reason`,
missing `sourceRef`, or a type outside that vocabulary — is dropped whole**, silently and by design.
There is no model-generated relationship and no fourth provenance source.

When you open a PR against either registry, cite where the feed URL or the relationship came from.
A citation is not paperwork here; it is the data.

## Running locally

```bash
git clone https://github.com/Attestel/attestel.git
cd attestel
cp .env.example .env      # every key is optional; an empty file works
docker compose up --build
```

Or run the services individually (each in its own terminal):

```bash
cd services/analysis && pip install -r requirements.txt && uvicorn app.main:app --port 8001
cd services/llm      && pip install -r requirements.txt && uvicorn app.main:app --port 8002
cd gateway           && go run .          # stdlib only; no `go get` needed
cd web               && npm install && npm run dev     # http://localhost:5173/app/
```

### Tests

Deterministic, no network, no model. Use each service's **own** virtualenv — a bare `python` is
usually too old and has no pandas, and `services/events` needs Python ≥ 3.10:

```bash
cd services/analysis   && ./.venv/bin/python -m pytest -q
cd services/prediction && ./.venv/bin/python -m pytest -q
cd services/llm        && ./.venv/bin/python -m pytest -q
cd services/events     && ./.venv/bin/python -m pytest -q

# Go: CI runs all six modules as a matrix, not just gateway.
for m in alerts auth feedback gateway journal paper; do (cd "$m" && go test ./...); done

cd web && npm ci && npm test
```

Run `make fmt-check` before opening a PR. It is the same `gofmt -l` sweep CI's `syntax` job runs over
the six Go modules, and catching drift locally costs a second instead of a CI round-trip; `make fmt`
rewrites the files in place.

Test-only dependencies live in each service's `requirements-dev.txt` and are deliberately not in the
runtime images. The web app does have a test step -- `npm test` runs `node --test` over
`web/tests/`, and CI runs it before `npm run build`, so a failure there fails the build job.

The Postgres-backed Go tests (`alerts`, `auth`, `feedback`, `journal`, `paper`) call `t.Skip` unless
the matching `<SERVICE>_TEST_DATABASE_URL` is set, so the loop above passes on a fresh clone with no
database. CI sets all five against a Postgres service, which is where those tests actually execute --
locally green is therefore weaker than CI green for those modules.

## Discipline rules

These are the load-bearing rules. A PR that breaks one will be asked to change, however good the
code is.

1. **No buy/sell language.** The language model's output — reads, committee stances, thesis checks,
   lenses — carries no recommendations, price targets or verdicts. A banned-phrase list enforces it
   server-side. The one directional signal in the product comes from the quant `prediction` service,
   is gated on a passing walk-forward backtest, and is always shown with its track record.

2. **Provenance is required.** Every number reaching a user carries where it came from and when.
   Synthetic data is labelled synthetic in the UI and is **never** written to the historical bar
   store — persisting one would make the point-in-time store non-reproducible forever.

3. **The model never invents a number.** Levels and indicators are computed deterministically and
   handed to the model to quote; the server re-imposes them afterwards. Do not remove that step.

4. **No scheduler-triggered model calls.** No timer, interval, cron or scheduler tick may cause a
   model call — not from a page load, a ticker change, a quote poll or an evaluator tick. Model work
   is on-demand or an explicitly-invoked single-pass worker that drains a bounded batch and exits. A
   `while True` or a `time.sleep` in that worker is a defect.

5. **The gateway is standard-library-only Go.** `gateway/go.mod` has zero dependencies. Keep it that
   way.

6. **Nothing executes a trade.** No order execution, no broker integration, no money movement —
   anywhere, ever. The `paper` service simulates and keeps score; that is the whole of it.

7. **Configuration has no hidden defaults.** Providers and model runtimes ship empty and are read
   with no fallback. An absent variable removes a provider from the chain; it never invents an
   address. An unconfigured process must open no socket nobody asked for.

## Pull requests

- Keep changes scoped; match the surrounding code's style, naming and comment density.
- Add or update tests for behaviour you change. Tests must pass without network or a model.
- If your change touches a documented contract (`docs/PAPER_EXECUTION_CONTRACT.md`,
  `docs/PAPER_DASHBOARD_CONTRACT.md`, `docs/PREDICTION_AUTOMATION_CONTRACT.md`), update the contract
  in the same PR — a divergence between the document and the code is a defect in whichever moved.
- Explain *why*, not just *what*. This codebase's comments carry reasoning on purpose.

## Reporting bugs

Open an issue with what you ran, what you expected and what happened, plus the provenance labels the
UI showed (`live` / `seed` / `synthetic`) and which runtime served the model read. For anything
security-related, use [SECURITY.md](SECURITY.md) instead of a public issue.

## Code of conduct

Participation here is governed by the [Code of Conduct](CODE_OF_CONDUCT.md). It is the Contributor
Covenant, unmodified; the short version is that disagreement about the code is welcome and
disagreement about a person is not.

## License

By contributing you agree that your contributions are licensed under the
[MIT License](LICENSE).
