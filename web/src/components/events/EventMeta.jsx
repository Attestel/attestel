import { Label } from "../ui/index.js";
import { cx } from "../../lib/cx.js";
import { eventTypeLabel, relativeTime } from "../../lib/eventsApi.js";

// EventMeta — the ticker · type · time line that sits ABOVE the headline on every event surface.
//
// Its own file because Lane 4B's Explore card and this lane's Today digest need the identical
// vocabulary, and three components deciding independently how to spell `regulatory_action` is how a
// feed starts disagreeing with itself.
//
// TIER 3 OF THREE (docs §16.3, §16.13). Everything here is metadata: mono micro-label, `dim`,
// small. It exists to be scanned past. The headline is the thing on this card that a user reads,
// and if this line competes with it the card has failed.

// TickerList renders the affected companies. The FOLLOWED ones lead and carry the accent tint —
// `feedTicker.followed` is why the item is in this feed at all, and it is a different fact from
// `isPrimary`, which is whether the event is ABOUT that company.
export function TickerList({ tickers = [], className }) {
  if (!tickers.length) return null;
  const ordered = [...tickers].sort((a, b) => Number(b.followed) - Number(a.followed));
  return (
    <span className={cx("inline-flex flex-wrap items-center gap-x-1.5", className)}>
      {ordered.map((t, i) => (
        <span key={t.ticker || i} className="inline-flex items-center">
          {i > 0 && <span className="mr-1.5 text-dim">·</span>}
          <span className={t.followed ? "text-accent" : "text-dim"}>{t.ticker}</span>
        </span>
      ))}
    </span>
  );
}

export function EventMeta({ event, className }) {
  const type = eventTypeLabel(event?.eventType);
  // `occurredAt` is when it happened; `publishedAt` is when it was reported. Prefer the former and
  // fall back, so an item with no occurrence time still carries an honest timestamp.
  const when = relativeTime(
    event?.occurredAtISO || event?.occurredAt || event?.publishedAtISO || event?.publishedAt
  );
  return (
    <div className={cx("flex flex-wrap items-center gap-x-2 gap-y-1", className)}>
      <Label as="span" className="!inline">
        <TickerList tickers={event?.tickers} />
        {type && (
          <>
            <span className="mx-1.5 text-dim">·</span>
            <span className="text-dim">{type}</span>
          </>
        )}
      </Label>
      {when && (
        <Label as="span" className="!inline ml-auto whitespace-nowrap">
          {when}
        </Label>
      )}
    </div>
  );
}

// SourceLine is the evidentiary footer: how well confirmed, and on how much.
//
// `officialConfirmed` is the TOP of the source hierarchy (§3.1) and the thing a user should trust
// most, so it gets words and the `filing` provenance colour rather than a bare tick. `documentCount`
// is a POINTER server-side (`gateway/feeds.go`) — absent on the degraded cascade, where the service
// that counts documents is the thing that is down — so it is only rendered when it actually exists.
export function SourceLine({ event, className }) {
  const parts = [];
  if (event?.officialConfirmed) parts.push("Officially confirmed");
  if (typeof event?.documentCount === "number") {
    parts.push(`${event.documentCount} ${event.documentCount === 1 ? "source" : "sources"}`);
  }
  if (event?.sourceTier && !event?.officialConfirmed) {
    parts.push(`${event.sourceTier === "official" ? "Official" : event.sourceTier === "professional" ? "Professional" : "Discussion"} source`);
  }
  if (!parts.length) return null;
  return (
    <Label as="span" className={cx("!inline", className)}>
      <span className={event?.officialConfirmed ? "text-prov-filing" : "text-dim"}>{parts[0]}</span>
      {parts.slice(1).map((p) => (
        <span key={p}>
          <span className="mx-1.5 text-dim">·</span>
          <span className="text-dim">{p}</span>
        </span>
      ))}
    </Label>
  );
}
