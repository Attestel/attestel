# BEGINNER.md — paving the way for beginner traders

> **HISTORICAL SPEC — now BUILT.** Every piece described here shipped: the reality-check first run
> (`web/src/components/onboarding/RealityCheck.jsx`), the position sizer
> (`components/risk/PositionSizer.jsx`), the pre-trade checklist
> (`components/checklist/PreTradeChecklist.jsx`), and beginner/standard progressive disclosure with
> graduation (`lib/routes.js` `minLevel` + `components/onboarding/GraduationPrompt.jsx`). Step 11
> then added the thesis-first guided review on top (`components/onboarding/ResearchGuide.jsx`,
> `docs/PRODUCT_METRICS.md` §4). Read this for the reasoning; read the code for the behaviour.

Design spec for the beginner-onboarding layer. **Nothing here is built yet** — this is the brief
we implement from (same role as `docs/PHASE_SWING.md` played before the swing phase). Companion
entries live in `GAPS.md`.

## The premise (why this layer, and why it's *subtraction*)

Research across trading-education platforms, broker resources, and community discussion converges on
one through-line: **beginners think their problem is knowledge** (which indicator, which pattern),
but the recurring evidence says their real problems are **filtering** (too much input → analysis
paralysis) and **psychology/discipline** (executing a plan under emotional pressure). Tools that
*reduce inputs* and *enforce process* beat more content.

That is unusually good news for this repo, because the platform is already a **discipline machine**:
the backtest-gated signal that refuses a call without a passing walk-forward test; the paper service
that won't call a signal "meaningful" until ≥30 closed trades where live ≈ backtest; the journal that
snapshots the analytical read *at entry* for the review moment; the edge harness that prints
EDGE / NO EDGE / SUSPECT. We already serve the *validate-an-edge* and *enforce-discipline* needs
better than almost anyone.

The problem is the opposite one. A day-1 beginner dropped into the current cockpit meets Committee
debates, multi-timeframe Confluence grids, PEAD event studies, VWAP/OBV/ADX, Transcripts, Scenario
mode — a **firehose**, which is the single loudest thing the research warns against. So this layer is
mostly **composition and gating of what already exists**, plus three small deterministic additions.
The guiding principle is **progressive disclosure**: a beginner sees one simple, linear, gated path;
the expert surface is *earned*, not defaulted.

**Scope of this spec (the four pieces chosen):** reality-check first-run, position sizer, pre-trade
checklist gate, beginner mode. Emotion-tagging on the journal is noted as a natural follow-on but is
out of scope here.

## What already serves beginners (do NOT rebuild)

| Beginner need (from research) | Already in the repo |
|---|---|
| Validate an edge | `services/prediction` walk-forward backtest, edge/event harnesses, `paper/` out-of-sample check |
| A trading journal / feedback loop | `journal/` — P&L, R-multiple, `attachedRead` at entry, `/stats` |
| Paper practice → live bridge | `paper/` — ≥30-closed-trade "meaningful" gate, live-vs-backtest divergence flag |
| Curated info instead of a firehose (partial) | Brief, News cascade Digest, crowd sentiment |
| Written thesis per position | `journal` theses + per-trade `thesis` field |

The four pieces below fill what's left: **expectations**, **risk sizing**, **a finite pre-trade
decision**, and **a surface a beginner won't drown in**.

---

## Piece 1 — Reality-check first-run (expectations reset)

**Problem it solves:** *Unrealistic expectations* — the research's driver of overleveraging and
emotional swings. Beginners expect fast profits; the honest arc to consistency is 2–3 years, real
edges are ~53–56% hit-rate and they decay. We already say this in `docs/PREDICTION_MODELS.md`; most
platforms bury it because it hurts conversion. Leading with it is a **trust moat**, not a cost.

**Design:** a one-time, full-screen acknowledgement shown **right after sign-in, before the terminal**
(and reachable later from Settings). Plain language, the honest numbers, no dark patterns. It is an
*expectations reset*, not a disclaimer wall — it ends with two actions: **"Start beginner journey"**
(→ Beginner mode) and **"Skip"** (→ Standard mode, the experienced path). Sign-out lands on the
sign-in screen (already the behavior of `AuthContext.logout()`); the next sign-in re-presents the modal
only if the user hasn't acknowledged yet.

Content (sourced verbatim from `docs/PREDICTION_MODELS.md`, never invented):
- Most people who trade actively lose money; the edge is small and perishable.
- Realistic validated edges are ~53–56% hit-rate, Sharpe ~1–2, and they decay.
- Consistency is a ~2–3 year arc. Anyone promising faster is selling something.
- This app **suggests and simulates; you execute**. It never places an order or moves money.

