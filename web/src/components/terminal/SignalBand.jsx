import { useEffect, useState } from "react";
import { AdminRequiredError, AuthRequiredError, fetchPrediction, trainModel } from "../../lib/api.js";
import { SkeletonText, Tooltip, useToast } from "../ui/index.js";
import { useSettings } from "../../settings/SettingsContext.jsx";
import { Tag, Micro } from "./bits.jsx";
import { cx } from "../../lib/cx.js";
import { Icon } from "../shell/icons.jsx";
import { useAuth } from "../../auth/AuthContext.jsx";

// SignalBand — the full-width directional-signal band that opens the Terminal (chart-free,
// verdict-first). The ONLY place a Buy/Hold/Sell bias appears, and only ever from the validated
// walk-forward quant model — never the LLM. Shown WITH its track record + confidence, or an
// honest "not yet validated". Suggestion only: the human executes; the app places no orders.

const HORIZONS = [3, 5, 10];
const pct = (n, d = 1) => (n == null ? "—" : `${(n * 100).toFixed(d)}%`);
const ago = (ms) => {
  const s = Math.floor((Date.now() - ms) / 1000);
  if (s < 60) return "just now";
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  return `${Math.floor(s / 3600)}h ago`;
};

const VERDICT = {
  buy: { box: "border-accent/35 bg-accent/10 text-accent", sub: "text-accent/70", side: "LONG" },
  sell: { box: "border-down/35 bg-down/10 text-down", sub: "text-down/70", side: "SHORT" },
  hold: { box: "border-line2 bg-panel2 text-muted", sub: "text-muted/70", side: "FLAT" },
};

function Bar({ pctFilled, tone }) {
  return (
    <div className="mt-1.5 h-[5px] overflow-hidden rounded-full bg-line">
      <span
        className={cx("block h-full rounded-full", tone)}
        style={{ width: `${Math.max(0, Math.min(100, pctFilled || 0))}%` }}
      />
    </div>
  );
}

function TrackRecord({ backtest }) {
  const cells = [
    { k: "HIT RATE", v: pct(backtest?.hitRate) },
    { k: "SHARPE (NET)", v: backtest?.sharpe != null ? backtest.sharpe.toFixed(2) : "—" },
    {
      k: "EXPECTANCY",
      v:
        backtest?.expectancy != null ? (
          <>
            {(backtest.expectancy * 10000).toFixed(1)}
            <span className="text-[10px] text-muted/60"> bps/bar</span>
          </>
        ) : (
          "—"
        ),
    },
    { k: "MAX DRAWDOWN", v: pct(backtest?.maxDrawdown), down: true },
    { k: "# TRADES", v: backtest?.numTrades ?? "—" },
  ];
  return (
    // The 5 stat cells stay readable by keeping a min width and scrolling horizontally on phones;
    // from ~440px up the row simply fills its container as before (desktop unchanged).
    <div className="overflow-x-auto">
      <div className="flex min-w-[420px] overflow-hidden rounded-lg border border-line">
        {cells.map((c, i) => (
          <div key={c.k} className={cx("flex-1 px-3.5 py-2.5", i < cells.length - 1 && "border-r border-line")}>
            <div className="mb-1 label-mono text-muted">{c.k}</div>
            <div className={cx("num text-[15px] font-bold", c.down ? "text-down" : "text-fg")}>{c.v}</div>
          </div>
        ))}
      </div>
    </div>
  );
}

// Compact mono segmented control (horizon selector).
function Seg({ items, value, onChange, ariaLabel }) {
  return (
    <div role="group" aria-label={ariaLabel} className="flex gap-[3px] rounded-sm border border-line p-[3px]">
      {items.map((it) => {
        const active = it === value;
        return (
          <button
            key={it}
            type="button"
            aria-pressed={active}
            onClick={() => onChange(it)}
            className={cx(
              "num rounded-full px-2.5 py-1 text-[11px] font-semibold transition-colors",
              active ? "bg-accent/15 text-accent" : "text-muted hover:bg-white/10 hover:text-fg"
            )}
          >
            {it}
          </button>
        );
      })}
    </div>
  );
}

