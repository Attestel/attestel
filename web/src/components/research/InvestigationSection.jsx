import { useEffect, useRef, useState } from "react";
import { motion } from "framer-motion";
import { runInvestigation } from "../../lib/api.js";
import { cx } from "../../lib/cx.js";
import { Button } from "../ui/index.js";

const ADD_ONS = [
  { id: "my_thesis", label: "My thesis", note: "Test the result against your active thesis." },
  { id: "sec_filings", label: "SEC filings", note: "Include recent filing records from the feed." },
  { id: "earnings_history", label: "Earnings & calls", note: "Use earnings history and saved call analyses." },
  { id: "news_events", label: "News & events", note: "Gather recent source-labelled headlines." },
  { id: "technical_context", label: "Technical context", note: "Include computed regime and timeframe context." },
  { id: "opposing_perspective", label: "Opposing perspective", note: "Actively search for contradictions and gaps." },
  { id: "management_credibility", label: "Management credibility", note: "Inspect saved analyses of management language." },
  { id: "scenario_analysis", label: "Scenario analysis", note: "Trace a qualitative cause-and-effect path." },
];

const DEFAULT_ADD_ONS = ["news_events", "technical_context", "opposing_perspective"];
const STAGES = [
  { id: "understanding", label: "Understanding" },
  { id: "gathering", label: "Gathering & verifying" },
  { id: "synthesizing", label: "Challenging & synthesizing" },
];

const WORKSPACE_FOR_SOURCE = {
  market_context: ["evidence", "chart", "Open chart context"],
  technical_context: ["evidence", "chart", "Open chart context"],
  news_events: ["evidence", "news", "Open news workspace"],
  sec_filings: ["evidence", "news", "Open filings workspace"],
  earnings_history: ["evidence", "fundamentals", "Open fundamentals"],
  stored_transcripts: ["evidence", "transcripts", "Open transcripts"],
  my_thesis: ["thesis", null, "Open My Thesis"],
  scenario_context: ["scenarios", null, "Open scenarios"],
  opposing_perspective: ["perspectives", null, "Open perspectives"],
};

const labelFor = (id) => ADD_ONS.find((item) => item.id === id)?.label || id.replaceAll("_", " ");

function AddOnPicker({ selected, onToggle, onClose }) {
  const ref = useRef(null);

  useEffect(() => {
    const close = (event) => {
      if (!ref.current?.contains(event.target)) onClose();
    };
    const closeOnEscape = (event) => {
      if (event.key === "Escape") onClose();
    };
    document.addEventListener("pointerdown", close);
    document.addEventListener("keydown", closeOnEscape);
    return () => {
      document.removeEventListener("pointerdown", close);
      document.removeEventListener("keydown", closeOnEscape);
    };
  }, [onClose]);

  return (
    <div
      ref={ref}
      className="absolute left-0 top-[calc(100%+10px)] z-30 w-[min(390px,calc(100vw-48px))] overflow-hidden rounded-2xl border border-line2 bg-panel/95 p-2 shadow-nav backdrop-blur-2xl"
      role="dialog"
      aria-label="Research add-ons"
    >
      <div className="px-3 pb-2 pt-1.5">
        <p className="text-[12px] font-semibold text-fg">Choose how this run should research</p>
        <p className="mt-0.5 text-[11px] text-muted">Only selected methods are added to this investigation.</p>
      </div>
      <div className="max-h-[270px] overflow-y-auto">
        {ADD_ONS.map((item) => {
          const active = selected.includes(item.id);
          return (
            <button
              key={item.id}
              type="button"
              onClick={() => onToggle(item.id)}
              aria-pressed={active}
              className={cx(
                "flex w-full items-start gap-3 rounded-xl px-3 py-2.5 text-left transition-colors",
                active ? "bg-accent/12" : "hover:bg-white/[0.055]"
              )}
            >
              <span
                className={cx(
                  "mt-0.5 grid h-[18px] w-[18px] shrink-0 place-items-center rounded-md border text-[11px]",
                  active ? "border-accent bg-accent text-white" : "border-line2 text-transparent"
                )}
                aria-hidden="true"
              >
                ✓
              </span>
              <span>
                <span className="block text-[12.5px] font-medium text-fg">{item.label}</span>
                <span className="mt-0.5 block text-[11px] leading-relaxed text-muted">{item.note}</span>
              </span>
            </button>
          );
        })}
      </div>
    </div>
  );
}

