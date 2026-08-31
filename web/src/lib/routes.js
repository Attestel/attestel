// routes.js — the single source of truth for customer-visible navigation.
//
// NINE primary destinations, each optionally holding subviews, and a subview may hold tabs.
// Contract §9.34 fixes both the count and the ORDER:
//
//   Today · Following · Explore · Calendar · Research · Watchlist · Journal · Library · Settings
//
// Wave 4 added Following and Explore and moved Calendar ahead of Research to match. Note that the
// two lane-recorded inserts alone do NOT produce this list — 4A inserts Following after Today and
// 4B inserts Explore before Research, which leaves Calendar sitting sixth, behind Watchlist. The
// order is the clause, not a by-product of the inserts. The old thirteen feature-inventory
// destinations survive as
// A destination's renamed subviews survive as `legacySubviews`. The pre-migration TOP-LEVEL hashes
// (#terminal, #brief, #committee, #transcripts, #fundamentals, #news, #alerts, #paper, #feedback)
// were retired once the migration window closed; parseHash resolves an unknown hash to the default
// destination, so such a bookmark lands on Today rather than dead-ending.
//
// Hash grammar:  #view  |  #view/subview  |  #view/subview/tab
//
// This module is deliberately pure data + pure functions: the whole redirect table is one object, so
// a route change is a one-line data edit and the mapping can be reasoned about without reading JSX.
//
// HARD CONSTRAINT — destination ids must match ^[a-z][a-z0-9-]{0,30}$ (no slash, dot, or uppercase).
// The auth service validates the post-OAuth `returnTo` against exactly that shape and SILENTLY falls
// back otherwise (auth/google.go sanitizeReturnTo). That is a backend file this migration must not
// change, so the hash's FIRST segment always stays within the shape and Google sign-in carries only
// that segment (see baseViewOf).

// Experience levels order beginner < standard. A destination or subview is visible once the user's
// level meets its minLevel — view-gating only, it unlocks no trading action (invariant #2).
// `hiddenInNav` keeps specialist workspaces deep-linkable without making the Research landing read
// like a feature inventory; Investigations opens them contextually when the user asks for detail.
const LEVEL_RANK = { beginner: 0, standard: 1 };
const rankOf = (level) => LEVEL_RANK[level] ?? 0; // unknown / guest / loading -> beginner

