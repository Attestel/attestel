import { cx } from "../../lib/cx.js";

// Label — the signature Attestel micro-label: mono, 10.5px, weight 500, .16em tracking, uppercase.
// It is the landing page's `.eyebrow` / `.folio` / `.label` and the single most recognisable
// typographic tell of the brand. Use it wherever a small uppercase caption sits above a block.
const TONES = {
  dim: "text-dim",
  muted: "text-muted",
  fg: "text-fg",
  accent: "text-accent",
  up: "text-up",
  down: "text-down",
  warn: "text-warn",
  info: "text-info",
  llm: "text-llm",
};

export function Label({ tone = "dim", as: Tag = "span", className, children, ...rest }) {
  return (
    <Tag className={cx("label-mono block", TONES[tone] || TONES.dim, className)} {...rest}>
      {children}
    </Tag>
  );
}