**Where it lives:** pure frontend (see prompt `docs/prompts/23-reality-check.md`).
- State: extend the per-user settings object (`web/src/lib/api.js` `DEFAULT_SETTINGS`, server-saved
  via `auth`/settings) with `ackRealityCheck: false` and `experienceLevel: "beginner"` (see Piece 4).
- Gate component `web/src/components/onboarding/RealityCheck.jsx`, mounted as an **overlay in the authed
  shell** (`App.jsx`) when `user && hydrated && settings.ackRealityCheck !== true`. Because `SignIn` /
  `SignUp` navigate into that shell via `consumeReturnTo()` after auth, the overlay naturally appears on
  sign-in before the terminal is usable — no separate route.
- **`hydrated` flag** (new, on `SettingsContext`): the modal must wait for the per-user settings GET to
  resolve, or it flashes for already-acknowledged users on the stale `DEFAULT_SETTINGS` frame (ack=false).
- **Guests are not gated** — the modal is tied to sign-in, so a read-only guest never sees it; it appears
  only once a user is signed in and unacknowledged.
- "Start beginner journey" persists `{ackRealityCheck:true, experienceLevel:"beginner"}`; "Skip" persists
  `{ackRealityCheck:true, experienceLevel:"standard"}`.
- Re-openable from `SettingsPanel.jsx` ("Review the reality check") in a review-only mode that does NOT
  change the user's experience level.

**Invariant notes:** no LLM, no network, no verdict — pure static content + a boolean. Preserves the
zero-key/zero-network invariant.

---

## Piece 2 — Position sizer (the 1–2% rule, made concrete)

**Problem it solves:** *Poor risk management* — risking too much per trade, no position sizing, the
thing that actually blows up accounts. Most beginners have never heard the 1–2%-per-trade convention.
The journal already computes R-multiple *after* the fact; the sizer enforces risk *before* entry.

**Design (deterministic math — no model, no network):**
```
riskPerShare   = |entry − stop|                     (per-unit risk)
dollarRisk     = accountEquity × (riskPct / 100)    (riskPct default 1%, capped at 2% with a warning)
shares         = floor(dollarRisk / riskPerShare)
positionValue  = shares × entry
rr             = target ? |target − entry| / riskPerShare : null     (reward:risk)
```
Inputs: `accountEquity`, `riskPct`, `entry`, `stop`, optional `target`. Outputs: `shares`,
`positionValue`, `dollarRisk`, `rr`, and human-readable flags (`riskPctOverCap`,
`positionExceedsEquity`, `stopMissing`, `rrBelowFloor` when `rr < ~1.5`).

**Stop assistance (reuses computed numbers, never invents them):** the sizer can pre-fill a suggested
stop from data the dashboard *already* carries — `regime` support/resistance and ATR (e.g. a
1.5×ATR(14) stop, or the nearest computed support below entry). These come from the analysis service;
the sizer only *consumes* them, so invariant #3 (the LLM never invents a number — and neither does the
sizer) holds trivially.

