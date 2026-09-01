## What and why

<!-- What changes, and the reasoning behind it. This codebase's comments carry reasoning on
     purpose; its PRs should too. Link the issue if there is one. -->

## How it was verified

<!-- The commands you ran and what they said. "CI is green" is the floor, not the answer. -->

```
```

## Discipline rules

The [rules in CONTRIBUTING.md](../blob/master/CONTRIBUTING.md#discipline-rules) are enforced in code
and in CI. Confirm the ones your change touches; strike through the ones it cannot possibly affect.

- [ ] **1 — No buy/sell language.** No recommendation, price target or verdict reaches a user from the model.
- [ ] **2 — Provenance is required.** Every new number carries where it came from and when. No synthetic data reaches the historical bar store.
- [ ] **3 — The model never invents a number.** Levels and indicators stay deterministic and are re-imposed server-side after the model replies.
- [ ] **4 — No scheduler-triggered model calls.** No timer, cron, poll or page load causes a model call; single-pass workers still drain a bounded batch and exit.
- [ ] **5 — The gateway is stdlib-only Go.** `gateway/go.mod` still has zero requires and `gateway/go.sum` is still empty.
- [ ] **6 — Nothing executes a trade.** No order execution, no broker integration, no money movement.
- [ ] **7 — Configuration has no hidden defaults.** New config is read with no fallback; an absent variable removes a capability rather than inventing an address.

## Checklist

- [ ] Tests added or updated for the behaviour that changed, and they pass with no network and no model.
- [ ] `gofmt -l` is clean for every Go module I touched.
- [ ] Any documented contract I changed (`docs/PAPER_EXECUTION_CONTRACT.md`, `docs/PAPER_DASHBOARD_CONTRACT.md`, `docs/PREDICTION_AUTOMATION_CONTRACT.md`, `.env.example`) is updated in this same PR.
- [ ] No `.env`, key or certificate is in the diff.
- [ ] The change is scoped, and matches the surrounding code's style, naming and comment density.

## Anything a reviewer should push back on

<!-- Trade-offs you made, things you were unsure about, follow-up you deliberately left out. -->