// ConfidenceBreakdown — the factor-by-factor "why is confidence 2%?" tooltip body. Mirrors
// services/prediction model.confidence_breakdown: base × sharpe × trades × suspect.
function ConfidenceBreakdown({ bd, backtest }) {
  const rows = [
    { k: "prob strength", v: bd.base, hint: "|p − 50%| / 50%" },
    {
      k: "sharpe factor",
      v: bd.sharpeFactor,
      hint: `Sharpe ${backtest?.sharpe != null ? backtest.sharpe.toFixed(2) : "—"} / 2.00 needed for full credit`,
    },
    {
      k: "trades factor",
      v: bd.tradesFactor,
      hint: `${backtest?.numTrades ?? "—"} / 100 trades needed for full credit`,
    },
    ...(bd.suspectFactor < 1
      ? [{ k: "suspect penalty", v: bd.suspectFactor, hint: "Sharpe > 3 — probable leakage" }]
      : []),
  ];
  return (
    <span className="block w-[210px]">
      <span className="mb-1 block label-mono text-muted">
        confidence = product of
      </span>
      {rows.map((r) => (
        <span key={r.k} className="flex items-baseline justify-between gap-2 py-px">
          <span className="text-muted">
            {r.k} <span className="text-muted/50">· {r.hint}</span>
          </span>
          <span className="num font-semibold text-fg">×{r.v.toFixed(2)}</span>
        </span>
      ))}
      <span className="mt-1 flex items-baseline justify-between border-t border-line pt-1">
        <span className="text-muted">confidence</span>
        <span className="num font-bold text-warn">{pct(bd.confidence, 0)}</span>
      </span>
    </span>
  );
}

const DISCLAIMER =
  "Not investment advice. A backtested probability of a modest edge — not a guarantee. Real edges are small and decay; a Sharpe above 3 is flagged as probable leakage, not celebrated.";

