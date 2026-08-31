import { useEffect, useState } from "react";
import { AuthRequiredError, checkThesis, createThesis, deleteThesis, listTheses, updateThesis } from "../lib/api.js";
import { useAuth } from "../auth/AuthContext.jsx";

// ThesisTracker — the user's own investment hypotheses, checked against the evidence.
//
// Theses are stored in the journal service (user records). Checks run automatically with each
// Brief (the gateway evaluates every active thesis against the same fact set the brief used and
// persists the verdict here), or on demand via the Check button. The verdict is about the THESIS
// — "evidence supports / is neutral toward / challenges your hypothesis" — never a trade call.

const VERDICT_TONE = {
  confirming: "text-up border-up/40 bg-up/10",
  neutral: "text-muted border-line bg-panel2",
  challenged: "text-down border-down/40 bg-down/10",
};

export default function ThesisTracker({ ticker, briefChecks = [] }) {
  const { requireAuth, promptSignIn } = useAuth();
  const [theses, setTheses] = useState(null);
  const [text, setText] = useState("");
  const [busy, setBusy] = useState(false);
  const [checking, setChecking] = useState(null); // thesis id being checked
  const [err, setErr] = useState(null);

  const load = () => {
    listTheses(ticker)
      .then((t) => { setTheses(t); setErr(null); })
      .catch((e) => { setTheses(null); setErr(e.message); });
  };
  useEffect(load, [ticker]); // eslint-disable-line react-hooks/exhaustive-deps

  // When the Brief just ran checks, refresh so the persisted verdicts show up.
  useEffect(() => {
    if (briefChecks.length > 0) load();
  }, [briefChecks.length]); // eslint-disable-line react-hooks/exhaustive-deps

  const add = (e) => {
    e.preventDefault();
    if (!text.trim()) return;
    if (!requireAuth()) return; // guest -> routed to sign-in before saving a thesis
    setBusy(true);
    createThesis(ticker, text.trim())
      .then(() => { setText(""); load(); })
      .catch((e2) => (e2 instanceof AuthRequiredError ? promptSignIn() : setErr(e2.message)))
      .finally(() => setBusy(false));
  };

  const runCheck = (id) => {
    setChecking(id);
    checkThesis(id).then(load).catch((e2) => setErr(e2.message)).finally(() => setChecking(null));
  };

  const active = (theses || []).filter((t) => t.status === "active");
  const archived = (theses || []).filter((t) => t.status === "archived");

  return (
    <section className="overflow-hidden surface-card">
      <div className="border-b border-line px-[22px] py-3.5">
        <p className="text-[13px] leading-[1.6] text-muted">
          <strong className="font-semibold text-info">Thesis tracker.</strong> Write down <em>why</em> you're
          interested in {ticker} — with every Brief, the AI re-checks your thesis against the newly collected
          facts: <span className="text-up">confirming</span> · <span className="text-muted">neutral</span> ·{" "}
          <span className="text-down">challenged</span>. It judges the evidence for <em>your hypothesis</em>,
          never the stock.
        </p>
      </div>

      <form onSubmit={add} className="flex gap-2 border-b border-line px-[22px] py-3">
        <input
          value={text}
          onChange={(e) => setText(e.target.value)}
          placeholder={`e.g. "Buying ${ticker} because data-center demand outpaces supply through 2027"`}
          className="flex-1 rounded-lg border border-line bg-panel2 px-3 py-2 text-[12.5px] text-fg placeholder:text-muted/50 focus:border-accent/60 focus:outline-none"
        />
        <button
          type="submit"
          disabled={busy || !text.trim()}
          className="rounded-full bg-accent/15 px-3.5 py-2 text-[12.5px] font-semibold text-accent hover:bg-accent/25 disabled:opacity-50"
        >
          Add thesis
        </button>
      </form>

      {err && <p className="px-[22px] py-3 text-[12px] text-down">Journal service: {err}</p>}
      {theses && active.length === 0 && archived.length === 0 && (
        <p className="px-[22px] py-5 text-center text-[13px] text-muted">No theses yet for {ticker}.</p>
      )}

      <ul className="divide-y divide-line">
        {active.map((t) => {
          const c = t.lastCheck;
          return (
            <li key={t.id} className="px-[22px] py-4">
              <p className="text-[13px] leading-relaxed text-fg/90">“{t.text}”</p>
              <div className="mt-2 flex flex-wrap items-center gap-2.5">
                {c ? (
                  <>
                    <span className={`rounded-md border px-2 py-0.5 text-[10px] font-bold uppercase ${VERDICT_TONE[c.verdict] || ""}`}>
                      {c.verdict}
                    </span>
                    <span className="text-[11px] text-muted/70">
                      checked {new Date(c.at * 1000).toLocaleString()}
                    </span>
                  </>
                ) : (
                  <span className="text-[11px] text-muted/60">not checked yet — runs with the next Brief</span>
                )}
                <span className="ml-auto flex gap-2.5">
                  <button
                    type="button"
                    onClick={() => runCheck(t.id)}
                    disabled={checking === t.id}
                    className="text-[11px] text-info hover:underline disabled:opacity-50"
                  >
                    {checking === t.id ? "checking…" : "check now"}
                  </button>
                  <button type="button" onClick={() => requireAuth(() => updateThesis(t.id, { status: "archived" }).then(load).catch((e) => e instanceof AuthRequiredError && promptSignIn()))} className="text-[11px] text-muted hover:underline">
                    archive
                  </button>
                  <button type="button" onClick={() => requireAuth(() => deleteThesis(t.id).then(load).catch((e) => e instanceof AuthRequiredError && promptSignIn()))} className="text-[11px] text-down/80 hover:underline">
                    delete
                  </button>
                </span>
              </div>
              {c?.summary && <p className="mt-2 text-[12.5px] leading-relaxed text-muted">{c.summary}</p>}
              {(c?.evidence || []).slice(0, 4).map((ev, i) => (
                <p key={i} className="mt-1 text-[12px] leading-relaxed text-fg/75">
                  <span className={ev.bearing === "supports" ? "text-up" : ev.bearing === "contradicts" ? "text-down" : "text-muted"}>
                    {ev.bearing === "supports" ? "▲" : ev.bearing === "contradicts" ? "▼" : "•"}
                  </span>{" "}
                  {ev.fact} <span className="text-muted/70">— {ev.note}</span>
                </p>
              ))}
            </li>
          );
        })}
      </ul>

      {archived.length > 0 && (
        <p className="border-t border-line px-[22px] py-2.5 text-[11px] text-muted/60">
          {archived.length} archived thesis{archived.length > 1 ? "es" : ""} (kept for the record).
        </p>
      )}
    </section>
  );
}
