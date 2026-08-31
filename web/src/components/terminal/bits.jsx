import { cx } from "../../lib/cx.js";

// Dense "institutional attestel" primitives for the redesigned Terminal (design_examples/1a-terminal).
// The terminal is NOT a set of rounded Panel cards — it is a single bordered container whose columns
// and sections are separated by hairline dividers. These are the shared building blocks.

// Small provenance / status tag pill.
const TAG_TONE = {
  outline: "border border-line2 text-muted/90",
  llm: "bg-llm/15 text-llm",
  accent: "bg-accent/12 text-accent",
  warn: "bg-warn/14 text-warn",
  info: "bg-info/12 text-info",
  down: "bg-down/12 text-down",
};

export function Tag({ tone = "outline", className, children }) {
  return (
    <span
      className={cx(
        // Same species as ui/Badge — full-round, mono 9px / .12em, per the landing's `.status`.
        "inline-flex items-center rounded-full label-mono",
        tone === "outline" ? "px-2 py-[3px]" : "px-2 py-1",
        TAG_TONE[tone] || TAG_TONE.outline,
        className
      )}
    >
      {children}
    </span>
  );
}

// Column header — 44px tall: title + provenance tag + optional right slot.
export function ColHeader({ title, tag, tagTone = "outline", right }) {
  return (
    <div className="flex h-11 shrink-0 items-center gap-2.5 border-b border-line px-[18px]">
      <span className="text-[14px] font-[550] tracking-[-0.02em] text-fg">{title}</span>
      {tag && <Tag tone={tagTone}>{tag}</Tag>}
      {right && <span className="ml-auto flex items-center">{right}</span>}
    </div>
  );
}

// Micro uppercase mono caption (section labels). `tone` picks the accent color.
const MICRO_TONE = {
  muted: "text-dim",
  llm: "text-llm",
  warn: "text-warn",
  accent: "text-accent",
};
export function Micro({ tone = "muted", className, children }) {
  return (
    <div
      className={cx(
        // The signature Attestel micro-label (globals.css `.label-mono`).
        "label-mono",
        MICRO_TONE[tone] || MICRO_TONE.muted,
        className
      )}
    >
      {children}
    </div>
  );
}

// A padded section inside a column. Bottom hairline by default (drop it on the last section).
export function Sec({ divide = true, className, children }) {
  return (
    <div className={cx("px-[18px] py-4", divide && "border-b border-line", className)}>{children}</div>
  );
}
