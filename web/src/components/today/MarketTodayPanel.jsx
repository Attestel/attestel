import { Icon } from "../shell/icons.jsx";
import { Skeleton, SkeletonText } from "../ui/index.js";
import { Micro, Tag } from "../terminal/bits.jsx";
import { deltaColor, fmtPrice, fmtSignedPct, relFromISO } from "../../lib/format.js";
import { cx } from "../../lib/cx.js";

function ModelTags({ brief }) {
  if (!brief || brief.error) return null;
  const stub = String(brief.modelUsed || "").includes("stub");
  return (
    <>
      <Tag tone={stub ? "warn" : "llm"}>{brief.modelUsed || "model"}</Tag>
      {brief.retried && <Tag tone="outline">retried</Tag>}
      {stub && <Tag tone="warn">offline read</Tag>}
    </>
  );
}

function PulseRow({ item, onOpenCompany }) {
  const change = Number(item?.changePct);
  const hasChange = item?.changePct != null && Number.isFinite(change);
  const body = (
    <>
      <div className="flex items-baseline gap-2">
        <span className="num text-[13px] font-bold text-fg">{item.ticker}</span>
        {item.last != null && <span className="num text-[11px] text-muted">{fmtPrice(item.last)}</span>}
        <span className={cx("num ml-auto text-[12.5px] font-semibold", deltaColor(item.changePct))}>
          {fmtSignedPct(item.changePct)}
        </span>
      </div>
      {item.topHeadline && (
        <p className="mt-1.5 line-clamp-2 text-[11.5px] leading-relaxed text-muted">{item.topHeadline}</p>
      )}
      <div className="mt-1.5 flex flex-wrap gap-1.5">
        {item.trend && <Tag tone="outline">{item.trend}</Tag>}
        {item.sentiment && <Tag tone="outline">crowd {item.sentiment}</Tag>}
        {item.sourceIsSynthetic && <Tag tone="warn">synthetic</Tag>}
        {!hasChange && <Tag tone="outline">price unavailable</Tag>}
      </div>
    </>
  );

  if (!onOpenCompany) return <div className="px-3.5 py-3">{body}</div>;
  return (
    <button
      type="button"
      onClick={() => onOpenCompany(item.ticker)}
      className="block w-full px-3.5 py-3 text-left transition-colors hover:bg-white/[0.045]"
    >
      {body}
    </button>
  );
}

function StoryCard({ label, items, tone }) {
  return (
    <section className="overflow-hidden surface-card">
      <div className="border-b border-line px-[18px] py-3">
        <Micro tone={tone}>{label}</Micro>
      </div>
      <ul className="divide-y divide-line">
        {items.slice(0, 3).map((item, index) => (
          <li key={`${label}-${index}`} className="px-[18px] py-3 text-[12.5px] leading-[1.55] text-fg/85">
            {item}
          </li>
        ))}
      </ul>
    </section>
  );
}

