import { Tag } from "../terminal/bits.jsx";
import { Button } from "../ui/index.js";
import {
  DATA_STATE_LABEL,
  INTENT_LABEL,
  REASONING_QUALITY,
  describeAction,
  fmtDecisionTime,
} from "../../lib/decisionApi.js";

// DecisionCard — one decision, as it was made.
//
// The card's job is to show the state the user decided IN, not the state the thesis is in now. Every
// belief field it renders comes from `decision.snapshot`, which the server froze at record time; the
// live thesis is never read here. That is what makes looking back at an old decision honest rather
// than flattering.
//
// The review block keeps §5.5's separation visible: the reasoning score is stated on its own line,
// and the market result — when there is one — is stated separately and labelled with its mode. They
// are never combined, ranked against each other, or coloured by one another.

const intentTone = { act: "accent", do_not_act: "outline", wait: "warn" };

function SnapshotList({ label, items }) {
  if (!items?.length) return null;
  return (
    <div className="min-w-0">
      <div className="label-mono text-muted">{label}</div>
      <ul className="mt-1 flex flex-col gap-0.5">
        {items.map((it) => (
          <li key={it.id} className="text-[11.5px] leading-relaxed text-muted">
            · {it.text}
          </li>
        ))}
      </ul>
    </div>
  );
}

// ReasoningBadge renders the process score alone. It carries no P&L colouring: a 5 on a losing
// decision and a 2 on a winning one must both read as ordinary.
function ReasoningBadge({ value }) {
  if (value == null) return <Tag tone="outline">reasoning not yet assessed</Tag>;
  const meta = REASONING_QUALITY.find((r) => r.value === value);
  return (
    <span className="inline-flex items-center gap-1.5 rounded-sm border border-line2 bg-panel2 px-2 py-1">
      <span className="label-mono text-muted">reasoning</span>
      <span className="num text-[12px] font-semibold text-fg">{value}/5</span>
      <span className="text-[11.5px] text-muted">{meta?.label}</span>
    </span>
  );
}

// MarketResult is deliberately plain. It states the number, its mode, and its provenance — and says
// nothing about whether the decision was good.
function MarketResult({ outcome }) {
  const mo = outcome?.marketOutcome;
  if (!mo) {
    return (
      <span className="text-[11.5px] text-muted">
        No position was linked, so there is no market result to report.
      </span>
    );
  }
  const pct = mo.pnlPct == null ? null : Number(mo.pnlPct);
  return (
    <span className="flex flex-wrap items-center gap-2">
      <span className="label-mono text-muted">
        linked trade
      </span>
      <span className="num text-[12px] text-fg/85">{pct == null ? "—" : `${pct > 0 ? "+" : ""}${pct}%`}</span>
      <Tag tone={mo.tradeMode === "paper" ? "warn" : "outline"}>
        {mo.tradeMode === "paper" ? "simulated" : "live"}
      </Tag>
      <Tag tone="outline">{mo.realized ? "realized" : "open · marked"}</Tag>
      {mo.synthetic && <Tag tone="warn">synthetic price</Tag>}
    </span>
  );
}

