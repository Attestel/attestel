import { cx } from "../../../lib/cx.js";
import { CONFIDENCE_STEPS, nearestConfidenceStep } from "../../../lib/thesisApi.js";

// ConfidenceControl — YOUR conviction, on the fixed 5-step ordinal scale from PARALLEL_CONTRACTS.md
// §1.6, so a stored number means the same thing across users.
//
// This is one of THREE separate confidence fields the contract keeps deliberately distinct, never
// merged into one badge:
//   * thesis.confidence            — this control. user-owned. no model may write it.
//   * thesis.lastCheck.confidence  — the MODEL's confidence in its own check (ModelCheckCard).
//   * review.lensConfidence        — Step 07's lens reviews.
//
// A value that did not come from this control (an API client sending 63) renders at the nearest step
// with its exact number shown — never silently clamped to a step, never rewritten on load.
//
// A radiogroup, not a slider: five named choices where the label carries the meaning, so the value is
// legible without seeing colour or pixel position.
export default function ConfidenceControl({ value, onChange, disabled = false, idPrefix }) {
  const groupId = `${idPrefix}-confidence`;
  const nearest = nearestConfidenceStep(value);
  const offStep = value != null && nearest && nearest.value !== value;

  return (
    <fieldset className="min-w-0" aria-describedby={`${groupId}-help`}>
      <legend id={`${groupId}-legend`} className="text-[11px] font-semibold uppercase tracking-wide text-muted">
        Your confidence
      </legend>
      <p id={`${groupId}-help`} className="mt-1 text-[11.5px] leading-relaxed text-muted/80">
        How strongly you hold this claim. Yours alone — no model writes here.
      </p>

      <div role="radiogroup" aria-labelledby={`${groupId}-legend`} className="mt-2 flex flex-wrap gap-1.5">
        {CONFIDENCE_STEPS.map((s) => {
          const active = nearest?.step === s.step;
          const id = `${groupId}-${s.step}`;
          return (
            <label
              key={s.step}
              htmlFor={id}
              className={cx(
                "flex cursor-pointer items-center gap-1.5 rounded-md border px-2.5 py-1.5 text-[11.5px] font-semibold transition-colors",
                active
                  ? "border-accent/70 bg-accent/15 text-accent"
                  : "border-line bg-panel2 text-muted hover:border-line2 hover:text-fg",
                disabled && "cursor-default opacity-50"
              )}
            >
              <input
                id={id}
                type="radio"
                name={groupId}
                disabled={disabled}
                checked={active}
                onChange={() => onChange(s.value)}
                className="sr-only"
              />
              {/* The step number is a non-colour carrier of "how far along the scale" this is. */}
              <span aria-hidden="true" className="font-mono text-[10px] opacity-70">
                {s.step}
              </span>
              {s.label}
            </label>
          );
        })}

        {value != null && (
          <button
            type="button"
            disabled={disabled}
            onClick={() => onChange(null)}
            className="rounded-full px-2 py-1.5 text-[11.5px] text-muted underline decoration-dotted transition-colors hover:text-fg disabled:opacity-40"
          >
            Clear
          </button>
        )}
      </div>

      <p className="mt-1.5 text-[11.5px] text-muted">
        {value == null ? (
          <span className="text-muted/70">Not set.</span>
        ) : (
          <>
            <span className="font-mono text-fg">{value}</span>
            <span className="text-muted/70">/100</span>
            {offStep && (
              <span className="text-muted/70">
                {" "}
                — set outside this app, shown at the nearest step ({nearest.label}).
              </span>
            )}
          </>
        )}
      </p>
    </fieldset>
  );
}
