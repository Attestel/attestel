import { cx } from "../../lib/cx.js";

// EmptyState — a designed "nothing here yet" block (not a bare line of text). Optional icon,
// title, supporting copy, and an action (e.g. "Train a model to see a signal").
//
// Dashed glass container + micro-label heading, so an empty panel still reads as Attestel chrome
// rather than as a gap.
export function EmptyState({ icon, title, children, action, className, tone = "muted" }) {
  return (
    <div
      className={cx(
        "relative flex flex-col items-center justify-center gap-2.5 rounded-2xl border border-dashed border-line2 bg-white/[.015] px-5 py-9 text-center backdrop-blur-[10px]",
        className
      )}
    >
      {icon && (
        <div className={cx("mb-0.5", tone === "warn" ? "text-warn" : "text-dim")}>{icon}</div>
      )}
      {title && (
        <div className="label-mono text-muted">{title}</div>
      )}
      {children && (
        <div className="max-w-[46ch] text-[13px] leading-[1.6] text-muted">{children}</div>
      )}
      {action && <div className="mt-1.5">{action}</div>}
    </div>
  );
}