function AddOnBar({ selected, onToggle, open, onOpen, onClose }) {
  return (
    <div className="flex min-h-9 flex-wrap items-center justify-center gap-2">
      <div className="relative">
        <button
          type="button"
          onClick={onOpen}
          className="inline-flex h-8 items-center gap-1.5 rounded-full border border-line2 bg-panel2/70 px-3 text-[11.5px] font-medium text-fg transition-colors hover:border-white/30 hover:bg-panel2"
          aria-expanded={open}
        >
          <span className="text-[17px] font-light leading-none text-accent">+</span>
          Add tools
        </button>
        {open && <AddOnPicker selected={selected} onToggle={onToggle} onClose={onClose} />}
      </div>

      {selected.map((id) => (
        <button
          key={id}
          type="button"
          onClick={() => onToggle(id)}
          className="inline-flex h-8 items-center gap-1.5 rounded-full border border-accent/25 bg-accent/10 px-3 text-[11.5px] text-fg/85 transition-colors hover:border-accent/45 hover:bg-accent/15"
          title={`Remove ${labelFor(id)}`}
        >
          {labelFor(id)}
          <span className="text-muted" aria-hidden="true">×</span>
        </button>
      ))}
    </div>
  );
}

function ResearchingState({ stage, message, trail, selected, showActivity, onShowActivity, onCancel }) {
  const current = Math.max(0, STAGES.findIndex((item) => item.id === stage));

  return (
    <div className="research-state-enter flex items-start gap-4">
      <div className="research-orbit mt-1 shrink-0" aria-hidden="true"><span /></div>
      <div className="min-w-0 flex-1">
        <p className="label-mono text-accent">Research in progress</p>
        <p className="mt-2 min-h-5 text-[14px] leading-relaxed text-fg/85">{message}</p>

        <ol className="mt-5 flex w-full max-w-2xl items-start">
          {STAGES.map((item, index) => {
            const complete = index < current;
            const active = index === current;
            return (
              <li key={item.id} className="relative flex flex-1 flex-col gap-2 pr-2">
                {index > 0 && (
                  <span className={cx("absolute right-full top-[6px] h-px w-full", complete || active ? "bg-accent/55" : "bg-line2")} />
                )}
                <span
                  className={cx(
                    "relative z-10 h-[13px] w-[13px] rounded-full border",
                    complete && "border-accent bg-accent",
                    active && "border-accent bg-accent/30 shadow-[0_0_18px_rgba(91,108,255,.7)] motion-safe:animate-[pulse-live_1.4s_ease-in-out_infinite]",
                    !complete && !active && "border-line2 bg-canvas"
                  )}
                />
                <span className={cx("text-[10px] sm:text-[11px]", active ? "text-fg" : "text-muted")}>{item.label}</span>
              </li>
            );
          })}
        </ol>

        <div className="mt-5 flex items-center gap-4">
          <button type="button" onClick={onShowActivity} className="text-[12px] text-muted underline decoration-dotted underline-offset-4 hover:text-fg">
            {showActivity ? "Hide activity" : "View activity"}
          </button>
          <button type="button" onClick={onCancel} className="text-[12px] text-muted hover:text-fg">Cancel</button>
        </div>

        {showActivity && (
          <div className="mt-4 max-w-2xl border-t border-line pt-3 text-left">
            {(trail.length ? trail : selected.map((key) => ({ key, label: labelFor(key), status: "running", detail: "Source collection in progress." }))).map((item) => (
              <div key={item.key} className="flex items-start gap-2.5 py-2 text-[11.5px]">
                <span className={cx("mt-1 h-1.5 w-1.5 shrink-0 rounded-full", item.status === "unavailable" ? "bg-warn" : item.status === "running" ? "bg-accent motion-safe:animate-pulse" : "bg-up")} />
                <span className="min-w-0">
                  <span className="text-fg/85">{item.label}</span>
                  <span className="ml-2 text-muted">{item.detail}</span>
                </span>
              </div>
            ))}
          </div>
        )}
      </div>
    </div>
  );
}