// DESTINATIONS — the nine of §9.34, in §9.34's order. `stage` names the research-loop stage each one
// serves, so the IA can be audited against the loop rather than against the old feature list.
//
// minLevel preserves the CURRENT beginner capability set: a beginner previously saw Terminal, Brief,
// Calendar, Journal, Paper and Settings, which is Today + Research(investigations, thesis) + Calendar +
// Journal(trades, experiments) + Settings/preferences. Nothing was added or removed for beginners by
// this migration; only the vocabulary changed.
export const DESTINATIONS = [
  {
    id: "today",
    label: "Today",
    icon: "today",
    minLevel: "beginner",
    stage: "Orient",
    subviews: [
      { id: "news", label: "News", minLevel: "beginner" },
      { id: "for-you", label: "For You", minLevel: "beginner" },
    ],
  },
  {
    // D-28: Following is beginner-visible. Hiding a destination is the thing D-28 abolished.
    id: "following",
    label: "Following",
    icon: "following",
    minLevel: "beginner",
    stage: "Monitor",
    subviews: [
      { id: "changes", label: "Changes", minLevel: "beginner" },
      { id: "portfolio", label: "Portfolio", minLevel: "beginner" },
    ],
  },
  {
    // `compass` is the EXISTING mark in icons.jsx — a dedicated `explore` glyph was offered by the
    // lane and is not needed.
    id: "explore",
    label: "Explore",
    icon: "compass",
    minLevel: "beginner",
    stage: "Orient",
    subviews: [],
  },
  {
    // §9.34 puts Calendar ahead of Research. It sat sixth before this wave, behind Watchlist.
    id: "calendar",
    label: "Calendar",
    icon: "calendar",
    minLevel: "beginner",
    stage: "Orient · Monitor",
    subviews: [],
  },
  {
    id: "research",
    label: "Research",
    icon: "research",
    minLevel: "beginner",
    stage: "Understand · Form · Challenge",
    // The company research file. Section ids are the stable deep-link targets; Step 01's ids that were
    // renamed here are kept resolving through legacySubviews below.
    subviews: [
      { id: "overview", label: "Overview", minLevel: "beginner" },
      { id: "events", label: "Events", minLevel: "beginner" },
      { id: "technical", label: "Technical", minLevel: "beginner" },
      { id: "fundamentals", label: "Fundamentals", minLevel: "beginner" },
      { id: "earnings", label: "Earnings", minLevel: "beginner" },
      { id: "investigations", label: "Investigations", minLevel: "beginner", hiddenInNav: true },
      { id: "thesis", label: "My Thesis", minLevel: "beginner", hiddenInNav: true },
      {
        id: "evidence",
        label: "Evidence",
        minLevel: "standard",
        hiddenInNav: true,
        // One category renders at a time — the section must not mount five full pages at once.
        tabs: [
          { id: "fundamentals", label: "Fundamentals" },
          { id: "chart", label: "Chart context" },
          { id: "news", label: "News & filings" },
          { id: "transcripts", label: "Transcripts" },
          { id: "catalysts", label: "Catalysts & risks" },
          { id: "model", label: "Model evidence" },
        ],
      },
      { id: "perspectives", label: "Perspectives", minLevel: "standard", hiddenInNav: true },
      { id: "scenarios", label: "Scenarios", minLevel: "standard", hiddenInNav: true },
      { id: "history", label: "History", minLevel: "standard", hiddenInNav: true },
      { id: "decisions", label: "Decision log", minLevel: "standard", hiddenInNav: true },
    ],
    // legacySubviews is UNCHANGED. Two of its aliases are now SHADOWED by real subviews above, and
    // both shadows were accepted deliberately at integration rather than routed around:
    //
    //   #research/overview      was → investigations   now → overview
    //   #research/fundamentals  was → evidence/fundamentals  now → fundamentals
    //
    // Neither loses a capability. `overview` pointed at Investigations only BECAUSE that was the
    // landing page; it now points at the real Overview, which is what the bookmark meant. The
    // `fundamentals` tab renders EvidenceSection opened on the fundamentals category — the same
    // FundamentalsView the alias reached. Renaming the new ids to `company-overview` /
    // `company-fundamentals` would have traded two clean URLs to protect a redirect that now points
    // somewhere better. The other three aliases (`snapshot`, `news`, `transcripts`) are NOT
    // shadowed and resolve exactly as before — verified by executing parseHash, not by reading it.
    legacySubviews: {
      snapshot: { subview: "investigations", tab: null },
      overview: { subview: "investigations", tab: null },
      fundamentals: { subview: "evidence", tab: "fundamentals" },
      news: { subview: "evidence", tab: "news" },
      transcripts: { subview: "evidence", tab: "transcripts" },
    },
  },
  {
    // D-28 is DECIDED (b) — DENSITY GATING, NOT DESTINATION HIDING — and names this destination's
    // `standard` gate as "the concrete mismatch to fix". Corrected here, with its subviews: a
    // destination on the rail whose every subview is hidden is destination-hiding wearing a hat.
    id: "watchlist",
    label: "Watchlist",
    icon: "watchlist",
    minLevel: "beginner",
    stage: "Monitor",
    subviews: [
      { id: "companies", label: "Companies", minLevel: "beginner" },
      { id: "monitoring", label: "Monitoring rules", minLevel: "beginner" },
    ],
  },
  {
    id: "journal",
    label: "Journal",
    icon: "journal",
    minLevel: "beginner",
    stage: "Decide · Review",
    subviews: [
      // Step 09 (§5.6) ordered these as the loop runs: why you decided, how it held up, what you
      // executed, and the simulated validation book last. Decisions leads because a decision — not a
      // trade — is the thing Journal is now about; a deliberate non-action has no trade at all.
      { id: "decisions", label: "Decisions", minLevel: "beginner" },
      { id: "outcomes", label: "Outcomes", minLevel: "beginner" },
      { id: "trades", label: "Trades", minLevel: "beginner" },
      { id: "experiments", label: "Experiments", minLevel: "beginner" },
    ],
  },
  {
    // Same correction, and §9.34 names this one explicitly: Wave 4's own prompt "silently dropped
    // Journal and Library — which is where three classified disclosure strings render, and which is
    // exactly the destination-hiding D-28 exists to abolish".
    //
    // Two of `docs/DISCLOSURE_CLASSIFICATION.md`'s rows live behind these subviews — row 10
    // (`LENS_DISCLAIMER`, on every saved review) and row 13 ("Language analysis, not investment
    // advice.", TranscriptsView). At `standard` they were UNREACHABLE for a beginner, which is the
    // reachability half of §9.34's gate check failing quietly.
    id: "library",
    label: "Library",
    icon: "library",
    minLevel: "beginner",
    stage: "Understand · Challenge",
    subviews: [
      { id: "transcripts", label: "Saved transcripts", minLevel: "beginner" },
      { id: "lenses", label: "Research lenses", minLevel: "beginner" },
    ],
  },
  {
    id: "settings",
    label: "Settings",
    icon: "settings",
    minLevel: "beginner",
    stage: null,
    subviews: [
      { id: "preferences", label: "Preferences", minLevel: "beginner" },
      { id: "help", label: "Help & feedback", minLevel: "standard" },
    ],
  },
];

