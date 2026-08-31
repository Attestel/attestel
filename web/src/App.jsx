import { useEffect, useMemo, useState } from "react";
import { AnimatePresence, MotionConfig, motion } from "framer-motion";
import {
  fetchDashboard,
  fetchTickers,
  fetchQuote,
  TIMEFRAMES,
  listEvents,
} from "./lib/api.js";
import {
  formatHash,
  isAuthRoute,
  parseHash,
  resolveForLevel,
} from "./lib/routes.js";
import { useHealth } from "./hooks/useHealth.js";
import { useSettings } from "./settings/SettingsContext.jsx";
import { useAuth } from "./auth/AuthContext.jsx";
import { TopBar } from "./components/shell/TopBar.jsx";
import { LeftRail } from "./components/shell/LeftRail.jsx";
import CalendarView from "./components/CalendarView.jsx";
import TodayView from "./components/destinations/TodayView.jsx";
import FollowingView from "./components/destinations/FollowingView.jsx";
import PortfolioView from "./components/portfolio/PortfolioView.jsx";
import ExploreView from "./components/destinations/ExploreView.jsx";
import EventDetailPanel from "./components/events/EventDetailPanel.jsx";
import ResearchView from "./components/destinations/ResearchView.jsx";
import WatchlistView from "./components/destinations/WatchlistView.jsx";
import JournalView from "./components/destinations/JournalView.jsx";
import LibraryView from "./components/destinations/LibraryView.jsx";
import SettingsView from "./components/destinations/SettingsView.jsx";
import CopilotDrawer from "./components/copilot/CopilotDrawer.jsx";
import { CopilotLauncher } from "./components/copilot/CopilotLauncher.jsx";
import NotificationsModal from "./components/shell/NotificationsModal.jsx";
import { useToast } from "./components/ui/Toast.jsx";
import GraduationPrompt from "./components/onboarding/GraduationPrompt.jsx";
import RealityCheck from "./components/onboarding/RealityCheck.jsx";
import AutoTrainer from "./settings/AutoTrainer.jsx";
import AlertNotifier from "./settings/AlertNotifier.jsx";
import { Icon } from "./components/shell/icons.jsx";

const QUOTE_POLL_MS = 20000;
const ALERTS_POLL_MS = 30000;

// Routing lives in lib/routes.js: seven primary destinations, each with optional subviews. The hash is
// `#view` or `#view/subview`; parseHash resolves it, falling back to the default destination for an
// unknown hash so a stale bookmark never dead-ends. Renamed SUBVIEWS still resolve via legacySubviews;
// the pre-migration top-level aliases were retired once the migration window closed.
//
// Route state is level-AGNOSTIC — it records what the user ASKED for. The experience-level gate is
// applied separately as `effective`, so a deep-link's intent survives the async settings load (a
// standard user's #committee isn't clobbered while settings are still loading) and graduating to
// Standard reveals a previously-hidden destination instead of having lost it.

