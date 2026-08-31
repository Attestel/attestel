import { useState } from "react";
import { Button, Field } from "../ui/index.js";
import { Tag } from "../terminal/bits.jsx";
import { cx } from "../../lib/cx.js";
import {
  ASSUMPTION_VERDICTS,
  REASONING_QUALITY,
  decisionErrorMessage,
  fmtDecisionTime,
  reviewOutcome,
  reviseOutcome,
} from "../../lib/decisionApi.js";

// OutcomeReviewForm — the Review half of the loop.
//
// The form asks two questions in a deliberate order and keeps them apart:
//
//   1. WHAT HAPPENED — observed outcome, which assumptions held, whether the conditions you named
//      actually occurred. This is observation.
//   2. WAS THE REASONING SOUND — a 1..5 process score plus lessons. This is judgement, and it is
//      YOURS: no model writes it, and the market result is not an input to it.
//
// There is no P&L field. If the decision links a trade, the server derives the number from the trade
// ledger and shows it beside the review; if it does not, there is no number, and a deliberate
// non-action is reviewed on its reasoning alone. The form never displays the P&L while the score is
// being chosen, because a number on screen is an anchor.

const textareaClass =
  "w-full rounded-md border border-line bg-panel2 px-2.5 py-2 text-[13px] leading-relaxed text-fg placeholder:text-muted/50 focus:outline-none focus-visible:border-accent";

const linesToList = (s) =>
  String(s || "")
    .split("\n")
    .map((l) => l.trim())
    .filter(Boolean);

// assumptionRows seeds one row per item that was on the thesis AT DECISION TIME. The server refuses
// a verdict on anything else, and rightly: grading an assumption the user added later would judge
// the decision by information it never had.
function seedRows(decision, existing) {
  const snap = decision.snapshot || {};
  const items = [
    ...(snap.assumptions || []).map((a) => ({ ...a, group: "assumption" })),
    ...(snap.invalidationConditions || []).map((a) => ({ ...a, group: "invalidation" })),
  ];
  const prior = Object.fromEntries((existing?.assumptions || []).map((a) => [a.thesisItemId, a]));
  return items.map((it) => ({
    id: it.id,
    text: it.text,
    group: it.group,
    verdict: prior[it.id]?.verdict || "unknown",
    note: prior[it.id]?.note || "",
  }));
}

