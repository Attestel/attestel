import { EmptyState } from "../ui/index.js";
import { Micro, Tag } from "../terminal/bits.jsx";
import { SectionShell } from "./Placeholder.jsx";
import { relDay, daysUntil } from "../../lib/format.js";
import { cx } from "../../lib/cx.js";

// CatalystsPanel — the verified seed material that had no renderer before: dated catalysts, the supplier
// chain, the known risk list, and where management actually speaks.
//
// All of it is the curated, human-verified seed (docs/NVIDIA_RESEARCH.md), NVDA-only today, and every block
// is labelled as such. Descriptive facts and known failure paths — a risk list is not a recommendation, and
// a date is not a trigger.

const CRIT_TONE = { critical: "down", high: "warn", medium: "outline", low: "outline" };

export default function CatalystsPanel({ ticker, catalysts, hasSeed }) {
  const events = Array.isArray(catalysts?.events) ? catalysts.events : [];
  const suppliers = Array.isArray(catalysts?.suppliers) ? catalysts.suppliers : [];
  const risks = Array.isArray(catalysts?.risks) ? catalysts.risks : [];
  const monitor = catalysts?.monitor || null;

  if (!hasSeed) {
    return (
      <SectionShell title={`${ticker} · Catalysts & risks`} tags={<Tag tone="outline">no seed</Tag>}>
        <div className="px-[22px] py-5">
          <EmptyState title={`No curated catalyst data for ${ticker}`}>
            The dated catalyst list, supply chain, and risk register are hand-verified and currently exist
            for NVDA only. Live news, filings, and the economic calendar still cover {ticker}.
          </EmptyState>
        </div>
      </SectionShell>
    );
  }

  const upcoming = events.filter((e) => (daysUntil(e?.date) ?? -1) >= 0);
  const past = events.filter((e) => (daysUntil(e?.date) ?? -1) < 0).slice(0, 6);

  return (
    <SectionShell
      title={`${ticker} · Catalysts & risks`}
      tags={<Tag tone="outline">verified seed</Tag>}
      note="Curated reference facts, not a live feed. Risks are known failure paths to watch — never a recommendation."
    >
      {upcoming.length > 0 && (
        <>
          <GroupHeader label="Upcoming catalysts" />
          <ul className="divide-y divide-line">
            {upcoming.map((e, i) => (
              <EventRow key={`u-${i}`} e={e} />
            ))}
          </ul>
        </>
      )}

      {past.length > 0 && (
        <>
          <GroupHeader label="Recent catalysts" />
          <ul className="divide-y divide-line">
            {past.map((e, i) => (
              <EventRow key={`p-${i}`} e={e} />
            ))}
          </ul>
        </>
      )}

      {risks.length > 0 && (
        <>
          <GroupHeader label="Known risks" tone="warn" />
          <ul className="divide-y divide-line">
            {risks.map((r, i) => (
              <li key={i} className="px-[22px] py-2.5 text-[12.5px] leading-relaxed text-fg/80">
                {typeof r === "string" ? r : r?.note || JSON.stringify(r)}
              </li>
            ))}
          </ul>
        </>
      )}

      {suppliers.length > 0 && (
        <>
          <GroupHeader label="Supply chain" />
          <ul className="divide-y divide-line">
            {suppliers.map((s, i) => (
              <li key={i} className="flex flex-wrap items-start gap-x-2.5 gap-y-1 px-[22px] py-2.5">
                <span className="num shrink-0 text-[12.5px] font-semibold text-fg">{s.name}</span>
                {s.criticality && <Tag tone={CRIT_TONE[s.criticality] || "outline"}>{s.criticality}</Tag>}
                <span className="text-[11.5px] text-muted/80">{s.role}</span>
                {s.note && <p className="w-full text-[11.5px] leading-relaxed text-muted">{s.note}</p>}
              </li>
            ))}
          </ul>
        </>
      )}

      {monitor && (
        <>
          <GroupHeader label="Where management speaks" />
          <div className="px-[22px] py-3">
            {monitor.note && <p className="text-[12px] leading-relaxed text-muted">{monitor.note}</p>}
            {Array.isArray(monitor.eventCadence) && monitor.eventCadence.length > 0 && (
              <div className="mt-2.5 flex flex-wrap gap-1.5">
                {monitor.eventCadence.map((c, i) => (
                  <Tag key={i} tone="outline">
                    {c}
                  </Tag>
                ))}
              </div>
            )}
            {Array.isArray(monitor.channels) && monitor.channels.length > 0 && (
              <ul className="mt-3 flex flex-col gap-1.5">
                {monitor.channels.map((c, i) => (
                  <li key={i} className="text-[12px]">
                    {c.url ? (
                      <a
                        href={c.url}
                        target="_blank"
                        rel="noopener noreferrer"
                        className="text-accent transition-[filter] hover:brightness-125"
                      >
                        {c.name}
                      </a>
                    ) : (
                      <span className="text-fg/85">{c.name}</span>
                    )}
                    {c.confidence && <span className="ml-2 text-[11px] text-muted/70">{c.confidence}</span>}
                  </li>
                ))}
              </ul>
            )}
          </div>
        </>
      )}

      {events.length === 0 && risks.length === 0 && suppliers.length === 0 && !monitor && (
        <div className="px-[22px] py-5">
          <EmptyState title="No catalyst data loaded">
            The dashboard did not return seed catalysts — the gateway may be unreachable.
          </EmptyState>
        </div>
      )}
    </SectionShell>
  );
}

function GroupHeader({ label, tone }) {
  return (
    <div className="flex h-8 items-center border-y border-line bg-panel2/30 px-[22px]">
      <Micro tone={tone}>{label}</Micro>
    </div>
  );
}

function EventRow({ e }) {
  const n = daysUntil(e?.date);
  return (
    <li className="flex items-start gap-3 px-[22px] py-2.5">
      <div className="w-[76px] shrink-0">
        <div className="num text-[11.5px] font-semibold text-fg/85">{e.date}</div>
        <div className="text-[10.5px] text-muted/70">{relDay(e.date)}</div>
      </div>
      <span
        aria-hidden="true"
        className={cx(
          "mt-1.5 h-1.5 w-1.5 shrink-0 rounded-full",
          e.impact === "high" ? "bg-warn" : "bg-muted/40"
        )}
      />
      <div className="min-w-0 flex-1">
        <div className="text-[12.5px] leading-snug text-fg/85">{e.title}</div>
        {e.note && <div className="mt-0.5 text-[11px] leading-relaxed text-muted">{e.note}</div>}
      </div>
      {e.type && (
        <span className="shrink-0">
          <Tag tone="outline">{e.type}</Tag>
        </span>
      )}
    </li>
  );
}
