import { cx } from "../../lib/cx.js";

// AskContextChip — "You are viewing: …", with a detach control (doc §16.12).
//
// DETACHING IS A REAL USER RIGHT, and that is why the × is here rather than being a nicety.
// A context the user cannot remove is a context they cannot debug: if the answer looks wrong, the
// first thing to try is asking the same question without the attachment, and a chip with no exit
// makes that impossible. It also keeps the attachment honest — a thing you can turn off is a thing
// you can see the effect of.
//
// The chip shows the LABEL only. The facts themselves are not hidden behind it: they are composed
// into a visible transcript turn on the first send (locked decision 6), so the full input is on
// screen in the conversation rather than summarised in a tooltip.
export default function AskContextChip({ label, kind, onDetach, className }) {
  if (!label) return null;
  return (
    <div
      className={cx(
        "flex items-center gap-2 rounded-full border border-accent/28 bg-accent/[0.08] py-1 pl-3 pr-1",
        className
      )}
    >
      <span className="label-mono shrink-0 text-accent">
        {kind === "event" ? "Viewing event" : kind === "following" ? "Viewing feed" : "Viewing"}
      </span>
      <span className="min-w-0 flex-1 truncate text-[12px] text-fg/85" title={label}>
        {label}
      </span>
      <button
        type="button"
        onClick={onDetach}
        // An accessible name, not a bare glyph: "×" read aloud is not a description of what
        // happens, and this control changes what the model is given.
        aria-label={`Detach context: ${label}`}
        title="Ask without this context"
        className="flex h-6 w-6 shrink-0 items-center justify-center rounded-full text-muted transition-colors hover:bg-white/10 hover:text-fg"
      >
        <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" aria-hidden="true">
          <path d="M6 6l12 12M18 6L6 18" />
        </svg>
      </button>
    </div>
  );
}
