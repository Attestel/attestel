import { cx } from "../../lib/cx.js";

// Stat — label + tabular-mono value (+ optional delta / sub line). The value should be a
// preformatted string or node; `tone` colors it. Used across header strips and stat grids.
export function Stat({ label, value, delta, sub, tone, size = "md", className, valueClassName }) {
  const valueSize =
    size === "lg" ? "text-2xl" : size === "sm" ? "text-[13px]" : "text-[16px]";
  return (
    <div className={cx("flex min-w-0 flex-col gap-0.5", className)}>
      {label && (
        <span className="label-mono truncate text-muted">
          {label}
        </span>
      )}
      <span className={cx("num font-semibold leading-none", valueSize, tone, valueClassName)}>
        {value}
      </span>
      {delta != null && <span className="num text-[11px] leading-none">{delta}</span>}
      {sub && <span className="text-[11px] leading-tight text-muted">{sub}</span>}
    </div>
  );
}
