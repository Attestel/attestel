import { useEffect, useState, useCallback } from "react";
import {
  TIMEFRAMES,
  ALERT_RULE_TYPES,
  listRules,
  createRule,
  updateRule,
  deleteRule,
  listEvents,
  AuthRequiredError,
} from "../lib/api.js";
import { useToast } from "./ui/index.js";
import { useAuth } from "../auth/AuthContext.jsx";
import { Tag } from "./terminal/bits.jsx";
import { cx } from "../lib/cx.js";

// AlertsPanel — descriptive alert rules + triggered events, redesigned as one dense attestel
// (design_examples/3b-alerts): an inline rule builder over a Rules | Triggered split. The alerts
// service is a separate microservice; everything fails silent if it's unreachable. NOTHING here is a
// trade action — rules only describe a technical condition, never buy/sell advice or an order.

const RULE_LABEL = Object.fromEntries(ALERT_RULE_TYPES.map((r) => [r.type, r.label]));

const relTime = (ts) => {
  const s = Math.max(0, Math.floor(Date.now() / 1000 - ts));
  if (s < 60) return `${s}s ago`;
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
  return `${Math.floor(s / 86400)}d ago`;
};

function describeRule(r) {
  const p = r.params || {};
  switch (r.type) {
    case "price_cross": return `price crosses ${p.direction} ${p.level}`;
    case "pct_move": return `day move ≥ ${p.pct}%`;
    case "rsi_threshold": return `RSI ${p.direction} ${p.level}`;
    case "macd_cross": return `MACD flips ${p.direction}`;
    case "trend_flip": return `trend flips`;
    case "confluence_conflict": return `confluence conflict ${p.mode}`;
    case "vwap_cross": return `price crosses ${p.direction} VWAP`;
    case "new_8k": return `new SEC 8-K filing`;
    case "calendar_event": return `${p.kind === "macro" ? "high-impact macro" : "earnings"} within ${p.lead}d`;
    // Step 08's one new type. A schedule, not a market condition — described as such so it is never
    // mistaken for something the system measures.
    case "thesis_review":
      return p.everyDays
        ? `research review every ${p.everyDays}d`
        : `research review on ${r.reviewAt ? new Date(r.reviewAt * 1000).toISOString().slice(0, 10) : "a set date"}`;
    default: return r.type;
  }
}

// describeThesisLink renders the thesis context a rule carries (Step 08, §4.1). A rule with no
// thesisId renders exactly as it did before — the vast majority of existing rules.
function describeThesisLink(r) {
  if (!r.thesisId) return "";
  switch (r.intent) {
    case "invalidation": return "watching an invalidation condition";
    case "catalyst": return "watching a catalyst";
    case "assumption_review": return "reminder to review an assumption";
    case "thesis_review": return "reminder to review the thesis";
    default: return "linked to a thesis";
  }
}

// Dense form-control styling to match the attestel aesthetic.
const CTL = "h-[38px] w-full rounded-lg border border-line2 bg-panel2 px-3 text-[13px] text-fg focus:border-accent/60 focus:outline-none";
const microLabel = "mb-1.5 label-mono text-muted";

function SecHeader({ title, count, countTone = "accent" }) {
  return (
    <div className="flex h-11 items-center gap-2.5 border-b border-line px-[22px]">
      <span className="text-[14px] font-semibold text-fg">{title}</span>
      {count != null && (
        <span
          className={cx(
            "num rounded-full px-2 py-0.5 text-[12px] font-semibold leading-none",
            countTone === "accent" ? "bg-accent text-bg" : "bg-line text-muted"
          )}
        >
          {count}
        </span>
      )}
    </div>
  );
}

function Labeled({ label, className, children }) {
  return (
    <div className={className}>
      <div className={microLabel}>{label}</div>
      {children}
    </div>
  );
}

