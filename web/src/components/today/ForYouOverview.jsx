import { rankTheses, recentEvents } from "../../lib/attention.js";
import { Icon } from "../shell/icons.jsx";
import { SkeletonText } from "../ui/index.js";
import { Tag } from "../terminal/bits.jsx";

function Metric({ value, label, tone = "text-fg" }) {
  return (
    <div className="rounded-2xl border border-line bg-panel2/35 px-4 py-3">
      <div className={`num text-[22px] font-semibold tracking-[-0.03em] ${tone}`}>{value}</div>
      <div className="mt-1 text-[11px] leading-snug text-muted">{label}</div>
    </div>
  );
}

export default function ForYouOverview({
  user,
  theses,
  thesesStatus,
  events,
  eventsStatus,
  monitor,
  monitorStatus,
  onRefresh,
}) {
  const review = thesesStatus === "ready" ? rankTheses(theses) : [];
  const alerts = eventsStatus === "ready" ? recentEvents(events) : [];
  const challenged = review.filter((item) => item.reason === "challenged").length;
  const unchecked = review.filter((item) => item.reason === "unchecked" || item.reason === "stale").length;
  const loading = thesesStatus === "loading" || eventsStatus === "loading" || monitorStatus === "loading";
  const staleMarkers = monitorStatus === "ready" ? (monitor?.markers || []).filter((m) => m.stale) : [];
  const thesisCountsAvailable = Boolean(user) && thesesStatus === "ready";
  const alertCountAvailable = Boolean(user) && eventsStatus === "ready";

  return (
    <section className="overflow-hidden surface-card-blue">
      <div className="flex flex-wrap items-center gap-2 border-b border-line px-[22px] py-3">
        <Tag tone="accent">personal research</Tag>
        <span className="text-[11px] text-muted">saved theses · review commitments · fired alerts</span>
        <button
          type="button"
          onClick={onRefresh}
          disabled={loading}
          className="ml-auto flex items-center gap-1.5 text-[12px] text-muted transition-colors hover:text-fg disabled:opacity-50"
        >
          <Icon name="reload" size={13} className={loading ? "motion-safe:animate-spin" : ""} />
          {loading ? "Refreshing…" : "Refresh"}
        </button>
      </div>

      <div className="grid gap-5 px-[22px] py-5 lg:grid-cols-[minmax(0,1fr)_minmax(300px,.72fr)] lg:items-end">
        <div>
          <h2 className="max-w-[780px] text-[clamp(27px,4vw,44px)] font-[550] leading-[1.04] tracking-[-0.045em] text-fg">
            {user ? "Your research, reordered by new evidence." : "A research feed that starts with what you believe."}
          </h2>
          <p className="mt-3 max-w-[68ch] text-[13.5px] leading-[1.65] text-muted">
            This feed uses only the theses you saved, the reviews you scheduled and monitoring conditions that
            actually fired. It does not pretend to remember activity you never recorded.
          </p>
        </div>

        {loading ? (
          <SkeletonText lines={3} />
        ) : (
          <div className="grid grid-cols-2 gap-2.5 sm:grid-cols-4">
            <Metric
              value={thesisCountsAvailable ? challenged : "—"}
              label="challenged theses"
              tone={challenged ? "text-down" : "text-fg"}
            />
            <Metric
              value={thesisCountsAvailable ? unchecked : "—"}
              label="checks due"
              tone={unchecked ? "text-warn" : "text-fg"}
            />
            <Metric
              value={alertCountAvailable ? alerts.length : "—"}
              label="recent alerts"
              tone={alerts.length ? "text-accent" : "text-fg"}
            />
            <Metric
              value={user && monitorStatus === "ready" ? staleMarkers.length : "—"}
              label="stale thesis markers"
              tone={staleMarkers.length ? "text-warn" : "text-fg"}
            />
          </div>
        )}
      </div>
    </section>
  );
}
