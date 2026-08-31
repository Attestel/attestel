import { SubNav } from "../shell/SubNav.jsx";
import Watchlist from "../Watchlist.jsx";
import AlertsPanel from "../AlertsPanel.jsx";
import { DestinationHeader } from "../shell/DestinationHeader.jsx";

// WatchlistView — the Watchlist destination (Monitor): the companies being followed, plus the
// monitoring rules that watch them.
//
// Monitoring rules are the former top-level Alerts destination, unchanged. The triggered EVENTS stay
// with the TopBar bell / notification center, which is deliberately outside the seven destinations —
// so #alerts resolves here, to the surface where rules are managed.
export default function WatchlistView({
  subview,
  onSubview,
  level,
  tickers,
  ticker,
  timeframe,
  onSelectTicker,
}) {
  return (
    <div className="flex flex-col gap-3">
      <DestinationHeader view="watchlist" className="mb-1" />

      <SubNav
        view="watchlist"
        subview={subview}
        onChange={onSubview}
        level={level}
        hint={subview === "monitoring" ? "Descriptive conditions — never a trade action." : undefined}
      />

      {subview === "companies" && (
        <Watchlist
          tickers={tickers}
          onSelect={onSelectTicker}
          activeTicker={ticker}
          timeframe={timeframe}
        />
      )}

      {subview === "monitoring" && <AlertsPanel tickers={tickers} defaultTicker={ticker} />}
    </div>
  );
}
