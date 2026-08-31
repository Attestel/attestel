import { memo, useEffect, useRef, useState } from "react";
import { createChart, CandlestickSeries, LineSeries } from "lightweight-charts";
import { cx } from "../lib/cx.js";
import { fmtVol } from "../lib/format.js";

// Interactive research chart. All numbers and levels are computed server-side; the client controls
// only which already-computed evidence is visible. A candle can be pinned to reveal same-day news,
// filings, earnings and curated catalysts supplied by the parent research view.

const MA = {
  ema20: { color: "#E0703C", label: "EMA20" },
  sma50: { color: "#5B6CFF", label: "SMA50" },
  sma200: { color: "#A78BE0", label: "SMA200" },
};

const CHART_UP = "#5BC79A";
const CHART_DOWN = "#E5484D";
const RANGE_OPTIONS = [
  { value: "1m", label: "1M", days: 31 },
  { value: "3m", label: "3M", days: 93 },
  { value: "6m", label: "6M", days: 186 },
  { value: "1y", label: "1Y", days: 366 },
  { value: "all", label: "All", days: null },
];

function fmtChartTime(t) {
  if (t == null) return "—";
  if (typeof t === "number") {
    const d = new Date(t * 1000);
    return d.toLocaleString("en-US", {
      month: "short",
      day: "numeric",
      hour: "2-digit",
      minute: "2-digit",
    });
  }
  if (typeof t === "object" && t.year && t.month && t.day) {
    return `${t.year}-${String(t.month).padStart(2, "0")}-${String(t.day).padStart(2, "0")}`;
  }
  return String(t);
}

function timeMs(t) {
  if (typeof t === "number") return t * 1000;
  if (typeof t === "object" && t?.year && t?.month && t?.day) {
    return Date.UTC(t.year, t.month - 1, t.day);
  }
  const parsed = Date.parse(String(t || ""));
  return Number.isNaN(parsed) ? null : parsed;
}

function dayKey(t) {
  const ms = timeMs(t);
  if (ms == null) return "";
  return new Date(ms).toISOString().slice(0, 10);
}

function timeKey(t) {
  if (typeof t === "number") return `n:${t}`;
  if (typeof t === "object") return `d:${t?.year}-${t?.month}-${t?.day}`;
  return `s:${String(t)}`;
}

function barsForRange(bars, range) {
  if (!Array.isArray(bars) || range === "all") return bars || [];
  const option = RANGE_OPTIONS.find((item) => item.value === range);
  const latest = timeMs(bars[bars.length - 1]?.time);
  if (!option?.days || latest == null) return bars;
  const cutoff = latest - option.days * 86400000;
  const filtered = bars.filter((bar) => (timeMs(bar.time) ?? latest) >= cutoff);
  return filtered.length >= 2 ? filtered : bars.slice(-Math.min(60, bars.length));
}

function activeMAs(maKeys) {
  if (!maKeys) return MA;
  return Object.fromEntries(Object.entries(MA).filter(([key]) => maKeys.includes(key)));
}

function Toggle({ active, color, children, onClick, title }) {
  return (
    <button
      type="button"
      aria-pressed={active}
      title={title}
      onClick={onClick}
      className={cx(
        "flex items-center gap-1.5 rounded-full border px-2.5 py-1 text-[10.5px] font-semibold transition-colors",
        active ? "border-line2 bg-white/[0.07] text-fg" : "border-line bg-transparent text-muted/60"
      )}
    >
      {color && <i className="h-1.5 w-1.5 rounded-full" style={{ background: color }} />}
      {children}
    </button>
  );
}

