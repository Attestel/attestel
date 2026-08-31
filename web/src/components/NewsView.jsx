import { useMemo, useState } from "react";
import { cx } from "../lib/cx.js";
import { SkeletonText } from "./ui/index.js";
import SentimentColumn from "./news/SentimentColumn.jsx";
import DigestStrip from "./DigestStrip.jsx";

// NewsView — the "News" tab, redesigned as a dual-column attestel (design_examples/2d-news): a tagged,
// filterable feed (market headlines + SEC 8-K filings, each with a sentiment/type tag) beside the
// social-sentiment column with its graceful source-down state. Headlines link out; filings do not
// (they carry no external URL). Descriptive only — sentiment tags are context, never a signal.

const SENT_BADGE = {
  positive: "bg-up/14 text-up",
  negative: "bg-down/14 text-down",
  neutral: "bg-panel2 text-muted",
};

// "2026-07-09T17:25:00.000000Z" -> "2026-07-09T17:25:00Z"; plain dates pass through.
function fmtTime(s) {
  const str = String(s || "");
  if (str.includes("T")) return `${str.split(".")[0].replace(/Z$/, "")}Z`;
  return str;
}

function Seg({ value, onChange, items }) {
  return (
    <div className="flex gap-[2px] rounded-sm border border-line p-[3px]">
      {items.map((it) => {
        const active = it.value === value;
        return (
          <button
            key={it.value}
            type="button"
            aria-pressed={active}
            onClick={() => onChange(it.value)}
            className={cx(
              "num rounded-full px-2.5 py-1 text-[11px] font-semibold transition-colors",
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

function Item({ n, last }) {
  const filing = n.category === "filing";
  const badge = filing
    ? { cls: "bg-info/12 text-info", label: "SEC Filing" }
    : { cls: SENT_BADGE[n.sentiment] || SENT_BADGE.neutral, label: n.sentiment || "neutral" };

  const body = (
    <>
      <div className="mb-2 flex items-center gap-2.5">
        <span
          className={cx(
            "rounded px-1.5 py-0.5 label-mono",
            badge.cls
          )}
        >
          {badge.label}
        </span>
        <span className="num text-[11px] text-muted/70">
          {fmtTime(n.publishedAt)} · {n.source}
        </span>
      </div>
      <div className={cx("text-[14px] leading-[1.4]", filing ? "text-fg/80" : "text-fg")}>{n.headline}</div>
    </>
  );

  const cls = cx("block px-[22px] py-3.5 transition-colors", !last && "border-b border-line/50");

  // Filings carry no external URL; headlines link out. Only wrap linkable items in <a>.
  return n.url ? (
    <a href={n.url} target="_blank" rel="noreferrer" className={cx(cls, "hover:bg-panel2/40")}>
      {body}
    </a>
  ) : (
    <div className={cls}>{body}</div>
  );
}

export default function NewsView({ news, source, sentiment, ticker = "NVDA", loading }) {
  const [filter, setFilter] = useState("all"); // all | market | filing
  const items = news || [];

  const src = String(source || "seed");
  const live = src.includes("marketaux") || src.includes("sec8k");
  const label = src.replace("sec8k", "SEC 8-K").replace("marketaux", "Marketaux").replace("+", " + ").toUpperCase();
  const filings = items.filter((n) => n.category === "filing").length;

  const shown = useMemo(() => {
    if (filter === "market") return items.filter((n) => n.category !== "filing");
    if (filter === "filing") return items.filter((n) => n.category === "filing");
    return items;
  }, [items, filter]);

  return (
    <>
    <DigestStrip ticker={ticker} />
    <section className="overflow-hidden surface-card">
      <div className="grid grid-cols-1 lg:grid-cols-[minmax(0,1fr)_400px]">
        {/* Feed */}
        <div className="border-b border-line lg:border-b-0 lg:border-r">
          <div className="flex h-11 items-center gap-2.5 border-b border-line px-[22px]">
            <span className="text-[14px] font-semibold text-fg">News &amp; signals</span>
            {live ? (
              <span className="inline-flex items-center gap-1.5 rounded bg-llm/15 px-2 py-1 label-mono text-llm">
                <span className="h-[5px] w-[5px] rounded-full bg-accent" /> {label}
              </span>
            ) : (
              <span className="rounded border border-line2 px-1.5 py-1 label-mono text-muted">
                {label}
              </span>
            )}
            <span className="ml-auto">
              <Seg
                value={filter}
                onChange={setFilter}
                items={[
                  { value: "all", label: "All" },
                  { value: "market", label: "Market" },
                  { value: "filing", label: `8-K${filings ? ` ${filings}` : ""}` },
                ]}
              />
            </span>
          </div>

          {loading ? (
            <div className="px-[22px] py-5">
              <SkeletonText lines={8} />
            </div>
          ) : shown.length === 0 ? (
            <div className="px-[22px] py-10 text-center text-[13px] text-muted">Nothing to show for this filter.</div>
          ) : (
            shown.map((n, i) => <Item key={n.url || `${n.headline}-${i}`} n={n} last={i === shown.length - 1} />)
          )}
        </div>

        {/* Social sentiment */}
        <SentimentColumn sentiment={sentiment} ticker={ticker} />
      </div>
    </section>
    </>
  );
}
