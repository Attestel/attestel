import { Badge, Label } from "../ui/index.js";
import { cx } from "../../lib/cx.js";
import { BAND_LABELS, importanceBand, isScored } from "../../lib/eventsApi.js";
import { EventMeta, SourceLine } from "./EventMeta.jsx";
import { ImpactChips } from "./ImpactChips.jsx";

// EventCard — doc §16.3's three-level card, as ONE component.
//
// The level is derived from `importanceBand(event)` and nothing else. Three components reading
// `importance > 0.7` in three different files is how the feed starts disagreeing with itself, so
// there is exactly one rule and it lives in `eventsApi.js` beside the two named thresholds.
//
//   normal    ticker(s), type, timestamp, title, one source badge
//   material  + "why it matters" + per-ticker direction
//   high      + severity band, official-confirmation line, all tickers, a detail action
//
// THREE TIERS OF TYPE, NOT ONE (§16.3, §16.13):
//   1. the HEADLINE — `fg`, 15px, weight 550. The largest, brightest thing on the card.
//   2. "why it matters" and impact — `muted`, 13px body.
//   3. metadata — mono micro-label, `dim`, 10px. Ticker, type, time, sources.
//
// NO WHOLE-CARD COLOUR WASH. A high-impact card earns at most a `warn` severity pill and a 2px
// `warn` left hairline. Not a second gradient, not a different radius, not a glow — §16.13 is
// explicit that repeated colour stops communicating state and starts being decoration.

// WhyItMatters is labelled as an INTERPRETATION, always.
//
// The contract stores `whyItMatters` in a separate column from `keyFacts` precisely so a later
// backtest can tell evidence from interpretation (doc §15.7). Rendering it as an unattributed
// sentence would erase that distinction on screen, so it always carries its label — and where the
// event was enriched by a model (`audit.modelUsed`), the `llm` provenance tint as well.
function WhyItMatters({ event }) {
  if (!event?.whyItMatters) return null;
  const modelAuthored = Boolean(event?.audit?.modelUsed);
  return (
    <div className="flex flex-col gap-1">
      <Label as="span" tone={modelAuthored ? "llm" : "dim"} className="!inline">
        {modelAuthored ? "Interpretation · model-authored" : "Interpretation"}
      </Label>
      <p className="m-0 max-w-[76ch] text-[13px] leading-[1.6] text-muted">{event.whyItMatters}</p>
    </div>
  );
}

export default function EventCard({
  event,
  read = false,
  onOpenEvent,
  onOpenCompany,
  // disclosure — §9.18's server string, from the feed response that produced this card. Threaded,
  // never held locally: with none, ImpactChips withholds the direction chips and says so.
  disclosure = "",
  className,
}) {
  if (!event) return null;
  const band = importanceBand(event);
  // "high" | "medium" | "low" — the SERVER's band vocabulary, served on the item (§AD-8). The
  // client no longer knows the thresholds, so it cannot disagree with the header counts about them.
  const isHigh = band === "high";
  const isMaterial = band === "medium" || isHigh;

  const open = () => onOpenEvent?.(event.id);

  return (
    <article
      role="button"
      tabIndex={0}
      aria-label={event.title}
      onClick={open}
      onKeyDown={(e) => {
        // Enter and Space activate, exactly as a real button does. Space is prevented so the feed
        // does not scroll out from under the card the user just chose.
        if (e.key === "Enter" || e.key === " ") {
          e.preventDefault();
          open();
        }
      }}
      className={cx(
        "group relative flex w-full cursor-pointer flex-col gap-2 rounded-xl border border-line bg-panel px-4 py-3 text-left",
        "transition-colors duration-200 ease-premium hover:border-line2 hover:bg-panel2",
        // The severity hairline. `warn`, never `down` — HIGH IMPACT is a band, not a bearing.
        isHigh && "border-l-2 border-l-warn",
        className
      )}
    >
      {/* Row 1 — severity and time. */}
      <div className="flex items-start gap-2">
        {!read && (
          // Unread. 6px accent dot; `accent` is brand/state, which is exactly what unread is.
          <span
            className="mt-[6px] h-1.5 w-1.5 flex-none rounded-full bg-accent"
            aria-label="Unread"
            role="img"
          />
        )}
        {isHigh && <Badge variant="warn">{BAND_LABELS.high}</Badge>}
        {band === "medium" && <Badge variant="outline">{BAND_LABELS.medium}</Badge>}
        {/* An item the server never scored says so, rather than borrowing "Routine" — absent is
            not zero, and the degraded cascade is where this happens. */}
        {!isScored(event) && <Badge variant="dim">Unscored</Badge>}
      </div>

      <EventMeta event={event} />

      {/* Row 3 — THE HEADLINE. Dominant on all three levels.
          A read event steps down in WEIGHT, never to `dim`: `dim` on `bg` is ~4.3:1 and would fail
          AA on the one string the user actually has to read. */}
      <h3
        className={cx(
          "m-0 max-w-[80ch] text-[15px] leading-[1.35] tracking-[-0.01em] text-fg",
          read ? "font-normal" : "font-[550]"
        )}
      >
        {event.title}
      </h3>

      {isMaterial && <WhyItMatters event={event} />}
      {isMaterial && <ImpactChips tickers={event.tickers} disclosure={disclosure} />}

      {/* High impact earns the evidentiary line and a direct action. */}
      {isHigh && (
        <div className="mt-0.5 flex flex-wrap items-center justify-between gap-2">
          <SourceLine event={event} />
          <span
            aria-hidden="true"
            className="font-mono text-[10px] uppercase tracking-[.12em] text-muted transition-colors group-hover:text-fg"
          >
            View event →
          </span>
        </div>
      )}
      {!isHigh && <SourceLine event={event} />}

      {/* Nested control: opening the company is a DIFFERENT action from opening the event, so it
          stops propagation and carries its own name and focus ring in the normal tab order. */}
      {onOpenCompany && event.tickers?.length > 0 && (
        <div className="flex flex-wrap gap-1.5">
          {event.tickers
            .filter((t) => t.followed)
            .map((t) => (
              <button
                key={t.ticker}
                type="button"
                onClick={(e) => {
                  e.stopPropagation();
                  onOpenCompany(t.ticker);
                }}
                onKeyDown={(e) => e.stopPropagation()}
                className="rounded-full border border-line px-2 py-[3px] font-mono text-[9px] uppercase tracking-[.12em] text-dim transition-colors hover:border-line2 hover:text-fg"
              >
                Open {t.ticker}
              </button>
            ))}
        </div>
      )}
    </article>
  );
}
