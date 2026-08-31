import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Button, Field, Label, Panel, controlClass } from "../ui/index.js";
import { DestinationHeader } from "../shell/DestinationHeader.jsx";
import { searchTickers } from "../../lib/api.js";
import { cx } from "../../lib/cx.js";
import {
  EVENT_TYPE_LABELS,
  MIN_IMPORTANCE_HIGH,
  MIN_IMPORTANCE_MATERIAL,
  fetchEventStates,
  fetchFollowing,
  followTicker,
  listSubscriptions,
  setEventState,
} from "../../lib/eventsApi.js";
import EventCard from "../events/EventCard.jsx";
import TickerChipBar from "../events/TickerChipBar.jsx";
import WhatChangedPanel from "../events/WhatChangedPanel.jsx";
import ThesisReviewQueue from "../events/ThesisReviewQueue.jsx";
import { useAuth } from "../../auth/AuthContext.jsx";
import {
  DegradedNotice,
  FeedEmpty,
  FeedError,
  FeedSkeleton,
  ProvenanceLine,
} from "../events/EventListStates.jsx";

// FollowingView — doc §16.2. "What happened to the companies I follow?", answered in CHANGES.
//
// ONE EVENT, ONE ROW. A canonical event matching three followed tickers is a single card carrying
// three ticker chips. The gateway already deduplicates into events; rendering the same event once
// per matching ticker would reintroduce by hand exactly the duplication this whole programme exists
// to remove.
//
// THE FEED DOES NOT UNMOUNT WHEN AN EVENT IS SELECTED (doc §16.4). Selection is local state and the
// detail surface renders BESIDE the list on desktop, so "scan → select → understand → return" never
// loses the user's place in the feed.

// The three filters, all SERVER-SIDE — every one of them is a query parameter, not a client-side
// slice of an already-fetched page. Filtering after the fact would silently narrow the wrong set:
// the page is a cursor window over the matched set, not the matched set.
const SINCE_OPTIONS = [
  { id: "24h", label: "Last 24 hours", seconds: 24 * 60 * 60 },
  { id: "7d", label: "Last 7 days", seconds: 7 * 24 * 60 * 60 },
  { id: "30d", label: "Last 30 days", seconds: 30 * 24 * 60 * 60 },
];

// Importance is presented as WORDS and never as the float behind it (AD-8). The values are
// `eventsApi`'s two named thresholds, which are in turn the gateway's own band edges — so
// "Material and above" returns exactly the set the server counts as material-or-higher.
const IMPORTANCE_OPTIONS = [
  { id: "all", label: "All importance", min: 0 },
  { id: "material", label: "Material and above", min: MIN_IMPORTANCE_MATERIAL },
  { id: "high", label: "High impact only", min: MIN_IMPORTANCE_HIGH },
];

const TYPE_OPTIONS = [
  { id: "", label: "All events" },
  ...Object.entries(EVENT_TYPE_LABELS).map(([id, label]) => ({ id, label })),
];

function Select({ label, value, onChange, options }) {
  return (
    <Field label={label} className="min-w-[150px] flex-none">
      <select
        className={cx(controlClass, "cursor-pointer py-2 text-[12.5px]")}
        value={value}
        onChange={(e) => onChange(e.target.value)}
      >
        {options.map((o) => (
          <option key={o.id} value={o.id}>
            {o.label}
          </option>
        ))}
      </select>
    </Field>
  );
}