export default function OutcomeReviewForm({ decision, existing = null, onSaved, onCancel }) {
  const [observed, setObserved] = useState(existing?.observedOutcome || "");
  const [horizonElapsed, setHorizonElapsed] = useState(existing?.horizonElapsed ?? false);
  const [rows, setRows] = useState(() => seedRows(decision, existing));
  const [conditions, setConditions] = useState(() =>
    (decision.conditionsToMonitor || []).map((label, i) => ({
      label,
      occurred: existing?.catalystsOccurred?.[i]?.occurred ?? false,
    }))
  );
  const [quality, setQuality] = useState(existing?.reasoningQuality ?? null);
  const [lessons, setLessons] = useState((existing?.processLessons || []).join("\n"));
  const [retro, setRetro] = useState(existing?.retrospective || "");
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");

  const revising = Boolean(existing);

  async function submit(e) {
    e.preventDefault();
    setError("");
    if (!observed.trim()) {
      setError("Record what actually happened before assessing the reasoning.");
      return;
    }
    setSaving(true);
    try {
      const saved = revising
        ? // A revision touches only the four fields a later reading can legitimately change.
          await reviseOutcome(existing.id, {
            observedOutcome: observed.trim(),
            reasoningQuality: quality,
            processLessons: linesToList(lessons),
            retrospective: retro.trim(),
          })
        : await reviewOutcome({
            decisionId: decision.id,
            observedOutcome: observed.trim(),
            horizonElapsed,
            catalystsOccurred: conditions.map((c) => ({ thesisItemId: "", label: c.label, occurred: c.occurred })),
            assumptions: rows.map((r) => ({ thesisItemId: r.id, verdict: r.verdict, note: r.note.trim() })),
            reasoningQuality: quality,
            processLessons: linesToList(lessons),
            retrospective: retro.trim(),
          });
      onSaved?.(saved);
    } catch (err) {
      setError(decisionErrorMessage(err));
    } finally {
      setSaving(false);
    }
  }

  return (
    <form onSubmit={submit} className="flex flex-col gap-4 px-[22px] py-4">
      <div className="rounded-lg border border-line bg-panel2/30 px-3.5 py-3">
        <div className="mb-1 flex flex-wrap items-center gap-2">
          <span className="label-mono text-muted">
            reviewing your decision of {fmtDecisionTime(decision.decidedAt)}
          </span>
          <Tag tone="outline">{decision.ticker}</Tag>
        </div>
        <p className="whitespace-pre-wrap text-[12px] leading-relaxed text-fg/80">{decision.rationale}</p>
      </div>

      {/* 1. what happened */}
      <Field label="What actually happened?">
        <textarea
          className={textareaClass}
          rows={3}
          value={observed}
          onChange={(e) => setObserved(e.target.value)}
          placeholder="The observable outcome — events, numbers, what the company or the market did."
        />
      </Field>

      {!revising && conditions.length > 0 && (
        <div className="flex flex-col gap-1.5">
          <span className="text-[11px] font-medium uppercase tracking-wide text-muted">
            Did the conditions you named occur?
          </span>
          <ul className="flex flex-col gap-1">
            {conditions.map((c, i) => (
              <li key={i}>
                <label className="flex cursor-pointer items-start gap-2 text-[12px] text-muted">
                  <input
                    type="checkbox"
                    checked={c.occurred}
                    onChange={() =>
                      setConditions((prev) =>
                        prev.map((x, j) => (j === i ? { ...x, occurred: !x.occurred } : x))
                      )
                    }
                    className="mt-[3px] accent-accent"
                  />
                  <span>{c.label}</span>
                </label>
              </li>
            ))}
          </ul>
        </div>
      )}

      {!revising && rows.length > 0 && (
        <div className="flex flex-col gap-1.5">
          <span className="text-[11px] font-medium uppercase tracking-wide text-muted">
            Which assumptions held?
          </span>
          <ul className="flex flex-col gap-2">
            {rows.map((r, i) => (
              <li key={r.id} className="rounded-md border border-line bg-panel2/40 px-2.5 py-2">
                <div className="mb-1.5 flex flex-wrap items-center gap-2">
                  <span className="text-[12px] text-fg/85">{r.text}</span>
                  {r.group === "invalidation" && <Tag tone="outline">would invalidate</Tag>}
                </div>
                <div className="flex flex-wrap items-center gap-1.5">
                  {ASSUMPTION_VERDICTS.map((v) => (
                    <button
                      key={v.id}
                      type="button"
                      aria-pressed={r.verdict === v.id}
                      onClick={() =>
                        setRows((prev) => prev.map((x, j) => (j === i ? { ...x, verdict: v.id } : x)))
                      }
                      className={cx(
                        "rounded-full border px-2 py-1 text-[11.5px] transition-colors",
                        r.verdict === v.id
                          ? "border-accent/60 bg-accent/10 text-fg"
                          : "border-line bg-panel text-muted hover:text-fg/85"
                      )}
                    >
                      {v.label}
                    </button>
                  ))}
                  <input
                    className="min-w-[160px] flex-1 rounded-md border border-line bg-panel px-2 py-1 text-[11.5px] text-fg placeholder:text-muted/50 focus:outline-none focus-visible:border-accent"
                    value={r.note}
                    onChange={(e) =>
                      setRows((prev) => prev.map((x, j) => (j === i ? { ...x, note: e.target.value } : x)))
                    }
                    placeholder="note (optional)"
                  />
                </div>
              </li>
            ))}
          </ul>
        </div>
      )}

      {!revising && (
        <label className="flex cursor-pointer items-center gap-2 text-[12px] text-muted">
          <input
            type="checkbox"
            checked={horizonElapsed}
            onChange={() => setHorizonElapsed((v) => !v)}
            className="accent-accent"
          />
          The horizon I set has fully elapsed
        </label>
      )}

      {/* 2. was the reasoning sound — asked separately, and never next to a P&L number */}
      <div className="rounded-lg border border-line2 bg-panel2/50 px-3.5 py-3">
        <div className="mb-1 text-[12.5px] font-semibold text-fg">Was the reasoning sound?</div>
        <p className="mb-2.5 text-[11.5px] leading-relaxed text-muted">
          This is about your process, not the result. A good decision can lose money and a bad one can
          make it — scoring the process is the only part that transfers to the next decision.
        </p>
        <div className="flex flex-col gap-1">
          {REASONING_QUALITY.map((r) => (
            <button
              key={r.value}
              type="button"
              aria-pressed={quality === r.value}
              onClick={() => setQuality(quality === r.value ? null : r.value)}
              className={cx(
                "flex items-baseline gap-2 rounded-sm border px-2.5 py-1.5 text-left transition-colors",
                quality === r.value
                  ? "border-accent/60 bg-accent/10"
                  : "border-line bg-panel hover:border-line2"
              )}
            >
              <span className="num w-[26px] shrink-0 text-[12px] font-semibold text-fg">{r.value}</span>
              <span className="text-[12px] font-semibold text-fg/85">{r.label}</span>
              <span className="text-[11.5px] text-muted">{r.blurb}</span>
            </button>
          ))}
        </div>
      </div>

      <Field label="What would you do differently? (one lesson per line)">
        <textarea
          className={textareaClass}
          rows={2}
          value={lessons}
          onChange={(e) => setLessons(e.target.value)}
          placeholder={"Write the invalidation condition BEFORE entering"}
        />
      </Field>

      <Field label="Retrospective (optional)">
        <textarea
          className={textareaClass}
          rows={3}
          value={retro}
          onChange={(e) => setRetro(e.target.value)}
          placeholder="The longer version, in your own words."
        />
      </Field>

      {error && (
        <p className="rounded-md border border-down/40 bg-down/[0.08] px-3 py-2 text-[12px] text-down">{error}</p>
      )}

      <div className="flex flex-wrap items-center gap-2">
        <Button type="submit" variant="primary" size="sm" disabled={saving}>
          {saving ? "Saving…" : revising ? "Save revision" : "Save review"}
        </Button>
        {onCancel && (
          <Button type="button" variant="ghost" size="sm" onClick={onCancel}>
            Cancel
          </Button>
        )}
        <span className="text-[11px] text-muted/70">
          A review never changes the decision it reviews — that record stays exactly as you made it.
        </span>
      </div>
    </form>
  );
}
