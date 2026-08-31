import { cx } from "../../lib/cx.js";

// HealthDot — one service liveness dot. up===true → green, up===false → red, undefined → grey
// (unknown / not yet probed). `derived` renders a hollow ring to signal the status is inferred
// (analysis/llm are behind the gateway, not directly polled).
export function HealthDot({ up, label, derived }) {
  const color =
    up === true ? "bg-up" : up === false ? "bg-down" : "bg-muted/50";
  const state = up === true ? "up" : up === false ? "down" : "unknown";
  return (
    <span
      className="group relative flex items-center"
      title={`${label}: ${state}${derived ? " (derived)" : ""}`}
    >
      <span
        className={cx(
          "h-2 w-2 rounded-full",
          color,
          derived && "ring-1 ring-inset ring-fg/30 bg-transparent",
          derived && up === true && "bg-up/40",
          derived && up === false && "bg-down/40"
        )}
        aria-hidden="true"
      />
      <span className="sr-only">
        {label} {state}
      </span>
    </span>
  );
}

// HealthCluster — a labelled row of dots. items = [{ key, label, up, derived }].
export function HealthCluster({ items, className }) {
  const down = items.filter((i) => i.up === false).length;
  return (
    <div
      className={cx("flex items-center gap-1.5", className)}
      title={down ? `${down} service(s) down` : "all monitored services up"}
      role="group"
      aria-label="Service health"
    >
      {items.map((it) => (
        <HealthDot key={it.key} up={it.up} label={it.label} derived={it.derived} />
      ))}
    </div>
  );
}
