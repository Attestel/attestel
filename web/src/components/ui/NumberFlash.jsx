import { useFlash } from "../../hooks/useFlash.js";
import { cx } from "../../lib/cx.js";

// NumberFlash — renders a mono numeral that briefly flashes green/red when `value` changes.
// Remounts on change (via key) so the CSS flash restarts reliably; reduced-motion disables the
// flash in globals.css. `format` maps the raw value to display text.
export function NumberFlash({ value, format = (v) => v, className, as: Tag = "span" }) {
  const n = typeof value === "number" ? value : Number(value);
  const flash = useFlash(Number.isFinite(n) ? n : null);
  return (
    <Tag key={flash.key} className={cx("num inline-block px-0.5", flash.cls, className)}>
      {format(value)}
    </Tag>
  );
}