export default function DecisionCard({
  decision,
  outcome = null,
  onReview,
  onRevise,
  onDelete,
  onOpenThesis,
  compact = false,
}) {
  const snap = decision.snapshot || {};
  const dataState = snap.dataState || "unknown";

  return (
    <article className="flex flex-col gap-3 px-[22px] py-4">
      {/* what was decided */}
      <header className="flex flex-wrap items-center gap-x-2.5 gap-y-1.5">
        <span className="num text-[11.5px] font-semibold text-fg/85">
          {fmtDecisionTime(decision.decidedAt)}
        </span>
        <Tag tone={intentTone[decision.intent] || "outline"}>{INTENT_LABEL[decision.intent] || decision.intent}</Tag>
        {decision.action?.kind && decision.action.kind !== "none" && (
          <Tag tone="accent">{describeAction(decision)}</Tag>
        )}
        <span className="num text-[11.5px] text-muted">{decision.ticker}</span>
        {decision.horizon && <Tag tone="outline">{decision.horizon}</Tag>}
        {decision.userConfidence != null && (
          <Tag tone="info">your confidence {decision.userConfidence}</Tag>
        )}
        {dataState === "synthetic" ? (
          <Tag tone="warn">SYNTHETIC data</Tag>
        ) : (
          <Tag tone="outline">{DATA_STATE_LABEL[dataState] || dataState}</Tag>
        )}
        {outcome ? <Tag tone="accent">reviewed</Tag> : <Tag tone="outline">awaiting review</Tag>}
      </header>

      {/* the user's own reasoning */}
      <p className="whitespace-pre-wrap text-[12.5px] leading-relaxed text-fg/85">{decision.rationale}</p>

      {/* the thesis AS IT WAS. never the live one. */}
      {!compact && (
        <div className="rounded-lg border border-line bg-panel2/30 px-3.5 py-3">
          <div className="mb-1.5 flex flex-wrap items-center gap-2">
            <span className="label-mono text-muted">
              thesis at decision time
            </span>
            <Tag tone="outline">frozen snapshot</Tag>
            {onOpenThesis && (
              <button
                type="button"
                onClick={() => onOpenThesis(decision.thesisId)}
                className="text-[11.5px] text-accent hover:brightness-125"
              >
                open the thesis as it is now →
              </button>
            )}
          </div>
          <p className="text-[12px] leading-relaxed text-fg/80">{snap.claim || "(claim unavailable)"}</p>
          <div className="mt-2 flex flex-wrap gap-x-5 gap-y-2">
            <SnapshotList label="assumptions" items={snap.assumptions} />
            <SnapshotList label="would invalidate it" items={snap.invalidationConditions} />
          </div>
          {snap.evidence?.length > 0 && (
            <div className="mt-2.5">
              <div className="label-mono text-muted">
                evidence considered ({snap.evidence.length})
              </div>
              <ul className="mt-1 flex flex-col gap-0.5">
                {snap.evidence.map((ev) => (
                  <li key={ev.id} className="flex flex-wrap items-center gap-1.5 text-[11.5px] text-muted">
                    <span className="truncate">· {ev.title || ev.kind}</span>
                    <Tag tone="outline">{ev.provider}</Tag>
                    {ev.dataState === "synthetic" && <Tag tone="warn">synthetic</Tag>}
                  </li>
                ))}
              </ul>
            </div>
          )}
          <p className="mt-2.5 text-[11px] leading-relaxed text-muted/70">
            Validated signal available at the time:{" "}
            <strong className="text-fg/80">{snap.modelContext?.signalPresent ? "yes" : "no"}</strong>
            {snap.modelContext?.signalReason ? ` — ${snap.modelContext.signalReason}` : ""}.
          </p>
        </div>
      )}

      {/* what the user said they would watch */}
      {(decision.conditionsToMonitor?.length > 0 || decision.openQuestions?.length > 0) && (
        <div className="flex flex-wrap gap-x-6 gap-y-2">
          {decision.conditionsToMonitor?.length > 0 && (
            <div className="min-w-0">
              <div className="label-mono text-muted">
                conditions to monitor
              </div>
              <ul className="mt-1 flex flex-col gap-0.5">
                {decision.conditionsToMonitor.map((c, i) => (
                  <li key={i} className="text-[11.5px] leading-relaxed text-muted">· {c}</li>
                ))}
              </ul>
            </div>
          )}
          {decision.openQuestions?.length > 0 && (
            <div className="min-w-0">
              <div className="label-mono text-muted">
                still unresolved
              </div>
              <ul className="mt-1 flex flex-col gap-0.5">
                {decision.openQuestions.map((q, i) => (
                  <li key={i} className="text-[11.5px] leading-relaxed text-muted">· {q}</li>
                ))}
              </ul>
            </div>
          )}
        </div>
      )}

      {/* the review — reasoning and result stated apart */}
      {outcome ? (
        <div className="rounded-lg border border-line bg-panel2/30 px-3.5 py-3">
          <div className="mb-1.5 flex flex-wrap items-center gap-2">
            <span className="label-mono text-muted">
              reviewed {fmtDecisionTime(outcome.reviewedAt)}
            </span>
            {outcome.horizonElapsed && <Tag tone="outline">horizon elapsed</Tag>}
          </div>
          <p className="whitespace-pre-wrap text-[12px] leading-relaxed text-fg/80">
            {outcome.observedOutcome}
          </p>
          {outcome.assumptions?.length > 0 && (
            <ul className="mt-2 flex flex-col gap-0.5">
              {outcome.assumptions.map((a) => (
                <li key={a.thesisItemId} className="flex flex-wrap items-center gap-1.5 text-[11.5px] text-muted">
                  <Tag tone={a.verdict === "failed" ? "down" : a.verdict === "held" ? "accent" : "outline"}>
                    {a.verdict}
                  </Tag>
                  <span className="truncate">
                    {snap.assumptions?.find((s) => s.id === a.thesisItemId)?.text ||
                      snap.invalidationConditions?.find((s) => s.id === a.thesisItemId)?.text ||
                      a.thesisItemId}
                  </span>
                  {a.note && <span className="text-muted/70">— {a.note}</span>}
                </li>
              ))}
            </ul>
          )}
          {outcome.retrospective && (
            <p className="mt-2 whitespace-pre-wrap text-[12px] leading-relaxed text-muted">
              {outcome.retrospective}
            </p>
          )}
          {outcome.processLessons?.length > 0 && (
            <ul className="mt-2 flex flex-col gap-0.5">
              {outcome.processLessons.map((l, i) => (
                <li key={i} className="text-[11.5px] leading-relaxed text-muted">· {l}</li>
              ))}
            </ul>
          )}
          {/* Two separate rows, deliberately. A good result is not evidence of good reasoning. */}
          <div className="mt-2.5 flex flex-col gap-1.5 border-t border-line pt-2.5">
            <ReasoningBadge value={outcome.reasoningQuality} />
            <MarketResult outcome={outcome} />
          </div>
          {onRevise && (
            <Button size="xs" variant="ghost" className="mt-2" onClick={() => onRevise(decision, outcome)}>
              Revise this review
            </Button>
          )}
        </div>
      ) : (
        onReview && (
          <div className="flex flex-wrap items-center gap-2">
            <Button size="sm" variant="secondary" onClick={() => onReview(decision)}>
              Review the outcome
            </Button>
            <span className="text-[11.5px] text-muted">
              Assess the reasoning separately from what the market did.
            </span>
          </div>
        )
      )}

      {onDelete && (
        <div>
          <Button size="xs" variant="danger" onClick={() => onDelete(decision)}>
            Delete this record
          </Button>
        </div>
      )}
    </article>
  );
}