export default function AlertsPanel({ tickers = [], defaultTicker = "NVDA" }) {
  const [rules, setRules] = useState([]);
  const [events, setEvents] = useState([]);
  const [down, setDown] = useState(false);
  const [busy, setBusy] = useState(false);
  const toast = useToast();
  const { requireAuth, promptSignIn } = useAuth();

  const [ticker, setTicker] = useState(defaultTicker);
  const [timeframe, setTimeframe] = useState("1D");
  const [type, setType] = useState("price_cross");
  const [level, setLevel] = useState("");
  const [pct, setPct] = useState("");
  const [direction, setDirection] = useState("above");
  const [macdDirection, setMacdDirection] = useState("bullish");
  const [mode, setMode] = useState("appears");
  const [calKind, setCalKind] = useState("earnings");
  const [calLead, setCalLead] = useState("1");
  const [cooldownMin, setCooldownMin] = useState(60);

  useEffect(() => setTicker(defaultTicker), [defaultTicker]);

  const fields = ALERT_RULE_TYPES.find((t) => t.type === type)?.fields || [];

  const refresh = useCallback(async () => {
    try {
      const [rs, ev] = await Promise.all([listRules(), listEvents(50)]);
      setRules(rs);
      setEvents(ev.events || []);
      setDown(false);
    } catch {
      setDown(true);
    }
  }, []);

  useEffect(() => {
    refresh();
    const id = setInterval(refresh, 15000);
    return () => clearInterval(id);
  }, [refresh]);

  async function submit(e) {
    e.preventDefault();
    if (!requireAuth()) return; // guest -> routed to sign-in before creating a rule
    const params = {};
    if (fields.includes("level")) params.level = Number(level);
    if (fields.includes("pct")) params.pct = Number(pct);
    if (fields.includes("direction")) params.direction = direction;
    if (fields.includes("macdDirection")) params.direction = macdDirection;
    if (fields.includes("mode")) params.mode = mode;
    if (fields.includes("kind")) params.kind = calKind;
    if (fields.includes("lead")) params.lead = Number(calLead);
    const rule = { ticker, timeframe, type, params, cooldownSec: Math.round(Number(cooldownMin) * 60) || 3600 };
    setBusy(true);
    try {
      await createRule(rule);
      setLevel("");
      setPct("");
      await refresh();
      toast({ tone: "success", title: "Rule added", message: `${ticker} · ${describeRule({ type, params })}` });
    } catch (err) {
      if (err instanceof AuthRequiredError) { promptSignIn(); return; }
      toast({ tone: "error", title: "Could not add rule", message: err.message });
    } finally {
      setBusy(false);
    }
  }

  async function toggle(r) {
    if (!requireAuth()) return;
    try {
      await updateRule(r.id, { active: !r.active });
      await refresh();
    } catch (err) {
      if (err instanceof AuthRequiredError) promptSignIn();
    }
  }
  async function remove(id) {
    if (!requireAuth()) return;
    try {
      await deleteRule(id);
      await refresh();
      toast({ tone: "info", title: "Rule deleted" });
    } catch (err) {
      if (err instanceof AuthRequiredError) promptSignIn();
    }
  }

  const baseTickerOpts = tickers.length ? tickers : [{ symbol: defaultTicker, name: defaultTicker }];
  const tickerOpts = baseTickerOpts.some((item) => item.symbol === ticker)
    ? baseTickerOpts
    : [{ symbol: ticker, name: ticker }, ...baseTickerOpts];

  if (down) {
    return (
      <section className="overflow-hidden surface-card">
        <div className="flex h-11 items-center gap-2.5 border-b border-line px-[22px]">
          <span className="text-[14px] font-semibold text-fg">Alerts</span>
          <Tag tone="warn">service down</Tag>
        </div>
        <p className="px-[22px] py-5 text-[13px] leading-relaxed text-muted">
          The alerts service (<code className="rounded bg-panel2 px-1">:8095</code>) is unreachable. Start it with{" "}
          <code className="rounded bg-panel2 px-1">cd alerts &amp;&amp; go run .</code> — the rest of the dashboard is
          unaffected.
        </p>
      </section>
    );
  }

  return (
    <section className="overflow-hidden surface-card">
      {/* New alert rule */}
      <div className="flex h-11 items-center gap-2.5 border-b border-line px-[22px]">
        <span className="text-[14px] font-semibold text-fg">New alert rule</span>
        <Tag tone="outline">Descriptive · No trading</Tag>
      </div>

      <div className="border-b border-line px-[22px] py-4">
        <p className="mb-4 max-w-[900px] text-[12.5px] leading-[1.6] text-muted">
          Alerts are neutral notifications of a technical condition — never buy/sell advice, never an order. They are most
          meaningful on <strong className="text-fg/85">real data</strong> (with API keys); on synthetic data they still
          evaluate and can fire, which is handy for testing.
        </p>
        <form className="flex flex-wrap items-end gap-3" onSubmit={submit}>
          <Labeled label="Ticker" className="w-[120px]">
            <select value={ticker} onChange={(e) => setTicker(e.target.value)} className={CTL}>
              {tickerOpts.map((t) => (
                <option key={t.symbol} value={t.symbol}>
                  {t.symbol}
                </option>
              ))}
            </select>
          </Labeled>
          <Labeled label="Timeframe" className="w-[100px]">
            <select value={timeframe} onChange={(e) => setTimeframe(e.target.value)} className={CTL}>
              {TIMEFRAMES.map((tf) => (
                <option key={tf} value={tf}>
                  {tf}
                </option>
              ))}
            </select>
          </Labeled>
          <Labeled label="Condition" className="min-w-[200px] flex-1">
            <select value={type} onChange={(e) => setType(e.target.value)} className={CTL}>
              {ALERT_RULE_TYPES.map((t) => (
                <option key={t.type} value={t.type}>
                  {t.label}
                </option>
              ))}
            </select>
          </Labeled>
          {fields.includes("level") && (
            <Labeled label="Level" className="w-[120px]">
              <input
                type="number"
                step="any"
                required
                value={level}
                onChange={(e) => setLevel(e.target.value)}
                placeholder="e.g. 150"
                className={cx(CTL, "num placeholder:text-muted/50")}
              />
            </Labeled>
          )}
          {fields.includes("pct") && (
            <Labeled label="% move" className="w-[120px]">
              <input
                type="number"
                step="any"
                required
                value={pct}
                onChange={(e) => setPct(e.target.value)}
                placeholder="e.g. 5"
                className={cx(CTL, "num placeholder:text-muted/50")}
              />
            </Labeled>
          )}
          {fields.includes("direction") && (
            <Labeled label="Direction" className="w-[120px]">
              <select value={direction} onChange={(e) => setDirection(e.target.value)} className={CTL}>
                <option value="above">above</option>
                <option value="below">below</option>
              </select>
            </Labeled>
          )}
          {fields.includes("macdDirection") && (
            <Labeled label="Flip to" className="w-[120px]">
              <select value={macdDirection} onChange={(e) => setMacdDirection(e.target.value)} className={CTL}>
                <option value="bullish">bullish</option>
                <option value="bearish">bearish</option>
              </select>
            </Labeled>
          )}
          {fields.includes("mode") && (
            <Labeled label="Mode" className="w-[120px]">
              <select value={mode} onChange={(e) => setMode(e.target.value)} className={CTL}>
                <option value="appears">appears</option>
                <option value="resolves">resolves</option>
              </select>
            </Labeled>
          )}
          {fields.includes("kind") && (
            <Labeled label="Event" className="w-[130px]">
              <select value={calKind} onChange={(e) => setCalKind(e.target.value)} className={CTL}>
                <option value="earnings">earnings</option>
                <option value="macro">macro (high)</option>
              </select>
            </Labeled>
          )}
          {fields.includes("lead") && (
            <Labeled label="Lead (days)" className="w-[110px]">
              <input
                type="number"
                min="0"
                step="1"
                value={calLead}
                onChange={(e) => setCalLead(e.target.value)}
                className={cx(CTL, "num")}
              />
            </Labeled>
          )}
          <Labeled label="Cooldown (min)" className="w-[110px]">
            <input
              type="number"
              min="0"
              step="1"
              value={cooldownMin}
              onChange={(e) => setCooldownMin(e.target.value)}
              className={cx(CTL, "num")}
            />
          </Labeled>
          <button
            type="submit"
            disabled={busy}
            className="pill-lift h-[38px] shrink-0 rounded-full bg-fg px-5 text-[12.5px] font-semibold text-bg transition-[transform,background-color,box-shadow] duration-200 ease-premium hover:-translate-y-px hover:bg-white hover:shadow-glow disabled:pointer-events-none disabled:opacity-50"
          >
            {busy ? "Adding…" : "Add rule"}
          </button>
        </form>
      </div>

      {/* Rules | Triggered */}
      <div className="grid grid-cols-1 lg:grid-cols-2">
        {/* Rules */}
        <div className="border-b border-line lg:border-b-0 lg:border-r">
          <SecHeader title="Rules" count={rules.length} countTone="accent" />
          {rules.length === 0 ? (
            <div className="px-[22px] py-10 text-center">
              <div className="mb-1.5 text-[14px] font-semibold text-fg/80">No rules yet</div>
              <p className="text-[12.5px] text-muted">Add a condition above to get notified when it triggers.</p>
            </div>
          ) : (
            rules.map((r, i) => (
              <div
                key={r.id}
                className={cx(
                  "px-[22px] py-4",
                  i < rules.length - 1 && "border-b border-line/50",
                  !r.active && "opacity-50"
                )}
              >
                <div className="mb-1.5 flex flex-wrap items-center gap-2.5">
                  <span className="num text-[15px] font-bold text-fg">{r.ticker}</span>
                  <span className="num rounded-full border border-line2 px-2 py-0.5 text-[10px] text-muted">
                    {r.timeframe}
                  </span>
                  <span className="text-[13px] text-fg/85">{describeRule(r)}</span>
                  {r.thesisId && (
                    <span className="rounded-full border border-line2 px-2 py-0.5 text-[10px] text-muted">
                      {describeThesisLink(r)}
                    </span>
                  )}
                </div>
                <div className="mb-3.5 text-[12px] text-muted">
                  {RULE_LABEL[r.type] || r.type} · cooldown {Math.round(r.cooldownSec / 60)}m
                  {r.lastTriggered > 0 && <> · last fired {relTime(r.lastTriggered)}</>}
                </div>
                <div className="flex gap-2.5">
                  <button
                    type="button"
                    onClick={() => toggle(r)}
                    className="rounded-full border border-line2 px-3.5 py-1.5 text-[12.5px] text-fg/85 transition-colors hover:border-accent/60"
                  >
                    {r.active ? "Pause" : "Resume"}
                  </button>
                  <button
                    type="button"
                    onClick={() => remove(r.id)}
                    className="rounded-full border border-down/30 px-3.5 py-1.5 text-[12.5px] text-down transition-colors hover:border-down/60"
                  >
                    Delete
                  </button>
                </div>
              </div>
            ))
          )}
        </div>

        {/* Triggered events */}
        <div>
          <SecHeader title="Triggered events" count={events.length} countTone={events.length ? "accent" : "muted"} />
          {events.length === 0 ? (
            <div className="px-[22px] py-10 text-center">
              <div className="mx-auto mb-3.5 flex h-10 w-10 items-center justify-center rounded-full border border-line2">
                <svg width="18" height="18" viewBox="0 0 24 24" fill="none" aria-hidden="true">
                  <path
                    d="M6 9a6 6 0 1112 0c0 5 2 6 2 6H4s2-1 2-6zM10 21h4"
                    stroke="currentColor"
                    strokeWidth="1.8"
                    strokeLinecap="round"
                    strokeLinejoin="round"
                    className="text-muted"
                  />
                </svg>
              </div>
              <div className="mb-1.5 text-[14px] font-semibold text-fg/80">Nothing has fired yet</div>
              <p className="text-[12.5px] text-muted">Triggered alerts will appear here.</p>
            </div>
          ) : (
            events.map((ev, i) => (
              <div
                key={ev.id}
                className={cx(
                  "px-[22px] py-3",
                  i < events.length - 1 && "border-b border-line/50",
                  !ev.read && "border-l-2 border-l-accent bg-accent/[0.04]"
                )}
              >
                <div className="text-[13px] leading-snug text-fg/90">{ev.message}</div>
                <div className="num mt-1 text-[11px] text-muted/70">
                  {ev.ticker} · {ev.timeframe} · {ev.type} · {relTime(ev.ts)}
                </div>
              </div>
            ))
          )}
        </div>
      </div>
    </section>
  );
}
