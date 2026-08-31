import { useCallback, useEffect, useState } from "react";
import { EmptyState, SkeletonText } from "../ui/index.js";
import { Micro, Tag } from "../terminal/bits.jsx";
import { useAuth } from "../../auth/AuthContext.jsx";
import {
  AuthRequiredError,
  REASONING_QUALITY,
  decisionErrorMessage,
  fmtDecisionTime,
  listDecisions,
  listOutcomes,
} from "../../lib/decisionApi.js";

// OutcomeLog — everything you have reviewed, newest first, plus what is still waiting.
//
// This is the Review stage read on its own. It answers "what have I learned?" rather than "what did I
// do?", so it leads with the reasoning assessment and the lessons, and mentions the market result
// only as a labelled aside on the records that have one.
//
// The distribution strip at the top counts REASONING scores. It deliberately does not chart P&L
// against them: the correlation between "I reasoned well" and "it paid" over a handful of decisions
// is noise, and drawing it would teach exactly the lesson this step exists to prevent.

function ScoreStrip({ outcomes }) {
  const scored = outcomes.filter((o) => o.reasoningQuality != null);
  if (scored.length === 0) return null;
  const counts = REASONING_QUALITY.map((r) => ({
    ...r,
    n: scored.filter((o) => o.reasoningQuality === r.value).length,
  }));
  const max = Math.max(...counts.map((c) => c.n), 1);
  return (
    <div className="border-b border-line px-[22px] py-3">
      <div className="mb-2 label-mono text-muted">
        how you rated your own reasoning ({scored.length} reviewed)
      </div>
      <div className="flex items-end gap-2">
        {counts.map((c) => (
          <div key={c.value} className="flex min-w-0 flex-1 flex-col items-center gap-1">
            <div
              className="w-full rounded-t bg-accent/30"
              style={{ height: `${Math.max(3, (c.n / max) * 46)}px` }}
              aria-hidden="true"
            />
            <span className="num text-[11px] text-fg/80">{c.n}</span>
            <span className="truncate text-[10px] text-muted/70">{c.label}</span>
          </div>
        ))}
      </div>
      <p className="mt-2 text-[11px] leading-relaxed text-muted/70">
        Process scores only. They are not compared with, divided by, or coloured by profit — a good
        result is not evidence of good reasoning, and this app will not imply that it is.
      </p>
    </div>
  );
}

