import { cx } from "../../lib/cx.js";

// Button — the single button primitive, built on the landing page's `.pill` family
// (landing/index-24-methodology-svg-icons.html `.pill`, `.pill-light`, `.pill-glass`, `.nav-cta`).
//
// The PRIMARY button is WHITE, not accent-coloured: the landing's primary CTA is
// `background:#F4F4F2; color:#060708`. Indigo is reserved for state, focus and provenance —
// never for a call to action.
const VARIANTS = {
  // .pill-light — hover goes to pure white and gains the soft outward glow.
  primary: "bg-fg text-bg hover:bg-white hover:shadow-glow",
  // .pill-glass — translucent slab, border brightens on hover.
  secondary:
    "bg-[rgba(24,26,32,.65)] text-fg border border-line2 backdrop-blur-[12px] hover:border-white/34 hover:shadow-lift",
  ghost: "text-muted border border-transparent hover:text-fg hover:bg-white/10",
  danger:
    "bg-[rgba(24,26,32,.65)] text-muted border border-line2 backdrop-blur-[12px] hover:border-down/70 hover:text-down",
};

// Sizes. `lg` is the landing's 52px hero pill; `md` is its compact nav-CTA scale.
const SIZES = {
  xs: "min-h-[26px] px-2.5 py-1 text-[11px] gap-1",
  sm: "min-h-[32px] px-3.5 py-1.5 text-xs gap-1.5",
  md: "min-h-[38px] px-4 py-[9px] text-[12.5px] gap-2",
  lg: "min-h-[52px] px-[26px] text-[14.5px] gap-2.5",
};

// Hover lift: -1px on the compact sizes, the landing's -2px on the full pills.
const LIFT = {
  xs: "hover:-translate-y-px",
  sm: "hover:-translate-y-px",
  md: "hover:-translate-y-0.5",
  lg: "hover:-translate-y-0.5",
};

export function Button({
  variant = "secondary",
  size = "md",
  className,
  type = "button",
  children,
  ...rest
}) {
  return (
    <button
      type={type}
      className={cx(
        // `pill-lift` is the hook globals.css uses to cancel the hover lift under
        // prefers-reduced-motion.
        "pill-lift inline-flex items-center justify-center rounded-full font-semibold",
        "transition-[transform,background-color,border-color,box-shadow,color] duration-200 ease-premium",
        "disabled:pointer-events-none disabled:cursor-default disabled:opacity-50",
        VARIANTS[variant] || VARIANTS.secondary,
        SIZES[size] || SIZES.md,
        LIFT[size] || LIFT.md,
        className
      )}
      {...rest}
    >
      {children}
    </button>
  );
}
