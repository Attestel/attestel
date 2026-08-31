import { cx } from "../../lib/cx.js";

// SuggestedActions — up to three contextual questions (doc §16.12).
//
// LOCKED DECISION 7: A SUGGESTION IS PREFILLED USER TEXT, NOT A HIDDEN INSTRUCTION.
// Clicking one sends exactly the sentence printed on the button, as a visible user turn. It never
// injects a system message, never expands into a longer hidden prompt, and never carries a
// parameter the user cannot see. The button's label IS the payload — that is the whole contract,
// and it is what makes the transcript a complete account of the conversation.
//
// INVARIANT #2: no suggestion may ask for a verdict. The defaults in `lib/askApi.js` are questions
// about evidence and mechanism; a suggestion reading "should I buy this?" would be the product
// inviting the exact output the model is forbidden to produce.
export default function SuggestedActions({ suggestions, onSelect, disabled, className }) {
  const list = (suggestions || []).filter(Boolean).slice(0, 3);
  if (list.length === 0) return null;

  return (
    <div className={cx("flex flex-col gap-1.5", className)}>
      <span className="label-mono text-muted">Suggested</span>
      <div className="flex flex-wrap gap-1.5">
        {list.map((text) => (
          <button
            key={text}
            type="button"
            disabled={disabled}
            onClick={() => onSelect(text)}
            className={cx(
              "rounded-full border border-line2 bg-panel2 px-3 py-1.5 text-left text-[12px] text-fg/80",
              "transition-colors duration-200 ease-premium",
              "hover:border-accent/40 hover:text-fg",
              "disabled:pointer-events-none disabled:opacity-50"
            )}
          >
            {text}
          </button>
        ))}
      </div>
    </div>
  );
}
