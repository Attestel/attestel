# Disclosure classification — required vs brand

**Step 11 deliverable** required by [`research-os/PARALLEL_CONTRACTS.md`](research-os/PARALLEL_CONTRACTS.md)
§6.5 ("Step 11 produces the table") for **D-17**.

> **This table is a proposal for counsel, not a legal opinion, and not clearance.**
> D-17 is `DECIDED` as a *product position* only; external counsel review is **outstanding**
> ([`research-os/DECISION_REGISTER.md`](research-os/DECISION_REGISTER.md) §4, "Legal caveat carried
> forward"). Until counsel classifies a string, **Step 11 removed none of them.** Every string below
> is present in the product today, in the location listed.

## Rules applied

1. **Nothing was removed.** §6.5: *"Step 11 may not remove a string counsel has not classified."*
   The only copy this step rewrote is positioning copy (the auth brand panel), and the disclosures
   that lived there were kept verbatim — they no longer *lead*, but they are not softened.
2. **The synthetic-data banner is not a disclaimer** and is never softened, shortened, relocated, or
   made conditional. It is invariant #1's visible half. It is listed separately below.
3. **Required disclosures stay adjacent to the output they qualify** — model output, simulation,
   prediction, and journal — never as navigation furniture. The nav-level footer was already removed
   in Step 01; nothing was moved in Step 11.

## Legend

- **Required (proposed)** — qualifies model output, a simulation, a prediction, or a personal
  financial record. Recommended to counsel as required; treat as required until told otherwise.
- **Brand (proposed)** — a trust/positioning choice that no output depends on.
- **Invariant** — not a disclaimer at all; a factual state label the architecture guarantees.

## Table

| # | String (abridged) | Location | Qualifies | Proposed class |
|---|---|---|---|---|
| 1 | "⚠ SYNTHETIC price data — generated seed bars (no price key / network). Nothing here is tradeable." | `web/src/App.jsx:284` | the price data itself | **Invariant** — never soften |
| 2 | "Suggestion only — you place the trade. This app never executes orders." | `web/src/components/terminal/SignalBand.jsx:252` | the directional signal | Required (proposed) |
| 3 | "Not investment advice. A backtested probability of a modest edge — not a guarantee… a Sharpe above 3 is flagged as probable leakage…" | `web/src/components/terminal/SignalBand.jsx:140` | the directional signal | Required (proposed) |
| 4 | "✎ PAPER — simulated, no real money." | `web/src/components/PaperTradingPanel.jsx:118` | the simulated book | Required (proposed) |
| 5 | "…directional correctness. Simulation only; not investment advice." | `web/src/components/PaperTradingPanel.jsx:287` | the simulated book | Required (proposed) |
| 6 | "Paper view — simulated, no real money" (badge title) | `web/src/components/shell/TopBar.jsx:93` | the simulated book | Required (proposed) |
| 7 | "For your own record-keeping… not financial advice and places no orders — you record trades you made yourself." | `web/src/components/JournalPanel.jsx:464` | the journal | Required (proposed) |
| 8 | "Manual record · No execution" | `web/src/components/JournalPanel.jsx:460` | the journal | Required (proposed) |
| 9 | "AI-generated summary — informational, not investment advice" | `web/src/components/brief/BriefPane.jsx:65`, `today/LeadPanel.jsx:100`, served by `services/llm/app/main.py:210` | the brief / lead read | Required (proposed) |
| 10 | "Contextual research support. Not investment advice, not a recommendation." (`LENS_DISCLAIMER`) | `services/llm/app/lens.py:44`, rendered on every saved review | lens output | Required (proposed) |
| 11 | `COMMITTEE_DISCLAIMER` | `services/llm/app/committee.py:37` | perspectives output | Required (proposed) |
| 12 | `DIGEST_DISCLAIMER` | `services/llm/app/digest.py:16` | headline clustering output | Required (proposed) |
| 13 | "Language analysis, not investment advice." | `web/src/components/TranscriptsView.jsx:62` | transcript analysis | Required (proposed) |
| 14 | "Informational, not financial advice — your decision and your risk." (`ASSISTANT_DISCLAIMER`) | `services/llm/app/assistant.py:18` | assistant note (route currently has no UI consumer — D-21) | Required (proposed) |
| 15 | "Position sizing only — this app never places an order. You decide and execute." | `web/src/components/risk/PositionSizer.jsx:236` | the position sizer | Required (proposed) |
| 16 | "…checklist is all-green. This app never places an order; you decide and execute." | `web/src/components/terminal/BeginnerTerminal.jsx:209` | the beginner checklist | Required (proposed) |
| 17 | "I understand this is an analysis tool and **not investment advice**." (signup consent checkbox) | `web/src/components/auth/SignUp.jsx:165`, enforced at `:70` | account creation | Required (proposed) |
| 18 | "By continuing you agree to the Terms & Privacy Policy. Informational tool — not investment advice." | `web/src/components/auth/SignIn.jsx:141` | account creation / sign-in | Required (proposed) |
| 19 | "Informational tool — not investment advice." | `web/src/components/auth/SignUp.jsx:184` | account creation | Required (proposed) |
| 20 | The five "honest odds" points, incl. "This app suggests and simulates — you execute. It never places an order, integrates a broker, or moves money — ever." | `web/src/components/onboarding/RealityCheck.jsx:17-23` | first-run expectations | Required (proposed) — **unchanged by Step 11** |
| 21 | "Figures are from this project's own research notes (docs/PREDICTION_MODELS.md). This app never places an order, integrates a broker, or moves money." | `web/src/components/onboarding/RealityCheck.jsx:154` | first-run expectations | Required (proposed) — unchanged |
| 22 | "Analysis tool — no buy/sell verdicts, no order execution." | `web/src/components/auth/AuthLayout.jsx:83` (brand panel footer) | the product as a whole | Brand (proposed) — **kept anyway**, pending classification |
| 23 | "Analysis only; you place every trade." | `web/src/components/auth/AuthLayout.jsx:80` (brand panel) | the product as a whole | Brand (proposed) — **kept**, relocated within the same panel below the proof points |
| 24 | "simulated" / "live" mode labels on every decision, outcome and trade row | `research/DecisionsSection.jsx:73`, `decisions/DecisionCard.jsx:73`, `decisions/OutcomeLog.jsx:196`, `decisions/RecordDecisionForm.jsx:348` | paper vs live records | **Invariant** — §5.6 forbids merging the two |

## What Step 11 changed

| Change | File | Why it is not a disclosure change |
|---|---|---|
| Brand panel now leads with the approved hero, supporting line, and three proof points instead of a feature list and a mocked `BUY · 65.7%` signal card | `web/src/components/auth/AuthLayout.jsx` | The removed content was a *feature list* and a *mock*, not a disclosure. Rows 22 and 23 were both retained. Leading with a mocked buy signal also violated §6.5's rule against leading with "prediction signal". |
| First-run action label: "Start beginner journey" → "Start a guided research review" | `web/src/components/onboarding/RealityCheck.jsx:137` | A button label. All five honest-odds points and the closing note (rows 20, 21) are byte-identical. |
| "What it does do" paragraph re-worded into research-loop vocabulary | `web/src/components/onboarding/RealityCheck.jsx:105` | A positive claim about the product, not a disclosure. It still refuses to promise profit. |

## Open external gate

**External counsel review of D-07, D-08, D-09 and D-17 remains outstanding.** Counsel may reclassify
any row above, and may narrow the evidence excerpt caps (kept as one named constant per kind in
`journal/evidence.go` precisely so a narrowing is a one-line change). No engineering work unblocks
this; it is a scheduling and legal-review item, not a defect.