// AddFollow — ONE CLICK to follow (doc §16.8). No notification-preference step in front of the
// action: advanced choices come after the company is added, never before. `followTicker` is
// idempotent at the server, so re-adding an existing ticker is a no-op that returns the real record.
function AddFollow({ onFollowed, onClose }) {
  const [query, setQuery] = useState("");
  const [results, setResults] = useState([]);
  const [busy, setBusy] = useState("");
  const [error, setError] = useState(null);

  useEffect(() => {
    const q = query.trim();
    if (!q) {
      setResults([]);
      return;
    }
    const ctl = new AbortController();
    const t = setTimeout(() => {
      searchTickers(q, { signal: ctl.signal })
        .then((r) => setResults((r || []).slice(0, 6)))
        .catch(() => {});
    }, 180);
    return () => {
      clearTimeout(t);
      ctl.abort();
    };
  }, [query]);

  const follow = async (ticker) => {
    setBusy(ticker);
    setError(null);
    try {
      await followTicker(ticker);
      onFollowed?.(ticker);
      setQuery("");
      setResults([]);
    } catch (e) {
      setError(e);
    } finally {
      setBusy("");
    }
  };

  return (
    <Panel className="mt-2">
      <div className="flex flex-col gap-2.5">
        <Field label="Follow a company">
          <input
            autoFocus
            className={controlClass}
            placeholder="Search ticker or name"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
          />
        </Field>
        {results.length > 0 && (
          <ul className="m-0 flex list-none flex-col gap-1 p-0">
            {results.map((r) => {
              const sym = r.ticker || r.symbol || r;
              return (
                <li key={sym} className="flex items-center gap-2">
                  <span className="font-mono text-[11px] uppercase tracking-[.1em] text-fg">
                    {sym}
                  </span>
                  <span className="min-w-0 flex-1 truncate text-[13px] text-muted">
                    {r.name || ""}
                  </span>
                  <Button size="xs" onClick={() => follow(sym)} disabled={busy === sym}>
                    {busy === sym ? "Following…" : "Follow"}
                  </Button>
                </li>
              );
            })}
          </ul>
        )}
        {error && (
          <Label as="p" tone="warn" className="!inline normal-case tracking-normal">
            {error.message}
          </Label>
        )}
        <div className="flex justify-end">
          <Button size="xs" variant="ghost" onClick={onClose}>
            Done
          </Button>
        </div>
      </div>
    </Panel>
  );
}

