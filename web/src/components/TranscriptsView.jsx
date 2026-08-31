import { useEffect, useState } from "react";
import { fetchTranscriptHistory, submitTranscript } from "../lib/api.js";
import { Icon } from "./shell/icons.jsx";

// TranscriptsView — earnings-call transcript analyzer ("reading between the lines").
//
// Paste a transcript (or give a URL — best-effort; paywalled/JS pages won't work) and the model
// returns: 5 key points, evasive-answer moments with QUOTE-VERIFIED evidence (quotes not found
// verbatim in the text are dropped server-side), a management-tone read, and a tone shift vs the
// STORED prior-quarter analysis. Descriptive language analysis — never a verdict on the stock.

const toneColor = (v, good, bad) => (v === good ? "text-up" : v === bad ? "text-down" : "text-warn");

export default function TranscriptsView({ ticker }) {
  const [label, setLabel] = useState("");
  const [text, setText] = useState("");
  const [url, setUrl] = useState("");
  const [running, setRunning] = useState(false);
  const [result, setResult] = useState(null);
  const [err, setErr] = useState(null);
  const [history, setHistory] = useState([]);

  const loadHistory = () => {
    fetchTranscriptHistory(ticker)
      .then((r) => setHistory(r.analyses || []))
      .catch(() => setHistory([]));
  };
  useEffect(() => {
    setResult(null);
    setErr(null);
    loadHistory();
  }, [ticker]); // eslint-disable-line react-hooks/exhaustive-deps

  const run = (e) => {
    e.preventDefault();
    if (!text.trim() && !url.trim()) return;
    setRunning(true);
    setErr(null);
    submitTranscript(ticker, { label: label.trim(), text, url: url.trim() })
      .then((r) => {
        if (r.error) throw new Error(r.error);
        setResult(r);
        setText("");
        loadHistory();
      })
      .catch((e2) => setErr(e2.message))
      .finally(() => setRunning(false));
  };

  const s = result?.structured;
  const stub = /stub/i.test(String(result?.modelUsed || ""));

  return (
    <div className="space-y-5">
      <section className="overflow-hidden surface-card">
        <div className="border-b border-line px-[22px] py-3.5">
          <p className="text-[13px] leading-[1.6] text-muted">
            <strong className="font-semibold text-info">Earnings-call analyzer.</strong> Paste the transcript of a
            {" "}{ticker} earnings call (IR page, prepared remarks + Q&amp;A). The model extracts the 5 critical
            points, flags <em>evasive answers</em> (only with verbatim quotes it can prove), reads management tone,
            and compares tone against the stored prior quarter.{" "}
            <strong className="font-semibold text-fg/85">Language analysis, not investment advice.</strong>
          </p>
        </div>

        <form onSubmit={run} className="space-y-3 px-[22px] py-4">
          <div className="flex flex-wrap gap-3">
            <input
              value={label}
              onChange={(e) => setLabel(e.target.value)}
              placeholder="Quarter label, e.g. 2026-Q2"
              className="w-48 rounded-lg border border-line bg-panel2 px-3 py-2 text-[12px] text-fg placeholder:text-muted/50 focus:border-accent/60 focus:outline-none"
            />
            <input
              value={url}
              onChange={(e) => setUrl(e.target.value)}
              placeholder="…or a URL to fetch (best-effort)"
              className="min-w-64 flex-1 rounded-lg border border-line bg-panel2 px-3 py-2 text-[12px] text-fg placeholder:text-muted/50 focus:border-accent/60 focus:outline-none"
            />
          </div>
          <textarea
            value={text}
            onChange={(e) => setText(e.target.value)}
            rows={6}
            placeholder="Paste the full transcript here (preferred over URL — always works)…"
            className="w-full rounded-lg border border-line bg-panel2 px-3 py-2.5 font-mono text-[12px] leading-relaxed text-fg placeholder:text-muted/50 focus:border-accent/60 focus:outline-none"
          />
          <div className="flex items-center gap-3">
            <button
              type="submit"
              disabled={running || (!text.trim() && !url.trim())}
              className="rounded-full bg-accent/15 px-4 py-2 text-[13px] font-semibold text-accent transition-colors hover:bg-accent/25 disabled:opacity-50"
            >
              {running ? "Reading between the lines…" : "Analyze transcript"}
            </button>
            {text && <span className="text-[11px] text-muted">{text.length.toLocaleString()} characters</span>}
            {err && <span className="text-[12px] text-down">Failed: {err}</span>}
          </div>
        </form>
      </section>

      {s && (
        <section className="space-y-4 rounded-xl border border-line bg-panel px-[22px] py-5 shadow-card">
          <div className="flex flex-wrap items-center gap-3">
            <h2 className="text-[14px] font-bold text-fg">{result.label} — analysis</h2>
            {stub && (
              <span className="rounded border border-warn/40 bg-warn/10 px-2 py-0.5 text-[11px] font-semibold text-warn">
                OFFLINE STUB — keyword counting only
              </span>
            )}
            <span className="text-[11px] text-muted/70">
              {result.chunks} chunk{result.chunks > 1 ? "s" : ""}
              {result.priorLabel ? ` · compared vs ${result.priorLabel}` : " · no prior quarter stored"}
            </span>
          </div>

          {/* Tone strip */}
          <div className="flex flex-wrap gap-x-6 gap-y-2 rounded-lg border border-line bg-panel2 px-4 py-3 text-[12px]">
            <span className="text-muted">
              confidence{" "}
              <strong className={`uppercase ${toneColor(s.tone?.confidence, "high", "low")}`}>{s.tone?.confidence}</strong>
            </span>
            <span className="text-muted">
              hedging{" "}
              <strong className={`uppercase ${toneColor(s.tone?.hedging, "low", "high")}`}>{s.tone?.hedging}</strong>
            </span>
            {(s.tone?.notableLanguage || []).slice(0, 4).map((w, i) => (
              <span key={i} className="rounded bg-panel px-2 py-0.5 font-mono text-[11px] text-muted">“{w}”</span>
            ))}
          </div>
          {s.toneShift && (
            <p className="rounded-lg border border-info/30 bg-info/10 px-4 py-2.5 text-[13px] text-info">
              <strong>Tone shift:</strong> <span className="text-fg/85">{s.toneShift}</span>
            </p>
          )}

          <div>
            <h3 className="mb-2 text-[12px] font-semibold uppercase tracking-wide text-muted">Key points</h3>
            <ul className="space-y-1.5 text-[13px] leading-relaxed text-fg/90">
              {(s.keyPoints || []).map((p, i) => (
                <li key={i} className="flex gap-2"><span className="text-accent">{i + 1}.</span><span>{p}</span></li>
              ))}
            </ul>
          </div>

          {(s.evasiveMoments || []).length > 0 && (
            <div>
              <h3 className="mb-2 text-[12px] font-semibold uppercase tracking-wide text-warn">
                Evasive moments <span className="normal-case text-muted/70">(quotes verified against the text)</span>
              </h3>
              <div className="space-y-3">
                {s.evasiveMoments.map((m, i) => (
                  <div key={i} className="rounded-lg border border-line bg-panel2/60 p-3.5 text-[12.5px] leading-relaxed">
                    {m.topic && <span className="mb-1.5 inline-block rounded bg-warn/15 px-2 py-0.5 text-[10px] font-bold uppercase text-warn">{m.topic}</span>}
                    <p className="text-muted"><strong className="text-fg/70">Q:</strong> “{m.question}”</p>
                    <p className="mt-1 text-muted"><strong className="text-fg/70">A:</strong> “{m.answer}”</p>
                    <p className="mt-1.5 text-fg/85">{m.why}</p>
                  </div>
                ))}
              </div>
            </div>
          )}

          {(s.riskFlags || []).length > 0 && (
            <div>
              <h3 className="mb-2 text-[12px] font-semibold uppercase tracking-wide text-down">Risk flags</h3>
              <ul className="space-y-1 text-[13px] text-fg/85">
                {s.riskFlags.map((r, i) => (<li key={i} className="flex items-start gap-1.5"><Icon name="warning" size={13} className="mt-px flex-none" /> {r}</li>))}
              </ul>
            </div>
          )}

          <p className="border-t border-line pt-3 text-[13px] leading-relaxed text-fg/90">{s.summary}</p>
          <p className="text-[11px] text-muted/70">{result.disclaimer}</p>
        </section>
      )}

      {/* History: the stored per-quarter analyses (what toneShift compares against) */}
      <section className="overflow-hidden surface-card">
        <div className="flex items-center gap-2 border-b border-line px-[22px] py-3">
          <h2 className="text-[13px] font-bold text-fg">Stored quarters ({history.length})</h2>
          <button type="button" onClick={loadHistory} className="ml-auto text-muted hover:text-fg" title="Reload">
            <Icon name="reload" size={13} />
          </button>
        </div>
        {history.length === 0 ? (
          <p className="px-[22px] py-6 text-center text-[13px] text-muted">
            No analyses stored yet — the first one becomes the baseline for tone-shift tracking.
          </p>
        ) : (
          <ul className="divide-y divide-line">
            {history.map((h) => (
              <li key={h.key} className="flex flex-wrap items-center gap-3 px-[22px] py-3 text-[12.5px]">
                <strong className="text-fg">{h.label}</strong>
                <span className="text-muted">
                  confidence <span className={toneColor(h.structured?.tone?.confidence, "high", "low")}>{h.structured?.tone?.confidence}</span>
                  {" · "}hedging <span className={toneColor(h.structured?.tone?.hedging, "low", "high")}>{h.structured?.tone?.hedging}</span>
                </span>
                <span className="ml-auto text-[11px] text-muted/60">{String(h.timestamp).slice(0, 10)}</span>
              </li>
            ))}
          </ul>
        )}
      </section>
    </div>
  );
}