function SourceCitation({ item, sourceManifest }) {
  const group = sourceManifest.find((source) => source.key === item?.sourceKey);
  const exact = Number.isInteger(item?.sourceIndex) ? group?.items?.[item.sourceIndex] : null;
  const citation = (
    <span className="font-mono text-[10px] uppercase tracking-[0.1em] text-info">
      {exact?.source || group?.label || labelFor(item?.sourceKey || "source")}
    </span>
  );
  return exact?.url ? (
    <a href={exact.url} target="_blank" rel="noreferrer" className="mt-1 inline-flex items-center gap-1 hover:text-accent" title={exact.title}>
      {citation} <span aria-hidden="true">↗</span>
    </a>
  ) : (
    <p className="mt-1">{citation}</p>
  );
}

function Trail({ report, onOpenSection, canOpenTools }) {
  const structured = report?.structured || {};
  const findings = Array.isArray(structured.keyFindings) ? structured.keyFindings : [];
  const counter = Array.isArray(structured.counterEvidence) ? structured.counterEvidence : [];
  const trail = Array.isArray(report?.trail) ? report.trail : [];
  const sourceManifest = Array.isArray(report?.sourceManifest) ? report.sourceManifest : [];

  return (
    <div className="mt-8 border-t border-line pt-6 text-left">
      <div className="grid gap-7 lg:grid-cols-[1.15fr_.85fr]">
        <div>
          <p className="label-mono">Grounded findings</p>
          {findings.length ? (
            <div className="mt-3 divide-y divide-line">
              {findings.map((item, index) => (
                <div key={`${item.sourceKey}-${index}`} className="py-3 first:pt-0">
                  <p className="text-[13px] leading-relaxed text-fg/85">{item.finding}</p>
                  <SourceCitation item={item} sourceManifest={sourceManifest} />
                </div>
              ))}
            </div>
          ) : (
            <p className="mt-3 text-[12px] text-muted">No synthesized findings were returned.</p>
          )}

          {counter.length > 0 && (
            <div className="mt-6">
              <p className="label-mono text-warn">What weakens the answer</p>
              <div className="mt-3 divide-y divide-line">
                {counter.map((item, index) => (
                  <div key={`${item.sourceKey}-${index}`} className="py-3 first:pt-0">
                    <p className="text-[13px] leading-relaxed text-fg/75">— {item.finding}</p>
                    <SourceCitation item={item} sourceManifest={sourceManifest} />
                  </div>
                ))}
              </div>
            </div>
          )}
        </div>

        <div>
          <p className="label-mono">Research activity</p>
          <div className="mt-3 divide-y divide-line">
            {trail.map((item) => (
              <div key={item.key} className="py-3 first:pt-0">
                <div className="flex items-center gap-2">
                  <span className={cx("h-1.5 w-1.5 rounded-full", item.status === "unavailable" ? "bg-warn" : item.status === "method" ? "bg-llm" : "bg-up")} />
                  <span className="text-[12px] font-medium text-fg/85">{item.label}</span>
                  {item.source && <span className="ml-auto font-mono text-[9.5px] uppercase tracking-wide text-dim">{item.source}</span>}
                </div>
                <p className="mt-1 pl-3.5 text-[11px] leading-relaxed text-muted">{item.detail}</p>
                {canOpenTools && item.status !== "unavailable" && WORKSPACE_FOR_SOURCE[item.key] && (
                  <button
                    type="button"
                    onClick={() => {
                      const [section, tab] = WORKSPACE_FOR_SOURCE[item.key];
                      onOpenSection(section, tab);
                    }}
                    className="mt-1.5 pl-3.5 text-[10.5px] font-medium text-accent hover:text-accent/80"
                  >
                    {WORKSPACE_FOR_SOURCE[item.key][2]} →
                  </button>
                )}
              </div>
            ))}
          </div>

          {sourceManifest.length > 0 && (
            <div className="mt-7 border-t border-line pt-5">
              <p className="label-mono">Direct sources</p>
              <div className="mt-3 space-y-5">
                {sourceManifest.map((group) => (
                  <div key={group.key}>
                    <p className="text-[11px] font-medium text-fg/75">{group.label}</p>
                    <div className="mt-1.5 space-y-2">
                      {(group.items || []).slice(0, 5).map((item, index) => {
                        const content = (
                          <>
                            <span className="block text-[11.5px] leading-relaxed text-fg/70">{item.title}</span>
                            {item.source && <span className="mt-0.5 block font-mono text-[9px] uppercase tracking-wide text-dim">{item.source}</span>}
                          </>
                        );
                        return item.url ? (
                          <a key={index} href={item.url} target="_blank" rel="noreferrer" className="block hover:text-accent">{content}</a>
                        ) : (
                          <div key={index}>{content}</div>
                        );
                      })}
                    </div>
                  </div>
                ))}
              </div>
            </div>
          )}
        </div>
      </div>
    </div>
  );
}