export default function App() {
  const [tickers, setTickers] = useState([{ symbol: "NVDA", name: "NVIDIA" }]);
  const { settings, hydrated: settingsHydrated, saveSettings } = useSettings();
  const [ticker, setTicker] = useState(() => settings?.activeTicker || "NVDA");
  const [timeframe, setTimeframe] = useState("1D");

  const [data, setData] = useState(null);
  const [quote, setQuote] = useState(null);
  const [err, setErr] = useState(null);
  const [loading, setLoading] = useState(true);

  const [route, setRoute] = useState(() => parseHash(window.location.hash));
  const [unread, setUnread] = useState(0);
  const [copilotOpen, setCopilotOpen] = useState(false);
  const [notifOpen, setNotifOpen] = useState(false);

  // Experience level drives destination/subview gating + Terminal density (docs/BEGINNER.md Piece 4).
  // A guest / while-loading is "beginner" (DEFAULT_SETTINGS).
  const { user, promptSignIn } = useAuth();
  const level = settings?.experienceLevel === "standard" ? "standard" : "beginner";
  const effective = useMemo(
    () => resolveForLevel(route.view, route.subview, route.tab, level),
    [route.view, route.subview, route.tab, level]
  );

  // go() is the single navigation entrypoint. Passing no subview/tab lands on the destination's defaults.
  const go = (view, subview = null, tab = null) => setRoute({ view, subview, tab, alias: null });
  const goSubview = (subview, tab = null) => setRoute((r) => ({ ...r, subview, tab, alias: null, params: undefined }));
  const goTab = (tab) => setRoute((r) => ({ ...r, tab, alias: null, params: undefined }));

  // Copilot launch preset — lets a Research section start the drawer on a concrete research task (a lens
  // applied to this company, or scenario mode) instead of an empty prompt box (guide §7.2). The nonce makes
  // a repeat launch re-apply. Nothing is sent; it only preselects what the user could pick by hand.
  const [copilotPreset, setCopilotPreset] = useState(null);
  const launchCopilot = (p) => {
    setCopilotPreset({ ...p, nonce: Date.now() });
    setCopilotOpen(true);
  };

  // ── The Wave 4 join: three lanes become one product here and nowhere else ────────────────────
  //
  // `renderDetail` hands Lane 4B's EventDetailPanel to Lane 4A's Following feed and Lane 4B's own
  // Explore feed. Both call it INSIDE their own grid, so at >= 1280 the panel is a column beside a
  // feed that stays mounted and scrolled — doc §16.4, and the reason it is a render prop rather
  // than an overlay hoisted up here. `disclosure` is threaded from the response that produced the
  // row: this build holds no client-side copy of §9.18's string, so a surface the server sent no
  // disclosure to withholds the direction and says so.
  const renderEventDetail = (eventId, { onClose, disclosure = "" } = {}) => (
    <EventDetailPanel
      eventId={eventId}
      open
      onClose={onClose}
      onOpenCompany={(sym) => openCompanyResearch(sym, "overview")}
      onAsk={askAttestel}
      onAuthRequired={promptSignIn}
      disclosure={disclosure}
    />
  );

  // A ticker page has no feed to sit beside, so `onOpenEvent` from Research opens the SAME panel as
  // an overlay instead of a second component. One panel, two hosts.
  const [detailEventId, setDetailEventId] = useState(null);

  // Review links carry their target inside the hash so they survive static hosting and bookmarks.
  // Applying a ticker here changes only the page in view; it does not silently persist a preference.
  useEffect(() => {
    const linkedTicker = route.params?.ticker;
    if (linkedTicker && linkedTicker !== ticker) setTicker(linkedTicker);
  }, [route.params?.ticker, ticker]);

  const linkedEventId = route.view === "calendar" ? route.params?.event : "";
  const activeDetailEventId = linkedEventId || detailEventId;

  const closeEventDetail = () => {
    setDetailEventId(null);
    setRoute((current) => current.params?.event
      ? { ...current, params: { ...current.params, event: undefined } }
      : current);
  };

  // askAttestel is the single Ask entry point for every event surface. It carries the event's own
  // context into the drawer rather than an empty prompt box. Nothing is sent — `launchCopilot`
  // preselects what the user could have typed, and no model is called by opening it (invariant #4).
  const askAttestel = ({ kind = "event", eventId = null, ticker: t = null, label, facts, question, suggestions } = {}) => {
    if (question) {
      launchCopilot({ mode: "scenario", draft: question });
      return;
    }
    launchCopilot({
      mode: "chat",
      context: { kind, eventId, ticker: t ?? ticker, label, facts },
      suggestions,
    });
  };

  // Reality-check first-run — sign-in triggered (docs/BEGINNER.md Piece 1). SignIn/SignUp navigate into
  // this shell after auth, so gating the overlay here shows it right after sign-in, over whatever view
  // they returned to, until they pick a path. Only a SIGNED-IN, UNACKNOWLEDGED user is gated — a guest
  // (no user) never sees it. Gated on `hydrated` so a returning acknowledged user never flashes it while
  // settings load (the modal must never render on stale DEFAULT_SETTINGS). RealityCheck persists the
  // choice itself; the gate then closes. Re-openable from Settings via reviewOpen (review never changes
  // the experience level).
  const [reviewOpen, setReviewOpen] = useState(false);
  const showFirstRun = Boolean(user) && settingsHydrated && settings.ackRealityCheck !== true;
  const showRealityCheck = showFirstRun || reviewOpen;

  // Keep the URL hash in sync with the active route (canonical `#view` / `#view/subview`), and react to
  // manual hash / back-button changes. Legacy hashes are normalised here: landing on #terminal rewrites
  // the URL to #research/overview, so the alias resolves once and the address bar tells the truth.
  // replaceState (not pushState) preserves the pre-migration history behaviour.
  useEffect(() => {
    if (isAuthRoute(window.location.hash)) return; // Root.jsx owns #signin / #signup
    const canonical = formatHash(route.view, route.subview, route.tab, route.params);
    if (window.location.hash.replace("#", "") !== canonical) {
      window.history.replaceState(null, "", `#${canonical}`);
    }
  }, [route]);
  useEffect(() => {
    const onHash = () => {
      if (isAuthRoute(window.location.hash)) return;
      setRoute(parseHash(window.location.hash));
    };
    window.addEventListener("hashchange", onHash);
    return () => window.removeEventListener("hashchange", onHash);
  }, []);

  // Google OAuth failure returns the browser to AppURL?authError=1 (google.go). Surface a small toast
  // once on load and strip the query so a refresh doesn't repeat it (the #view hash is preserved).
  const toast = useToast();
  useEffect(() => {
    const params = new URLSearchParams(window.location.search);
    if (params.get("authError") === "1") {
      toast({ title: "Google sign-in failed", message: "Please try again.", tone: "error" });
      window.history.replaceState(null, "", window.location.pathname + window.location.hash);
    }
  }, [toast]);
  const [journalPrefill, setJournalPrefill] = useState(null);

  // One active-company action keeps every entry point consistent. Search requires an account so the
  // confirmed choice can be remembered; existing market/watchlist links remain browsable by guests.
  const activateCompany = async (company, { requireSignIn = false } = {}) => {
    const symbol = String(company?.symbol || company || "").trim().toUpperCase();
    if (!symbol) return false;
    if (!user && requireSignIn) {
      try {
        sessionStorage.setItem(
          "activeCompany.pending",
          JSON.stringify({ symbol, name: company?.name || symbol })
        );
      } catch {
        /* sign-in still works when storage is unavailable */
      }
      promptSignIn();
      return false;
    }
    if (user) await saveSettings({ activeTicker: symbol });
    setTicker(symbol);
    toast({
      tone: "success",
      title: `${symbol} is your active company`,
      message: user ? "This company will be restored on your next visit." : "Sign in to remember this company.",
    });
    return true;
  };

  // A guest may confirm a company and then sign in. Carry that one explicit action through the auth
  // screen, save it to the newly resolved account, and remove the short-lived handoff either way.
  useEffect(() => {
    if (!user) return;
    let pending = null;
    try {
      pending = JSON.parse(sessionStorage.getItem("activeCompany.pending") || "null");
      sessionStorage.removeItem("activeCompany.pending");
    } catch {
      pending = null;
    }
    const symbol = String(pending?.symbol || "").trim().toUpperCase();
    if (!symbol) return;
    saveSettings({ activeTicker: symbol })
      .then(() => {
        setTicker(symbol);
        toast({ tone: "success", title: `${symbol} is your active company`, message: "Saved to your account." });
      })
      .catch(() => {
        toast({ tone: "error", title: "Company was not saved", message: "Choose it again from company search." });
      });
    // The handoff belongs to the identity that just resolved; subsequent settings updates must not replay it.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [user?.id]);

  const openCompanyResearch = (symbol, subview) => {
    const target = symbol || ticker;
    activateCompany(target)
      .then((changed) => changed && go("research", subview))
      .catch((error) => toast({ tone: "error", title: "Company was not changed", message: error.message }));
  };

  const svc = useHealth();

  // From the signal panel: prefill the journal and jump to it (records a call, never places a trade).
  const logAsTrade = (prefill) => {
    setJournalPrefill(prefill);
    go("journal", "trades");
  };

  // Load the configured universe once for the switcher.
  useEffect(() => {
    fetchTickers()
      .then((list) => {
        if (list.length) setTickers(list);
      })
      .catch(() => {});
  }, []);

  // (Re)load the dashboard whenever ticker or timeframe changes. Also invoked by "Reload".
  const load = () => {
    setLoading(true);
    fetchDashboard(ticker, timeframe)
      .then((d) => {
        setData(d);
        setErr(null);
      })
      .catch((e) => setErr(e.message))
      .finally(() => setLoading(false));
  };
  useEffect(load, [ticker, timeframe]);

  // Poll the cheap quote endpoint. Reset + restart on ticker change. The LLM read is NOT here.
  useEffect(() => {
    setQuote(null);
    let alive = true;
    const tick = () =>
      fetchQuote(ticker)
        .then((q) => {
          if (alive) setQuote(q);
        })
        .catch(() => {});
    tick();
    const id = setInterval(tick, QUOTE_POLL_MS);
    return () => {
      alive = false;
      clearInterval(id);
    };
  }, [ticker]);

  // Poll the alerts service for the unread badge. Fails silent. Paused while the monitoring-rules
  // surface or the notifications modal is open (they own the unread count while visible).
  const onMonitoring = effective.view === "watchlist" && effective.subview === "monitoring";
  useEffect(() => {
    if (onMonitoring || notifOpen) return;
    let alive = true;
    const tick = () =>
      listEvents(1)
        .then((r) => {
          if (alive) setUnread(r.unread || 0);
        })
        .catch(() => {});
    tick();
    const id = setInterval(tick, ALERTS_POLL_MS);
    return () => {
      alive = false;
      clearInterval(id);
    };
  }, [onMonitoring, notifOpen]);

  // Keyboard shortcut: "c" toggles the Copilot drawer (ignored while typing in a field).
  useEffect(() => {
    const onKey = (e) => {
      if (e.metaKey || e.ctrlKey || e.altKey) return;
      const t = e.target;
      const typing =
        t &&
        (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.tagName === "SELECT" || t.isContentEditable);
      if (typing) return;
      if (e.key === "c" || e.key === "C") {
        e.preventDefault();
        setCopilotOpen((o) => !o);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  // The bell opens the notifications modal (click a notification there to mark it read); the modal's
  // "Monitoring rules" jumps to Watchlist → Monitoring for rule management.
  const onBell = () => setNotifOpen((o) => !o);

  const price = data?.price;
  const headPrice = quote?.price ?? price?.lastClose;
  const headChange = quote?.changePct ?? price?.changePct;
  const synthetic = Boolean(data?.priceSourceIsSynthetic);
  const quoteLive = Boolean(quote) && !/synth/i.test(String(quote?.source || ""));
  const hasSeed = data?.hasSeed ?? (ticker === "NVDA");

  // Health cluster: gateway/alerts/journal/paper polled directly; analysis/llm derived from the
  // dashboard payload (they sit behind the gateway, unreachable from the browser).
  const health = useMemo(
    () => [
      { key: "gateway", label: "Gateway", up: svc.gateway },
      { key: "analysis", label: "Analysis", up: data ? true : err ? false : undefined, derived: true },
      {
        key: "llm",
        label: "LLM",
        up: data?.read ? !data.read.error : data ? false : undefined,
        derived: true,
      },
      { key: "auth", label: "Auth", up: svc.auth },
      { key: "alerts", label: "Alerts", up: svc.alerts },
      { key: "journal", label: "Journal", up: svc.journal },
      { key: "paper", label: "Paper", up: svc.paper },
      { key: "feedback", label: "Feedback", up: svc.feedback },
    ],
    [svc, data, err]
  );

  return (
    <MotionConfig reducedMotion="user">
      {/* overflow-x-clip (not hidden) kills any stray sideways scroll without creating a scroll
          container — so the sticky TopBar / LeftRail keep working. */}
      <div className="min-h-screen overflow-x-clip bg-bg text-fg">
        <TopBar
          tickers={tickers}
          ticker={ticker}
          onTicker={(company) => activateCompany(company, { requireSignIn: true })}
          timeframe={timeframe}
          onTimeframe={setTimeframe}
          timeframes={TIMEFRAMES}
          quote={quote}
          price={headPrice}
          changePct={headChange}
          quoteLive={quoteLive}
          health={health}
          synthetic={synthetic}
          paper={effective.view === "journal" && effective.subview === "experiments"}
          unread={unread}
          onBell={onBell}
          bellActive={notifOpen}
          onReload={load}
          loading={loading}
          onCopilot={() => setCopilotOpen(true)}
          copilotActive={copilotOpen}
          signedIn={Boolean(user)}
        />

        <div className="mx-auto flex w-full max-w-[1700px]">
          <LeftRail view={effective.view} onChange={(v) => go(v)} level={level} />

          <main className="min-w-0 flex-1 px-3 py-4 sm:px-5 sm:pb-16">
            {synthetic && (
              <div className="mb-4 rounded-lg border border-warn/30 bg-warn/10 px-4 py-2.5 text-[13px] text-warn">
                <strong className="inline-flex items-center gap-1.5 font-bold"><Icon name="warning" size={14} /> SYNTHETIC price data</strong> — generated seed bars (no price key / network).
                Nothing here is tradeable. Wire a real price key in the analysis service for live bars &amp; quote.
              </div>
            )}
            {err && (
              <div className="mb-4 rounded-lg border border-down/40 bg-down/10 px-4 py-2.5 text-[13px] text-down">
                Gateway error: {err}. Is the Go gateway running on <code className="rounded bg-panel2 px-1">:8080</code>?
              </div>
            )}

            {/* Earned graduation to the full cockpit — self-hides unless a beginner has logged enough
                checklist-complete trades and hasn't dismissed it this session. */}
            <GraduationPrompt />

            <AnimatePresence mode="wait">
              <motion.div
                key={`${effective.view}/${effective.subview || ""}/${effective.tab || ""}`}
                initial={{ opacity: 0, y: 8 }}
                animate={{ opacity: 1, y: 0 }}
                exit={{ opacity: 0, y: -8 }}
                transition={{ duration: 0.18, ease: "easeOut" }}
              >
                <ViewRouter
                  view={effective.view}
                  subview={effective.subview}
                  onSubview={goSubview}
                  tab={effective.tab}
                  onTab={goTab}
                  level={level}
                  data={data}
                  quote={quote}
                  quoteLive={quoteLive}
                  ticker={ticker}
                  timeframe={timeframe}
                  onTimeframe={setTimeframe}
                  tickers={tickers}
                  hasSeed={hasSeed}
                  loading={loading}
                  onLogTrade={logAsTrade}
                  journalPrefill={journalPrefill}
                  onReviewRealityCheck={() => setReviewOpen(true)}
                  onOpenCopilot={() => setCopilotOpen(true)}
                  onOpenWatchlist={() => go("watchlist", "companies")}
                  onApplyLens={(personaId) => launchCopilot({ mode: "chat", personaId })}
                  onOpenScenario={(draft) => launchCopilot({ mode: "scenario", draft })}
                  onOpenThesis={(sym) => openCompanyResearch(sym, "thesis")}
                  onSelectTicker={(sym) => openCompanyResearch(sym, "overview")}
                  renderDetail={renderEventDetail}
                  onAsk={askAttestel}
                  onOpenEvent={setDetailEventId}
                />
              </motion.div>
            </AnimatePresence>
          </main>
        </div>

        {/* Bell notifications modal — the user's triggered alert events; click one to mark it read. The
            bell stays outside the seven destinations; its "Monitoring rules" link goes to the rule
            surface under Watchlist. */}
        <NotificationsModal
          open={notifOpen}
          onClose={() => setNotifOpen(false)}
          onOpenAlerts={() => go("watchlist", "monitoring")}
          onUnread={setUnread}
        />

        {/* Publishes the company in view for explicit model lifecycle actions; runs no timer. */}
        <AutoTrainer ticker={ticker} timeframe={timeframe} />

        {/* Desktop notifications for new alert events (null-render): opt-in per user + OS permission. */}
        <AlertNotifier />

        {/* Global Copilot — floating launcher + right-anchored chat drawer, grounded in the ticker in view. */}
        <CopilotLauncher
          open={copilotOpen}
          onClick={() => setCopilotOpen(true)}
          hasContext={Boolean(copilotPreset?.context)}
        />
        <CopilotDrawer
          open={copilotOpen}
          onClose={() => setCopilotOpen(false)}
          ticker={ticker}
          timeframe={timeframe}
          preset={copilotPreset}
        />

        {/* Event detail opened from a ticker page, which has no feed to sit beside — the same panel
            Following and Explore render as a column, here as an overlay. */}
        {activeDetailEventId &&
          renderEventDetail(activeDetailEventId, { onClose: closeEventDetail })}

        {/* Reality-check — sign-in-triggered overlay over the shell, or a Settings re-open (Piece 1). */}
        {showRealityCheck && (
          <RealityCheck mode={showFirstRun ? "firstRun" : "review"} onClose={() => setReviewOpen(false)} />
        )}
      </div>
    </MotionConfig>
  );
}

// ViewRouter — one branch per primary destination. Destinations WITH subviews delegate to a container
// under components/destinations/ that owns its SubNav; Calendar has no subviews and renders its
// capability directly.
function ViewRouter({
  view,
  subview,
  onSubview,
  tab,
  onTab,
  level,
  data,
  quote,
  quoteLive,
  ticker,
  timeframe,
  onTimeframe,
  tickers,
  hasSeed,
  loading,
  onLogTrade,
  journalPrefill,
  onReviewRealityCheck,
  onOpenCopilot,
  onOpenWatchlist,
  onApplyLens,
  onOpenScenario,
  onOpenThesis,
  onSelectTicker,
  renderDetail,
  onAsk,
  onOpenEvent,
}) {
  switch (view) {
    // Today (Orient) — market news and the user's personal review queue are separate contexts.
    case "today":
      return (
        <TodayView
          subview={subview}
          onSubview={onSubview}
          level={level}
          ticker={ticker}
          onOpenCompany={onSelectTicker}
          onOpenThesis={onOpenThesis}
        />
      );

    // Following (Monitor) — the user's own companies, as canonical events. The detail panel is a
    // column INSIDE this destination: selecting an event must not take the feed off the screen.
    case "following":
      if (subview === "portfolio") return <PortfolioView />;
      return (
        <FollowingView
          level={level}
          onOpenCompany={onSelectTicker}
          renderDetail={renderDetail}
        />
      );

    // Explore (Orient) — discovery, six server-ordered sections, every card carrying both of §8's
    // mandatory reason strings.
    case "explore":
      return (
        <ExploreView
          level={level}
          onOpenCompany={onSelectTicker}
          onAsk={onAsk}
          renderDetail={renderDetail}
        />
      );

    case "research":
      return (
        <ResearchView
          subview={subview}
          onSubview={onSubview}
          tab={tab}
          onTab={onTab}
          level={level}
          data={data}
          quote={quote}
          quoteLive={quoteLive}
          ticker={ticker}
          timeframe={timeframe}
          timeframes={TIMEFRAMES}
          onTimeframe={onTimeframe}
          hasSeed={hasSeed}
          loading={loading}
          onLogTrade={onLogTrade}
          onApplyLens={onApplyLens}
          onOpenScenario={onOpenScenario}
          onAsk={onAsk}
          onOpenEvent={onOpenEvent}
        />
      );

    case "watchlist":
      return (
        <WatchlistView
          subview={subview}
          onSubview={onSubview}
          level={level}
          tickers={tickers}
          ticker={ticker}
          timeframe={timeframe}
          onSelectTicker={onSelectTicker}
        />
      );

    case "calendar":
      return <CalendarView />;

    case "journal":
      return (
        <JournalView
          subview={subview}
          onSubview={onSubview}
          level={level}
          tickers={tickers}
          ticker={ticker}
          prefill={journalPrefill}
          dashboard={data}
        />
      );

    case "library":
      return (
        <LibraryView
          subview={subview}
          onSubview={onSubview}
          level={level}
          ticker={ticker}
          onOpenCopilot={onOpenCopilot}
          onOpenThesis={onOpenThesis}
          onSelectTicker={onSelectTicker}
          onApplyLens={onApplyLens}
        />
      );

    case "settings":
      return (
        <SettingsView
          subview={subview}
          onSubview={onSubview}
          level={level}
          onReviewRealityCheck={onReviewRealityCheck}
          ticker={ticker}
          timeframe={timeframe}
          data={data}
        />
      );

    default:
      return null;
  }
}
