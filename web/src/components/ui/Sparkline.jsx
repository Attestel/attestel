import { cx } from "../../lib/cx.js";

// Sparkline — tiny inline SVG trend line. Colors green/red by last-vs-first unless `tone` is set.
// Scales to its viewBox; safe with short/empty series.
export function Sparkline({ data, width = 88, height = 24, className, strokeWidth = 1.5, fill = true, tone }) {
  const nums = (data || []).map(Number).filter((n) => Number.isFinite(n));
  if (nums.length < 2) {
    return <svg width={width} height={height} className={cx("text-muted/40", className)} aria-hidden="true" />;
  }
  const min = Math.min(...nums);
  const max = Math.max(...nums);
  const span = max - min || 1;
  const stepX = width / (nums.length - 1);
  const pts = nums.map((n, i) => [i * stepX, height - ((n - min) / span) * (height - 2) - 1]);
  const d = pts.map(([x, y], i) => `${i ? "L" : "M"}${x.toFixed(1)} ${y.toFixed(1)}`).join(" ");
  const up = nums[nums.length - 1] >= nums[0];
  const color = tone || (up ? "text-up" : "text-down");

  return (
    <svg
      width={width}
      height={height}
      viewBox={`0 0 ${width} ${height}`}
      preserveAspectRatio="none"
      className={cx(color, className)}
      aria-hidden="true"
    >
      {fill && <path d={`${d} L ${width} ${height} L 0 ${height} Z`} fill="currentColor" opacity="0.12" />}
      <path
        d={d}
        fill="none"
        stroke="currentColor"
        strokeWidth={strokeWidth}
        strokeLinecap="round"
        strokeLinejoin="round"
      />
    </svg>
  );
}