function PriceChartBase({
  price,
  indicators,
  levels,
  height = 400,
  maKeys = null,
  contextEvents = null,
}) {
  const wrapRef = useRef(null);
  const configuredMA = activeMAs(maKeys);
  const maSignature = Object.keys(configuredMA).join(",");
  const [range, setRange] = useState("6m");
  const [enabledMAs, setEnabledMAs] = useState(() => Object.keys(configuredMA));
  const [showLevels, setShowLevels] = useState(true);
  const [readout, setReadout] = useState(null);
  const [selected, setSelected] = useState(null);

  useEffect(() => {
    setEnabledMAs(maSignature ? maSignature.split(",") : []);
  }, [maSignature]);

  const sourceBars = price?.ohlcv || [];
  const visibleBars = barsForRange(sourceBars, range);
  const visibleTimes = new Set(visibleBars.map((bar) => timeKey(bar.time)));
  const shownMA = Object.fromEntries(
    Object.entries(configuredMA).filter(([key]) => enabledMAs.includes(key))
  );
  const enabledKey = enabledMAs.join(",");

  useEffect(() => {
    if (!wrapRef.current || !visibleBars.length) return;

    const chart = createChart(wrapRef.current, {
      autoSize: true,
      layout: {
        background: { color: "transparent" },
        textColor: "#9A9CA3",
        fontFamily: "ui-monospace, SFMono-Regular, Menlo, monospace",
        fontSize: 11,
      },
      grid: {
        vertLines: { color: "rgba(255,255,255,0.06)" },
        horzLines: { color: "rgba(255,255,255,0.06)" },
      },
      rightPriceScale: { borderColor: "rgba(255,255,255,0.08)" },
      timeScale: { borderColor: "rgba(255,255,255,0.08)", rightOffset: 4 },
      crosshair: {
        mode: 0,
        vertLine: { color: "rgba(91,108,255,0.4)", labelBackgroundColor: "#14161B" },
        horzLine: { color: "rgba(91,108,255,0.4)", labelBackgroundColor: "#14161B" },
      },
    });

    const candles = chart.addSeries(CandlestickSeries, {
      upColor: CHART_UP,
      downColor: CHART_DOWN,
      wickUpColor: CHART_UP,
      wickDownColor: CHART_DOWN,
      borderVisible: false,
    });
    candles.setData(visibleBars);

    const seriesMap = {};
    for (const [key, { color }] of Object.entries(shownMA)) {
      const series = indicators?.series?.[key];
      if (!series?.length) continue;
      const line = chart.addSeries(LineSeries, {
        color,
        lineWidth: 1.5,
        priceLineVisible: false,
        lastValueVisible: false,
        crosshairMarkerVisible: false,
      });
      line.setData(series.filter((point) => point.value != null && visibleTimes.has(timeKey(point.time))));
      seriesMap[key] = line;
    }

    if (showLevels) {
      (levels?.support || []).forEach((level) =>
        candles.createPriceLine({
          price: Number(level),
          color: CHART_UP,
          lineWidth: 1,
          lineStyle: 2,
          axisLabelVisible: true,
          title: `S ${level}`,
        })
      );
      (levels?.resistance || []).forEach((level) =>
        candles.createPriceLine({
          price: Number(level),
          color: CHART_DOWN,
          lineWidth: 1,
          lineStyle: 2,
          axisLabelVisible: true,
          title: `R ${level}`,
        })
      );
    }

    const pointFrom = (param) => {
      if (!param.time) return null;
      const candle = param.seriesData.get(candles);
      if (!candle) return null;
      const mas = {};
      for (const [key, series] of Object.entries(seriesMap)) {
        const datum = param.seriesData.get(series);
        if (datum && datum.value != null) mas[key] = datum.value;
      }
      return { ...candle, mas, time: param.time };
    };

    const onMove = (param) => {
      if (!param.time || !param.point) {
        setReadout(null);
        return;
      }
      setReadout(pointFrom(param));
    };
    const onClick = (param) => {
      const point = pointFrom(param);
      if (point) setSelected(point);
    };
    chart.subscribeCrosshairMove(onMove);
    chart.subscribeClick(onClick);
    chart.timeScale().fitContent();

    return () => {
      chart.unsubscribeCrosshairMove(onMove);
      chart.unsubscribeClick(onClick);
      chart.remove();
    };
  }, [price, indicators, levels, range, enabledKey, showLevels]); // eslint-disable-line react-hooks/exhaustive-deps

  if (!sourceBars.length) {
    return (
      <div
        style={{ height }}
        className="flex items-center justify-center rounded-lg border border-dashed border-line text-sm text-muted"
      >
        No price data to chart.
      </div>
    );
  }

  const last = visibleBars[visibleBars.length - 1];
  const candle = readout || selected || last;
  const change = candle ? ((candle.close / candle.open - 1) * 100).toFixed(2) : null;
  const latestMAs = indicators?.latest || {};
  const selectedDay = dayKey(selected?.time);
  const relatedEvents = selectedDay && Array.isArray(contextEvents)
    ? contextEvents.filter((event) => dayKey(event.date) === selectedDay)
    : [];

  const maValue = (key) => {
    if (readout) return readout.mas?.[key];
    if (selected) return selected.mas?.[key];
    const series = indicators?.series?.[key];
    const value = series?.length ? series[series.length - 1]?.value : null;
    return value ?? latestMAs[key];
  };

  const chooseRange = (value) => {
    setRange(value);
    setSelected(null);
    setReadout(null);
  };
  const toggleMA = (key) =>
    setEnabledMAs((current) =>
      current.includes(key) ? current.filter((item) => item !== key) : [...current, key]
    );

  return (
    <div className="overflow-hidden rounded-xl border border-line bg-bg/20">
      <div className="flex flex-wrap items-center gap-2 border-b border-line bg-panel2/35 px-3 py-2.5">
        <div className="flex items-center rounded-full border border-line bg-bg/30 p-[2px]" aria-label="Chart range">
          {RANGE_OPTIONS.map((option) => (
            <button
              key={option.value}
              type="button"
              aria-pressed={range === option.value}
              onClick={() => chooseRange(option.value)}
              className={cx(
                "rounded-full px-2.5 py-1 text-[10.5px] font-semibold transition-colors",
                range === option.value ? "bg-accent/15 text-accent" : "text-muted hover:text-fg"
              )}
            >
              {option.label}
            </button>
          ))}
        </div>
        <span className="hidden h-4 w-px bg-line sm:block" />
        <div className="flex flex-wrap items-center gap-1.5">
          {Object.entries(configuredMA).map(([key, item]) => (
            <Toggle
              key={key}
              active={enabledMAs.includes(key)}
              color={item.color}
              onClick={() => toggleMA(key)}
              title={`Show or hide ${item.label}`}
            >
              {item.label}
            </Toggle>
          ))}
          <Toggle
            active={showLevels}
            color={showLevels ? CHART_UP : null}
            onClick={() => setShowLevels((value) => !value)}
            title="Show or hide computed support and resistance"
          >
            S/R levels
          </Toggle>
        </div>
        <span className="ml-auto text-[10.5px] text-muted/60">drag to pan · scroll to zoom · click to pin</span>
      </div>

      <div className="relative">
        <div className="pointer-events-none absolute left-2 top-2 z-10 flex max-w-[calc(100%-1rem)] flex-col gap-1 text-[11px]">
          <div className="num flex flex-wrap items-center gap-x-3 gap-y-0.5 rounded-md bg-bg/80 px-2 py-1 backdrop-blur-sm">
            <span className="text-muted">{fmtChartTime(candle?.time)}</span>
            <span>O <span className="text-fg">{candle?.open}</span></span>
            <span>H <span className="text-fg">{candle?.high}</span></span>
            <span>L <span className="text-fg">{candle?.low}</span></span>
            <span>C <span className="text-fg">{candle?.close}</span></span>
            <span className={Number(change) >= 0 ? "text-up" : "text-down"}>
              {Number(change) >= 0 ? "+" : ""}{change}%
            </span>
            {candle?.volume != null && (
              <span className="text-muted">Vol <span className="text-fg">{fmtVol(candle.volume)}</span></span>
            )}
          </div>
          {Object.keys(shownMA).length > 0 && (
            <div className="flex flex-wrap items-center gap-x-3 gap-y-0.5 rounded-md bg-bg/80 px-2 py-1 backdrop-blur-sm">
              {Object.entries(shownMA).map(([key, { color, label }]) => {
                const value = maValue(key);
                return (
                  <span key={key} className="flex items-center gap-1">
                    <i style={{ background: color }} className="inline-block h-[3px] w-3.5 rounded-full" />
                    <span className="text-muted">{label}</span>
                    <span className="num text-fg">{value != null ? Number(value).toFixed(2) : "—"}</span>
                  </span>
                );
              })}
            </div>
          )}
        </div>
        <div ref={wrapRef} style={{ height }} className="w-full" />
      </div>

      {Array.isArray(contextEvents) && (
        <div className="border-t border-line bg-panel2/25 px-3.5 py-3">
          {!selected ? (
            <p className="text-[11.5px] leading-relaxed text-muted">
              Select a candle to inspect news, filings, earnings and curated catalysts recorded on that date.
            </p>
          ) : (
            <div>
              <div className="flex flex-wrap items-center gap-2">
                <span className="text-[11.5px] font-semibold text-fg">Research context · {selectedDay}</span>
                <button
                  type="button"
                  onClick={() => setSelected(null)}
                  className="ml-auto text-[10.5px] text-muted transition-colors hover:text-fg"
                >
                  Clear selection
                </button>
              </div>
              {relatedEvents.length > 0 ? (
                <ul className="mt-2 divide-y divide-line rounded-lg border border-line bg-bg/25">
                  {relatedEvents.map((event, index) => (
                    <li key={`${event.kind}-${event.title}-${index}`} className="flex items-start gap-2.5 px-3 py-2.5">
                      <span className="mt-0.5 rounded-full border border-line2 px-2 py-0.5 label-mono text-muted">
                        {event.kind}
                      </span>
                      <div className="min-w-0 flex-1">
                        {event.url ? (
                          <a
                            href={event.url}
                            target="_blank"
                            rel="noopener noreferrer"
                            className="text-[12px] leading-snug text-fg/90 transition-colors hover:text-accent"
                          >
                            {event.title}
                          </a>
                        ) : (
                          <p className="text-[12px] leading-snug text-fg/90">{event.title}</p>
                        )}
                        {event.meta && <p className="mt-0.5 text-[10.5px] text-muted/70">{event.meta}</p>}
                      </div>
                    </li>
                  ))}
                </ul>
              ) : (
                <p className="mt-2 text-[11.5px] leading-relaxed text-muted">
                  No collected event matches this date. The price move is visible; its cause is not asserted.
                </p>
              )}
            </div>
          )}
        </div>
      )}
    </div>
  );
}

export const PriceChart = memo(PriceChartBase);
export default PriceChart;
