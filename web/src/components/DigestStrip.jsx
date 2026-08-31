import { useEffect, useState } from "react";
import { fetchDigest } from "../lib/api.js";
import { Icon } from "./shell/icons.jsx";

// DigestStrip — the news-cascade digest above the raw feed: the model clusters related headlines
// into stories, rates each story's potential impact (low/medium/high, with a one-line why), and
// compresses the day into three sentences. Clusters reference only supplied headlines (validated
// server-side). News TRIAGE — never advice. Cached hard at the gateway; fails soft.

const IMPACT_TONE = {
  high: "text-down border-down/40 bg-down/10",
  medium: "text-warn border-warn/40 bg-warn/10",
  low: "text-muted border-line bg-panel2",
};

export default function DigestStrip({ ticker }) {
  const [data, setData] = useState(null);
  const [loading, setLoading] = useState(true);

  const load = (refresh = false) => {
    setLoading(true);
    fetchDigest(ticker, refresh)
      .then(setData)
      .catch(() => setData(null))
      .finally(() => setLoading(false));
  };
  useEffect(() => {
    setData(null);
    load();
  }, [ticker]); // eslint-disable-line react-hooks/exhaustive-deps

  const s = data?.structured;
  const items = data?.items || [];
  if (!loading && (!data || data.available === false)) return null; // fail soft, raw feed still renders

  const stub = /stub/i.test(String(data?.modelUsed || ""));
  const clusters = [...(s?.clusters || [])].sort(
    (a, b) => ({ high: 0, medium: 1, low: 2 }[a.impact] ?? 3) - ({ high: 0, medium: 1, low: 2 }[b.impact] ?? 3)
  );

  return (
    <section className="mb-5 overflow-hidden surface-card">
      <div className="flex items-start gap-3 border-b border-line px-[22px] py-3">
        <p className="flex-1 text-[12.5px] leading-[1.6] text-muted">
          <strong className="font-semibold text-info">News cascade.</strong> The feed below, clustered into stories
          and impact-rated by the AI — triage, not advice.
          {stub && <span className="ml-2 rounded border border-warn/40 bg-warn/10 px-1.5 py-0.5 text-[10px] font-semibold text-warn">OFFLINE STUB</span>}
        </p>
        <button type="button" onClick={() => load(true)} disabled={loading} className="shrink-0 pt-0.5 text-muted hover:text-fg disabled:opacity-50" title="Re-run digest">
          <Icon name="reload" size={13} className={loading ? "motion-safe:animate-spin" : ""} />
        </button>
      </div>

      {loading && !s ? (
        <p className="px-[22px] py-4 text-[12.5px] text-muted">Clustering the feed…</p>
      ) : (
        <div className="space-y-3 px-[22px] py-4">
          {s?.digest && <p className="text-[13.5px] leading-relaxed text-fg/90">{s.digest}</p>}
          <div className="flex flex-wrap gap-2.5">
            {clusters.map((c, i) => (
              <div key={i} className="min-w-56 flex-1 rounded-lg border border-line bg-panel2/60 px-3 py-2.5">
                <div className="mb-1 flex items-center gap-2">
                  <span className={`rounded border px-1.5 py-0.5 text-[9.5px] font-bold uppercase ${IMPACT_TONE[c.impact] || ""}`}>
                    {c.impact}
                  </span>
                  <strong className="truncate text-[12.5px] text-fg">{c.topic}</strong>
                  <span className="ml-auto text-[10.5px] text-muted/60">{(c.headlineIdx || []).length}×</span>
                </div>
                <p className="text-[11.5px] leading-relaxed text-muted">{c.why}</p>
                {(c.headlineIdx || []).slice(0, 2).map((idx) => (
                  <p key={idx} className="mt-1 truncate text-[11px] text-fg/70">• {items[idx]?.headline}</p>
                ))}
              </div>
            ))}
          </div>
        </div>
      )}
    </section>
  );
}
