import { cx } from "../../lib/cx.js";
import { Label } from "./Label.jsx";

// PageHeader — the landing's section-header pattern (`.sec-head`): a `folio` micro-label above a
// weight-550, tightly-tracked heading, with an optional right-aligned action slot and optional
// supporting prose.
export function PageHeader({ folio, title, subtitle, actions, className }) {
  return (
    <header className={cx("flex flex-wrap items-end justify-between gap-x-6 gap-y-3", className)}>
      <div className="min-w-0">
        {folio && <Label className="mb-2.5">{folio}</Label>}
        {title && (
          <h1 className="m-0 text-[26px] font-[550] leading-[1.04] tracking-[-0.03em] text-fg">
            {title}
          </h1>
        )}
        {subtitle && (
          <p className="m-0 mt-2.5 max-w-[62ch] text-[14px] leading-[1.6] text-muted">{subtitle}</p>
        )}
      </div>
      {actions && <div className="flex flex-none items-center gap-2">{actions}</div>}
    </header>
  );
}