function ResultState({ report, onUseQuestion, onOpenSection, canOpenTools }) {
  const [showTrail, setShowTrail] = useState(false);
  const [showImpact, setShowImpact] = useState(false);
  const structured = report?.structured || {};
  const impact = structured.thesisImpact || {};
  const offline = Boolean(structured._stub) || String(report?.modelUsed || "").startsWith("stub:");

  return (
    <div className="research-state-enter">
      <div className="flex flex-wrap items-center gap-3">
        <span className="label-mono text-accent">Investigation complete</span>
        {offline && <span className="rounded-full border border-warn/30 bg-warn/10 px-2.5 py-1 text-[10px] font-medium text-warn">Offline fact mode</span>}
        {report?.dataState === "synthetic" && <span className="rounded-full border border-warn/30 bg-warn/10 px-2.5 py-1 text-[10px] font-medium text-warn">Synthetic market data</span>}
      </div>

      <h2 className="mt-5 max-w-3xl text-[clamp(20px,2vw,28px)] leading-[1.3] text-fg">{structured.answer}</h2>

      <div className="mt-9 max-w-3xl border-l border-warn/60 pl-4">
        <p className="label-mono text-warn">Key uncertainty</p>
        <p className="mt-2 text-[13px] leading-relaxed text-fg/75">{structured.uncertainty || "No uncertainty statement was returned."}</p>
      </div>

      <div className="mt-9 flex flex-wrap gap-2.5">
        <Button variant="primary" size="md" onClick={() => setShowTrail((value) => !value)}>
          {showTrail ? "Hide research trail" : "Show research trail"}
        </Button>
        <Button variant="secondary" size="md" onClick={() => setShowImpact((value) => !value)}>
          Review thesis impact
        </Button>
      </div>

      {showImpact && (
        <div className="mt-6 max-w-3xl border-t border-line pt-5">
          <div className="flex flex-wrap items-center gap-2.5">
            <p className="label-mono">Thesis impact</p>
            <span className="rounded-full border border-line2 bg-panel2/55 px-2.5 py-1 text-[10.5px] text-fg/80">{String(impact.status || "not_assessed").replaceAll("_", " ")}</span>
          </div>
          <p className="mt-2 text-[13px] leading-relaxed text-fg/75">{impact.reason || "This run did not assess an active thesis."}</p>
          <button type="button" onClick={() => onOpenSection("thesis")} className="mt-3 inline-flex items-center gap-1 text-[12px] font-medium text-accent hover:text-accent/80">
            Open My Thesis <span aria-hidden="true">→</span>
          </button>
          <p className="mt-2 text-[10.5px] text-muted">The investigation never rewrites your thesis automatically.</p>
        </div>
      )}

      {showTrail && <Trail report={report} onOpenSection={onOpenSection} canOpenTools={canOpenTools} />}

      {structured.nextQuestion && (
        <div className="mt-8 border-t border-line pt-5">
          <p className="label-mono">A sharper next question</p>
          <button type="button" onClick={() => onUseQuestion(structured.nextQuestion)} className="mt-2 text-left text-[13px] leading-relaxed text-accent hover:text-accent/80">
            {structured.nextQuestion} <span aria-hidden="true">→</span>
          </button>
        </div>
      )}

      <p className="mt-8 max-w-3xl text-[10.5px] leading-relaxed text-dim">{report?.disclaimer}</p>
    </div>
  );
}

