import { useCallback, useEffect, useState } from "react";
import { fetchReactions } from "../../lib/api.js";
import { Badge } from "../ui/index.js";

// ReactionEvidence — Phase 4: what the market did after this event, shown as EVIDENCE rather than
// as a headline number.
//
// Every window carries the bars it was computed from, the bar source, whether those bars were
// synthetic, and the calculation version — because a return with no provenance is indistinguishable
// from a return somebody made up. An unresolved window says so; it never renders as a dash the eye
// reads as zero.
//
// Nothing here says the event CAUSED the move.

const HORIZON_LABEL = { "1d": "1 day", "5d": "5 days", "20d": "20 days" };

const SESSION_LABEL = {
  before_market: "before the open",
  regular: "during the session",
  after_market: "after the close",
  non_trading_day: "on a non-trading day",
};

function pct(value) {
  if (typeof value !== "number" || !Number.isFinite(value)) return null;
  const sign = value > 0 ? "+" : "";
  return `${sign}${(value * 100).toFixed(2)}%`;
}

function Move({ value, muted }) {
  const rendered = pct(value);
  if (rendered === null) return <span className="text-dim">—</span>;
  const tone = muted ? "text-muted" : value > 0 ? "text-up" : value < 0 ? "text-down" : "text-muted";
  return <span className={`num ${tone}`}>{rendered}</span>;
}

function WindowRow({ window }) {
  if (window.state !== "resolved") {
    return (
      <tr className="border-t border-line/50">
        <td className="py-1.5 pr-3 text-[11.5px] text-muted">{HORIZON_LABEL[window.horizon] || window.horizon}</td>
        <td className="py-1.5 pr-3 text-[11.5px] text-dim" colSpan={4}>
          {window.state === "pending"
            ? "not yet matured — this window has no result to report"
            : `unavailable — ${window.missingReason || "no stored bars"}`}
        </td>
      </tr>
    );
  }
  return (
    <tr className="border-t border-line/50">
      <td className="py-1.5 pr-3 text-[11.5px] text-muted">{HORIZON_LABEL[window.horizon] || window.horizon}</td>
      <td className="py-1.5 pr-3 text-[11.5px]"><Move value={window.rawReturn} /></td>
      <td className="py-1.5 pr-3 text-[11.5px]">
        {window.benchmarkReturn === null || window.benchmarkReturn === undefined ? (
          <span className="text-dim" title="no benchmark bars stored for this window">no benchmark</span>
        ) : (
          <Move value={window.benchmarkReturn} muted />
        )}
      </td>
      <td className="py-1.5 pr-3 text-[11.5px]"><Move value={window.excessReturn} /></td>
      <td className="py-1.5 text-[11px] text-dim">
        {window.barsUsed} bar(s) · {window.barSource || "—"}
        {window.synthetic && <span className="text-warn"> · synthetic</span>}
      </td>
    </tr>
  );
}

export default function ReactionEvidence({ eventId, ticker }) {
  const [state, setState] = useState({ status: "idle", data: null, error: "" });

  const load = useCallback(async () => {
    if (!eventId && !ticker) return;
    setState({ status: "loading", data: null, error: "" });
    try {
      const data = await fetchReactions({ eventId, ticker });
      setState({ status: "ready", data, error: "" });
    } catch (err) {
      setState({ status: "error", data: null, error: err?.message || "unavailable" });
    }
  }, [eventId, ticker]);

  // One read when the event is opened. No interval.
  useEffect(() => {
    load();
  }, [load]);

  const reactions = state.data?.reactions || [];

  if (state.status === "loading") {
    return <p className="mt-2 text-[11px] text-dim">Loading stored reaction evidence…</p>;
  }
  if (state.status === "error") {
    return <p className="mt-2 text-[11px] text-warn">Reaction evidence unavailable: {state.error}</p>;
  }
  if (reactions.length === 0) {
    return (
      <p className="mt-2 text-[11px] leading-relaxed text-dim">
        No stored reaction for this event yet. Reactions are captured from stored bars by a
        background pass; an empty result means the evidence has not been recorded, not that
        nothing happened.
      </p>
    );
  }

  return (
    <div className="mt-2 flex flex-col gap-3">
      {reactions.map((reaction) => (
        <div key={`${reaction.eventId}-${reaction.ticker}`} className="rounded-lg border border-line/70 bg-panel2/30 p-3">
          <div className="mb-1.5 flex flex-wrap items-center gap-2">
            <span className="num text-[12px] font-semibold text-fg">{reaction.ticker}</span>
            <span className="text-[11px] text-muted">
              landed {SESSION_LABEL[reaction.session] || reaction.session}
            </span>
            {reaction.referenceClose != null && (
              <span className="num text-[11px] text-muted">
                from {reaction.referenceClose} on {String(reaction.referenceTs || "").slice(0, 10)}
              </span>
            )}
            {reaction.synthetic && <Badge variant="warn">synthetic bars — excluded from history</Badge>}
          </div>

          <div className="overflow-x-auto">
            <table className="w-full min-w-[420px] text-left">
              <thead>
                <tr className="label-mono text-dim">
                  <th className="pb-1 pr-3 font-normal">window</th>
                  <th className="pb-1 pr-3 font-normal">move</th>
                  <th className="pb-1 pr-3 font-normal">benchmark</th>
                  <th className="pb-1 pr-3 font-normal">excess</th>
                  <th className="pb-1 font-normal">bars</th>
                </tr>
              </thead>
              <tbody>
                {(reaction.windows || []).map((window) => (
                  <WindowRow key={window.horizon} window={window} />
                ))}
              </tbody>
            </table>
          </div>

          <p className="mt-2 text-[10.5px] leading-relaxed text-dim">
            Association, not causation: these are the moves recorded around the event, from stored
            bars ({reaction.calcVersion}). They are not an estimate of what the event caused.
          </p>
        </div>
      ))}
    </div>
  );
}