export default function OutcomeLog({ ticker = "" }) {
  const { user, promptSignIn } = useAuth();
  const [state, setState] = useState({ status: "loading", outcomes: [], decisions: {}, pending: 0 });

  const load = useCallback(async () => {
    if (!user) {
      setState({ status: "guest", outcomes: [], decisions: {}, pending: 0 });
      return;
    }
    setState((s) => ({ ...s, status: "loading" }));
    try {
      const [outcomes, { decisions }] = await Promise.all([
        listOutcomes({ ticker }),
        listDecisions({ ticker }),
      ]);
      const byId = {};
      for (const d of decisions) byId[d.id] = d;
      const reviewed = new Set(outcomes.map((o) => o.decisionId));
      setState({
        status: "ready",
        outcomes,
        decisions: byId,
        pending: decisions.filter((d) => !reviewed.has(d.id)).length,
      });
    } catch (err) {
      if (err instanceof AuthRequiredError) {
        setState({ status: "guest", outcomes: [], decisions: {}, pending: 0 });
        return;
      }
      setState({ status: "error", outcomes: [], decisions: {}, pending: 0, message: decisionErrorMessage(err) });
    }
  }, [user, ticker]);

  useEffect(() => {
    load();
  }, [load]);

  if (state.status === "guest") {
    return (
      <div className="px-[22px] py-5">
        <EmptyState title="Sign in to see your reviews">
          <button type="button" onClick={promptSignIn} className="text-accent hover:brightness-125">
            Sign in
          </button>{" "}
          to look back at the decisions you have reviewed.
        </EmptyState>
      </div>
    );
  }

  if (state.status === "error") {
    return <p className="px-[22px] py-4 text-[12.5px] text-muted">{state.message}</p>;
  }

  if (state.status === "loading") {
    return (
      <div className="px-[22px] py-4">
        <SkeletonText lines={4} />
      </div>
    );
  }

  if (state.outcomes.length === 0) {
    return (
      <div className="px-[22px] py-5">
        <EmptyState title="No outcome reviews yet">
          {state.pending > 0
            ? `You have ${state.pending} decision${state.pending === 1 ? "" : "s"} waiting to be reviewed. A review asks two separate questions: what happened, and was the reasoning sound.`
            : "Once you have recorded a decision, come back here to assess how the reasoning held up."}
        </EmptyState>
      </div>
    );
  }

  return (
    <div className="flex flex-col">
      <ScoreStrip outcomes={state.outcomes} />

      {state.pending > 0 && (
        <p className="border-b border-line bg-panel2/30 px-[22px] py-2.5 text-[11.5px] text-muted">
          {state.pending} decision{state.pending === 1 ? "" : "s"} still awaiting a review — find them under
          Decisions.
        </p>
      )}

      <div className="flex h-9 items-center gap-2 border-b border-line bg-panel2/30 px-[22px]">
        <Micro>Reviews</Micro>
        <span className="num text-[10.5px] text-muted/60">{state.outcomes.length}</span>
      </div>

      <ul className="divide-y divide-line">
        {state.outcomes.map((o) => {
          const decision = state.decisions[o.decisionId];
          const meta = REASONING_QUALITY.find((r) => r.value === o.reasoningQuality);
          const mo = o.marketOutcome;
          return (
            <li key={o.id} className="flex flex-col gap-2 px-[22px] py-4">
              <div className="flex flex-wrap items-center gap-x-2.5 gap-y-1.5">
                <span className="num text-[11.5px] font-semibold text-fg/85">
                  {fmtDecisionTime(o.reviewedAt)}
                </span>
                <span className="num text-[11.5px] text-muted">{o.ticker}</span>
                {o.reasoningQuality != null ? (
                  <Tag tone="info">
                    reasoning {o.reasoningQuality}/5 · {meta?.label}
                  </Tag>
                ) : (
                  <Tag tone="outline">reasoning not assessed</Tag>
                )}
                {o.horizonElapsed && <Tag tone="outline">horizon elapsed</Tag>}
              </div>

              {decision && (
                <p className="text-[11.5px] leading-relaxed text-muted/80">
                  Decided {fmtDecisionTime(decision.decidedAt)}: {decision.rationale}
                </p>
              )}

              <p className="whitespace-pre-wrap text-[12.5px] leading-relaxed text-fg/85">{o.observedOutcome}</p>

              {o.processLessons?.length > 0 && (
                <ul className="flex flex-col gap-0.5">
                  {o.processLessons.map((l, i) => (
                    <li key={i} className="text-[11.5px] leading-relaxed text-muted">· {l}</li>
                  ))}
                </ul>
              )}

              {/* the market result, if there is one — stated apart, and labelled with its mode */}
              {mo && (
                <div className="flex flex-wrap items-center gap-2 border-t border-line pt-2">
                  <span className="label-mono text-muted">
                    linked trade result
                  </span>
                  <span className="num text-[12px] text-fg/85">
                    {mo.pnlPct == null ? "—" : `${mo.pnlPct > 0 ? "+" : ""}${mo.pnlPct}%`}
                  </span>
                  <Tag tone={mo.tradeMode === "paper" ? "warn" : "outline"}>
                    {mo.tradeMode === "paper" ? "simulated" : "live"}
                  </Tag>
                  {mo.synthetic && <Tag tone="warn">synthetic price</Tag>}
                </div>
              )}
            </li>
          );
        })}
      </ul>
    </div>
  );
}
