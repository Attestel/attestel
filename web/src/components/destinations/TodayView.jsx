import { useEffect, useRef, useState } from "react";
import { fetchMarketBrief, listEvents, listTheses } from "../../lib/api.js";
import { useAuth } from "../../auth/AuthContext.jsx";
import AttentionPanel from "../today/AttentionPanel.jsx";
import ForYouOverview from "../today/ForYouOverview.jsx";
import MarketTodayPanel from "../today/MarketTodayPanel.jsx";
import TodayDigest from "../events/TodayDigest.jsx";
import { fetchThesisMonitor } from "../../lib/monitoringApi.js";
import { DestinationHeader } from "../shell/DestinationHeader.jsx";
import { SubNav } from "../shell/SubNav.jsx";

// Today has two distinct jobs and never mixes them into one dashboard:
//   * News — a market-wide, answer-first editorial read grounded in deterministic market facts;
//   * For You — the private review queue built from saved theses, due reviews and fired alerts.
//
// Generic calendar and watchlist content live in their own destinations. Model work is limited to the
// cached market brief and runs only when News is first opened or the user explicitly asks for a re-read.
export default function TodayView({
  subview,
  onSubview,
  level,
  ticker,
  onOpenCompany,
  onOpenThesis,
}) {
  const { user, promptSignIn } = useAuth();
  const isNews = subview !== "for-you";

  const [market, setMarket] = useState(null);
  const [loadingMarket, setLoadingMarket] = useState(false);
  const [marketNonce, setMarketNonce] = useState(0);
  const marketRequestedNonce = useRef(-1);

  const [theses, setTheses] = useState([]);
  const [thesesStatus, setThesesStatus] = useState("idle");
  const [events, setEvents] = useState([]);
  const [eventsStatus, setEventsStatus] = useState("idle");
  const [monitor, setMonitor] = useState(null);
  const [monitorStatus, setMonitorStatus] = useState("idle");
  const [personalNonce, setPersonalNonce] = useState(0);

  // One cached synthesis for News. Switching away and back does not re-run it.
  useEffect(() => {
    if (!isNews || marketRequestedNonce.current === marketNonce) return;
    marketRequestedNonce.current = marketNonce;
    let alive = true;
    setLoadingMarket(true);
    fetchMarketBrief()
      .then((brief) => alive && setMarket(brief))
      .catch(() => alive && setMarket({ error: true }))
      .finally(() => alive && setLoadingMarket(false));
    return () => {
      alive = false;
    };
  }, [isNews, marketNonce]); // eslint-disable-line react-hooks/exhaustive-deps

  // The personal feed is deterministic and account-scoped. Each source fails independently.
  useEffect(() => {
    if (isNews) return;
    let alive = true;
    setThesesStatus("loading");
    setEventsStatus("loading");
    setMonitorStatus("loading");

    listTheses("", "active")
      .then((items) => {
        if (!alive) return;
        setTheses(items);
        setThesesStatus("ready");
      })
      .catch(() => alive && setThesesStatus("down"));

    listEvents(20)
      .then((result) => {
        if (!alive) return;
        setEvents(result.events || []);
        setEventsStatus("ready");
      })
      .catch(() => alive && setEventsStatus("down"));

    fetchThesisMonitor()
      .then((result) => {
        if (!alive) return;
        setMonitor(result);
        setMonitorStatus(result.available === false ? "down" : "ready");
      })
      .catch(() => alive && setMonitorStatus("down"));

    return () => {
      alive = false;
    };
  }, [isNews, personalNonce, user?.id]);

  const refreshPersonal = () => setPersonalNonce((value) => value + 1);

  return (
    <div className="flex flex-col gap-3">
      <DestinationHeader view="today" className="mb-1" />

      <SubNav
        view="today"
        subview={subview}
        onChange={onSubview}
        level={level}
        hint={isNews ? "What matters in the market today" : "What changed the order of your research"}
      />

      {/* The digest sits ABOVE the subview split so it shows on both — it is the answer to "what
          changed since I last looked", which is the question Today exists for, and it is the same
          answer whichever half of Today the user is on.

          `onViewBrief` is a CLICK HANDLER, not a fetch: TodayDigest never calls /api/brief/* itself,
          so invariant #4 cannot be broken from inside that file. Without the handler the button is
          not rendered at all — a button that does nothing is worse than no button. */}
      <TodayDigest
        uid={user?.id}
        onOpenCompany={onOpenCompany}
        onViewBrief={() => onSubview("news")}
      />

      {isNews ? (
        <MarketTodayPanel
          market={market}
          loading={loadingMarket}
          onRefresh={() => setMarketNonce((value) => value + 1)}
          onOpenCompany={onOpenCompany}
        />
      ) : (
        <>
          <ForYouOverview
            user={user}
            theses={theses}
            thesesStatus={thesesStatus}
            events={events}
            eventsStatus={eventsStatus}
            monitor={monitor}
            monitorStatus={monitorStatus}
            onRefresh={refreshPersonal}
          />
          <AttentionPanel
            theses={theses}
            thesesStatus={thesesStatus}
            events={events}
            eventsStatus={eventsStatus}
            monitor={monitor}
            monitorStatus={monitorStatus}
            user={user}
            onOpenCompany={onOpenCompany}
            onOpenThesis={onOpenThesis}
            onSignIn={promptSignIn}
            onCreateThesis={() => onOpenThesis(ticker)}
          />
        </>
      )}
    </div>
  );
}