export default function InvestigationSection({ ticker, level, onOpenSection }) {
  const [question, setQuestion] = useState("");
  const [submittedQuestion, setSubmittedQuestion] = useState("");
  const [selected, setSelected] = useState(DEFAULT_ADD_ONS);
  const [pickerOpen, setPickerOpen] = useState(false);
  const [mode, setMode] = useState("idle");
  const [stage, setStage] = useState("understanding");
  const [message, setMessage] = useState("");
  const [trail, setTrail] = useState([]);
  const [report, setReport] = useState(null);
  const [error, setError] = useState("");
  const [showActivity, setShowActivity] = useState(false);
  const abortRef = useRef(null);
  const inputRef = useRef(null);

  useEffect(() => {
    abortRef.current?.abort();
    abortRef.current = null;
    setQuestion("");
    setSubmittedQuestion("");
    setSelected(DEFAULT_ADD_ONS);
    setMode("idle");
    setReport(null);
    setTrail([]);
    setError("");
  }, [ticker]);

  useEffect(() => () => abortRef.current?.abort(), []);

  useEffect(() => {
    const previousBodyOverflow = document.body.style.overflow;
    const previousRootOverflow = document.documentElement.style.overflow;
    document.body.style.overflow = "hidden";
    document.documentElement.style.overflow = "hidden";
    return () => {
      document.body.style.overflow = previousBodyOverflow;
      document.documentElement.style.overflow = previousRootOverflow;
    };
  }, []);

  const toggleAddOn = (id) => {
    setSelected((current) => current.includes(id) ? current.filter((key) => key !== id) : [...current, id]);
  };

  const start = async (event) => {
    event?.preventDefault();
    if (mode === "running") return;
    const scopedQuestion = question.trim();
    if (scopedQuestion.length < 8) {
      setError("Write a specific research question first.");
      return;
    }
    const controller = new AbortController();
    abortRef.current = controller;
    setSubmittedQuestion(scopedQuestion);
    setQuestion("");
    setMode("running");
    setStage("understanding");
    setMessage(`Scoping one bounded investigation for ${ticker}.`);
    setTrail([]);
    setReport(null);
    setError("");
    setShowActivity(false);
    setPickerOpen(false);

    try {
      const result = await runInvestigation(ticker, {
        question: scopedQuestion,
        addOns: selected,
        signal: controller.signal,
        onEvent: (eventData) => {
          if (eventData.type === "stage") {
            setStage(eventData.stage);
            setMessage(eventData.message || "Researching…");
          }
          if (eventData.type === "sources") setTrail(eventData.trail || []);
        },
      });
      if (controller.signal.aborted) {
        setQuestion(scopedQuestion);
        setSubmittedQuestion("");
        setMode("idle");
        return;
      }
      setReport(result);
      setMode("result");
    } catch (runError) {
      if (runError?.name === "AbortError") {
        if (abortRef.current === controller) {
          setQuestion(scopedQuestion);
          setSubmittedQuestion("");
          setMode("idle");
        }
        return;
      }
      setError(runError?.message || "The investigation could not be completed.");
      setMode("error");
    } finally {
      if (abortRef.current === controller) abortRef.current = null;
    }
  };

  const cancel = () => abortRef.current?.abort();
  const useQuestion = (nextQuestion) => {
    setQuestion(typeof nextQuestion === "string" ? nextQuestion : "");
    requestAnimationFrame(() => inputRef.current?.focus());
  };
  const hasConversation = mode !== "idle";

  return (
    <section className="investigation-canvas relative isolate flex min-h-0 flex-1 overflow-hidden rounded-[28px] border border-line" aria-busy={mode === "running"}>
      <div className="investigation-grid absolute inset-0 -z-20" aria-hidden="true" />
      <div className="investigation-glow absolute inset-0 -z-10" aria-hidden="true" />

      <div className="relative z-10 flex h-full min-h-0 w-full flex-col">
        {!hasConversation ? (
          <div className="flex min-h-0 flex-1 flex-col items-center justify-end px-5 text-center sm:px-10">
          <p className="label-mono text-accent">Research automation · {ticker}</p>
          <h2 className="mt-4 text-[clamp(30px,4vw,54px)] leading-none text-fg">
            Investigate what matters.
          </h2>
          <p className="mt-4 max-w-xl text-[13.5px] leading-relaxed text-muted">
            Ask one focused question. The workspace gathers the selected evidence, challenges the apparent answer, and returns one finite report.
          </p>
          </div>
        ) : (
          <div className="research-conversation min-h-0 flex-1 overflow-y-auto overscroll-contain px-4 py-6 sm:px-8 sm:py-8">
            <div className="mx-auto w-full max-w-3xl">
              <div className="flex justify-end">
                <p className="max-w-[82%] rounded-[22px] bg-panel2 px-4 py-3 text-[14px] leading-relaxed text-fg/90 sm:max-w-[70%]">
                  {submittedQuestion}
                </p>
              </div>

              <div className="mt-7" aria-live="polite">
                {mode === "running" && (
                  <ResearchingState
                    stage={stage}
                    message={message}
                    trail={trail}
                    selected={selected}
                    showActivity={showActivity}
                    onShowActivity={() => setShowActivity((value) => !value)}
                    onCancel={cancel}
                  />
                )}

                {mode === "result" && report && (
                  <ResultState
                    report={report}
                    onUseQuestion={useQuestion}
                    onOpenSection={onOpenSection}
                    canOpenTools={level === "standard"}
                  />
                )}

                {mode === "error" && (
                  <div className="research-state-enter border-l border-down/60 pl-4">
                    <p className="label-mono text-down">Research stopped</p>
                    <p className="mt-2 text-[13px] leading-relaxed text-fg/75">{error}</p>
                  </div>
                )}
              </div>
            </div>
          </div>
        )}

        <motion.div
          layout="position"
          transition={{ layout: { duration: 0.42, ease: [0.16, 0.8, 0.24, 1] } }}
          className={cx(
            "relative mx-auto w-full max-w-3xl px-4 sm:px-6",
            hasConversation ? "shrink-0 border-t border-line bg-canvas/70 py-4 backdrop-blur-xl" : "flex-1 pt-8 sm:pt-9"
          )}
        >
          <AddOnBar
            selected={selected}
            onToggle={toggleAddOn}
            open={pickerOpen}
            onOpen={() => setPickerOpen((value) => !value)}
            onClose={() => setPickerOpen(false)}
          />

          <form onSubmit={start} className="research-composer relative mt-3 overflow-hidden rounded-[26px] border border-white/15 bg-[rgba(10,11,14,.88)] p-2 text-left shadow-[0_28px_80px_-38px_rgba(91,108,255,.75)] backdrop-blur-2xl transition-colors focus-within:border-accent/55">
            <textarea
              ref={inputRef}
              value={question}
              onChange={(event) => { setQuestion(event.target.value); setError(""); }}
              onKeyDown={(event) => {
                if (event.key === "Enter" && !event.shiftKey) {
                  event.preventDefault();
                  start(event);
                }
              }}
              rows={hasConversation ? 2 : 3}
              maxLength={2000}
              placeholder={hasConversation ? `Ask another question about ${ticker}` : `What do you need to understand about ${ticker}?`}
              aria-label={`Research question for ${ticker}`}
              className={cx(
                "w-full resize-none bg-transparent px-4 pb-2 pt-2.5 leading-relaxed text-fg outline-none placeholder:text-muted/55",
                hasConversation ? "min-h-[68px] text-[15px]" : "min-h-[100px] text-[17px]"
              )}
            />
            <div className="flex flex-wrap items-center justify-between gap-3 border-t border-line px-2 pb-1 pt-2">
              <span className="pl-1 text-[10.5px] text-muted">Enter to run · Shift + Enter for a new line</span>
              <Button type="submit" variant="primary" size="md" disabled={mode === "running" || question.trim().length < 8}>
                {mode === "running" ? "Researching…" : "Run research"} <span aria-hidden="true">→</span>
              </Button>
            </div>
          </form>
          {error && mode !== "error" && <p className="mt-2 text-[12px] text-down">{error}</p>}
          {!hasConversation && <p className="mt-3 text-center text-[10.5px] text-dim">One question in, one report out. No chat history and no automatic thesis changes.</p>}
        </motion.div>
      </div>
    </section>
  );
}
