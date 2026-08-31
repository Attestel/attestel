import { useEffect, useMemo, useRef, useState } from "react";
import { computeSize, suggestStops } from "../../lib/positionSize.js";
import { useSettings } from "../../settings/SettingsContext.jsx";
import { Tag } from "../terminal/bits.jsx";
import { cx } from "../../lib/cx.js";
import { Icon } from "../shell/icons.jsx";

// PositionSizer — the 1–2%-per-trade rule made concrete (docs/BEGINNER.md Piece 2). A compact card
// that reads the log-trade form's entry/stop/target/side, adds the user's account equity + risk %,
// and shows the resulting share count, position value, dollars at risk and reward:risk in plain
// language, with amber warnings when the plan breaks a rule of thumb.
//
// DESCRIPTIVE RISK MATH ONLY — it emits no buy/sell verdict, executes nothing, and moves no money
// (invariant #2). A "Suggest stop" affordance only fills a level the analysis service already computed
// (ATR / support), never an invented one (invariant #3). No LLM, no network — pure client math over
// data already on the page.
//
// It is CONTROLLED: entry/stop/target/side come from the form (a single source of truth), so there is
// no duplicate data entry. onSuggestStop writes a computed stop back to the form, onApplySize fills the
// form's share count, and onResult reports the sizing up so a later step (checklist / risk snapshot)
// can read it.

const usd = (n) => `$${Number(n).toLocaleString(undefined, { maximumFractionDigits: 2 })}`;
const usd0 = (n) => `$${Number(n).toLocaleString(undefined, { maximumFractionDigits: 0 })}`;

const RISK_PRESETS = [0.5, 1, 2];

const CTL = "h-[34px] w-full rounded-md border border-line2 bg-panel px-2.5 text-[13px] text-fg focus:border-accent/60 focus:outline-none";
const microLabel = "mb-1 label-mono text-muted";

function Metric({ label, value, tone = "text-fg" }) {
  return (
    <div>
      <div className={microLabel}>{label}</div>
      <div className={cx("num text-[16px] font-bold leading-tight", tone)}>{value}</div>
    </div>
  );
}