export default function SignalBand({ ticker, timeframe, onLogTrade }) {
  const { settings, saveSettings, lastTrainedAt, refreshModelRegistry } = useSettings();
  const [horizon, setHorizon] = useState(settings.defaultHorizon);
  const [allowShort, setAllowShort] = useState(settings.allowShort);
  const [pred, setPred] = useState(null);
  const [status, setStatus] = useState("loading"); // loading | ready | down
  const [training, setTraining] = useState(false);
  const [activeChangedAt, setActiveChangedAt] = useState(0);
  const toast = useToast();
  const { promptSignIn } = useAuth();

  // Keep the band's controls in sync with the user's saved defaults — on hydrate and when the Settings
  // page changes them, so allow-short toggled in either place matches. (These effects only fire when
  // the saved value actually changes, so they don't clobber a local horizon tweak.)
  useEffect(() => {
    setAllowShort(settings.allowShort);
  }, [settings.allowShort]);
  useEffect(() => {
    setHorizon(settings.defaultHorizon);
  }, [settings.defaultHorizon]);

  useEffect(() => {
    let alive = true;
    setStatus("loading");
    fetchPrediction(ticker, timeframe, horizon)
      .then((p) => alive && (setPred(p), setStatus("ready")))
      .catch(() => alive && setStatus("down"));
    return () => {
      alive = false;
    };
  }, [ticker, timeframe, horizon]);

  // Only an explicit promotion/rollback bumps lastTrainedAt: refetch the newly active immutable
  // model cache-busted. Candidate training alone deliberately does not enter this path.
  // (lastTrainedAt starts at 0). Reads the current ticker/timeframe/horizon from the closure.
  useEffect(() => {
    if (!lastTrainedAt) return;
    let alive = true;
    fetchPrediction(ticker, timeframe, horizon, true)
      .then((p) => {
        if (!alive) return;
        setPred(p);
        setStatus("ready");
        setActiveChangedAt(Date.now());
      })
      .catch(() => {});
    return () => {
      alive = false;
    };
  }, [lastTrainedAt]); // eslint-disable-line react-hooks/exhaustive-deps

  async function train() {
    setTraining(true);
    try {
      const candidate = await trainModel(ticker, { timeframe, horizon, allowShort });
      await refreshModelRegistry().catch(() => {});
      toast({
        tone: "success",
        title: "Challenger trained",
        message: `${candidate.modelVersion} is stored but not serving. Review it in Settings.`,
      });
    } catch (err) {
      if (err instanceof AuthRequiredError) {
        promptSignIn();
      } else {
        toast({
          tone: err instanceof AdminRequiredError ? "info" : "error",
          title: err instanceof AdminRequiredError ? "Admin access required" : "Training failed",
          message: err.message,
        });
      }
    } finally {
      setTraining(false);
    }
  }

  const signal = pred?.signal;
  const backtest = pred?.backtest;
  const synthetic = pred?.trainedOnSynthetic;
  const dir = (signal?.direction || "Hold").toLowerCase();
  const v = VERDICT[dir] || VERDICT.hold;

  // Controls block — shared across ready/not-validated states.
  const controls = (
    <div className="flex w-full flex-col justify-center gap-3 px-5 py-3.5 lg:w-[256px] lg:shrink-0">
      <div className="flex items-center gap-2.5">
        <Micro className="tracking-[0.1em]">Horizon</Micro>
        <Seg items={HORIZONS} value={horizon} onChange={setHorizon} ariaLabel="Horizon (bars)" />
      </div>
      <div className="flex items-center gap-2">
        <label className="flex cursor-pointer items-center gap-2 whitespace-nowrap text-[12px] text-muted">
          <input
            type="checkbox"
            checked={allowShort}
            onChange={(e) => {
              const next = e.target.checked;
              setAllowShort(next); // local so the manual retrain respects it at once (guests too)
              saveSettings({ allowShort: next }).catch(() => {}); // persist for signed-in users; guests fail soft
            }}
            className="h-3.5 w-3.5 accent-accent"
          />
          allow short
        </label>
        <button
          type="button"
          onClick={train}
          disabled={training}
          className="num ml-auto whitespace-nowrap rounded-full border border-line2 px-2.5 py-1.5 text-[11.5px] text-fg/90 transition-colors hover:border-accent/60 disabled:opacity-50"
        >
          {training ? "Training…" : "Train challenger"}
        </button>
      </div>
      <button
        type="button"
        disabled={!signal}
        onClick={() => onLogTrade?.({ ticker, side: dir === "sell" ? "short" : "long", timeframe })}
        className="pill-lift flex h-10 items-center justify-center rounded-full bg-fg text-[13px] font-semibold text-bg transition-[transform,background-color,box-shadow] duration-200 ease-premium hover:-translate-y-px hover:bg-white hover:shadow-glow disabled:pointer-events-none disabled:opacity-40"
      >
        Log as trade →
      </button>
      <p className="text-[11px] leading-relaxed text-muted">
        Suggestion only — <strong className="text-llm">you place the trade. This app never executes orders.</strong>
      </p>
    </div>
  );

  return (
    <div className="border-b border-line bg-panel">
      {synthetic && (
        <div className="border-b border-warn/30 bg-warn/10 px-[18px] py-2 text-[12px] text-warn">
          <strong className="inline-flex items-center gap-1.5 font-bold"><Icon name="warning" size={14} /> SYNTHETIC DATA</strong> — this is <strong>not a real backtest</strong>. The model
          trained on generated bars; do not trade on this.
        </div>
      )}

      {status === "loading" && (
        <div className="px-[18px] py-5">
          <SkeletonText lines={3} />
        </div>
      )}

      {status === "down" && (
        <div className="px-[18px] py-5 text-[12.5px] leading-relaxed text-muted">
          The prediction service (<code className="rounded bg-panel2 px-1">:8003</code>) is unreachable — the rest of the
          dashboard is unaffected. Start it with{" "}
          <code className="rounded bg-panel2 px-1">cd services/prediction &amp;&amp; uvicorn app.main:app --port 8003</code>.
        </div>
      )}

      {status === "ready" && (
        <div className="flex flex-col lg:flex-row lg:items-stretch">
          {/* Verdict block — wraps the prob/confidence column below the verdict box on very narrow
              phones (104px + 184px would otherwise overflow); one line everywhere it fits. */}
          <div className="flex flex-wrap items-center gap-4 border-b border-line px-[22px] py-4 lg:flex-nowrap lg:shrink-0 lg:border-b-0 lg:border-r">
            <div
              className={cx(
                "flex h-[76px] w-[104px] flex-col items-center justify-center gap-0.5 rounded-lg border",
                v.box
              )}
            >
              <span className="num text-[28px] font-bold uppercase leading-none tracking-[0.04em]">
                {signal ? signal.direction : "N/A"}
              </span>
              <span className={cx("num text-[9px] tracking-[0.14em]", v.sub)}>
                H{horizon}
                {signal ? ` · ${v.side}` : ""}
              </span>
            </div>
            <div className="w-[184px]">
              <div className="mb-2 text-[14px] font-semibold text-fg">Directional signal</div>
              <div className="flex items-baseline justify-between">
                {signal?.calibrated ? (
                  <Tooltip
                    underline
                    side="bottom"
                    label={`Calibrated probability (isotonic, fitted on out-of-fold predictions) — the number means what it says. Raw model output: ${pct(signal?.probUpRaw)}. The Buy/Hold/Sell threshold still uses the raw value the backtest validated.`}
                  >
                    <span className="text-[12px] text-muted">prob up · cal.</span>
                  </Tooltip>
                ) : (
                  <span className="text-[12px] text-muted">prob up</span>
                )}
                <span className="num text-[14px] font-bold text-fg">{pct(signal?.probUp)}</span>
              </div>
              <Bar pctFilled={(signal?.probUp || 0) * 100} tone="bg-accent" />
              <div className="mt-2.5 flex items-baseline justify-between">
                {signal?.confidenceBreakdown ? (
                  <Tooltip
                    underline
                    side="bottom"
                    label={<ConfidenceBreakdown bd={signal.confidenceBreakdown} backtest={backtest} />}
                  >
                    <span className="text-[12px] text-muted">confidence</span>
                  </Tooltip>
                ) : (
                  <span className="text-[12px] text-muted">confidence</span>
                )}
                <span className="num text-[14px] font-bold text-warn">{pct(signal?.confidence, 0)}</span>
              </div>
              <Bar pctFilled={(signal?.confidence || 0) * 100} tone="bg-warn" />
            </div>
          </div>

          {/* Track record block */}
          <div className="flex flex-1 flex-col justify-center border-b border-line px-[22px] py-3.5 lg:border-b-0 lg:border-r">
            <div className="mb-3 flex items-center gap-2.5">
              <Micro>Walk-forward track record</Micro>
              <Tag tone="llm">Quant · Walk-forward</Tag>
              {backtest?.suspect && <Tag tone="warn">Suspect · Sharpe&gt;3</Tag>}
              {activeChangedAt > 0 && <Tag tone="accent">active changed · {ago(activeChangedAt)}</Tag>}
            </div>
            {signal || backtest ? (
              <TrackRecord backtest={backtest} />
            ) : (
              <p className="text-[12.5px] leading-relaxed text-muted">
                No signal is shown until a model passes the walk-forward gate
                {pred?.reason ? ` (${pred.reason})` : ""}. Train one to see a bias — never a bare guess.
              </p>
            )}
            <p className="mt-2.5 text-[10.5px] leading-relaxed text-muted/70">{DISCLAIMER}</p>
          </div>

          {/* Controls block */}
          {controls}
        </div>
      )}
    </div>
  );
}
