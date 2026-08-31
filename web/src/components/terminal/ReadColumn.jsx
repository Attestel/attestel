import { useState } from "react";
import { motion } from "framer-motion";
import { cx } from "../../lib/cx.js";
import { useToast } from "../ui/index.js";
import { ColHeader, Micro, Sec, Tag } from "./bits.jsx";
import { Icon } from "../shell/icons.jsx";

// COL 3 — The structured LLM read (model output only). Reads as weighing evidence: a lead summary,
// the "since last snapshot" diff, a bull|bear balance, computed key levels with distance-from-price,
// and an invalidation toggle. Key levels stay the SERVER-COMPUTED ones — never model-invented.

// Computed levels numbered per kind, sorted by proximity to price.
function levelRows(kl, lastClose) {
  const res = (kl.resistance || [])
    .map(Number)
    .filter(Number.isFinite)
    .sort((a, b) => a - b)
    .map((v, i) => ({ v, label: `R${i + 1}`, kind: "R" }));
  const sup = (kl.support || [])
    .map(Number)
    .filter(Number.isFinite)
    .sort((a, b) => b - a)
    .map((v, i) => ({ v, label: `S${i + 1}`, kind: "S" }));
  return [...res, ...sup]
    .map((x) => ({ ...x, dist: lastClose ? (x.v / lastClose - 1) * 100 : null }))
    .sort((a, b) => Math.abs(a.dist ?? 1e9) - Math.abs(b.dist ?? 1e9));
}

