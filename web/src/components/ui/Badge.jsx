import { cx } from "../../lib/cx.js";

// Badge — small status/provenance pill, cut to the landing's `.claim-pill` /`.status` shape:
// full-round, mono 9px, .12em tracking, uppercase.
const VARIANTS = {
  default: "bg-panel2 text-muted",
  dim: "bg-panel2 text-dim",
  accent: "border border-accent/28 bg-accent/10 text-prov-company",
  ok: "bg-up/15 text-up",
  up: "bg-up/15 text-up",
  down: "bg-down/15 text-down",
  // `warn` is a guardrail surface, not decoration: full-opacity orange plus a visible border.
  warn: "border border-warn/70 bg-warn/15 text-warn",
  info: "bg-info/15 text-info",
  llm: "bg-llm/15 text-llm",
  outline: "border border-line text-muted",
};

// Badge — `dot` prepends a filled dot (used for "live").
export function Badge({ variant = "default", dot, pulse, className, children, title }) {
  return (
    <span
      title={title}
      className={cx(
        "inline-flex items-center gap-1.5 rounded-full px-2 py-[3px] font-mono text-[9px] font-medium uppercase leading-none tracking-[.12em]",
        VARIANTS[variant] || VARIANTS.default,
        className
      )}
    >
      {dot && (
        <span
          className={cx(
            "h-1.5 w-1.5 rounded-full bg-current",
            pulse && "motion-safe:animate-[pulse-live_1.4s_ease-in-out_infinite]"
          )}
        />
      )}
      {children}
    </span>
  );
}

// ProvenanceBadge — consistent live / seed / synthetic / composite provenance labelling on data
// panels. This is an honesty guardrail, not decoration: "synthetic" stays the loudest of the four
// (full-opacity orange + border), and never becomes quieter than the rest.
export function ProvenanceBadge({ kind = "seed", label, className }) {
  const variant =
    kind === "live"
      ? "ok"
      : kind === "synthetic"
        ? "warn"
        : kind === "composite"
          ? "info"
          : "dim";
  return (
    <Badge variant={variant} dot={kind === "live"} className={className}>
      {label || kind}
    </Badge>
  );
}
