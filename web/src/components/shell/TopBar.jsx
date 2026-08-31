import { useEffect, useRef, useState } from "react";
import { createPortal } from "react-dom";
import { Tabs, Badge, Button, HealthCluster, NumberFlash, AttestelWordmark } from "../ui/index.js";
import { Icon } from "./icons.jsx";
import { AccountMenu } from "./AccountMenu.jsx";
import { cx } from "../../lib/cx.js";
import { fmtPrice, fmtSignedPct } from "../../lib/format.js";
import { searchTickers } from "../../lib/api.js";

// TopBar — persistent header: brand · ticker switcher · timeframe toggle · (right) state chips ·
// live quote (flashes on update) · service-health cluster · reload · alerts bell.
export function TopBar({
  tickers,
  ticker,
  onTicker,
  timeframe,
  onTimeframe,
  timeframes,
  quote,
  price,
  changePct,
  quoteLive,
  health,
  synthetic,
  paper,
  unread,
  onBell,
  bellActive,
  onReload,
  loading,
  onCopilot,
  copilotActive,
  signedIn,
}) {
  const p = quote?.price ?? price;
  const chg = quote?.changePct ?? changePct;
  const tfItems = timeframes.map((t) => ({ value: t, label: t }));

  return (
    <header
      className="sticky top-0 z-40 flex h-14 items-center gap-2 border-b border-line bg-bg/95 px-3 shadow-nav backdrop-blur-[24px] backdrop-saturate-150 supports-[backdrop-filter]:bg-[rgba(20,22,27,.55)] sm:gap-3 sm:px-4"
    >
      {/* Brand — the wordmark stands alone, exactly as in the landing nav. There is no second
          mark in this brand. */}
      <div className="flex items-center">
        <AttestelWordmark height={16} className="hidden text-fg sm:block" />
      </div>

      <TickerPicker
        tickers={tickers}
        ticker={ticker}
        onTicker={onTicker}
        signedIn={signedIn}
      />

      {/* Timeframe — segmented Tabs from xs up; a compact <select> below xs so phones can still change
          it (the Tabs would otherwise be hidden with no replacement). */}
      <Tabs
        items={tfItems}
        value={timeframe}
        onChange={onTimeframe}
        size="sm"
        ariaLabel="Timeframe"
        className="hidden xs:inline-flex"
      />
      <select
        aria-label="Timeframe"
        value={timeframe}
        onChange={(e) => onTimeframe(e.target.value)}
        className="num shrink-0 rounded-full border border-line2 bg-panel2 px-2.5 py-1.5 text-sm font-semibold text-fg focus:outline-none xs:hidden"
      >
        {timeframes.map((t) => (
          <option key={t} value={t}>
            {t}
          </option>
        ))}
      </select>

      {/* Right cluster (shrink-0 so the ticker absorbs any tightness, never these controls) */}
      <div className="ml-auto flex shrink-0 items-center gap-2 sm:gap-3">
        {synthetic && (
          <Badge variant="warn" className="hidden sm:inline-flex" title="Price feed is synthetic seed data">
            <Icon name="warning" size={11} /> Synthetic
          </Badge>
        )}
        {paper && (
          <Badge variant="warn" className="hidden sm:inline-flex" title="Paper view — simulated, no real money">
            <Icon name="flask" size={11} /> Paper
          </Badge>
        )}

        {/* Live quote */}
        <div className="flex items-center gap-1.5" aria-live="polite">
          <span
            className={cx(
              "h-1.5 w-1.5 rounded-full",
              quoteLive
                ? "bg-accent motion-safe:animate-[pulse-live_1.4s_ease-in-out_infinite]"
                : "bg-muted/50"
            )}
            title={quoteLive ? "live quote" : "no live quote"}
            aria-hidden="true"
          />
          <NumberFlash
            value={p}
            format={(v) => fmtPrice(v)}
            className="text-[15px] font-semibold text-fg"
          />
          <span
            className={cx(
              // Secondary vs the price — hidden below xs so the header fits at 360px; returns at >=xs
              // alongside the timeframe Tabs. The price itself always stays.
              "num hidden rounded-full px-2 py-0.5 text-xs font-semibold xs:inline-block",
              chg == null
                ? "text-muted"
                : chg >= 0
                  ? "bg-up/12 text-up"
                  : "bg-down/12 text-down"
            )}
          >
            {fmtSignedPct(chg, 2)}
          </span>
        </div>

        {health?.length > 0 && (
          <HealthCluster items={health} className="hidden rounded-full border border-line bg-panel2 px-2.5 py-1.5 md:flex" />
        )}

        {onCopilot && (
          <button
            onClick={onCopilot}
            aria-label="Open Copilot"
            title="Copilot (press c)"
            className={cx(
              // Hidden on phones — the floating CopilotLauncher (bottom-right) is the mobile entry point,
              // so this reclaims header width without losing access.
              "hidden h-9 items-center gap-1.5 rounded-full border px-3.5 text-[12.5px] font-semibold transition-colors sm:flex",
              copilotActive
                ? "border-accent/60 bg-accent/12 text-accent"
                : "border-line2 bg-panel2 text-muted hover:border-white/34 hover:text-fg"
            )}
          >
            <Icon name="copilot" size={16} />
            <span className="hidden lg:inline">Copilot</span>
          </button>
        )}

        <button
          onClick={onReload}
          disabled={loading}
          aria-label="Reload dashboard"
          title="Reload"
          className="flex h-9 w-9 items-center justify-center rounded-full border border-line2 bg-panel2 text-muted transition-colors hover:border-white/34 hover:text-fg disabled:opacity-50"
        >
          <Icon name="reload" size={16} className={loading ? "motion-safe:animate-spin" : ""} />
        </button>

        <button
          onClick={onBell}
          aria-label={`Alerts${unread ? ` (${unread} unread)` : ""}`}
          title="Alerts"
          className={cx(
            "relative flex h-9 w-9 items-center justify-center rounded-full border transition-colors",
            bellActive
              ? "border-accent/60 bg-accent/12 text-accent"
              : "border-line2 bg-panel2 text-muted hover:border-white/34 hover:text-fg"
          )}
        >
          <Icon name="bell" size={17} />
          {unread > 0 && !bellActive && (
            <span className="absolute -right-1.5 -top-1.5 flex h-4 min-w-4 items-center justify-center rounded-full bg-down px-1 text-[9px] font-bold text-white">
              {unread > 99 ? "99+" : unread}
            </span>
          )}
        </button>

        {/* Account: guest -> "Sign in"; signed in -> avatar menu with "Sign out". */}
        <AccountMenu />
      </div>
    </header>
  );
}