export default function PositionSizer({
  dashboard = null,
  side = "long",
  entry = "",
  stop = "",
  target = "",
  onSuggestStop,
  onApplySize,
  onResult,
}) {
  const { settings, saveSettings } = useSettings();

  // Equity + risk % are backed by settings so they follow the user across devices, but kept in local
  // state for a responsive control (guests, who can't persist, still get an in-memory value). Persist
  // is best-effort: a failed save (e.g. a guest) never breaks the sizer.
  const [equityInput, setEquityInput] = useState(() => (settings.accountEquity ? String(settings.accountEquity) : ""));
  const [riskPct, setRiskPct] = useState(settings.defaultRiskPct || 1);

  useEffect(() => {
    setEquityInput(settings.accountEquity ? String(settings.accountEquity) : "");
  }, [settings.accountEquity]);
  useEffect(() => {
    setRiskPct(settings.defaultRiskPct || 1);
  }, [settings.defaultRiskPct]);

  const equityNum = Number(equityInput) || 0;

  const result = useMemo(
    () => computeSize({ accountEquity: equityNum, riskPct, entry, stop, target }),
    [equityNum, riskPct, entry, stop, target]
  );

  // Report the sizing up (augmented with the equity + risk % used) whenever it changes, via a ref so
  // onResult need not be a stable reference.
  const onResultRef = useRef(onResult);
  onResultRef.current = onResult;
  useEffect(() => {
    onResultRef.current?.({ ...result, accountEquity: equityNum, riskPct });
  }, [result, equityNum, riskPct]);

  const suggestions = useMemo(() => suggestStops(dashboard, entry, side), [dashboard, entry, side]);

  function persistEquity() {
    const v = Math.max(0, Number(equityInput) || 0);
    if (v === settings.accountEquity) return;
    saveSettings({ accountEquity: v }).catch(() => {}); // best-effort (guest / offline): keep local value
  }
  function pickRisk(v) {
    setRiskPct(v);
    saveSettings({ defaultRiskPct: v }).catch(() => {});
  }

  const f = result.flags;
  const warnings = [];
  if (f.riskPctOverCap) warnings.push("Risk % is above the 2% cap — keep it ≤ 2% to survive a losing streak.");
  if (f.stopMissing) warnings.push("No stop set — enter a stop so risk-per-share (and your size) is defined.");
  if (f.positionExceedsEquity) warnings.push("Position value exceeds your account equity — this would require margin.");
  if (f.rrBelowFloor) warnings.push("Reward:risk is below 1.5 — the target is close relative to the risk taken.");

  // Plain-language read of the sizing — descriptive, never a verdict.
  let plain;
  if (equityNum <= 0) {
    plain = "Set your account equity to size this trade.";
  } else if (result.shares <= 0) {
    plain = f.stopMissing
      ? "Enter an entry and a stop to compute your share count."
      : "Risk per share is too large for this equity/risk % — no whole shares fit.";
  } else {
    plain =
      `Risking ${usd(result.dollarRisk)} (${riskPct}% of ${usd0(equityNum)}) → buy ${result.shares.toLocaleString()} shares ` +
      `(${usd0(result.positionValue)} position).` +
      (result.rr != null ? ` Reward:risk ${result.rr.toFixed(1)}.` : "");
  }

  const entryTxt = Number(entry) > 0 ? usd(Number(entry)) : "—";
  const stopTxt = Number(stop) > 0 ? usd(Number(stop)) : "—";
  const targetTxt = Number(target) > 0 ? usd(Number(target)) : "—";

  return (
    <div className="rounded-lg border border-line2 bg-panel2/40 p-4">
      <div className="mb-3 flex items-center gap-2.5">
        <span className="text-[13px] font-semibold text-fg">Position sizer</span>
        <Tag tone="outline">Risk math · Not advice</Tag>
      </div>

      {/* Equity + risk % */}
      <div className="mb-3 flex flex-wrap items-end gap-4">
        <div className="w-full xs:w-[150px]">
          <div className={microLabel}>Account equity</div>
          <div className="relative">
            <span className="pointer-events-none absolute left-2.5 top-1/2 -translate-y-1/2 text-[13px] text-muted/60">$</span>
            <input
              type="number"
              step="any"
              min="0"
              inputMode="decimal"
              value={equityInput}
              onChange={(e) => setEquityInput(e.target.value)}
              onBlur={persistEquity}
              placeholder="25000"
              className={cx(CTL, "num pl-5 placeholder:text-muted/40")}
              aria-label="Account equity"
            />
          </div>
        </div>
        <div>
          <div className={microLabel}>Risk per trade</div>
          <div role="group" aria-label="Risk percent" className="inline-flex gap-[3px] rounded-md border border-line p-[3px]">
            {RISK_PRESETS.map((v) => {
              const active = v === riskPct;
              return (
                <button
                  key={v}
                  type="button"
                  aria-pressed={active}
                  onClick={() => pickRisk(v)}
                  className={cx(
                    "num rounded-full px-3 py-1 text-[12px] font-semibold transition-colors",
                    active ? "bg-accent/15 text-accent" : "text-muted hover:bg-white/10 hover:text-fg"
                  )}
                >
                  {v}%
                </button>
              );
            })}
          </div>
        </div>
      </div>

      {/* Which form values feed the sizing */}
      <div className="mb-3 font-mono text-[10.5px] text-muted/70">
        <span className="capitalize">{side}</span> · entry <span className="text-fg/80">{entryTxt}</span> · stop{" "}
        <span className={cx(f.stopMissing ? "text-warn" : "text-fg/80")}>{stopTxt}</span> · target{" "}
        <span className="text-fg/80">{targetTxt}</span>
      </div>

      {/* Suggest stop from computed levels (hidden when analysis carries neither ATR nor support) */}
      {suggestions.length > 0 && (
        <div className="mb-3">
          <div className={microLabel}>Suggest stop — computed from volatility, not a recommendation</div>
          <div className="flex flex-wrap gap-1.5">
            {suggestions.map((c) => (
              <button
                key={c.basis}
                type="button"
                onClick={() => onSuggestStop?.(c.stop)}
                title={c.detail}
                className="num rounded-full border border-line2 bg-panel px-2.5 py-1 text-[11.5px] text-fg/85 transition-colors hover:border-accent/60"
              >
                {c.basis} → {usd(c.stop)}
              </button>
            ))}
          </div>
        </div>
      )}

      {/* Live output — 2-up on phones (four 16px numbers crush at grid-cols-4), 4-up from sm. */}
      <div className="grid grid-cols-2 gap-3 rounded-md border border-line/60 bg-panel/50 px-3.5 py-3 sm:grid-cols-4">
        <Metric label="Shares" value={result.shares > 0 ? result.shares.toLocaleString() : "—"} tone="text-accent" />
        <Metric label="Position" value={result.positionValue > 0 ? usd0(result.positionValue) : "—"} />
        <Metric label="$ at risk" value={result.dollarRisk > 0 ? usd(result.dollarRisk) : "—"} />
        <Metric
          label="Reward : risk"
          value={result.rr != null ? result.rr.toFixed(1) : "—"}
          tone={f.rrBelowFloor ? "text-warn" : "text-fg"}
        />
      </div>

      <p className="mt-2.5 text-[12px] leading-relaxed text-muted">{plain}</p>

      {warnings.length > 0 && (
        <ul className="mt-2 space-y-1">
          {warnings.map((w) => (
            <li key={w} className="flex items-start gap-1.5 text-[11.5px] leading-snug text-warn">
              <Icon name="warning" size={13} className="mt-px flex-none" />
              <span>{w}</span>
            </li>
          ))}
        </ul>
      )}

      {/* Apply-to-form button only where there's a size field to fill (the Journal log form); the
          Terminal planner omits onApplySize and carries the share count via its own Log handoff. */}
      {onApplySize && (
        <div className="mt-3 flex items-center justify-between gap-3">
          <button
            type="button"
            onClick={() => result.shares > 0 && onApplySize(result.shares)}
            disabled={result.shares <= 0}
            className="inline-flex h-8 items-center rounded-full border border-accent/50 bg-accent/10 px-3.5 text-[12px] font-semibold text-accent transition-colors hover:bg-accent/16 disabled:opacity-40"
          >
            Use {result.shares > 0 ? result.shares.toLocaleString() : ""} shares →
          </button>
        </div>
      )}

      <p className="mt-3 border-t border-line/60 pt-2.5 text-[10.5px] leading-relaxed text-muted/70">
        Position sizing only — this app never places an order. You decide and execute.
      </p>
    </div>
  );
}
