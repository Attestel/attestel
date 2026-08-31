import { useEffect, useState } from "react";
import { fetchCommittee } from "../lib/api.js";
import { Icon } from "./shell/icons.jsx";

// CommitteePanel — the analyst-committee agent pipeline (ai-hedge-fund style, invariant-preserving).
//
// Three persona analysts (technical / fundamentals / news) each read ONLY their slice of the
// collected facts and emit an analytical STANCE; a bull and a bear researcher then argue the
// strongest case for each side; a judge digests the debate. The consensus strip is a DETERMINISTIC
// weighted vote computed in code, never by the model. Stances are leans, not recommendations —
// the only actionable signal in the app remains the backtest-gated quant SignalPanel.
//
// On-demand only: nothing fetches until the user clicks Run (the pipeline makes many sequential
// local-model calls and can take minutes on first run). Results are cached by the gateway.

const AGENT_META = {
  technical: { label: "Technical", desc: "regime · confluence · key levels" },
  fundamentals: { label: "Fundamentals", desc: "EPS beat/miss history" },
  news: { label: "News & Sentiment", desc: "headlines · filings · crowd" },
};

const stanceTone = (s) =>
  s === "bullish" ? "text-up border-up/40 bg-up/10"
  : s === "bearish" ? "text-down border-down/40 bg-down/10"
  : "text-muted border-line bg-panel2";

const isStub = (m) => /stub/i.test(String(m || ""));

