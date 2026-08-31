import { cx } from "../../lib/cx.js";
import { Icon } from "../shell/icons.jsx";

// Shared tone → color/glyph mapping for derived bull/bear/neutral/caution states. Panels derive the
// tone from the deterministic string facts (e.g. trend "uptrend" → bull).
//
// Colour discipline: bull/bear are the DATA colours (price direction, thesis strengthened /
// challenged). They are never the brand accent — indigo means interaction, not "up".
// The numeric-delta glyphs are kept as characters — the landing uses the up-triangle itself for a
// positive delta. The two non-numeric marks (caution, info) become stroked icons instead.
export const TONES = {
  bull: { pill: "border-up/60 text-up", dot: "bg-up", text: "text-up", glyph: "▲" },
  bear: { pill: "border-down/60 text-down", dot: "bg-down", text: "text-down", glyph: "▼" },
  neutral: { pill: "border-dim/60 text-dim", dot: "bg-dim", text: "text-dim", glyph: "●" },
  caution: { pill: "border-warn/70 text-warn", dot: "bg-warn", text: "text-warn", icon: "warning" },
  info: { pill: "border-info/60 text-info", dot: "bg-info", text: "text-info", icon: "diamond" },
};

// StatusPill — the landing's `.status` pill: full-round, 1px border in the current colour,
// mono 9px / .12em uppercase.
export function StatusPill({ tone = "neutral", children, glyph = true, className, title }) {
  const t = TONES[tone] || TONES.neutral;
  return (
    <span
      title={title}
      className={cx(
        "inline-flex items-center gap-1.5 rounded-full border px-2.5 py-[4px] font-mono text-[9px] font-medium uppercase leading-none tracking-[.12em]",
        t.pill,
        className
      )}
    >
      {glyph &&
        (t.icon ? (
          <Icon name={t.icon} size={10} />
        ) : (
          <span className="text-[8px] leading-none">{t.glyph}</span>
        ))}
      {children}
    </span>
  );
}

// StatusDot — just the colored dot (for compact rows / matrix cells).
export function StatusDot({ tone = "neutral", className }) {
  const t = TONES[tone] || TONES.neutral;
  return (
    <span
      className={cx("inline-block h-2 w-2 rounded-full", t.dot, className)}
      aria-hidden="true"
    />
  );
}