export default function MarketTodayPanel({ market, loading, onRefresh, onOpenCompany }) {
  const structured = market?.structured;
  const isStub = String(market?.modelUsed || "").includes("stub");
  const snapshot = market?.marketSnapshot || {};
  const indices = Array.isArray(snapshot.indices) ? snapshot.indices : [];
  const movers = Array.isArray(snapshot.tickers) ? snapshot.tickers : [];
  const factsAge = relFromISO(snapshot.asOf);
  const cards = [
    { label: "What moved", items: structured?.whatHappened || [], tone: "accent" },
    { label: "Why it moved", items: structured?.whyItMoved || [], tone: "llm" },
    { label: "What could change the read", items: structured?.riskFlags || [], tone: "warn" },
  ].filter((card) => Array.isArray(card.items) && card.items.length > 0);

  if (loading && !structured) {
    return (
      <>
        <section className="surface-deep p-[22px]">
          <Skeleton className="h-5 w-40" />
          <SkeletonText lines={4} className="mt-8 max-w-[760px]" />
          <div className="mt-8 grid grid-cols-2 gap-2 md:grid-cols-4">
            {Array.from({ length: 4 }).map((_, index) => (
              <Skeleton key={index} className="h-20" />
            ))}
          </div>
        </section>
      </>
    );
  }

  if (!structured) {
    return (
      <section className="surface-card px-[22px] py-8">
        <Micro tone="warn">Market read unavailable</Micro>
        <p className="mt-3 max-w-[62ch] text-[13px] leading-relaxed text-muted">
          The gateway or analysis service may be unreachable. No substitute story has been invented.
        </p>
        <button type="button" onClick={onRefresh} className="mt-4 text-[12.5px] font-semibold text-accent">
          Try again →
        </button>
      </section>
    );
  }

  return (
    <>
      <section className="overflow-hidden surface-deep">
        <div className="flex flex-wrap items-center gap-2 border-b border-line px-[22px] py-3">
          <Tag tone="accent">market today</Tag>
          <ModelTags brief={market} />
          {factsAge && <span className="num text-[11px] text-muted/70">facts {factsAge}</span>}
          <button
            type="button"
            onClick={onRefresh}
            disabled={loading}
            className="ml-auto flex items-center gap-1.5 text-[12px] text-muted transition-colors hover:text-fg disabled:opacity-50"
          >
            <Icon name="reload" size={13} className={loading ? "motion-safe:animate-spin" : ""} />
            {loading ? "Re-reading…" : "Re-read"}
          </button>
        </div>

        <div className="grid lg:grid-cols-[minmax(0,1.55fr)_minmax(290px,.7fr)]">
          <article className="px-[22px] py-7 sm:px-8 sm:py-9 lg:min-h-[390px]">
            <Micro>Lead market story</Micro>
            <h2 className="mt-4 max-w-[900px] text-[clamp(30px,4.6vw,58px)] font-[520] leading-[1.01] tracking-[-0.052em] text-fg">
              {structured.headline || "The market read has no headline."}
            </h2>
            {structured.summary && (
              <p className="mt-5 max-w-[72ch] text-[14px] leading-[1.72] text-muted">{structured.summary}</p>
            )}
            <div className="mt-7 flex flex-wrap items-center gap-x-2 gap-y-1.5 text-[10.5px] text-muted/70">
              <span>{isStub ? "Deterministic fallback from collected facts" : "AI synthesis of collected facts"}</span>
              {market.contextUsed?.length > 0 && (
                <>
                  <span aria-hidden="true">·</span>
                  <span>grounded in {market.contextUsed.join(" · ")}</span>
                </>
              )}
            </div>
          </article>

          <aside className="border-t border-line bg-panel2/25 lg:border-l lg:border-t-0">
            <div className="border-b border-line px-[18px] py-3">
              <Micro>Market pulse</Micro>
              <p className="mt-1 text-[10.5px] text-muted/70">deterministic price facts, separate from the summary</p>
            </div>
            {indices.length > 0 && (
              <div className="grid grid-cols-2 border-b border-line">
                {indices.map((item) => (
                  <div key={item.ticker} className="border-r border-line px-3.5 py-3 last:border-r-0">
                    <div className="num text-[12px] font-bold text-fg">{item.ticker}</div>
                    <div className="mt-1 flex items-baseline gap-2">
                      <span className="num text-[11px] text-muted">{fmtPrice(item.last)}</span>
                      <span className={cx("num ml-auto text-[12px] font-semibold", deltaColor(item.changePct))}>
                        {fmtSignedPct(item.changePct)}
                      </span>
                    </div>
                    {item.sourceIsSynthetic && <Tag tone="warn" className="mt-2">synthetic</Tag>}
                  </div>
                ))}
              </div>
            )}
            <div className="divide-y divide-line">
              {movers.length > 0 ? (
                movers.slice(0, 5).map((item) => (
                  <PulseRow key={item.ticker} item={item} onOpenCompany={onOpenCompany} />
                ))
              ) : (
                <p className="px-[18px] py-4 text-[12px] leading-relaxed text-muted">
                  No tracked-company facts were available. The lead story remains labelled with the facts it used.
                </p>
              )}
            </div>
          </aside>
        </div>
      </section>

      {cards.length > 0 && (
        <div
          className={cx(
            "grid grid-cols-1 gap-3",
            cards.length === 2 ? "lg:grid-cols-2" : cards.length >= 3 ? "lg:grid-cols-3" : ""
          )}
        >
          {cards.map((card) => (
            <StoryCard key={card.label} {...card} />
          ))}
        </div>
      )}

      <p className="px-1 text-[10.5px] leading-relaxed text-muted/60">
        {isStub
          ? "Deterministic fallback from collected facts — informational, not investment advice"
          : market.disclaimer || "AI-generated summary — informational, not investment advice"}
      </p>
    </>
  );
}
