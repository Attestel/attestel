import { useState } from "react";
import { cx } from "../../lib/cx.js";

// Tooltip — hover/focus explanation (educational one-liners). Keyboard-focusable so the hint is
// reachable without a mouse. `underline` adds a dotted "there's info here" affordance to the child.
export function Tooltip({ label, children, className, underline, side = "top" }) {
  const [open, setOpen] = useState(false);
  if (!label) return children;

  const pos =
    side === "bottom"
      ? "top-full mt-1.5"
      : side === "left"
        ? "right-full top-1/2 -translate-y-1/2 mr-1.5"
        : side === "right"
          ? "left-full top-1/2 -translate-y-1/2 ml-1.5"
          : "bottom-full mb-1.5";
  const centerX = side === "top" || side === "bottom" ? "left-1/2 -translate-x-1/2" : "";

  return (
    <span
      className={cx(
        "relative inline-flex items-center",
        underline && "cursor-help border-b border-dotted border-muted/50",
        className
      )}
      onMouseEnter={() => setOpen(true)}
      onMouseLeave={() => setOpen(false)}
      onFocus={() => setOpen(true)}
      onBlur={() => setOpen(false)}
      tabIndex={0}
    >
      {children}
      {open && (
        <span
          role="tooltip"
          className={cx(
            "pointer-events-none absolute z-50 w-max max-w-[220px] rounded-md border border-line bg-panel2 px-2.5 py-1.5 text-[11px] font-normal normal-case leading-snug tracking-normal text-fg shadow-lg shadow-black/40",
            pos,
            centerX
          )}
        >
          {label}
        </span>
      )}
    </span>
  );
}