// DEFAULT_VIEW — where an empty or unrecognised hash lands. Today (Orient) rather than the old
// Terminal: "what changed since I last looked" is the daily entry point.
export const DEFAULT_VIEW = "today";

// Auth screens live OUTSIDE the shell (Root.jsx owns them), so they are not destinations. Exported so
// the shell never rewrites a hash it does not own.
export const AUTH_ROUTES = ["signin", "signup"];

export function destinationById(id) {
  return DESTINATIONS.find((d) => d.id === id) || null;
}

function subviewDef(viewId, subviewId) {
  return (destinationById(viewId)?.subviews || []).find((s) => s.id === subviewId) || null;
}

// defaultSubviewOf returns a destination's first subview id, or null when it has none.
export function defaultSubviewOf(viewId) {
  const dest = destinationById(viewId);
  return dest?.subviews?.length ? dest.subviews[0].id : null;
}

// defaultTabOf returns a subview's first tab id, or null when it has none.
export function defaultTabOf(viewId, subviewId) {
  const s = subviewDef(viewId, subviewId);
  return s?.tabs?.length ? s.tabs[0].id : null;
}

// tabsFor returns a subview's tab list (empty when it has none).
export function tabsFor(viewId, subviewId) {
  return subviewDef(viewId, subviewId)?.tabs || [];
}

