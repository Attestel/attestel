import { useEffect, useState, useCallback, useMemo } from "react";
import {
  TIMEFRAMES,
  listTrades,
  createTrade,
  closeTrade,
  deleteTrade,
  fetchJournalStats,
  fetchPrediction,
  fmtPct,
  fmtMoney,
  AuthRequiredError,
} from "../lib/api.js";
import { useToast } from "./ui/index.js";
import { useAuth } from "../auth/AuthContext.jsx";
import { useSettings } from "../settings/SettingsContext.jsx";
import { Tag } from "./terminal/bits.jsx";
import { cx } from "../lib/cx.js";
import PositionSizer from "./risk/PositionSizer.jsx";
import PreTradeChecklist from "./checklist/PreTradeChecklist.jsx";
import { Icon } from "./shell/icons.jsx";

// JournalPanel — a manual RECORD of your own trades, redesigned as one dense attestel
// (design_examples/3c-journal): a mode toggle, an inline "log a trade" form, a descriptive
// performance stat grid beside open positions, and the closed-trades ledger.
//
// NOT financial advice, and it executes NOTHING — no broker, no orders. Stats describe your own past
// decisions; they are not recommendations. Fails silent if the journal service is down.

const STATS_POLL_MS = 30000;
const today = () => new Date().toISOString().slice(0, 10);
const pnlTone = (n) => (n == null ? "text-fg" : n >= 0 ? "text-accent" : "text-down");
const sideTone = (s) => (s === "long" ? "text-accent" : "text-down");

const CTL = "h-[38px] w-full rounded-lg border border-line2 bg-panel2 px-3 text-[13px] text-fg focus:border-accent/60 focus:outline-none";
const microLabel = "mb-1.5 label-mono text-muted";
const btnSm = "rounded-sm border px-2.5 py-1 text-[12px] transition-colors";

function Labeled({ label, className, children }) {
  return (
    <div className={className}>
      <div className={microLabel}>{label}</div>
      {children}
    </div>
  );
}

function SecHeader({ title, borderTop, children }) {
  return (
    <div className={cx("flex h-11 items-center gap-2.5 border-b border-line px-[22px]", borderTop && "border-t border-t-line")}>
      <span className="text-[14px] font-semibold text-fg">{title}</span>
      {children}
    </div>
  );
}

function CountPill({ n }) {
  const on = n > 0;
  return (
    <span
      className={cx(
        "num rounded-full px-2 py-0.5 text-[12px] font-semibold leading-none",
        on ? "bg-accent text-bg" : "bg-line text-muted"
      )}
    >
      {n}
    </span>
  );
}

function Seg({ value, onChange, items }) {
  return (
    <div className="flex gap-[3px] rounded-sm border border-line p-[3px]">
      {items.map((it) => {
        const active = it.value === value;
        return (
          <button
            key={it.value}
            type="button"
            aria-pressed={active}
            onClick={() => onChange(it.value)}
            className={cx(
              "num rounded-full px-3 py-1 text-[11px] font-semibold transition-colors",
              active ? "bg-accent/15 text-accent" : "text-muted hover:bg-white/10 hover:text-fg"
            )}
          >
            {it.label}
          </button>
        );
      })}
    </div>
  );
}

function CloseForm({ trade, onClose }) {
  const [price, setPrice] = useState(trade.markPrice ?? "");
  const [date, setDate] = useState(today());
  const [busy, setBusy] = useState(false);
  return (
    <form
      className="flex items-center gap-1.5"
      onSubmit={async (e) => {
        e.preventDefault();
        if (!price) return;
        setBusy(true);
        try {
          await onClose({ date, price: Number(price) });
        } finally {
          setBusy(false);
        }
      }}
    >
      <input
        type="number"
        step="any"
        required
        placeholder="exit"
        value={price}
        onChange={(e) => setPrice(e.target.value)}
        className="num h-11 w-20 rounded-md border border-line2 bg-panel2 px-2 text-[12px] text-fg focus:border-accent/60 focus:outline-none sm:h-7"
      />
      <input
        type="date"
        value={date}
        onChange={(e) => setDate(e.target.value)}
        className="num h-11 w-32 rounded-md border border-line2 bg-panel2 px-2 text-[12px] text-fg focus:border-accent/60 focus:outline-none sm:h-7"
      />
      <button type="submit" disabled={busy} className={cx(btnSm, "min-h-[44px] border-line2 text-fg/85 hover:border-accent/60 sm:min-h-0")}>
        {busy ? "…" : "Close"}
      </button>
    </form>
  );
}

