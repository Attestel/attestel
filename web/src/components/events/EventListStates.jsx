import { Button, EmptyState, Label, Skeleton } from "../ui/index.js";
import { Icon } from "../shell/icons.jsx";
import { cx } from "../../lib/cx.js";
import { degradedNotice } from "../../lib/eventsApi.js";

// EventListStates — the four states an event feed can be in, none of which is a blank panel.
//
// They live together in one file because they are one decision, not four: a feed is loading, or it
// has an answer, or it failed, or it is answering from less than it wanted to. Splitting them is
// how a surface ends up with three of the four and a white rectangle for the fourth.

// FeedSkeleton matches the CARD's real height so the page does not jump when the answer lands.
export function FeedSkeleton({ rows = 4, className }) {
  return (
    <div className={cx("flex flex-col gap-2", className)} aria-busy="true" aria-live="polite">
      {Array.from({ length: rows }).map((_, i) => (
        <div key={i} className="flex flex-col gap-2 rounded-xl border border-line bg-panel px-4 py-3">
          <Skeleton className="h-2.5 w-24" />
          <Skeleton className="h-4 w-3/4" />
          <Skeleton className="h-3 w-1/2" />
        </div>
      ))}
    </div>
  );
}

// FeedEmpty — AN EMPTY FEED IS A REAL ANSWER AND READS AS ONE.
//
// "No material events for these companies in this window" is the product working: Attestel looked
// and there was nothing material. Rendering that as a failure would teach the user to distrust the
// quiet days, which are most days.
export function FeedEmpty({ title, children, action, className }) {
  return (
    <EmptyState
      className={className}
      icon={<Icon name="today" size={22} />}
      title={title || "Nothing material in this window"}
      action={action}
    >
      {children || "No material events for these companies in this window."}
    </EmptyState>
  );
}

export function FeedError({ error, onRetry, className }) {
  return (
    <EmptyState
      className={className}
      tone="warn"
      icon={<Icon name="warning" size={22} />}
      title="Could not load events"
      action={
        onRetry && (
          <Button size="sm" onClick={onRetry}>
            Retry
          </Button>
        )
      }
    >
      {error?.message || "The request failed."}
    </EmptyState>
  );
}

// DegradedNotice renders what is ACTUALLY true, per signal, and keeps the feed usable underneath.
//
// It is `warn`, never silent, and never collapsed into a generic "some data may be missing": the
// difference between "the event service is down so these are raw headlines" and "you are signed out
// so this is the default universe" is the difference between two completely different answers.
export function DegradedNotice({ degraded = [], className, action }) {
  const codes = (degraded || []).filter(Boolean);
  if (!codes.length) return null;
  return (
    <div
      className={cx(
        "flex flex-wrap items-center gap-x-3 gap-y-1.5 rounded-xl border border-warn/40 bg-warn/[.07] px-3.5 py-2.5",
        className
      )}
    >
      <span className="flex-none text-warn">
        <Icon name="warning" size={15} />
      </span>
      <div className="flex min-w-0 flex-col gap-1">
        {codes.map((c) => (
          <p key={c} className="m-0 text-[13px] leading-[1.55] text-fg">
            {degradedNotice(c)}
          </p>
        ))}
      </div>
      {action && <div className="ml-auto flex-none">{action}</div>}
    </div>
  );
}

// ProvenanceLine reports where the rows actually came from. `dataState` and `origin` are per-item
// facts from `gateway/feeds.go`, and a feed mixing live events with embedded seed rows (§9.33's
// offline floor) must say so rather than presenting one uniform surface.
export function ProvenanceLine({ items = [], asOf, className }) {
  const states = new Set((items || []).map((i) => i?.dataState).filter(Boolean));
  const origins = new Set((items || []).map((i) => i?.origin).filter(Boolean));
  if (!states.size && !asOf) return null;
  const seed = states.has("seed");
  return (
    <Label
      as="p"
      tone={seed ? "warn" : "dim"}
      className={cx("!inline normal-case tracking-normal", className)}
    >
      {seed && "Includes embedded seed data, not today's market. "}
      {states.size > 0 && `Source: ${[...states].join(" · ")}`}
      {origins.size > 0 && ` · via ${[...origins].join(" · ")}`}
      {asOf && ` · as of ${asOf}`}
    </Label>
  );
}
