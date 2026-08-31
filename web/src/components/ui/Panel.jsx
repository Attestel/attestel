import { cx } from "../../lib/cx.js";

// Panel — the base surface (docs/UX_DIRECTION.md § primitives).
//
// `variant` selects the landing page's surface vocabulary:
//   flat    — the dense default: hairline border on bg-panel. Nested panels stay here.
//   glass   — `.surface-card` (landing `.pcard`): liquid glass + top highlight. Top-level cards.
//   deep    — `.surface-deep` (landing `.method-card`): dark blurred slab. Modals, drawers.
//   canvas  — `.surface-canvas` (landing `.lens-slide`): inset well for charts and hero panels.
//
// Default stays `flat`, so nothing changes until a screen opts in.
//
// Optional header with title, inline badges, and right-aligned actions. Pass `flush` to drop body
// padding (tables/charts), `accent` to tint the border (used to make the SignalPanel stand out).
const VARIANTS = {
  flat: "rounded-xl border bg-panel",
  glass: "surface-card",
  deep: "surface-deep",
  canvas: "surface-canvas",
};

export function Panel({
  title,
  badges,
  actions,
  accent,
  danger,
  flush,
  variant = "flat",
  className,
  bodyClassName,
  children,
  ...rest
}) {
  const hasHeader = title || badges || actions;
  const isFlat = variant === "flat" || !VARIANTS[variant];
  return (
    <section
      className={cx(
        VARIANTS[variant] || VARIANTS.flat,
        // Border tinting only applies to the flat variant; the glass surfaces carry their own
        // border as part of the recipe.
        isFlat && (accent ? "border-accent/30" : danger ? "border-down/40" : "border-line"),
        !isFlat && accent && "!border-accent/40",
        !isFlat && danger && "!border-down/45",
        className
      )}
      {...rest}
    >
      {hasHeader && (
        <header
          className={cx(
            "relative flex items-center gap-2 border-b border-line px-4 py-3",
            !isFlat && "px-5 py-4"
          )}
        >
          {title && (
            <h3 className="flex min-w-0 items-center gap-2 text-[13px] font-[550] tracking-[-0.02em] text-fg">
              <span className="truncate">{title}</span>
            </h3>
          )}
          {badges && <div className="flex flex-wrap items-center gap-1.5">{badges}</div>}
          {actions && <div className="ml-auto flex items-center gap-2">{actions}</div>}
        </header>
      )}
      <div className={cx("relative", !flush && (isFlat ? "p-4" : "p-5"), bodyClassName)}>
        {children}
      </div>
    </section>
  );
}

// Section — a labelled sub-block inside a panel body (micro-label + content).
export function Section({ title, children, className }) {
  return (
    <div className={cx("mt-3 first:mt-0", className)}>
      {title && <h4 className="label-mono mb-1.5 block text-accent">{title}</h4>}
      {children}
    </div>
  );
}