function AttachedReadView({ read }) {
  if (!read) return <p className="text-[12px] text-muted">No read was captured at entry.</p>;
  const s = read.structured || {};
  return (
    <div className="text-[12px]">
      <div className="text-muted">Read captured at entry ({read.capturedAt?.slice(0, 10) || "—"})</div>
      {s.summary && <p className="my-1.5 text-[13px] text-fg/85">{s.summary}</p>}
      <div className="grid grid-cols-1 gap-3 sm:grid-cols-2">
        <div>
          <h5 className="mb-0.5 label-mono text-up">Bull (at entry)</h5>
          <ul className="list-disc space-y-0.5 pl-4 text-muted">{(s.bullCase || []).map((x, i) => <li key={i}>{x}</li>)}</ul>
        </div>
        <div>
          <h5 className="mb-0.5 label-mono text-down">Bear (at entry)</h5>
          <ul className="list-disc space-y-0.5 pl-4 text-muted">{(s.bearCase || []).map((x, i) => <li key={i}>{x}</li>)}</ul>
        </div>
      </div>
      {read.regime?.trend && (
        <div className="mt-1.5 text-muted">
          Regime at entry: {read.regime.trend}
          {read.regime.momentum ? ` · ${read.regime.momentum}` : ""}
        </div>
      )}
    </div>
  );
}

const TH = "border-b border-line py-2 px-2 text-left label-mono text-muted";
const TD = "border-b border-line/50 py-2.5 px-2 align-middle";

function StatCell({ label, value, tone = "text-fg", i, cols = 4, total }) {
  const rightEdge = (i + 1) % cols === 0;
  const lastRow = i >= total - (total % cols || cols);
  return (
    <div
      className={cx(
        "px-[18px] py-3.5",
        !rightEdge && "border-r border-line/50",
        !lastRow && "border-b border-line/50"
      )}
    >
      <div className="mb-1.5 label-mono text-muted">{label}</div>
      <div className={cx("num text-[18px] font-bold", tone)}>{value}</div>
    </div>
  );
}

// ChecklistSplitView — the coaching payoff: the user's OWN checklist-complete trades vs the rest.
// Purely descriptive (win rate + expectancy per bucket), from their record — never a recommendation.
function ChecklistSplitView({ split }) {
  const a = split.withAllPass || { count: 0 };
  const b = split.without || { count: 0 };
  const winTxt = (x) => (x.count > 0 ? `${Math.round(x.winRate * 100)}%` : "—");
  const expCell = (x) => (x.count > 0 ? fmtPct(x.expectancy) : "—");
  const both = a.count > 0 && b.count > 0;
  return (
    <div>
      <table className="w-full">
        <thead>
          <tr>
            <th className={TH}>Checklist</th>
            <th className={cx(TH, "text-right")}>Trades</th>
            <th className={cx(TH, "text-right")}>Win%</th>
            <th className={cx(TH, "text-right")}>Exp/trade</th>
          </tr>
        </thead>
        <tbody>
          <tr>
            <td className={cx(TD, "font-semibold text-fg")}>All-pass</td>
            <td className={cx(TD, "num text-right text-muted")}>{a.count}</td>
            <td className={cx(TD, "num text-right", a.count > 0 ? "text-accent" : "text-muted")}>{winTxt(a)}</td>
            <td className={cx(TD, "num text-right", pnlTone(a.count > 0 ? a.expectancy : null))}>{expCell(a)}</td>
          </tr>
          <tr>
            <td className={cx(TD, "font-semibold text-fg/80")}>Not all-pass</td>
            <td className={cx(TD, "num text-right text-muted")}>{b.count}</td>
            <td className={cx(TD, "num text-right text-muted")}>{winTxt(b)}</td>
            <td className={cx(TD, "num text-right", pnlTone(b.count > 0 ? b.expectancy : null))}>{expCell(b)}</td>
          </tr>
        </tbody>
      </table>
      <p className="mt-2 text-[11.5px] leading-relaxed text-muted">
        {both
          ? `Your checklist-complete trades won ${Math.round(a.winRate * 100)}% vs ${Math.round(b.winRate * 100)}% — descriptive, from your own record, not a recommendation.`
          : "Descriptive — from your own record. Log trades on both sides of the checklist to compare."}
      </p>
    </div>
  );
}

