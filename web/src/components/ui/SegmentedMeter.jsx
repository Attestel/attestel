import { cx } from "../../lib/cx.js";

// SegmentedMeter — a −max…+max segmented bar (used for the confluence alignmentScore). Center is 0;
// segments fill from the center toward the value: green to the right (bullish), red to the left.
export function SegmentedMeter({ value = 0, max = 3, className }) {
  const v = Math.max(-max, Math.min(max, Number(value) || 0));
  const seg = (i) => {
    // i runs -max..-1 (left) and 1..max (right)
    if (i < 0) return v <= i ? "bg-down" : "bg-panel2"; // filled when value reaches this negative slot
    return v >= i ? "bg-up" : "bg-panel2";
  };
  const lefts = Array.from({ length: max }, (_, k) => -(max - k)); // -max..-1
  const rights = Array.from({ length: max }, (_, k) => k + 1); // 1..max

  return (
    <div className={cx("flex items-center gap-1", className)} aria-hidden="true">
      {lefts.map((i) => (
        <span key={i} className={cx("h-4 flex-1 rounded-sm transition-colors", seg(i))} />
      ))}
      <span className="mx-0.5 h-5 w-px bg-fg/40" />
      {rights.map((i) => (
        <span key={i} className={cx("h-4 flex-1 rounded-sm transition-colors", seg(i))} />
      ))}
    </div>
  );
}