function TickerPicker({ tickers, ticker, onTicker, signedIn }) {
  const rootRef = useRef(null);
  const inputRef = useRef(null);
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const [results, setResults] = useState([]);
  const [searching, setSearching] = useState(false);
  const [searchError, setSearchError] = useState("");
  const [activeIndex, setActiveIndex] = useState(0);
  const [resolvedActive, setResolvedActive] = useState(null);
  const [pending, setPending] = useState(null);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState("");

  const configured = tickers.find((item) => item.symbol === ticker);
  const active = configured || resolvedActive || { symbol: ticker, name: ticker };

  useEffect(() => {
    if (!ticker || configured) {
      setResolvedActive(configured || null);
      return;
    }
    const ctrl = new AbortController();
    searchTickers(ticker, { signal: ctrl.signal })
      .then((items) => {
        const exact = items.find((item) => item.symbol === ticker);
        setResolvedActive(exact || { symbol: ticker, name: ticker });
      })
      .catch((err) => {
        if (err.name !== "AbortError") setResolvedActive({ symbol: ticker, name: ticker });
      });
    return () => ctrl.abort();
  }, [ticker, configured]);

  useEffect(() => {
    if (!open) return;
    const ctrl = new AbortController();
    const delay = query.trim() ? 220 : 0;
    const timer = window.setTimeout(() => {
      setSearching(true);
      setSearchError("");
      searchTickers(query.trim(), { signal: ctrl.signal })
        .then((items) => {
          setResults(items);
          setActiveIndex(0);
        })
        .catch((err) => {
          if (err.name !== "AbortError") {
            setResults([]);
            setSearchError("Company search is unavailable.");
          }
        })
        .finally(() => {
          if (!ctrl.signal.aborted) setSearching(false);
        });
    }, delay);
    return () => {
      window.clearTimeout(timer);
      ctrl.abort();
    };
  }, [open, query]);

  useEffect(() => {
    if (!open) return;
    const onPointerDown = (event) => {
      if (!rootRef.current?.contains(event.target)) setOpen(false);
    };
    document.addEventListener("pointerdown", onPointerDown);
    const timer = window.setTimeout(() => inputRef.current?.focus(), 0);
    return () => {
      document.removeEventListener("pointerdown", onPointerDown);
      window.clearTimeout(timer);
    };
  }, [open]);

  useEffect(() => {
    if (!pending) return;
    const onKey = (event) => {
      if (event.key === "Escape" && !saving) setPending(null);
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [pending, saving]);

  const choose = (company) => {
    setOpen(false);
    setSaveError("");
    setPending(company);
  };

  const confirm = async () => {
    setSaving(true);
    setSaveError("");
    try {
      const changed = await onTicker(pending);
      if (changed !== false) {
        setResolvedActive(pending);
        setPending(null);
        setQuery("");
      }
    } catch (err) {
      setSaveError(err?.message || "The active company could not be saved.");
    } finally {
      setSaving(false);
    }
  };

  const onInputKeyDown = (event) => {
    if (event.key === "Escape") {
      setOpen(false);
      return;
    }
    if (event.key === "ArrowDown") {
      event.preventDefault();
      setActiveIndex((index) => Math.min(index + 1, Math.max(0, results.length - 1)));
    } else if (event.key === "ArrowUp") {
      event.preventDefault();
      setActiveIndex((index) => Math.max(0, index - 1));
    } else if (event.key === "Enter" && results[activeIndex]) {
      event.preventDefault();
      choose(results[activeIndex]);
    }
  };

  return (
    <div ref={rootRef} className="relative min-w-0">
      <button
        type="button"
        aria-haspopup="listbox"
        aria-expanded={open}
        onClick={() => {
          setQuery("");
          setOpen((value) => !value);
        }}
        className="num flex min-w-0 max-w-[42vw] items-center gap-1.5 rounded-full border border-line2 bg-panel2 px-3 py-1.5 text-left text-sm font-semibold text-fg transition-colors hover:border-accent/60 focus:outline-none focus-visible:border-accent sm:max-w-[260px]"
      >
        <span className="truncate">{active.symbol} · {active.name}</span>
        <span className="shrink-0 text-[10px] text-muted" aria-hidden="true">▼</span>
      </button>

      {open && (
        <div className="absolute left-0 top-full z-[70] mt-2 w-[min(360px,calc(100vw-24px))] overflow-hidden surface-card shadow-lift">
          <div className="border-b border-line p-2.5">
            <label className="sr-only" htmlFor="company-search">Search ticker or company</label>
            <div className="flex items-center gap-2 rounded-lg border border-line2 bg-panel2 px-3 focus-within:border-accent/70">
              <Icon name="research" size={14} className="shrink-0 text-muted" />
              <input
                ref={inputRef}
                id="company-search"
                value={query}
                onChange={(event) => setQuery(event.target.value)}
                onKeyDown={onInputKeyDown}
                role="combobox"
                aria-autocomplete="list"
                aria-controls="company-search-results"
                aria-expanded="true"
                placeholder="Search ticker or company"
                className="min-w-0 flex-1 bg-transparent py-2 text-[13px] text-fg outline-none placeholder:text-muted/70"
              />
            </div>
          </div>

          <div id="company-search-results" role="listbox" aria-label="Company results" className="max-h-[330px] overflow-y-auto p-1.5">
            {results.map((company, index) => (
              <button
                key={company.symbol}
                type="button"
                role="option"
                aria-selected={index === activeIndex}
                onMouseEnter={() => setActiveIndex(index)}
                onClick={() => choose(company)}
                className={cx(
                  "flex w-full items-center gap-3 rounded-lg px-3 py-2.5 text-left",
                  index === activeIndex ? "bg-white/10" : "hover:bg-white/7"
                )}
              >
                <span className="num w-[62px] shrink-0 text-[13px] font-semibold text-fg">{company.symbol}</span>
                <span className="min-w-0 truncate text-[12.5px] text-muted">{company.name}</span>
                {company.symbol === ticker && <Badge variant="outline" className="ml-auto shrink-0">Active</Badge>}
              </button>
            ))}
            {!searching && !searchError && results.length === 0 && (
              <p className="px-3 py-5 text-center text-[12px] text-muted">No matching company found.</p>
            )}
            {searching && <p className="px-3 py-3 text-[12px] text-muted">Searching…</p>}
            {searchError && <p className="px-3 py-3 text-[12px] text-warn">{searchError}</p>}
          </div>
        </div>
      )}

      {pending && createPortal((
        <div className="fixed inset-0 z-[90] flex items-center justify-center bg-black/60 p-4 backdrop-blur-sm">
          <button
            type="button"
            aria-label="Cancel company change"
            className="absolute inset-0 cursor-default"
            onClick={() => !saving && setPending(null)}
          />
          <div role="dialog" aria-modal="true" aria-labelledby="active-company-title" className="relative w-[min(460px,100%)] surface-card">
            <div className="border-b border-line px-5 py-4">
              <h2 id="active-company-title" className="text-[15px] font-semibold text-fg">
                Work on {pending.name} ({pending.symbol})?
              </h2>
              <p className="mt-1.5 text-[12.5px] leading-relaxed text-muted">
                Research, charts, Copilot and new journal entries will use {pending.symbol}. This does not add it to your watchlist or place a trade.
              </p>
            </div>
            <div className="px-5 py-4">
              {!signedIn && (
                <p className="mb-3 rounded-lg border border-line2 bg-panel2/50 px-3 py-2 text-[11.5px] leading-relaxed text-muted">
                  Sign in to save this active company and restore it on your next visit.
                </p>
              )}
              {saveError && <p className="mb-3 text-[12px] text-down">{saveError}</p>}
              <div className="flex justify-end gap-2">
                <Button variant="ghost" size="sm" disabled={saving} onClick={() => setPending(null)}>Cancel</Button>
                <Button variant="primary" size="sm" disabled={saving} onClick={confirm} autoFocus>
                  {saving ? "Saving…" : signedIn ? "Set as active company" : "Sign in to continue"}
                </Button>
              </div>
            </div>
          </div>
        </div>
      ), document.body)}
    </div>
  );
}
