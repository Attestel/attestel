import { useMemo, useRef } from "react";
import { Button, Tabs, SkeletonText } from "../ui/index.js";
import { Tag } from "../terminal/bits.jsx";
import { SectionShell } from "./Placeholder.jsx";
import { tabsFor } from "../../lib/routes.js";
import FundamentalsView from "../FundamentalsView.jsx";
import NewsView from "../NewsView.jsx";
import TranscriptsView from "../TranscriptsView.jsx";
import PriceChart from "../PriceChart.jsx";
import RegimeColumn from "../terminal/RegimeColumn.jsx";
import ConfluenceColumn from "../terminal/ConfluenceColumn.jsx";
import SignalBand from "../terminal/SignalBand.jsx";
import CatalystsPanel from "./CatalystsPanel.jsx";
import CaptureStrip from "../evidence/CaptureStrip.jsx";
import TranscriptQuotePicker from "../evidence/TranscriptQuotePicker.jsx";
import { catalystItems, chartItems, fundamentalsItems, newsItems } from "../evidence/captureItems.js";

// EvidenceSection — everything known about the company, organised by kind.
//
// ONE category renders at a time. That is a requirement, not a style choice: mounting Fundamentals +
// News + Transcripts + Chart + Model at once would fire every one of their fetches on section entry and
// build a very large DOM. The selected category is in the URL (#research/evidence/<category>), so an old
// deep link such as #fundamentals or #news lands on exactly the right category.
//
// Nothing here is expensive-on-mount: Fundamentals and Catalysts read the dashboard payload the shell
// already has, Chart context is pure client rendering of it, News fetches the (cached) digest only when the
// user expands it, Transcripts lists stored analyses without running one, and Model evidence loads a
// prediction record. Transcript ANALYSIS and committee runs stay explicitly invoked.
export default function EvidenceSection({
  ticker,
  tab,
  onTab,
  data,
  hasSeed,
  loading,
  timeframe,
  onTimeframe,
  onLogTrade,
}) {
  const categories = tabsFor("research", "evidence");
  const captureRef = useRef(null);

  // Savable objects for the OPEN category only. Each entry is a selector plus a preview of what is on
  // screen — never provenance, which the server stamps when it resolves the selector (see
  // components/evidence/captureItems.js). Computed per tab so an unopened category costs nothing.
  const capture = useMemo(() => {
    switch (tab) {
      case "chart":
        return chartItems(data, ticker, timeframe);
      case "fundamentals":
        return fundamentalsItems(data, ticker);
      case "news":
        return newsItems(data);
      case "catalysts":
        return catalystItems(data);
      default:
        return [];
    }
  }, [tab, data, ticker, timeframe]);

  // Events that can be honestly joined to a selected candle by calendar date. No causal claim is made:
  // the chart calls these "research context" and explicitly states when no matching record exists.
  const chartContextEvents = useMemo(() => {
    if (tab !== "chart" || !data) return [];
    const items = [];
    (data.news || []).forEach((item) => {
      if (!item?.publishedAt || !item?.headline) return;
      items.push({
        date: item.publishedAt,
        title: item.headline,
        meta: item.source || "source unknown",
        url: item.url || "",
        kind: item.category === "filing" ? "Filing" : "News",
      });
    });
    if (data.nextEarnings?.date) {
      items.push({
        date: data.nextEarnings.date,
        title: `${ticker} earnings`,
        meta: `scheduled · ${data.nextEarnings.source || "source unknown"}`,
        kind: "Earnings",
      });
    }
    (data.catalysts?.events || []).forEach((item) => {
      if (!/^\d{4}-\d{2}-\d{2}/.test(String(item?.date || "")) || !item?.title) return;
      items.push({
        date: item.date,
        title: item.title,
        meta: [item.type, item.note].filter(Boolean).join(" · "),
        kind: "Catalyst",
      });
    });
    return items;
  }, [tab, data, ticker]);

  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
        <span className="label-mono text-muted">
          {ticker} · Evidence
        </span>
        <Tabs
          items={categories.map((c) => ({ value: c.id, label: c.label }))}
          value={tab}
          onChange={onTab}
          size="sm"
          ariaLabel="Evidence categories"
        />
      </div>

      {tab === "fundamentals" && (
        <FundamentalsView data={data} ticker={ticker} hasSeed={hasSeed} loading={loading} />
      )}

      {tab === "chart" && (
        <SectionShell
          title={`${ticker} · Chart context`}
          tags={
            <>
              <Tag tone="outline">deterministic</Tag>
              {data?.priceSourceIsSynthetic && <Tag tone="warn">synthetic bars</Tag>}
              {data?.priceSource && <Tag tone="outline">{data.priceSource}</Tag>}
            </>
          }
          right={
            <Button
              variant="secondary"
              size="xs"
              disabled={capture.length === 0}
              onClick={() => captureRef.current?.scrollIntoView({ block: "center" })}
            >
              Use chart evidence ↓
            </Button>
          }
          note="Filter the visible period, toggle computed overlays, then select a candle to inspect same-day evidence. Support and resistance are server-computed — the model quotes them, it never invents them."
        >
          {data ? (
            <>
              <div className="px-[18px] py-4">
                <PriceChart
                  price={data.price}
                  indicators={data.indicators}
                  levels={data.regime?.keyLevels}
                  contextEvents={chartContextEvents}
                />
              </div>
              <div className="grid grid-cols-1 border-t border-line xl:grid-cols-2">
                <div className="border-b border-line xl:border-b-0 xl:border-r">
                  <ConfluenceColumn
                    confluence={data.confluence}
                    timeframe={timeframe}
                    onSelectTimeframe={onTimeframe}
                  />
                </div>
                <div>
                  <RegimeColumn regime={data.regime} indicators={data.indicators} />
                </div>
              </div>
            </>
          ) : (
            <div className="px-[18px] py-5">
              <SkeletonText lines={8} />
            </div>
          )}
        </SectionShell>
      )}

      {tab === "news" && (
        <NewsView
          news={data?.news}
          source={data?.newsSource}
          sentiment={data?.sentiment}
          ticker={ticker}
          loading={loading || !data}
        />
      )}

      {tab === "transcripts" && <TranscriptsView ticker={ticker} />}

      {tab === "catalysts" && (
        <CatalystsPanel ticker={ticker} catalysts={data?.catalysts} hasSeed={hasSeed} />
      )}

      {/* Model evidence — the gated quant signal WITH its walk-forward track record. SignalBand already
          refuses to show a direction unless the backtest passed, and shows hit rate / Sharpe / expectancy /
          drawdown / confidence alongside it (guide §9.4). It is one input, filed with the other evidence. */}
      {tab === "model" && (
        <div className="flex flex-col gap-3">
          <SectionShell
            title="Model evidence"
            tags={<Tag tone="info">quant model</Tag>}
            note="A directional suggestion from a walk-forward-validated model — shown only with its track record, and only when that validation passes. One piece of evidence to weigh against your thesis, never the answer, and never an order. You place every trade."
          >
            <div className="px-[18px] py-1">
              <SignalBand ticker={ticker} timeframe={timeframe} onLogTrade={onLogTrade} />
            </div>
            {/* No "save to thesis" here on purpose. A model output is a REVIEW of evidence, not
                evidence — filing it as a source-backed fact would let a model's own reasoning satisfy
                a "backed by a source" requirement it cannot satisfy (PARALLEL_CONTRACTS §2.7). */}
            <p className="border-t border-line px-[18px] py-2.5 text-[11px] leading-relaxed text-muted">
              Model output is not filed as source evidence. It is a review of the evidence — weigh it
              against your thesis, then capture the underlying facts it reasons over.
            </p>
          </SectionShell>
        </div>
      )}

      {/* Contextual capture. Placed after the category content so the user reads the object first and
          saves it second, and every row states its own source + live/seed/synthetic state before the
          save dialog opens. */}
      {tab === "transcripts" ? (
        <TranscriptQuotePicker ticker={ticker} timeframe={timeframe} />
      ) : (
        capture.length > 0 && (
          <div ref={tab === "chart" ? captureRef : null}>
            <CaptureStrip
              ticker={ticker}
              timeframe={timeframe}
              items={capture}
              hint={CAPTURE_HINTS[tab]}
              emptyNote="Nothing in this category can be captured with a source yet."
            />
          </div>
        )
      )}
    </div>
  );
}

// CAPTURE_HINTS says, per category, what is actually stored — so the promise is made before the click,
// not after. The news line matters most: it is the visible half of the rule that fetched page text is
// never persisted.
const CAPTURE_HINTS = {
  chart: "Computed values are stored with the date and timeframe they were computed for, so they never read as timeless.",
  fundamentals: "Reported figures are stored with their fiscal period and their source.",
  news: "Only the link, headline and provider metadata are stored — article text is never fetched or kept.",
  catalysts: "Dated roadmap and catalyst items from the verified reference dataset.",
};
