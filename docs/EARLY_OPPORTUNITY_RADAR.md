# Early Opportunity Radar contract

Version 2 implements a bounded, deterministic research scanner over real, completed daily bars.
It preserves Version 1's price thresholds while requiring the headline-subject-filtered
`scout@2` evidence context; pre-fix Radar snapshots are withheld rather than presented as current.
Its question is narrower than “what rose today?”:

> Which covered companies have evidence accumulating before a move becomes extended, and which
> companies have already moved too far for this detector to call them early?

## States

| State | Meaning | Allowed interpretation |
|---|---|---|
| `emerging` | Pre-breakout evidence cleared the versioned threshold. | Open or update a research file. |
| `confirmed` | Price closed beyond the prior 20-session high with volume and relative-strength confirmation, without tripping a no-chase rule. | The setup received price confirmation; it is still only a research lead. |
| `extended` | The 1/2-session move or ATR extension crossed a fixed no-chase threshold. | The radar may have found the move late. Do not describe this state as early. |
| `invalidated` | A previously visible setup lost trend support or enough evidence. | The prior research setup no longer qualifies under this detector version. |

The state machine uses hysteresis: a previously active setup does not disappear on a small score
oscillation. It becomes `invalidated` only after a material trend/score failure.

## Evidence and frozen thresholds (`early-opportunity@2`)

The price setup score is an ordering device, not a probability:

- 30%: distance to the prior 20-session high;
- 25%: five-session return relative to the configured benchmark (SPY by default);
- 20%: close above EMA20/SMA50 and a rising EMA20;
- 15%: current volume relative to the prior 20 sessions;
- 10%: five-session true-range compression relative to the prior 20 sessions.

Stored Scout event/catalyst attention is context, not a duplicate direction call. It contributes
at most 25% of the combined evidence score and can surface an event-led setup only when the price
setup is already at least partial. A new detector version is required before changing a weight or
threshold.

Rules:

- emerging: combined evidence `>= 0.62`, or event/catalyst evidence `>= 0.55` with price evidence
  `>= 0.45`;
- confirmed: close at/above the prior 20-session high, relative volume `>= 1.20`, positive
  five-session excess return, and combined evidence `>= 0.62`;
- extended/no-chase: one-session return `>= 6%`, two-session return `>= 8%`, or close `>= 2.5 ATR`
  above EMA20;
- invalidated: a previously active setup closes below EMA20 or its combined evidence falls below
  `0.45`.

## Safety and operation

- Synthetic, stale, incomplete, or insufficient bar histories are excluded and named in coverage.
- The scheduler lane is model-free. It cannot call Qwen, prediction, promotion, paper, a broker, or
  an order endpoint.
- `GET /opportunities` and `GET /api/opportunities` read only the latest PostgreSQL snapshot.
- Repeated scheduler passes over the same bar/source/context fingerprint are idempotent.
- Every card reports paper eligibility as **not assessed**. A paper position remains possible only
  through the prediction service's independent walk-forward and evaluator gates.
- Version 1 is daily-primary. Intraday early detection and forward outcome calibration are separate
  later phases; neither is implied by this first slice.
