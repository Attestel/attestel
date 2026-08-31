import { Tag } from "../../terminal/bits.jsx";
import { cx } from "../../../lib/cx.js";
import { fmtWhenExact } from "../../../lib/thesisApi.js";

// ModelCheckCard — the SYSTEM's latest review of the user's claim (`thesis.lastCheck`).
//
// The whole point of this component is that it is visibly NOT the user's writing. It sits in its own
// olive-bordered frame (--color-llm is the app's model-provenance colour), is labelled "model review",
// is entirely read-only, and offers no "apply to my thesis" affordance — there is deliberately no path
// by which model text reaches a user-owned field (guide §8.4, prompt: "no AI-generated content is
// silently inserted into user-owned fields").
//
// The verdict is about the THESIS — whether collected evidence confirms, is neutral toward, or
// challenges the user's reasoning. Never about the stock, and never a buy/sell call (invariant #2).
//
// A legacy thesis's stored check renders here unchanged and correctly attributed, which is how a v1
// record keeps its history visible after the structured migration.

const VERDICT = {
  confirming: { label: "Confirming", glyph: "▲", tone: "text-up", border: "border-up/40", bg: "bg-up/10" },
  neutral: { label: "Neutral", glyph: "•", tone: "text-muted", border: "border-line", bg: "bg-panel2" },
  challenged: { label: "Challenged", glyph: "▼", tone: "text-down", border: "border-down/40", bg: "bg-down/10" },
};

const BEARING = {
  supports: { glyph: "▲", tone: "text-up", label: "supports" },
  contradicts: { glyph: "▼", tone: "text-down", label: "contradicts" },
};

export default function ModelCheckCard({ check, onRun, busy = false, canRun = true }) {
  return (
    <section
      aria-label="Model review of your claim"
      className="overflow-hidden rounded-lg border border-llm/40 bg-llm/[0.04]"
    >
      <div className="flex flex-wrap items-center gap-2 border-b border-llm/25 px-3 py-2">
        <Tag tone="llm">model review</Tag>
        <span className="text-[11.5px] text-muted">
          Written by the system, not by you. Read-only.
        </span>
        {onRun && (
          <button
            type="button"
            onClick={onRun}
            disabled={busy || !canRun}
            className="ml-auto rounded-full border border-llm/40 px-2 py-1 text-[11px] font-semibold text-llm transition-colors hover:bg-llm/10 disabled:opacity-40"
          >
            {busy ? "Checking…" : check ? "Re-check" : "Run check"}
          </button>
        )}
      </div>

      {!check ? (
        <p className="px-3 py-3 text-[12px] leading-relaxed text-muted">
          No model review stored yet. A review compares your claim against the facts collected for this
          company and says whether they confirm, are neutral toward, or challenge it — a verdict on your
          reasoning, not on the stock, and not a decision.
        </p>
      ) : (
        <div className="flex flex-col gap-2 px-3 py-3">
          <div className="flex flex-wrap items-center gap-2">
            {(() => {
              const v = VERDICT[check.verdict] || VERDICT.neutral;
              return (
                <span
                  className={cx(
                    "inline-flex items-center gap-1.5 rounded-md border px-2 py-0.5 text-[10.5px] font-bold uppercase tracking-wide",
                    v.border,
                    v.bg,
                    v.tone
                  )}
                >
                  {/* Glyph + word: the verdict never depends on colour alone. */}
                  <span aria-hidden="true">{v.glyph}</span>
                  {v.label}
                </span>
              );
            })()}
            <span className="text-[11px] text-muted/70">{fmtWhenExact(check.at)}</span>
            {/* The MODEL's confidence in its own check — a different field from the user's
                conviction, and labelled so the two can never be read as one badge (§1.6). */}
            {check.confidence != null && (
              <span className="text-[11px] text-muted">
                Model check confidence <span className="font-mono text-fg">{check.confidence}</span>
                <span className="text-muted/70">/100</span>
              </span>
            )}
            {check.modelUsed && (
              <span className="font-mono text-[10.5px] text-muted/60">{check.modelUsed}</span>
            )}
          </div>

          {check.summary && (
            <p className="break-words text-[12.5px] leading-relaxed text-fg/85">{check.summary}</p>
          )}

          {(check.evidence || []).length > 0 && (
            <ul className="flex flex-col gap-1">
              {check.evidence.map((ev, i) => {
                const b = BEARING[ev.bearing];
                return (
                  <li key={i} className="break-words text-[12px] leading-relaxed text-fg/75">
                    <span className={cx("font-semibold", b?.tone || "text-muted")}>
                      <span aria-hidden="true">{b?.glyph || "•"}</span>
                      <span className="sr-only">{b?.label || "neutral"}</span>
                    </span>{" "}
                    {ev.fact}
                    {ev.note && <span className="text-muted/70"> — {ev.note}</span>}
                  </li>
                );
              })}
            </ul>
          )}

          {(check.watchFor || []).length > 0 && (
            <div>
              <p className="text-[10.5px] font-semibold uppercase tracking-wide text-muted">Watch for</p>
              <ul className="mt-1 flex flex-col gap-0.5">
                {check.watchFor.map((w, i) => (
                  <li key={i} className="break-words text-[12px] leading-relaxed text-muted">
                    · {w}
                  </li>
                ))}
              </ul>
            </div>
          )}
        </div>
      )}
    </section>
  );
}