export default function CommitteePanel({ ticker }) {
  const [data, setData] = useState(null);
  const [loading, setLoading] = useState(false);
  const [err, setErr] = useState(null);

  // Reset when the ticker changes — never auto-run (the pipeline is expensive and on-demand only).
  useEffect(() => {
    setData(null);
    setErr(null);
  }, [ticker]);

  const run = (refresh = false) => {
    setLoading(true);
    setErr(null);
    fetchCommittee(ticker, refresh)
      .then((d) => setData(d))
      .catch((e) => setErr(e.message))
      .finally(() => setLoading(false));
  };

  const consensus = data?.consensus;
  const models = data?.modelsUsed || {};
  const anyStub = Object.values(models).some(isStub);

  return (
    <section className="overflow-hidden surface-card">
      {/* Disclaimer strip */}
      <div className="flex items-start gap-3 border-b border-line px-[22px] py-3.5">
        <p className="flex-1 text-[13px] leading-[1.6] text-muted">
          <strong className="font-semibold text-info">Analyst Committee.</strong> Three AI personas each read one
          slice of the collected evidence and take an analytical <em>stance</em>; a bull and a bear researcher argue
          the strongest case for each side; a judge digests the debate. Stances are descriptive leans —{" "}
          <strong className="font-semibold text-fg/85">never a buy/sell/hold recommendation or target.</strong> The
          consensus is a deterministic vote computed in code, not by the model.
        </p>
        <div className="flex shrink-0 items-center gap-3 pt-0.5">
          {data && (
            <button
              type="button"
              onClick={() => run(true)}
              disabled={loading}
              className="flex items-center gap-1.5 text-[12px] text-muted transition-colors hover:text-fg disabled:opacity-50"
            >
              <Icon name="reload" size={13} className={loading ? "motion-safe:animate-spin" : ""} />
              Re-run
            </button>
          )}
        </div>
      </div>

      {/* Empty / error / run states */}
      {!data && (
        <div className="flex flex-col items-center gap-3 px-6 py-14 text-center">
          <p className="max-w-md text-[13px] leading-relaxed text-muted">
            The committee runs its agents <em>sequentially</em> through the local model (one call at a time) and can
            take a while on first run. It never runs automatically — start it when you want a fresh read.
          </p>
          {err && <p className="text-[13px] text-down">Committee failed: {err}</p>}
          <button
            type="button"
            onClick={() => run(false)}
            disabled={loading}
            className="rounded-full bg-accent/15 px-4 py-2 text-[13px] font-semibold text-accent transition-colors hover:bg-accent/25 disabled:opacity-50"
          >
            {loading ? "Convening the committee…" : `Run committee on ${ticker}`}
          </button>
        </div>
      )}

      {data && data.available === false && (
        <div className="px-[22px] py-8 text-[13px] text-down">
          Committee unavailable — the LLM service is unreachable ({data.error}).
        </div>
      )}

      {data && data.available !== false && (
        <div className="space-y-5 px-[22px] py-5">
          {/* Consensus strip — deterministic, computed in code */}
          {consensus && (
            <div className="flex flex-wrap items-center gap-x-5 gap-y-2 rounded-lg border border-line bg-panel2 px-4 py-3">
              <span className={`rounded-md border px-2.5 py-1 text-[12px] font-bold uppercase tracking-wide ${stanceTone(
                consensus.label === "bullish-lean" ? "bullish" : consensus.label === "bearish-lean" ? "bearish" : "neutral"
              )}`}>
                {consensus.label}
              </span>
              <span className="text-[12px] text-muted">
                score <strong className="text-fg">{consensus.score}</strong>
              </span>
              <span className="text-[12px] text-muted">
                agreement <strong className="text-fg">{Math.round((consensus.agreement || 0) * 100)}%</strong>
              </span>
              <span className="text-[12px] text-muted">
                mean confidence <strong className="text-fg">{Math.round((consensus.meanConfidence || 0) * 100)}%</strong>
              </span>
              <span className="ml-auto text-[11px] text-muted/70">
                deterministic vote over {consensus.n} stances — not a signal
              </span>
              {anyStub && (
                <span className="rounded border border-warn/40 bg-warn/10 px-2 py-0.5 text-[11px] font-semibold text-warn">
                  OFFLINE STUB — model unreachable for some agents
                </span>
              )}
            </div>
          )}

          {/* Analyst cards */}
          <div className="grid grid-cols-1 gap-4 lg:grid-cols-3">
            {Object.entries(AGENT_META).map(([key, meta]) => {
              const a = data.analysts?.[key];
              if (!a) return null;
              const conf = Math.round((a.confidence || 0) * 100);
              return (
                <div key={key} className="rounded-lg border border-line bg-panel2/60 p-4">
                  <div className="mb-1 flex items-center justify-between gap-2">
                    <h3 className="text-[13px] font-bold text-fg">{meta.label}</h3>
                    <span className={`rounded-md border px-2 py-0.5 text-[11px] font-bold uppercase ${stanceTone(a.stance)}`}>
                      {a.stance}
                    </span>
                  </div>
                  <p className="mb-3 text-[11px] text-muted/70">{meta.desc}{isStub(models[key]) ? " · stub" : ""}</p>
                  <div className="mb-3 h-1.5 overflow-hidden rounded-full bg-panel">
                    <div className="h-full rounded-full bg-accent/70" style={{ width: `${conf}%` }} />
                  </div>
                  <p className="mb-2 text-[11px] text-muted">confidence {conf}%</p>
                  <ul className="space-y-1.5 text-[12px] leading-relaxed text-fg/85">
                    {(a.reasoning || []).map((r, i) => (
                      <li key={i} className="flex gap-2">
                        <span className="text-muted/60">•</span>
                        <span>{r}</span>
                      </li>
                    ))}
                  </ul>
                  {a.watchFor && (
                    <p className="mt-3 border-t border-line pt-2 text-[11px] text-muted">
                      <strong className="text-fg/70">Watch:</strong> {a.watchFor}
                    </p>
                  )}
                </div>
              );
            })}
          </div>

          {/* Debate */}
          <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
            {["bull", "bear"].map((side) => {
              const d = data.debate?.[side];
              if (!d) return null;
              const tone = side === "bull" ? "text-up" : "text-down";
              return (
                <div key={side} className="rounded-lg border border-line bg-panel2/60 p-4">
                  <h3 className={`mb-3 text-[13px] font-bold uppercase tracking-wide ${tone}`}>
                    {side} researcher{isStub(models[side]) ? " · stub" : ""}
                  </h3>
                  <ul className="space-y-1.5 text-[12px] leading-relaxed text-fg/85">
                    {(d.arguments || []).map((x, i) => (
                      <li key={i} className="flex gap-2">
                        <Icon name="caret" size={12} className={tone} />
                        <span>{x}</span>
                      </li>
                    ))}
                  </ul>
                  {(d.rebuttals || []).length > 0 && (
                    <div className="mt-3 border-t border-line pt-2">
                      <p className="mb-1 text-[11px] font-semibold uppercase text-muted">rebuttals</p>
                      <ul className="space-y-1 text-[12px] text-muted">
                        {d.rebuttals.map((x, i) => (
                          <li key={i}>— {x}</li>
                        ))}
                      </ul>
                    </div>
                  )}
                </div>
              );
            })}
          </div>

          {/* Judge */}
          {data.judge && (
            <div className="rounded-lg border border-line bg-panel2/60 p-4">
              <h3 className="mb-3 text-[13px] font-bold text-fg">
                Judge — state of the argument{isStub(models.judge) ? " · stub" : ""}
              </h3>
              <div className="grid grid-cols-1 gap-4 md:grid-cols-3">
                {[
                  ["strongestBull", "Strongest bull points", "text-up"],
                  ["strongestBear", "Strongest bear points", "text-down"],
                  ["unresolved", "Unresolved", "text-warn"],
                ].map(([key, label, tone]) => (
                  <div key={key}>
                    <p className={`mb-1.5 text-[11px] font-semibold uppercase ${tone}`}>{label}</p>
                    <ul className="space-y-1 text-[12px] leading-relaxed text-fg/85">
                      {(data.judge[key] || []).map((x, i) => (
                        <li key={i}>• {x}</li>
                      ))}
                      {(data.judge[key] || []).length === 0 && <li className="text-muted/60">—</li>}
                    </ul>
                  </div>
                ))}
              </div>
              {data.judge.summary && (
                <p className="mt-3 border-t border-line pt-3 text-[13px] leading-relaxed text-fg/90">
                  {data.judge.summary}
                </p>
              )}
            </div>
          )}

          {/* Footer: provenance + hybrid note */}
          <p className="text-[11px] leading-relaxed text-muted/70">
            Grounded in: {(data.contextUsed || []).join(", ") || "n/a"}. Snapshots are persisted daily; once enough
            real history accumulates, the stance record becomes a candidate feature for the walk-forward-validated
            quant model — until then the signal is unaffected. {data.disclaimer}
          </p>
        </div>
      )}
    </section>
  );
}
