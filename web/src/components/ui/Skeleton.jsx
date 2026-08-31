import { cx } from "../../lib/cx.js";

// Skeleton — shimmer placeholder. `.skeleton` (globals.css) carries the animated gradient and is
// disabled under prefers-reduced-motion.
export function Skeleton({ className }) {
  return <div className={cx("skeleton rounded-lg", className)} />;
}

// SkeletonText — a stack of shimmer lines; the last is shortened for a natural paragraph look.
export function SkeletonText({ lines = 3, className }) {
  return (
    <div className={cx("flex flex-col gap-2", className)}>
      {Array.from({ length: lines }).map((_, i) => (
        <Skeleton key={i} className={cx("h-3", i === lines - 1 ? "w-2/3" : "w-full")} />
      ))}
    </div>
  );
}