export default function JournalPanel({ tickers = [], defaultTicker = "NVDA", prefill = null, dashboard = null }) {
  const [trades, setTrades] = useState([]);
  const [stats, setStats] = useState(null);
  const [down, setDown] = useState(false);
  const [busy, setBusy] = useState(false);
  const [expanded, setExpanded] = useState(null);
  const [modeFilter, setModeFilter] = useState("live");
  const toast = useToast();
  const { requireAuth, promptSignIn } = useAuth();
  const { settings } = useSettings();

  const [ticker, setTicker] = useState(defaultTicker);
  const [side, setSide] = useState("long");
  const [date, setDate] = useState(today());
  const [price, setPrice] = useState("");
  const [size, setSize] = useState("");
  const [timeframe, setTimeframe] = useState("1D");
  const [stop, setStop] = useState("");
  const [target, setTarget] = useState("");
  const [thesis, setThesis] = useState("");
  const [tags, setTags] = useState("");
  const [captureRead, setCaptureRead] = useState(true);
  useEffect(() => setTicker(defaultTicker), [defaultTicker]);
  // logMode is the mode a NEW trade is recorded under (separate from the list's modeFilter). Beginner
  // mode defaults it to paper (paper-first, docs/BEGINNER.md Piece 4); still switchable. Re-applies the
  // level default whenever the level flips.
  const [logMode, setLogMode] = useState(settings.experienceLevel === "beginner" ? "paper" : "live");
  useEffect(() => {
    setLogMode(settings.experienceLevel === "beginner" ? "paper" : "live");
  }, [settings.experienceLevel]);
  // The latest sizing from the inline PositionSizer (shares/dollarRisk/rr + the equity/risk % used).
  // Persisted onto the trade as an optional `risk` plan so a later review can ask "did I size to 1%?".
  const [sizerResult, setSizerResult] = useState(null);
  // The latest pre-trade checklist result + the validated-signal fact it reads. The signal isn't in
  // the dashboard payload (SignalBand fetches it), so we fetch it here fail-soft and merge it in.
  // formResetKey remounts the checklist after a submit so its self-attest checkboxes clear.
  const [checklistResult, setChecklistResult] = useState(null);
  const [prediction, setPrediction] = useState(null);
  const [formResetKey, setFormResetKey] = useState(0);

  const refresh = useCallback(async () => {
    try {
      const [ts, st] = await Promise.all([listTrades("all", modeFilter), fetchJournalStats(modeFilter)]);
      setTrades(ts);
      setStats(st);
      setDown(false);
    } catch {
      setDown(true);
    }
  }, [modeFilter]);

  useEffect(() => {
    refresh();
    const id = setInterval(refresh, STATS_POLL_MS);
    return () => clearInterval(id);
  }, [refresh]);

  // The prefill may carry a full plan from the beginner Terminal (entry/stop/target/thesis/size), not
  // just ticker/side/timeframe from the SignalBand. Key on every field so a fresh plan re-applies.
  const prefillKey = prefill
    ? [prefill.ticker, prefill.side, prefill.timeframe, prefill.entry, prefill.stop, prefill.target, prefill.thesis, prefill.size].join("|")
    : null;
  useEffect(() => {
    if (!prefill) return;
    if (prefill.ticker) setTicker(prefill.ticker);
    if (prefill.side) setSide(prefill.side);
    if (prefill.timeframe) setTimeframe(prefill.timeframe);
    if (prefill.entry != null) setPrice(String(prefill.entry));
    if (prefill.stop != null) setStop(String(prefill.stop));
    if (prefill.target != null) setTarget(String(prefill.target));
    if (prefill.thesis != null) setThesis(prefill.thesis);
    if (prefill.size != null) setSize(String(prefill.size));
  }, [prefillKey]); // eslint-disable-line react-hooks/exhaustive-deps

  // Fetch the validated signal for the checklist's item 1 (fail-soft — a down service / no model just
  // leaves it null, and the item falls back to the thesis; never a fabricated pass).
  useEffect(() => {
    let alive = true;
    fetchPrediction(ticker, timeframe, settings.defaultHorizon)
      .then((p) => alive && setPrediction(p))
      .catch(() => alive && setPrediction(null));
    return () => {
      alive = false;
    };
  }, [ticker, timeframe, settings.defaultHorizon]);

  // Memoized so their identity is stable across the re-renders the checklist's own onChange triggers —
  // otherwise a fresh object every render would loop the evaluator. The checklist only reads confluence
  // + the merged-in signal from the dashboard, so pass a minimal object rather than copy the whole one.
  const parsedTags = useMemo(() => tags.split(",").map((t) => t.trim()).filter(Boolean), [tags]);
  const checklistDraft = useMemo(() => ({ thesis, stop, tags: parsedTags }), [thesis, stop, parsedTags]);
  const checklistDashboard = useMemo(
    () => ({ confluence: dashboard?.confluence, signal: prediction?.signal, backtest: prediction?.backtest }),
    [dashboard, prediction]
  );

  // In beginner mode the pre-trade checklist is a real GATE: the log is blocked until it's all-green
  // (prompt 21's advisory checklist made blocking here). Standard mode stays advisory — submit only
  // waits on `busy`. Unlocks no trading action either way; it only gates the RECORD (invariant #2).
  const isBeginner = settings.experienceLevel === "beginner";
  const checklistBlocks = isBeginner && !checklistResult?.allPass;

  async function submit(e) {
    e.preventDefault();
    if (!requireAuth()) return; // guest -> routed to sign-in before any write
    if (checklistBlocks) return; // beginner gate: complete the checklist first (button is disabled too)
    const trade = {
      ticker,
      side,
      mode: logMode,
      entry: { date, price: Number(price), size: Number(size), timeframe },
      thesis,
      tags: tags.split(",").map((t) => t.trim()).filter(Boolean),
      captureRead,
    };
    if (stop) trade.stop = Number(stop);
    if (target) trade.target = Number(target);
    // Attach the sizing plan when the sizer produced a meaningful one (equity set, risk within cap).
    // It's a RECORD of the plan — never used to compute P&L. Old/manual trades simply omit it.
    if (sizerResult && sizerResult.accountEquity > 0 && sizerResult.dollarRisk > 0 && sizerResult.riskPct > 0 && sizerResult.riskPct <= 2) {
      trade.risk = {
        accountEquity: sizerResult.accountEquity,
        riskPct: sizerResult.riskPct,
        dollarRisk: sizerResult.dollarRisk,
      };
      if (sizerResult.rr != null) trade.risk.rr = sizerResult.rr;
    }
    // Attach the pre-trade checklist result (plan-completeness record, advisory — it blocks nothing).
    // Strip the UI-only `detail` field; the journal Checklist keeps {key,label,passed,auto,source}.
    if (checklistResult) {
      trade.checklist = {
        items: checklistResult.items.map(({ key, label, passed, auto, source }) => ({ key, label, passed, auto, source })),
        allPass: checklistResult.allPass,
        mode: checklistResult.mode,
      };
    }
    setBusy(true);
    try {
      await createTrade(trade);
      setPrice("");
      setSize("");
      setStop("");
      setTarget("");
      setThesis("");
      setTags("");
      setFormResetKey((k) => k + 1); // remount the checklist so its self-attest checkboxes clear
      await refresh();
      toast({ tone: "success", title: "Trade logged", message: `${side} ${ticker} @ ${trade.entry.price}` });
    } catch (err) {
      if (err instanceof AuthRequiredError) { promptSignIn(); return; }
      toast({ tone: "error", title: "Could not log trade", message: err.message });
    } finally {
      setBusy(false);
    }
  }

  async function doClose(id, exit) {
    if (!requireAuth()) return;
    try {
      await closeTrade(id, exit);
      await refresh();
      toast({ tone: "info", title: "Position closed" });
    } catch (err) {
      if (err instanceof AuthRequiredError) promptSignIn();
    }
  }
  async function remove(id) {
    if (!requireAuth()) return;
    try {
      await deleteTrade(id);
      await refresh();
    } catch (err) {
      if (err instanceof AuthRequiredError) promptSignIn();
    }
  }

  if (down) {
    return (
      <section className="overflow-hidden surface-card">
        <SecHeader title="Trade journal">
          <Tag tone="warn">service down</Tag>
        </SecHeader>
        <p className="px-[22px] py-5 text-[13px] leading-relaxed text-muted">
          The journal service (<code className="rounded bg-panel2 px-1">:8096</code>) is unreachable. Start it with{" "}
          <code className="rounded bg-panel2 px-1">cd journal &amp;&amp; go run .</code> — the rest of the dashboard is
          unaffected.
        </p>
      </section>
    );
  }

  const open = trades.filter((t) => t.status === "open");
  const closed = trades.filter((t) => t.status === "closed");
  const baseTickerOpts = tickers.length ? tickers : [{ symbol: defaultTicker, name: defaultTicker }];
  const tickerOpts = baseTickerOpts.some((item) => item.symbol === ticker)
    ? baseTickerOpts
    : [{ symbol: ticker, name: ticker }, ...baseTickerOpts];
  const hint =
    modeFilter === "live"
      ? "your real trades only"
      : modeFilter === "paper"
        ? "simulated signal-driven trades (not real money)"
        : "live + paper combined — paper is simulation only";

  const cells = stats
    ? [
        { label: "Closed trades", value: stats.count },
        { label: "Win rate", value: `${(stats.winRate * 100).toFixed(0)}%` },
        { label: "Expectancy / trade", value: fmtPct(stats.expectancy), tone: pnlTone(stats.expectancy) },
        { label: "Avg win", value: fmtPct(stats.avgWinPct), tone: "text-accent" },
        { label: "Avg loss", value: `-${Number(stats.avgLossPct).toFixed(1)}%`, tone: "text-down" },
        { label: "Realized P&L", value: fmtMoney(stats.totalRealizedPnl), tone: pnlTone(stats.totalRealizedPnl) },
        { label: "Avg R-multiple", value: stats.avgRMultiple != null ? `${stats.avgRMultiple}R` : "—", tone: "text-muted" },
        {
          label: `Open (${stats.openCount}) unreal.`,
          value: fmtMoney(stats.totalUnrealizedPnl),
          tone: pnlTone(stats.totalUnrealizedPnl),
        },
      ]
    : [];

  return (
    <section className="overflow-hidden surface-card">
      {/* Showing toggle */}
      <div className="flex flex-wrap items-center gap-3 border-b border-line px-[22px] py-3">
        <span className="text-[12.5px] text-muted">Showing:</span>
        <Seg
          value={modeFilter}
          onChange={setModeFilter}
          items={[
            { value: "live", label: "Live" },
            { value: "paper", label: "Paper" },
            { value: "all", label: "All" },
          ]}
        />
        <span className="text-[12.5px] text-muted/70">{hint}</span>
      </div>

      {/* Log a trade */}
      <SecHeader title="Log a trade">
        <Tag tone="outline">Manual record · No execution</Tag>
      </SecHeader>
      <div className="border-b border-line px-[22px] py-4">
        <p className="mb-4 max-w-[900px] text-[12.5px] leading-[1.6] text-muted">
          For your own record-keeping. This is <strong className="text-fg/85">not financial advice</strong> and places no
          orders — you record trades you made yourself. Stats describe your past decisions.
        </p>
        <div className="mb-4 flex flex-wrap items-center gap-3">
          <span className="text-[12.5px] text-muted">Log to:</span>
          <Seg
            value={logMode}
            onChange={setLogMode}
            items={[
              { value: "paper", label: "Paper" },
              { value: "live", label: "Live" },
            ]}
          />
          <span className="text-[12px] text-muted/70">
            {logMode === "paper" ? "simulation — not real money" : "your real trade"}
          </span>
        </div>
        {isBeginner && (
          <p className="mb-4 max-w-[900px] rounded-lg border border-line2 bg-panel2/40 px-3.5 py-2.5 text-[12px] leading-relaxed text-muted">
            <strong className="text-fg/85">Paper-first.</strong> Don't commit real capital until a setup is paper-validated —
            roughly <strong className="text-fg/85">≥30 closed paper trades where live ≈ backtest</strong> (see the Paper tab). The
            checklist below must be all-green before you can log.
          </p>
        )}
        <form onSubmit={submit}>
          {/* Fields fill a 2-up (sm: 3-up) grid on small screens so nothing exceeds the column; the
              original fixed-width flex-wrap row is restored from md up, so desktop is unchanged. */}
          <div className="mb-4 grid grid-cols-2 gap-3 sm:grid-cols-3 md:flex md:flex-wrap md:items-end">
            <Labeled label="Ticker" className="md:w-[110px]">
              <select value={ticker} onChange={(e) => setTicker(e.target.value)} className={CTL}>
                {tickerOpts.map((t) => (
                  <option key={t.symbol} value={t.symbol}>
                    {t.symbol}
                  </option>
                ))}
              </select>
            </Labeled>
            <Labeled label="Side" className="md:w-[100px]">
              <select value={side} onChange={(e) => setSide(e.target.value)} className={cx(CTL, sideTone(side))}>
                <option value="long">long</option>
                <option value="short">short</option>
              </select>
            </Labeled>
            <Labeled label="Entry date" className="md:w-[150px]">
              <input type="date" value={date} onChange={(e) => setDate(e.target.value)} className={cx(CTL, "num")} />
            </Labeled>
            <Labeled label="Entry price" className="md:w-[120px]">
              <input
                type="number"
                step="any"
                required
                value={price}
                onChange={(e) => setPrice(e.target.value)}
                placeholder="150"
                className={cx(CTL, "num placeholder:text-muted/50")}
              />
            </Labeled>
            <Labeled label="Size (shares)" className="md:w-[120px]">
              <input
                type="number"
                step="any"
                required
                value={size}
                onChange={(e) => setSize(e.target.value)}
                placeholder="100"
                className={cx(CTL, "num placeholder:text-muted/50")}
              />
            </Labeled>
            <Labeled label="Timeframe" className="md:w-[100px]">
              <select value={timeframe} onChange={(e) => setTimeframe(e.target.value)} className={CTL}>
                {TIMEFRAMES.map((tf) => (
                  <option key={tf} value={tf}>
                    {tf}
                  </option>
                ))}
              </select>
            </Labeled>
            <Labeled label="Stop (opt)" className="md:w-[130px]">
              <input
                type="number"
                step="any"
                value={stop}
                onChange={(e) => setStop(e.target.value)}
                placeholder="R-multiple"
                className={cx(CTL, "num placeholder:text-muted/50")}
              />
            </Labeled>
            <Labeled label="Target (opt)" className="md:w-[130px]">
              <input
                type="number"
                step="any"
                value={target}
                onChange={(e) => setTarget(e.target.value)}
                className={cx(CTL, "num")}
              />
            </Labeled>
          </div>
          <div className="mb-4">
            <PositionSizer
              dashboard={dashboard}
              side={side}
              entry={price}
              stop={stop}
              target={target}
              onSuggestStop={(v) => setStop(String(v))}
              onApplySize={(shares) => setSize(String(shares))}
              onResult={setSizerResult}
            />
          </div>
          <div className="mb-4">
            <PreTradeChecklist
              key={formResetKey}
              dashboard={checklistDashboard}
              sizer={sizerResult}
              draft={checklistDraft}
              mode={settings.experienceLevel}
              onChange={setChecklistResult}
            />
          </div>
          <div className="mb-4 grid grid-cols-1 gap-4 sm:grid-cols-2 md:flex md:flex-wrap md:items-end">
            <Labeled label="Thesis" className="md:min-w-[240px] md:flex-1">
              <input
                type="text"
                value={thesis}
                onChange={(e) => setThesis(e.target.value)}
                placeholder="why you took the trade"
                className={cx(CTL, "placeholder:text-muted/50")}
              />
            </Labeled>
            <Labeled label="Tags (comma)" className="md:w-[300px]">
              <input
                type="text"
                value={tags}
                onChange={(e) => setTags(e.target.value)}
                placeholder="swing, breakout"
                className={cx(CTL, "placeholder:text-muted/50")}
              />
            </Labeled>
          </div>
          <label className="mb-4 flex w-fit cursor-pointer items-center gap-2.5 text-[13px] text-fg/85">
            <input
              type="checkbox"
              checked={captureRead}
              onChange={(e) => setCaptureRead(e.target.checked)}
              className="h-[18px] w-[18px] rounded accent-accent"
            />
            Capture current read at entry
          </label>
          <div className="flex flex-wrap items-center gap-3">
            <button
              type="submit"
              disabled={busy || checklistBlocks}
              className="pill-lift inline-flex h-10 items-center rounded-full bg-fg px-6 text-[13px] font-semibold text-bg transition-[transform,background-color,box-shadow] duration-200 ease-premium hover:-translate-y-px hover:bg-white hover:shadow-glow disabled:pointer-events-none disabled:opacity-50"
            >
              {busy ? "Saving…" : "Log trade"}
            </button>
            {checklistResult && (
              <span className="text-[12px] text-muted">
                Checklist{" "}
                <span className={cx("num font-semibold", checklistResult.allPass ? "text-accent" : "text-warn")}>
                  {checklistResult.score.passed} / {checklistResult.score.total}
                </span>{" "}
                <span className="text-muted/60">
                  {isBeginner
                    ? checklistBlocks
                      ? "· complete every item to log (beginner mode)"
                      : "· all clear — you can log"
                    : "· advisory — you can still log"}
                </span>
              </span>
            )}
          </div>
        </form>
      </div>

      {/* Performance | Open positions */}
      <div className="grid grid-cols-1 lg:grid-cols-2">
        <div className="border-b border-line lg:border-b-0 lg:border-r">
          <SecHeader title="Performance">
            <Tag tone="outline">Descriptive · Your record</Tag>
          </SecHeader>
          {stats ? (
            <div className="grid grid-cols-2 sm:grid-cols-4">
              {cells.map((c, i) => (
                <StatCell key={c.label} label={c.label} value={c.value} tone={c.tone} i={i} cols={4} total={cells.length} />
              ))}
            </div>
          ) : (
            <div className="px-[22px] py-8 text-center text-[12.5px] text-muted">Loading…</div>
          )}
          {stats?.byTicker?.length > 0 && (
            <div className="px-[22px] py-3">
              <table className="w-full">
                <thead>
                  <tr>
                    <th className={TH}>Ticker</th>
                    <th className={cx(TH, "text-right")}>Trades</th>
                    <th className={cx(TH, "text-right")}>Win%</th>
                    <th className={cx(TH, "text-right")}>Avg %</th>
                    <th className={cx(TH, "text-right")}>P&L</th>
                  </tr>
                </thead>
                <tbody>
                  {stats.byTicker.map((b) => (
                    <tr key={b.ticker}>
                      <td className={cx(TD, "num font-semibold text-fg")}>{b.ticker}</td>
                      <td className={cx(TD, "num text-right text-muted")}>{b.count}</td>
                      <td className={cx(TD, "num text-right text-muted")}>{(b.winRate * 100).toFixed(0)}%</td>
                      <td className={cx(TD, "num text-right", pnlTone(b.avgPnlPct))}>{fmtPct(b.avgPnlPct)}</td>
                      <td className={cx(TD, "num text-right", pnlTone(b.totalPnl))}>{fmtMoney(b.totalPnl)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
          {stats?.checklistSplit && (
            <div className="border-t border-line/60 px-[22px] py-3.5">
              <div className="mb-2 flex items-center gap-2.5">
                <span className="label-mono text-muted">Checklist discipline</span>
                <Tag tone="outline">Descriptive · Your record</Tag>
              </div>
              <ChecklistSplitView split={stats.checklistSplit} />
            </div>
          )}
        </div>

        {/* Open positions */}
        <div>
          <SecHeader title="Open positions">
            <CountPill n={open.length} />
          </SecHeader>
          {open.length === 0 ? (
            <div className="px-[22px] py-10 text-center">
              <div className="mb-1.5 text-[14px] font-semibold text-fg/80">No open positions</div>
              <p className="text-[12.5px] text-muted">Log a trade above to start tracking.</p>
            </div>
          ) : (
            <div className="overflow-x-auto px-3 py-2 sm:px-[22px]">
              <table className="w-full text-[13px]">
                <thead>
                  <tr>
                    <th className={TH}>Ticker</th>
                    <th className={TH}>Side</th>
                    <th className={TH}>Entry</th>
                    <th className={TH}>Mark</th>
                    <th className={TH}>Unreal. P&L</th>
                    <th className={TH}>Days</th>
                    <th className={TH}></th>
                  </tr>
                </thead>
                <tbody>
                  {open.map((t) => (
                    <tr key={t.id}>
                      <td className={cx(TD, "num font-semibold text-fg")}>{t.ticker}</td>
                      <td className={cx(TD, "font-semibold capitalize", sideTone(t.side))}>{t.side}</td>
                      <td className={cx(TD, "num text-muted")}>
                        ${t.entry.price} × {t.entry.size}
                      </td>
                      <td className={cx(TD, "num text-fg/85")}>
                        {t.markPrice != null ? `$${t.markPrice}` : "—"}
                        {t.markIsSynthetic && <Tag tone="warn" className="ml-1">syn</Tag>}
                      </td>
                      <td className={cx(TD, "num", pnlTone(t.pnlAbs))}>
                        {t.pnlAbs != null ? `${fmtMoney(t.pnlAbs)} (${fmtPct(t.pnlPct)})` : "—"}
                      </td>
                      <td className={cx(TD, "num text-muted")}>{t.holdingDays ?? "—"}</td>
                      <td className={TD}>
                        <div className="flex items-center gap-2">
                          <CloseForm trade={t} onClose={(exit) => doClose(t.id, exit)} />
                          <button
                            type="button"
                            onClick={() => remove(t.id)}
                            className={cx(btnSm, "border-down/30 text-down hover:border-down/60")}
                          >
                            <Icon name="close" size={11} />
                          </button>
                        </div>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </div>
      </div>

      {/* Closed trades */}
      <SecHeader title="Closed trades" borderTop>
        <CountPill n={closed.length} />
      </SecHeader>
      {closed.length === 0 ? (
        <div className="px-[22px] py-10 text-center">
          <div className="mb-1.5 text-[14px] font-semibold text-fg/80">No closed trades yet</div>
          <p className="text-[12.5px] text-muted">Close an open position to review it here.</p>
        </div>
      ) : (
        <div className="overflow-x-auto px-3 py-2 sm:px-[22px]">
          <table className="w-full text-[13px]">
            <thead>
              <tr>
                <th className={TH}>Ticker</th>
                <th className={TH}>Side</th>
                <th className={TH}>Entry→Exit</th>
                <th className={TH}>P&L %</th>
                <th className={TH}>R</th>
                <th className={TH}>Days</th>
                <th className={TH}></th>
              </tr>
            </thead>
            <tbody>
              {closed.map((t) => (
                <FragmentRow
                  key={t.id}
                  t={t}
                  expanded={expanded === t.id}
                  onToggle={() => setExpanded(expanded === t.id ? null : t.id)}
                  onDelete={() => remove(t.id)}
                />
              ))}
            </tbody>
          </table>
        </div>
      )}

      <p className="border-t border-line px-[22px] py-3 text-[11px] leading-relaxed text-muted/60">
        Not financial advice — for your own record-keeping. This journal records trades you made yourself; it executes nothing
        and recommends nothing.
      </p>
    </section>
  );
}

function FragmentRow({ t, expanded, onToggle, onDelete }) {
  const tone = pnlTone(t.pnlPct);
  return (
    <>
      <tr className="cursor-pointer transition-colors hover:bg-panel2/50" onClick={onToggle}>
        <td className={cx(TD, "num font-semibold text-fg")}>{t.ticker}</td>
        <td className={cx(TD, "font-semibold capitalize", sideTone(t.side))}>{t.side}</td>
        <td className={cx(TD, "num text-muted")}>
          ${t.entry.price} → ${t.exit?.price}
        </td>
        <td className={cx(TD, "num", tone)}>{fmtPct(t.pnlPct)}</td>
        <td className={cx(TD, "num text-muted")}>{t.rMultiple != null ? `${t.rMultiple}R` : "—"}</td>
        <td className={cx(TD, "num text-muted")}>{t.holdingDays ?? "—"}</td>
        <td className={TD}>
          <div className="flex items-center gap-2">
            <button
              type="button"
              onClick={(e) => { e.stopPropagation(); onToggle(); }}
              className={cx(btnSm, "border-line2 text-fg/85 hover:border-accent/60")}
            >
              {expanded ? "▲" : "▼"}
            </button>
            <button
              type="button"
              onClick={(e) => { e.stopPropagation(); onDelete(); }}
              className={cx(btnSm, "border-down/30 text-down hover:border-down/60")}
            >
              <Icon name="close" size={11} />
            </button>
          </div>
        </td>
      </tr>
      {expanded && (
        <tr>
          <td colSpan={7} className="bg-panel2/40 px-3 py-3">
            <div className="grid grid-cols-1 gap-4 md:grid-cols-[1fr_1.2fr]">
              <div>
                <div className={cx("num text-2xl font-extrabold", tone)}>
                  {fmtPct(t.pnlPct)} · {fmtMoney(t.pnlAbs)}
                </div>
                <div className="num mt-1 text-[12px] text-muted">
                  {t.side} {t.entry.size} @ ${t.entry.price} → ${t.exit?.price}
                  {t.rMultiple != null && ` · ${t.rMultiple}R`}
                  {t.holdingDays != null && ` · ${t.holdingDays}d held`}
                </div>
                {t.thesis && (
                  <p className="mt-2 text-[13px] text-fg/85">
                    <span className="text-muted">Thesis: </span>
                    {t.thesis}
                  </p>
                )}
                {t.tags?.length > 0 && (
                  <div className="mt-2 flex flex-wrap gap-1.5">
                    {t.tags.map((tg) => (
                      <Tag key={tg} tone="outline">
                        {tg}
                      </Tag>
                    ))}
                  </div>
                )}
              </div>
              <div className="border-t border-line pt-3 md:border-l md:border-t-0 md:pl-4 md:pt-0">
                <AttachedReadView read={t.attachedRead} />
              </div>
            </div>
          </td>
        </tr>
      )}
    </>
  );
}