export function isAuthRoute(hash) {
  return AUTH_ROUTES.includes(String(hash || "").replace(/^#/, "").toLowerCase());
}

// parseHash resolves any hash — new, legacy, or junk — to a concrete { view, subview, tab }.
// `alias` carries the legacy id that was matched (null for a native route), so callers can normalise
// the URL and so the behaviour is testable without a browser.
export function parseHash(rawHash) {
  const h = String(rawHash || "").replace(/^#/, "").trim();
  const queryAt = h.indexOf("?");
  const path = queryAt >= 0 ? h.slice(0, queryAt) : h;
  const query = queryAt >= 0 ? new URLSearchParams(h.slice(queryAt + 1)) : null;
  const params = {};
  const event = String(query?.get("event") || "").trim();
  const ticker = String(query?.get("ticker") || "").trim().toUpperCase();
  if (event) params.event = event;
  if (/^[A-Z][A-Z0-9.-]{0,14}$/.test(ticker)) params.ticker = ticker;
  const withParams = (route) => Object.keys(params).length ? { ...route, params } : route;
  const fallback = () => ({
    view: DEFAULT_VIEW,
    subview: defaultSubviewOf(DEFAULT_VIEW),
    tab: defaultTabOf(DEFAULT_VIEW, defaultSubviewOf(DEFAULT_VIEW)),
    alias: null,
  });
  if (!path) return withParams(fallback());

  const [rawView, rawSub, rawTab] = path.split("/", 3);
  const view = String(rawView || "").toLowerCase();

  const dest = destinationById(view);
  if (!dest) return withParams(fallback()); // unknown hash -> the default destination, never a dead end

  const wantSub = String(rawSub || "").toLowerCase();
  const wantTab = String(rawTab || "").toLowerCase();

  // Exact subview match.
  if (dest.subviews.some((s) => s.id === wantSub)) {
    const tabs = tabsFor(view, wantSub);
    return withParams({
      view,
      subview: wantSub,
      tab: tabs.some((t) => t.id === wantTab) ? wantTab : defaultTabOf(view, wantSub),
      alias: null,
    });
  }

  // A subview id this destination used BEFORE a later step renamed it.
  const ls = dest.legacySubviews?.[wantSub];
  if (ls) {
    return withParams({
      view,
      subview: ls.subview,
      tab: ls.tab ?? defaultTabOf(view, ls.subview),
      alias: `${view}/${wantSub}`,
    });
  }

  const subview = defaultSubviewOf(view);
  return withParams({ view, subview, tab: defaultTabOf(view, subview), alias: null });
}

// formatHash builds the canonical hash for a (view, subview, tab). Destinations WITH subviews always
// carry theirs, and subviews WITH tabs carry theirs, so the URL is self-describing and shareable;
// only destinations without subviews (such as Calendar) stay bare.
export function formatHash(view, subview, tab, params = null) {
  const dest = destinationById(view);
  if (!dest) return DEFAULT_VIEW;
  let path = dest.id;
  if (dest.subviews.length) {
    const sub = dest.subviews.some((s) => s.id === subview) ? subview : defaultSubviewOf(view);
    const tabs = tabsFor(view, sub);
    path = tabs.length
      ? `${dest.id}/${sub}/${tabs.some((x) => x.id === tab) ? tab : defaultTabOf(view, sub)}`
      : `${dest.id}/${sub}`;
  }
  const query = new URLSearchParams();
  if (params?.event) query.set("event", String(params.event));
  if (params?.ticker) query.set("ticker", String(params.ticker).toUpperCase());
  const encoded = query.toString();
  return encoded ? `${path}?${encoded}` : path;
}

// baseViewOf strips the subview and tab. Used for the Google OAuth returnTo, which can only carry a
// single slash-free segment (see the HARD CONSTRAINT above) — the user lands on the destination's
// default section rather than being bounced to a fallback.
export function baseViewOf(route) {
  return String(route || "").replace(/^#/, "").split("/", 1)[0] || DEFAULT_VIEW;
}

// ---- level gating -------------------------------------------------------------------------------

export function navForLevel(level) {
  const r = rankOf(level);
  return DESTINATIONS.filter((d) => rankOf(d.minLevel) <= r);
}

export function isViewAllowed(viewId, level) {
  return navForLevel(level).some((d) => d.id === viewId);
}

export function subviewsForLevel(viewId, level) {
  const r = rankOf(level);
  return (destinationById(viewId)?.subviews || []).filter((s) => rankOf(s.minLevel) <= r);
}

// The route remains allowed even when it is not part of the destination's primary tab strip. This
// separates information architecture (two calm Research tabs) from specialist deep links.
export function navSubviewsForLevel(viewId, level) {
  return subviewsForLevel(viewId, level).filter((s) => !s.hiddenInNav);
}

export function isSubviewAllowed(viewId, subviewId, level) {
  const subs = subviewsForLevel(viewId, level);
  if (!subs.length) return true; // destination has no subviews (or none visible) -> nothing to gate
  return subs.some((s) => s.id === subviewId);
}

// resolveForLevel applies gating WITHOUT discarding intent: an out-of-level destination renders the
// default one and an out-of-level subview renders the first visible sibling, while the caller keeps
// the requested route in state — so graduating to Standard reveals it instead of losing it. Tabs are
// not level-gated (their parent subview already is).
export function resolveForLevel(viewId, subviewId, tabId, level) {
  const view = isViewAllowed(viewId, level) ? viewId : DEFAULT_VIEW;
  const subs = subviewsForLevel(view, level);
  if (!subs.length) return { view, subview: null, tab: null };
  const subview = subs.some((s) => s.id === subviewId) ? subviewId : subs[0].id;
  const tabs = tabsFor(view, subview);
  const tab = tabs.some((t) => t.id === tabId) ? tabId : defaultTabOf(view, subview);
  return { view, subview, tab };
}