export default function ReadColumn({ read, diff, lastClose }) {
  const toast = useToast();
  const [breaksOpen, setBreaksOpen] = useState(false);

  if (!read || read.error) {
    return (
      <div className="flex flex-col">
        <ColHeader title="Analytical read" />
        <Sec divide={false}>
          <p className="text-[12px] text-muted">{read?.error || "unavailable"}</p>
        </Sec>
      </div>
    );
  }

  const s = read.structured || {};
  const stub = String(read.modelUsed || "").includes("stub");
  const warnings = read.warnings || [];
  const inv = s.invalidation || {};
  const levels = levelRows(s.keyLevels || {}, lastClose);

  const quality = stub
    ? "offline stub — deterministic, not a live model"
    : warnings.length
      ? `${warnings.length} schema warning(s)`
      : "validated output";

  const copyLevel = (label, v) => {
    try {
      navigator.clipboard?.writeText(String(v));
      toast({ tone: "info", title: `Copied ${label}`, message: String(v), ttl: 1800 });
    } catch {
      /* clipboard unavailable — no-op */
    }
  };

  return (
    <div className="flex flex-col">
      <ColHeader
        title="Analytical read"
        right={
          <span className="flex items-center gap-1.5">
            <Tag tone={stub ? "warn" : "llm"}>{read.modelUsed}</Tag>
            {read.retried && <Tag tone="outline">retried</Tag>}
          </span>
        }
      />

      {/* Summary */}
      <Sec>
        <Micro tone="llm" className="mb-2.5 flex items-center gap-1.5">
          <span className={cx("h-1.5 w-1.5 rounded-full", stub || warnings.length ? "bg-warn" : "bg-llm")} />
          Summary · <span className="normal-case tracking-normal text-muted/70">{quality}</span>
        </Micro>
        <p className="text-[13px] leading-[1.65] text-fg/85">{s.summary || "—"}</p>
      </Sec>

      {/* Changes since last snapshot */}
      {diff?.changes?.length > 0 && (
        <Sec>
          <Micro tone="llm" className="mb-3 flex items-center gap-2">
            Changes since {diff.from} → {diff.to}
            {diff.trendChanged && <Tag tone="warn">trend changed</Tag>}
          </Micro>
          <div className="flex flex-col gap-2.5 text-[12.5px] leading-[1.5] text-muted">
            {diff.changes.map((c, i) => (
              <div key={i} className="flex gap-2.5">
                <span className="text-muted/50">•</span>
                <span>{c}</span>
              </div>
            ))}
          </div>
        </Sec>
      )}

      {warnings.length > 0 && (
        <Sec>
          <div className="flex items-start gap-1.5 rounded-lg border border-warn/30 bg-warn/[0.08] px-3 py-2 text-[12px] text-warn"><Icon name="warning" size={13} className="mt-px flex-none" /> {warnings.join("; ")}</div>
        </Sec>
      )}

      {/* Bull vs Bear */}
      <Sec>
        <Micro tone="llm" className="mb-3">Bull vs Bear</Micro>
        <div className="grid grid-cols-2 gap-3">
          <motion.div initial={{ opacity: 0, x: -8 }} animate={{ opacity: 1, x: 0 }} transition={{ duration: 0.22 }}>
            <div className="mb-2.5 text-[12px] font-semibold text-up">▲ BULL</div>
            <div className="flex flex-col gap-2.5 text-[12px] leading-[1.5] text-muted">
              {(s.bullCase || []).map((x, i) => (
                <div key={i} className="flex gap-2">
                  <span className="text-accent">•</span>
                  <span>{x}</span>
                </div>
              ))}
            </div>
          </motion.div>
          <motion.div initial={{ opacity: 0, x: 8 }} animate={{ opacity: 1, x: 0 }} transition={{ duration: 0.22 }}>
            <div className="mb-2.5 text-[12px] font-semibold text-down">▼ BEAR</div>
            <div className="flex flex-col gap-2.5 text-[12px] leading-[1.5] text-muted">
              {(s.bearCase || []).map((x, i) => (
                <div key={i} className="flex gap-2">
                  <span className="text-down">•</span>
                  <span>{x}</span>
                </div>
              ))}
            </div>
          </motion.div>
        </div>
      </Sec>

      {/* Key levels */}
      <Sec divide={false}>
        <div className="mb-1.5 flex items-center justify-between">
          <Micro tone="llm">Key levels</Micro>
          <Tag tone="outline">Computed</Tag>
        </div>
        <div className="mb-3 text-[10.5px] text-muted/60">
          distance from last close{lastClose ? ` (${Number(lastClose).toFixed(2)})` : ""} · click to copy · nearest first
        </div>
        <div className="flex flex-col gap-1.5">
          {levels.length === 0 && <span className="text-[12px] text-muted">none</span>}
          {levels.map((l) => (
            <button
              key={`${l.kind}${l.v}`}
              type="button"
              onClick={() => copyLevel(l.label, l.v)}
              title={`Copy ${l.v}`}
              className="flex items-center gap-3 rounded-sm border border-line bg-panel2 px-3 py-2 text-left transition-colors hover:border-accent/50"
            >
              <span
                className={cx(
                  "num rounded px-1.5 py-0.5 text-[10px] font-bold",
                  l.kind === "S" ? "bg-accent/14 text-accent" : "bg-down/14 text-down"
                )}
              >
                {l.label}
              </span>
              <span className="num text-[14px] font-bold text-fg">{l.v.toFixed(2)}</span>
              <span className="flex-1" />
              {l.dist != null && (
                <span className={cx("num text-[12.5px]", l.dist >= 0 ? "text-accent" : "text-down")}>
                  {l.dist >= 0 ? "+" : ""}
                  {l.dist.toFixed(1)}%
                </span>
              )}
            </button>
          ))}
        </div>

        {/* What breaks this — invalidation toggle */}
        <button
          type="button"
          onClick={() => setBreaksOpen((o) => !o)}
          aria-expanded={breaksOpen}
          className="mt-3.5 flex w-full items-center gap-2 border-t border-line pt-3.5"
        >
          <span className={cx("text-[11px] text-llm transition-transform", breaksOpen && "rotate-90")}>›</span>
          <Micro tone="llm">What breaks this</Micro>
        </button>
        {breaksOpen && (
          <div className="mt-2.5 flex flex-col gap-2">
            <div className="rounded-md border border-accent/20 bg-accent/[0.05] px-2.5 py-1.5 text-[12px]">
              <span className="font-semibold text-up">Bull invalid if: </span>
              <span className="text-fg/85">{inv.bull || "—"}</span>
            </div>
            <div className="rounded-md border border-down/20 bg-down/[0.05] px-2.5 py-1.5 text-[12px]">
              <span className="font-semibold text-down">Bear invalid if: </span>
              <span className="text-fg/85">{inv.bear || "—"}</span>
            </div>
          </div>
        )}

        <p className="mt-3.5 text-[10.5px] leading-[1.5] text-muted/60">{read.disclaimer}</p>
      </Sec>
    </div>
  );
}