**Where it lives:** client-side is the right call — the math is trivial and instant, and it keeps the
gateway stdlib-only (invariant #5) and needs no new service.
- `web/src/lib/positionSize.js` — the pure function + flag logic (unit-testable in isolation; note the
  repo has no web test runner today, so keep it a plain pure module).
- `web/src/components/risk/PositionSizer.jsx` — the widget. Lives (a) inline in the Journal log-trade
  form (`JournalPanel.jsx`), auto-filling `entry.size` from the computed `shares`, and (b) as a step
  in the pre-trade checklist (Piece 3).
- Account equity: add `accountEquity` + `defaultRiskPct` to the per-user settings object so it follows
  the user; guests use an in-memory value.

**Persist the risk decision (enables the review loop):** when a trade is logged, store the sizing
inputs so a later review can ask "did I actually size to 1%?". Add an **optional** nested field to the
journal `Trade` (backward-compatible, `omitempty`):
```go
// journal/trades.go — Trade
Risk *RiskPlan `json:"risk,omitempty"`

type RiskPlan struct {
    AccountEquity float64 `json:"accountEquity"`
    RiskPct       float64 `json:"riskPct"`
    DollarRisk    float64 `json:"dollarRisk"`
    RR            *float64 `json:"rr,omitempty"`
}
```
`validateTrade` accepts it if present, ignores it if absent — old records and manual trades are
unaffected.

**Invariant notes:** descriptive risk math only; no order, no broker, no money movement (invariant #2).
No LLM. Gateway untouched. Zero-key/zero-network preserved.

---

## Piece 3 — Pre-trade checklist gate (the cure for paralysis)

**Problem it solves:** *Analysis paralysis* — the #1 beginner trap. The most-recommended fix in the
research is a **4–6 binary yes/no checklist** that converts open-ended analysis into a *finite*
decision. The elegant part: we assemble it almost entirely from things the platform already computes,
so it's a **synthesis of existing panels**, not new analysis.

**The checklist (each item is yes/no, and each maps to existing data):**

| # | Question | Source (already computed) |
|---|---|---|
| 1 | Is there a **validated signal** (passed backtest) **or** a written **thesis**? | `dash.signal` (prediction) / trade `thesis` |
| 2 | Is my **risk ≤ my cap** (1–2% of equity)? | Piece 2 position sizer |
| 3 | Is a **stop set** and is **R:R ≥ ~1.5**? | entry/stop/target + sizer `rr` |
| 4 | Does **multi-timeframe confluence agree**, or have I **acknowledged the conflict**? | `dash.confluence` |
| 5 | Am I entering on my **plan**, not FOMO / news-chasing? | self-attest |
| 6 | Have I **written down why** (the thesis)? | trade `thesis` non-empty |

Items 1–4 can be **auto-evaluated** from the dashboard/sizer and shown pre-ticked (green) or failing
(amber) so the beginner sees the state of their setup at a glance; 5–6 are self-attest / require input.
The gate is **advisory-not-prescriptive**: it never says "buy" — it says "your plan is complete" or
"3 of 6 unmet". It composes facts; it does not add an opinion (invariant #2).

**Behavior:** the existing **"Log as trade →"** flow (SignalBand → `logAsTrade` prefill → JournalPanel)
gains the checklist as the step before submit. In **Beginner mode**, all six must be green (or item 4's
conflict explicitly acknowledged) before "Log as trade" activates. In **Standard mode** the checklist
is *shown and scored* but non-blocking — an expert can log anything. This keeps discipline for
beginners without patronising advanced users.

**Where it lives:**
- `web/src/lib/checklist.js` — pure evaluator: `(dashboard, sizerResult, tradeDraft, mode) → {items[], allPass}`.
- `web/src/components/checklist/PreTradeChecklist.jsx` — rendered in the log-trade flow.
- **Persist the checklist result with the trade** (the review loop — do skipped-checklist trades do
  worse?). Optional nested field, backward-compatible:
```go
// journal/trades.go — Trade
Checklist *Checklist `json:"checklist,omitempty"`

type Checklist struct {
    Items      []ChecklistItem `json:"items"`      // {key, label, passed, source}
    AllPass    bool            `json:"allPass"`
    Mode       string          `json:"mode"`       // beginner | standard
}
```
`journal/stats.go` can later split win-rate by `checklist.allPass` to *show the beginner their own
evidence* that following the checklist helped — the highest-value coaching we can give, and it's
descriptive, from their own record.

**Invariant notes:** the gate is a composition of already-descriptive facts; it emits no buy/sell
verdict and executes nothing. Runs offline (every input is already local by the time the dashboard has
loaded). No LLM.

---

## Piece 4 — Beginner mode (progressive disclosure)

**Problem it solves:** *Information overload.* The platform's own richness is the liability. Beginner
mode is the **strong default** that collapses ~90% of the surface into a linear path, and unlocks the
rest as the user earns it.

**The `experienceLevel` setting** (server-saved, per user; guests default to `beginner` in memory):
`"beginner" | "standard"`. Drives three things:

1. **Navigation (`LeftRail.jsx` / `NAV`).** Tag each `NAV` entry with a `minLevel`. Beginner sees a
   short rail: **Terminal, Journal, Paper, Settings** (+ Brief for curated context). Standard sees the
   full rail (Committee, Confluence, Transcripts, News, Fundamentals, Alerts, Feedback…). `App.jsx`
   `VALID_VIEWS` is derived from the *visible* set so a hidden view isn't hash-routable in beginner
   mode (deep-link falls back to Terminal).
2. **Terminal density.** In beginner mode the Terminal shows a **simplified column set**: price chart,
   plain-language regime, the position sizer, the pre-trade checklist, and the signal *with its track
   record and confidence framing* — but hides the dense Confluence grid / committee cross-refs. The
   research's consensus fix is **2–3 non-redundant indicators**, so the beginner chart defaults to a
   small, curated indicator set rather than the full suite.
3. **Paper-first nudge.** Beginner mode defaults the Journal log-trade `mode` to `paper` and surfaces
   the "don't commit real capital until ≥30 paper trades with live ≈ backtest" rule of thumb inline.

**Graduation (earned, not just a toggle).** Compute a readiness hint from the user's *own* journal —
e.g. **≥N closed trades logged with the checklist completed** (and, ideally, paper-validated). When met,
prompt: "You've logged 20 disciplined trades — unlock the full cockpit?" The user can always flip
`experienceLevel` manually in Settings (with a short "here's what this adds" note), but the *default*
path is earned. Criterion is computable today from `journal /stats` + the new `checklist` field.

**Where it lives:** frontend only.
- Setting on the per-user settings object (`experienceLevel`), read via `SettingsContext` /
  `useSettings()` (already the pattern for signal prefs).
- `NAV` entries gain `minLevel`; `LeftRail` and `App.jsx` filter by it.
- `Terminal.jsx` chooses a column layout by level.
- Graduation check: a small selector over journal stats; surfaced as a dismissible prompt.

**Invariant notes:** purely a *view/gating* concern. Signing in / graduating unlocks **no trading
action** (invariant #2 — accounts only scope data). Zero-key/zero-network preserved; the whole
beginner path works on synthetic data (loudly labelled, as today).

---

## Cross-cutting invariant compliance

All four pieces are deliberately inside the existing rails:
1. **Zero keys / zero network still boots the whole beginner path.** Reality-check is static; sizer and
   checklist are client math over already-loaded data; beginner mode is view gating. Nothing new needs
   a key or the network.
2. **No buy/sell verdicts, no execution.** The sizer computes *risk*, the checklist scores *plan
   completeness*, neither emits a recommendation; the only directional call remains the backtest-gated
   `prediction` signal, unchanged. No broker, no orders, no money — anywhere.
3. **No invented numbers.** The sizer consumes analysis-computed ATR / support-resistance; it never
   fabricates levels. No LLM touches any of these features.
4. **LLM stays out of fast loops.** None of these four use the LLM at all.
5. **Gateway stays stdlib-only Go.** The only backend change is *optional, backward-compatible* fields
   on the journal `Trade` (`risk`, `checklist`) — stdlib JSON, no new modules, no new service.

## Phased build order

1. **Settings scaffolding** — add `experienceLevel`, `ackRealityCheck`, `accountEquity`,
   `defaultRiskPct` to `DEFAULT_SETTINGS` and the server settings validation. Everything else hangs
   off these.
2. **Position sizer** (`lib/positionSize.js` + `PositionSizer.jsx`), wired into the Journal form.
   Self-contained, immediately useful, no dependency on the other pieces.
3. **Pre-trade checklist** (`lib/checklist.js` + `PreTradeChecklist.jsx`) — depends on the sizer (item
   2/3). Non-blocking in Standard first; wire the blocking behavior once Beginner mode exists.
4. **Journal persistence** — add optional `risk` + `checklist` fields to `journal/trades.go`
   (`omitempty`, accepted-if-present). Then extend `journal/stats.go` to split win-rate by
   `checklist.allPass` for the coaching view.
5. **Beginner mode** — `minLevel` on `NAV`, level-aware `LeftRail` / `App.jsx` / `Terminal.jsx`,
   paper-first default, graduation prompt.
6. **Reality-check first-run** — the entry gate; ships last so it can hand off cleanly into Beginner
   mode.

## Open decisions (worth a call before building)

- **Blocking vs. advisory checklist in Beginner mode.** Spec assumes *blocking* for beginners
  (all-green to log) and *advisory* for standard. Confirm we're comfortable gating the log action.
- **Graduation threshold `N`.** Placeholder is ~20 checklist-complete closed trades; pick the real
  number (and whether paper-validation is required to graduate).
- **Reality-check trigger (RESOLVED).** Shown as a sign-in-triggered, `ackRealityCheck`-gated overlay
  (not every login, not a Root pre-shell gate); guests aren't gated; "Skip" → Standard mode. See
  `docs/prompts/23-reality-check.md`.
- **Emotion tagging** (out of scope here) — the journal `Tags[]` field already exists and is the
  natural home for a fear/greed/FOMO/revenge tag set + a stats breakdown. Flag for a follow-on if we
  want the psychology loop to go beyond the checklist.
