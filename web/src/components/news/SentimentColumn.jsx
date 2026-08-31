import { useState } from "react";
import { fetchSentiment } from "../../lib/api.js";
import { cx } from "../../lib/cx.js";
import { Icon } from "../shell/icons.jsx";
import { Tag } from "../terminal/bits.jsx";

// SentimentColumn — StockTwits crowd color as a dense column. DESCRIPTIVE, NOT A SIGNAL (extremes can
// be contrarian). Additive + fail-silent: a quiet, graceful "unavailable" state when the (unofficial,
// rate-limited) source is down, with a self-contained Retry. Never links out to X.

const CAPTION = "crowd sentiment — not a signal · extremes can be contrarian";

function relTime(iso) {
  if (!iso) return "";
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return "";
  const s = Math.max(0, Math.floor((Date.now() - t) / 1000));
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m`;
  if (s < 86400) return `${Math.floor(s / 3600)}h`;
  return `${Math.floor(s / 86400)}d`;
}

function decode(s = "") {
  return s
    .replace(/&amp;/g, "&")
    .replace(/&#39;/g, "'")
    .replace(/&#x27;/g, "'")
    .replace(/&quot;/g, '"')
    .replace(/&lt;/g, "<")
    .replace(/&gt;/g, ">");
}

function BullBearBar({ bullish = 0, bearish = 0, tagged = 0, bullRatio }) {
  const ratio = tagged > 0 ? (bullRatio ?? bullish / tagged) : null;
  const bullPct = ratio != null ? Math.round(ratio * 100) : 50;
  return (
    <div>
      <div
        className="flex h-2.5 overflow-hidden rounded-full bg-panel2"
        role="img"
        aria-label={`${bullish} bullish, ${bearish} bearish`}
      >
        {tagged > 0 && (
          <>
            <div className="bg-up" style={{ width: `${bullPct}%` }} />
            <div className="bg-down" style={{ width: `${100 - bullPct}%` }} />
          </>
        )}
      </div>
      <div className="mt-1.5 flex items-center justify-between text-[11px]">
        <span className="num text-up">{bullish} bullish</span>
        <span className="num text-muted">{tagged} tagged</span>
        <span className="num text-down">{bearish} bearish</span>
      </div>
    </div>
  );
}

export default function SentimentColumn({ sentiment, ticker = "NVDA" }) {
  const [local, setLocal] = useState(sentiment);
  const [retrying, setRetrying] = useState(false);
  const s = local || sentiment || {};

  async function retry() {
    setRetrying(true);
    try {
      const fresh = await fetchSentiment(ticker);
      setLocal(fresh);
    } catch {
      setLocal({ available: false });
    } finally {
      setRetrying(false);
    }
  }

  const header = (
    <div className="flex h-11 items-center gap-2.5 border-b border-line px-5">
      <span className="text-[14px] font-semibold text-fg">Social sentiment</span>
      <Tag tone="info">Sentiment · StockTwits</Tag>
    </div>
  );

  if (!s.available) {
    return (
      <div className="flex flex-col">
        {header}
        <div className="border-b border-line px-5 py-3.5">
          <p className="text-[11.5px] leading-relaxed text-muted/70">{CAPTION}</p>
        </div>
        <div className="px-5 py-10 text-center">
          <div className="mx-auto mb-4 flex h-11 w-11 items-center justify-center rounded-full border border-line2">
            <svg width="20" height="20" viewBox="0 0 24 24" fill="none" aria-hidden="true">
              <path
                d="M12 8v5M12 16h.01M12 3l9 16H3l9-16z"
                stroke="currentColor"
                strokeWidth="1.8"
                strokeLinecap="round"
                strokeLinejoin="round"
                className="text-muted"
              />
            </svg>
          </div>
          <div className="mb-2 text-[14px] font-semibold text-fg/80">Social sentiment unavailable</div>
          <p className="mx-auto max-w-[280px] text-[12.5px] leading-[1.55] text-muted">
            StockTwits is unreachable or rate-limited. This is an additive source — the rest of the app is unaffected.
          </p>
          <button
            type="button"
            onClick={retry}
            disabled={retrying}
            className="mt-[18px] inline-flex h-8 items-center gap-1.5 rounded-full border border-line2 bg-panel2 px-3.5 text-[12.5px] text-fg/85 transition-colors hover:border-accent/60 disabled:opacity-50"
          >
            <Icon name="reload" size={13} className={retrying ? "motion-safe:animate-spin" : ""} />
            {retrying ? "Retrying…" : "Retry source"}
          </button>
        </div>
      </div>
    );
  }

  const { buzz, bullish, bearish, tagged, bullRatio, sentimentLabel, spike, messages } = s;
  const labelTone =
    sentimentLabel === "bullish" ? "text-up" : sentimentLabel === "bearish" ? "text-down" : "text-muted";

  return (
    <div className="flex flex-col">
      {header}
      <div className="border-b border-line px-5 py-3.5">
        <p className="text-[11.5px] leading-relaxed text-muted/70">{CAPTION}</p>
      </div>

      <div className="border-b border-line px-5 py-4">
        <div className="mb-3 flex items-center justify-between gap-2">
          <div className="flex items-baseline gap-2">
            <span className="num text-2xl font-bold leading-none text-fg">{buzz ?? "—"}</span>
            <span className="text-[11px] uppercase tracking-wide text-muted">messages</span>
            {spike && <Tag tone="warn"><span className="inline-flex items-center gap-1"><Icon name="flame" size={11} /> buzz spike</span></Tag>}
          </div>
          <span className={cx("text-sm font-semibold capitalize", labelTone)}>{sentimentLabel}</span>
        </div>
        <BullBearBar bullish={bullish} bearish={bearish} tagged={tagged} bullRatio={bullRatio} />
      </div>

      {messages?.length > 0 && (
        <div className="flex flex-col">
          {messages.map((m, i) => (
            <div key={i} className="border-b border-line/50 px-5 py-2.5">
              <div className="mb-1 flex items-center gap-2 text-[11px]">
                <span className="font-semibold text-fg">@{m.username}</span>
                {m.sentiment && (
                  <span
                    className={cx(
                      "rounded px-1.5 py-0.5 font-mono text-[10px] font-bold uppercase leading-none",
                      m.sentiment === "bullish" ? "bg-up/15 text-up" : "bg-down/15 text-down"
                    )}
                  >
                    {m.sentiment}
                  </span>
                )}
                <span className="num ml-auto text-muted">{relTime(m.at)}</span>
              </div>
              {m.url ? (
                <a
                  href={m.url}
                  target="_blank"
                  rel="noreferrer"
                  className="text-[12px] leading-snug text-fg/85 transition-colors hover:text-accent"
                >
                  {decode(m.text)}
                </a>
              ) : (
                <span className="text-[12px] leading-snug text-fg/85">{decode(m.text)}</span>
              )}
            </div>
          ))}
        </div>
      )}

      <div className="px-5 py-3.5">
        <p className="text-[10px] leading-snug text-muted/60">
          Unofficial public StockTwits endpoint — may change or rate-limit. Retail chatter is noisy; it is not investment
          advice.
        </p>
      </div>
    </div>
  );
}
