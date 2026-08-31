import { cx } from "../../lib/cx.js";
import { Tooltip } from "./Tooltip.jsx";

// Gauge — a compact 0–100 zoned bar with a marker at the current value and the number in mono.
// Reused for RSI, Stochastic %K, ADX. `zones` tint ranges of the track; `ticks` mark thresholds.
//
// Zone/tick positions are given in value units (min..max) and converted to %.
export function Gauge({
  value,
  min = 0,
  max = 100,
  zones = [],
  ticks = [],
  label,
  tooltip,
  sub,
  valueDigits = 0,
  className,
}) {
  const has = value != null && Number.isFinite(Number(value));
  const v = has ? Math.min(max, Math.max(min, Number(value))) : min;
  const pctOf = (x) => ((Math.min(max, Math.max(min, x)) - min) / (max - min)) * 100;
  const markerPct = pctOf(v);

  return (
    <div className={cx("flex flex-col gap-1", className)}>
      <div className="flex items-baseline justify-between gap-2">
        <span className="label-mono text-muted">
          {tooltip ? (
            <Tooltip label={tooltip} underline>
              {label}
            </Tooltip>
          ) : (
            label
          )}
        </span>
        <span className="num text-[13px] font-semibold text-fg">
          {has ? Number(value).toFixed(valueDigits) : "—"}
        </span>
      </div>

      <div className="relative h-2 w-full rounded-full bg-panel2">
        {zones.map((z, i) => (
          <span
            key={i}
            className={cx("absolute inset-y-0 rounded-full", z.className)}
            style={{ left: `${pctOf(z.from)}%`, width: `${pctOf(z.to) - pctOf(z.from)}%` }}
          />
        ))}
        {ticks.map((t, i) => (
          <span
            key={`t${i}`}
            className="absolute inset-y-0 w-px bg-fg/25"
            style={{ left: `${pctOf(t)}%` }}
            aria-hidden="true"
          />
        ))}
        {has && (
          <span
            className="absolute top-1/2 h-3.5 w-[3px] -translate-x-1/2 -translate-y-1/2 rounded-full bg-fg shadow-sm shadow-black/50"
            style={{ left: `${markerPct}%` }}
            aria-hidden="true"
          />
        )}
      </div>

      {sub && <span className="text-[10px] leading-tight text-dim">{sub}</span>}
    </div>
  );
}

// Preset zones (value-unit ranges). Demand/oversold reads green-ish, overbought reads red-ish.
export const RSI_ZONES = [
  { from: 0, to: 30, className: "bg-up/30" },
  { from: 30, to: 70, className: "bg-panel2" },
  { from: 70, to: 100, className: "bg-down/30" },
];
export const RSI_TICKS = [30, 70];

export const STOCH_ZONES = [
  { from: 0, to: 20, className: "bg-up/30" },
  { from: 20, to: 80, className: "bg-panel2" },
  { from: 80, to: 100, className: "bg-down/30" },
];
export const STOCH_TICKS = [20, 80];

// ADX measures trend STRENGTH (not direction): weak → developing → strong.
export const ADX_ZONES = [
  { from: 0, to: 20, className: "bg-dim/30" },
  { from: 20, to: 40, className: "bg-info/25" },
  { from: 40, to: 100, className: "bg-accent/30" },
];
export const ADX_TICKS = [20, 40];