export default function FollowingView({ level, onOpenCompany, renderDetail }) {
  const { user } = useAuth();
  const [subscriptions, setSubscriptions] = useState([]);
  const [selected, setSelected] = useState([]);
  const [typeFilter, setTypeFilter] = useState("");
  const [importanceFilter, setImportanceFilter] = useState("all");
  const [sinceFilter, setSinceFilter] = useState("24h");
  const [adding, setAdding] = useState(false);

  const [feed, setFeed] = useState({ status: "loading" });
  const [pages, setPages] = useState([]);
  const [cursor, setCursor] = useState("");
  const [nonce, setNonce] = useState(0);
  const [states, setStates] = useState({});
  const [selectedEventId, setSelectedEventId] = useState(null);

  // Focus restoration (doc §16.4): closing the detail returns focus to the card that opened it, so
  // a keyboard user is not dropped back at the top of a forty-row feed.
  const cardRefs = useRef(new Map());

  // A filter change resets the cursor AT THE POINT OF CHANGE: paging is only meaningful within one
  // query. Resetting it in a follow-up effect instead would leave one request for the OLD cursor,
  // under the NEW filters, in flight on every single filter change.
  const changeFilter = (setter) => (value) => {
    setCursor("");
    setter(value);
  };

  useEffect(() => {
    let alive = true;
    listSubscriptions()
      .then((subs) => alive && setSubscriptions(subs))
      .catch(() => alive && setSubscriptions([]));
    return () => {
      alive = false;
    };
  }, [nonce]);

  const sinceSeconds = useMemo(
    () => SINCE_OPTIONS.find((o) => o.id === sinceFilter)?.seconds || 0,
    [sinceFilter]
  );
  const minImportance = useMemo(
    () => IMPORTANCE_OPTIONS.find((o) => o.id === importanceFilter)?.min || 0,
    [importanceFilter]
  );

  useEffect(() => {
    let alive = true;
    if (!cursor) setFeed({ status: "loading" });
    fetchFollowing({
      tickers: selected,
      types: typeFilter ? [typeFilter] : [],
      minImportance,
      since: sinceSeconds ? Math.floor(Date.now() / 1000) - sinceSeconds : 0,
      limit: 50,
      cursor,
    })
      .then((data) => {
        if (!alive) return;
        setFeed({ status: "ready", data });
        setPages((prev) => (cursor ? [...prev, data.items || []] : [data.items || []]));
      })
      .catch((err) => alive && setFeed({ status: "error", error: err }));
    return () => {
      alive = false;
    };
  }, [selected, typeFilter, minImportance, sinceSeconds, cursor, nonce]);

  const items = useMemo(() => pages.flat(), [pages]);

  // Read/unread comes from the server (§2.3), fetched for the ids actually on screen. It is never
  // inferred from scrolling.
  useEffect(() => {
    const ids = items.map((i) => i.id).filter(Boolean);
    if (!ids.length) return;
    let alive = true;
    fetchEventStates(ids)
      .then((list) => {
        if (!alive) return;
        setStates((prev) => {
          const next = { ...prev };
          for (const s of list) next[s.eventId] = s;
          return next;
        });
      })
      .catch(() => {});
    return () => {
      alive = false;
    };
  }, [items]);

  // Marking read is fired when the user OPENS the event — a deliberate act — and never on scroll.
  const openEvent = useCallback((id) => {
    setSelectedEventId(id);
    setStates((prev) => ({ ...prev, [id]: { ...(prev[id] || {}), eventId: id, read: true } }));
    setEventState(id, { read: true }).catch(() => {});
  }, []);

  const closeDetail = useCallback(() => {
    const id = selectedEventId;
    setSelectedEventId(null);
    // Return focus to the card that opened the detail.
    requestAnimationFrame(() => cardRefs.current.get(id)?.focus?.());
  }, [selectedEventId]);

  const data = feed.data || {};
  const tickerUniverse = useMemo(() => {
    const fromSubs = subscriptions.map((s) => s.ticker).filter(Boolean);
    // `data.tickers` is the scope the SERVER actually used — for a guest that is the default
    // universe, which is a different set from "your subscriptions" and must not be presented as one.
    return fromSubs.length ? fromSubs : data.tickers || [];
  }, [subscriptions, data.tickers]);

  const isGuest = (data.degraded || []).includes("subscriptions:guest");
  const counts = data.counts || {};

  const body = (() => {
    if (feed.status === "loading") return <FeedSkeleton />;
    if (feed.status === "error")
      return <FeedError error={feed.error} onRetry={() => setNonce((n) => n + 1)} />;
    if (!items.length)
      return (
        <FeedEmpty
          title="Nothing material in this window"
          action={
            sinceFilter !== "30d" && (
              <Button size="sm" onClick={() => changeFilter(setSinceFilter)("30d")}>
                Widen to 30 days
              </Button>
            )
          }
        >
          No material events for these companies in this window.
        </FeedEmpty>
      );
    return (
      <div className="flex flex-col gap-2">
        {items.map((item) => (
          <div
            key={item.id}
            ref={(el) => {
              // Query the activatable element rather than assuming `firstChild` is it: focus
              // restoration silently breaks if the card ever gains a wrapper.
              if (el) cardRefs.current.set(item.id, el.querySelector('[role="button"]'));
              else cardRefs.current.delete(item.id);
            }}
          >
            <EventCard
              event={item}
              read={Boolean(states[item.id]?.read)}
              onOpenEvent={openEvent}
              onOpenCompany={onOpenCompany}
              // §9.18: the disclosure the SERVER sent with this feed, threaded to the chips it
              // qualifies. Absent ⇒ EventCard's ImpactChips withhold the directions and say so.
              disclosure={data.disclaimer}
              className={selectedEventId === item.id ? "border-accent/40" : undefined}
            />
          </div>
        ))}
        {/* Explicit paging, never infinite scroll: infinite scroll fights "scan and select" and
            destroys the focus restoration the detail panel depends on. */}
        {data.nextCursor && (
          <div className="flex justify-center pt-1">
            <Button size="sm" onClick={() => setCursor(data.nextCursor)}>
              Load more
            </Button>
          </div>
        )}
      </div>
    );
  })();

  return (
    <div className="flex flex-col gap-3">
      {/* `following` is in DESTINATIONS as of Wave 4 integration, so `DestinationHeader` resolves
          its folio and title from `lib/routes.js` and yields `02 · Monitor` automatically. The
          hand-written PageHeader fallback that stood here became dead code the moment the route
          landed and is deleted — a second source of a destination's title is how two of them drift. */}
      <DestinationHeader view="following" className="mb-1" />

      {/* Doc §16.5 / §15.12 as a FIRST-CLASS object, mounted here at integration.
          It shipped in Lane 4A imported by nothing, which is why `make check-web-bundle` never saw
          a byte of it: Rollup tree-shook the whole module out and the gate passed on its absence.
          Following is its home — it is the CROSS-PORTFOLIO change record ("Across your companies"),
          and it is the sole WRITER of the last-check boundary, which belongs on the destination the
          user visits to check. TodayDigest only reads that boundary, so the two cannot race. */}
      <WhatChangedPanel
        uid={user?.id}
        onOpenEvent={openEvent}
        onOpenCompany={onOpenCompany}
        className="mb-1"
      />

      {/* Phase 3. The forward half of the same question. WhatChangedPanel answers "what already
          happened across my companies"; this answers "what is COMING that touches something I
          wrote down". Both are deterministic, both are reads, and neither can start a model. */}
      <ThesisReviewQueue />

      <TickerChipBar
        tickers={tickerUniverse}
        selected={selected}
        onChange={changeFilter(setSelected)}
        actions={
          <Button size="sm" onClick={() => setAdding((v) => !v)}>
            + Add
          </Button>
        }
      />
      {adding && (
        <AddFollow
          onFollowed={() => {
            setAdding(false);
            setNonce((n) => n + 1);
          }}
          onClose={() => setAdding(false)}
        />
      )}

      <div className="flex flex-wrap items-end gap-2">
        <Select
          label="Type"
          value={typeFilter}
          onChange={changeFilter(setTypeFilter)}
          options={TYPE_OPTIONS}
        />
        <Select
          label="Importance"
          value={importanceFilter}
          onChange={changeFilter(setImportanceFilter)}
          options={IMPORTANCE_OPTIONS}
        />
        <Select
          label="Window"
          value={sinceFilter}
          onChange={changeFilter(setSinceFilter)}
          options={SINCE_OPTIONS.map((o) => ({ id: o.id, label: o.label }))}
        />
      </div>

      <DegradedNotice degraded={data.degraded} />

      {/* The counts header describes the WHOLE matched set, which is why it can legitimately read
          higher than the rows on screen. These are counts of events and companies — never scores. */}
      {feed.status === "ready" && items.length > 0 && (
        <div
          className="flex flex-wrap items-baseline gap-x-3"
          aria-live="polite"
          aria-label="Feed summary"
        >
          <Label as="span" className="!inline">
            {counts.total ?? items.length} events
            {counts.companiesAffected != null && ` · ${counts.companiesAffected} companies affected`}
            {selected.length > 0 && ` · filtered to ${selected.join(", ")}`}
            {isGuest && " · default universe"}
          </Label>
        </div>
      )}

      {/* Feed and detail SIDE BY SIDE on desktop: the feed keeps its scroll position and its
          selection, which is the §16.4 requirement this lane is judged on. */}
      <div className={cx("flex gap-3", selectedEventId ? "flex-col xl:flex-row" : "flex-col")}>
        <div className="min-w-0 flex-1">{body}</div>
        {selectedEventId && (
          <aside className="min-w-0 xl:w-[420px] xl:flex-none">
            {renderDetail ? (
              renderDetail(selectedEventId, {
                onClose: closeDetail,
                // §9.18, threaded from the feed response that produced this row.
                disclosure: data.disclaimer,
              })
            ) : (
              // Local placeholder ONLY. Lane 4B ships the real `EventDetailPanel`; this file never
              // imports it, and the integrator passes it in through `renderDetail`.
              <Panel
                variant="deep"
                title="Event"
                actions={
                  <Button size="xs" variant="ghost" onClick={closeDetail}>
                    Close
                  </Button>
                }
              >
                <Label as="p" className="!inline normal-case tracking-normal">
                  Event detail is provided by the integrator. Selected: {selectedEventId}
                </Label>
              </Panel>
            )}
          </aside>
        )}
      </div>

      <ProvenanceLine items={items} asOf={data.asOf} />
    </div>
  );
}
