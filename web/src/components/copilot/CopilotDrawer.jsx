import { useEffect, useRef, useState } from "react";
import { AnimatePresence, motion, useReducedMotion } from "framer-motion";
import { cx } from "../../lib/cx.js";
import {
  createPersona,
  deletePersona,
  listPersonas,
  runScenario,
  updatePersona,
} from "../../lib/api.js";
import { buildContextTurn, sendContextualChat, suggestionsFor } from "../../lib/askApi.js";
import PersonaMenu from "./PersonaMenu.jsx";
import AskContextChip from "./AskContextChip.jsx";
import SuggestedActions from "./SuggestedActions.jsx";
import { Icon } from "../shell/icons.jsx";

const PERSONA_STORAGE_KEY = "copilot.personaId";

// CopilotDrawer — a global, right-anchored chat drawer wired to the grounded /api/chat endpoint
// (design_examples/4a-welcome + 4b-conversation). It helps the user THINK: replies are grounded in
// the selected ticker's live context and use uncertainty language. The user decides and acts.
// Fail-silent (offline state), a11y (Esc, focus trap, Enter-to-send), reduced-motion aware.

const FOOTER = "Informational, not financial advice — your decision.";

// Context-aware starter prompts, each with a tone-matched icon (design 4a).
const STARTERS = (ticker) => [
  { text: `What's the setup on ${ticker} right now?`, icon: "bars", tone: "llm" },
  { text: "What's today's expected close range?", icon: "clock", tone: "llm" },
  { text: "What would invalidate the current read?", icon: "warn", tone: "warn" },
  { text: `What moved ${ticker} today?`, icon: "trend", tone: "accent", highlight: true },
];

const ICON_PATHS = {
  bars: <path d="M4 20V8M10 20V4M16 20v-7M22 20H2" strokeWidth="1.8" strokeLinecap="round" />,
  clock: <path d="M12 8v4l3 2M21 12a9 9 0 11-18 0 9 9 0 0118 0z" strokeWidth="1.8" strokeLinecap="round" />,
  warn: <path d="M12 9v4M12 16h.01M12 3l9 16H3l9-16z" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" />,
  trend: <path d="M3 17l6-6 4 4 8-8M21 7v5h-5" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" />,
};
const ICON_TONE = { llm: "text-llm", warn: "text-warn", accent: "text-accent" };

function PromptIcon({ name, tone }) {
  return (
    <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" className={cx("shrink-0", ICON_TONE[tone])} aria-hidden="true">
      {ICON_PATHS[name]}
    </svg>
  );
}

function BotAvatar() {
  return (
    <span
      className="flex h-10 w-10 shrink-0 items-center justify-center rounded-md border border-accent/30 bg-accent/12 text-accent"
      aria-hidden="true"
    >
      <svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor">
        <rect x="4" y="7" width="16" height="12" rx="3" strokeWidth="1.8" />
        <path d="M12 3v4M9 12h.01M15 12h.01" strokeWidth="2" strokeLinecap="round" />
      </svg>
    </span>
  );
}

const isOffline = (m) => m.offline || /stub:(offline|gateway-offline|quota)/.test(m.modelUsed || "");
// 429 from the model API: the key works but the free-tier quota is spent — a different story
// than "offline", so tell the user the truth (it comes back when the quota resets).
const isQuota = (m) => /stub:quota/.test(m.modelUsed || "");

// Inline prose coloring: trend words, ± percentages, Buy/Sell/Hold, decimal numbers, the ticker.
function decorate(text, ticker, kp) {
  const re = new RegExp(`([+-]\\d[\\d.]*%|\\d+\\.\\d+|\\buptrend\\b|\\bdowntrend\\b|\\bBuy\\b|\\bSell\\b|\\bHold\\b|\\b${ticker}\\b)`, "g");
  return String(text)
    .split(re)
    .map((p, i) => {
      const key = `${kp}-${i}`;
      if (!p) return null;
      if (/^[+-]\d[\d.]*%$/.test(p)) return <span key={key} className={cx("num", p[0] === "-" ? "text-down" : "text-accent")}>{p}</span>;
      if (/^\d+\.\d+$/.test(p) || p === ticker) return <span key={key} className="num text-fg">{p}</span>;
      if (p === "uptrend" || p === "Buy") return <span key={key} className="text-up">{p}</span>;
      if (p === "downtrend" || p === "Sell") return <span key={key} className="text-down">{p}</span>;
      if (p === "Hold") return <span key={key} className="text-muted">{p}</span>;
      return <span key={key}>{p}</span>;
    });
}

function renderProse(text, ticker) {
  return String(text)
    .split(/(\*\*[^*]+\*\*)/g)
    .filter(Boolean)
    .flatMap((seg, si) =>
      seg.startsWith("**") && seg.endsWith("**")
        ? [<strong key={`b${si}`} className="font-semibold text-fg">{seg.slice(2, -2)}</strong>]
        : decorate(seg, ticker, si)
    );
}

// Pull the computed key levels + expected-close band out of the offline-stub reply so they can be
// rendered as a structured grid (design 4b), and clean them out of the prose to avoid redundancy.
function extractContext(reply) {
  let s = ` ${String(reply)} `;
  const levels = {};
  const rRes = s.match(/Nearest resistance ([\d., ]+?)\s*[–—-][^.]*\.\s*/i);
  if (rRes) { levels.resistance = rRes[1].split(/[\s,]+/).filter(Boolean); s = s.replace(rRes[0], " "); }
  const rSup = s.match(/Nearest support ([\d., ]+?)\s*[–—-][^.]*\.\s*/i);
  if (rSup) { levels.support = rSup[1].split(/[\s,]+/).filter(Boolean); s = s.replace(rSup[0], " "); }
  const rExp = s.match(/The expected-close range is [\d.]+[–—-][\d.]+\s*\(±\s*([\d.]+)\s*around\s*([\d.]+)\)\s*[–—-]\s*/i);
  if (rExp) { levels.band = rExp[1]; levels.mid = rExp[2]; s = s.replace(rExp[0], " "); }
  const prose = s.replace(/\s+/g, " ").trim();
  const hasLevels = levels.resistance?.length || levels.support?.length || levels.mid;
  return { prose, levels: hasLevels ? levels : null };
}

// Split a trailing statistical-band / disclaimer note off so it renders muted, below the levels grid.
function splitNote(prose) {
  const m = prose.match(/^([\s\S]*?)((?:A statistical band|a statistical band|Informational,)[\s\S]*)$/);
  if (m && m[1].trim()) return { body: m[1].trim(), note: m[2].trim() };
  return { body: prose, note: null };
}

function LevelsGrid({ levels }) {
  return (
    <div className="mt-3.5 grid grid-cols-2 gap-2">
      {levels.resistance?.length > 0 && (
        <div className="rounded-md border border-line px-2.5 py-2">
          <div className="mb-1 label-mono text-down">Resistance</div>
          <div className="num text-[13px] font-semibold text-fg">{levels.resistance.join(" · ")}</div>
        </div>
      )}
      {levels.support?.length > 0 && (
        <div className="rounded-md border border-line px-2.5 py-2">
          <div className="mb-1 label-mono text-accent">Support</div>
          <div className="num text-[13px] font-semibold text-fg">{levels.support.join(" · ")}</div>
        </div>
      )}
      {levels.mid && (
        <div className="col-span-2 flex items-center justify-between rounded-md border border-line px-2.5 py-2">
          <div className="label-mono text-muted">Expected-close band</div>
          <div className="num text-[13px] font-semibold text-fg">
            {levels.mid} <span className="text-muted/60">±{levels.band}</span>
          </div>
        </div>
      )}
    </div>
  );
}

function Message({ m, ticker }) {
  const reduce = useReducedMotion();
  const anim = reduce ? {} : { initial: { opacity: 0, y: 6 }, animate: { opacity: 1, y: 0 }, transition: { duration: 0.18 } };

  if (m.role === "user") {
    // The context turn is a user turn and is styled as one, with a caption naming it. The caption
    // is the honesty property made visible: this text was attached on the user's behalf, it went
    // to the model verbatim, and it is right here to be read. `whitespace-pre-wrap` because the
    // composed context is a multi-line list and would otherwise collapse into one run-on line.
    return (
      <motion.div {...anim} className="flex flex-col items-end">
        {m.isContext && (
          <span className="mb-1.5 label-mono text-accent">Context attached · sent to the model</span>
        )}
        <div className="max-w-[82%] whitespace-pre-wrap rounded-[12px_12px_4px_12px] border border-accent/28 bg-accent/10 px-3.5 py-2.5 text-[13.5px] leading-relaxed text-fg">
          {m.content}
        </div>
      </motion.div>
    );
  }

  const offline = isOffline(m);
  const { prose, levels } = extractContext(m.content);
  const { body, note } = splitNote(prose);

  return (
    <motion.div {...anim} className="flex flex-col items-start">
      <div className="mb-2 flex items-center gap-2">
        <span className="label-mono text-muted">Copilot</span>
        {offline ? (
          <>
            <span className="inline-flex items-center gap-1.5 rounded bg-warn/14 px-1.5 py-0.5 label-mono text-warn">
              <span className="h-[5px] w-[5px] rounded-full bg-warn" />
              {isQuota(m) ? "Model quota exhausted — retries after reset" : "Assistant offline"}
            </span>
            <span className="rounded bg-llm/15 px-1.5 py-0.5 label-mono text-llm">Computed context</span>
          </>
        ) : (
          m.modelUsed && <span className="rounded bg-llm/15 px-1.5 py-0.5 label-mono text-llm">{m.modelUsed}</span>
        )}
      </div>
      <div className="max-w-[94%] rounded-[4px_12px_12px_12px] border border-line bg-panel2 px-4 py-3.5">
        <p className="text-[13px] leading-[1.65] text-fg/80">{renderProse(body, ticker)}</p>
        {levels && <LevelsGrid levels={levels} />}
        {note && <p className="mt-3 text-[12px] leading-[1.6] text-muted">{note}</p>}
      </div>
    </motion.div>
  );
}

// ScenarioMessage — the structured what-if result: assumptions -> effect chain -> mitigants ->
// horizons -> watch list. Hypothetical thought experiment, labelled as such; never a prediction.
function ScenarioMessage({ m }) {
  const s = m.scenario;
  const stub = /stub/i.test(String(m.modelUsed || ""));
  const block = (title, items, tone = "text-fg/85") =>
    (items || []).length > 0 && (
      <div className="mt-3">
        <div className="mb-1 label-mono text-muted">{title}</div>
        <ul className={cx("space-y-1 text-[12.5px] leading-relaxed", tone)}>
          {items.map((x, i) => (<li key={i}>• {x}</li>))}
        </ul>
      </div>
    );
  return (
    <div className="flex flex-col items-start">
      <div className="mb-2 flex items-center gap-2">
        <span className="label-mono text-muted">Scenario</span>
        <span className="rounded bg-warn/14 px-1.5 py-0.5 label-mono text-warn">
          Hypothetical — not a prediction
        </span>
        {stub && <span className="rounded bg-warn/14 px-1.5 py-0.5 font-mono text-[9px] uppercase text-warn">offline stub</span>}
      </div>
      <div className="max-w-[94%] rounded-[4px_12px_12px_12px] border border-line bg-panel2 px-4 py-3.5">
        <p className="text-[13px] font-semibold text-fg">{s.scenario}</p>
        {block("Assumptions", s.assumptions, "text-muted")}
        {block("First-order effects", s.firstOrder)}
        {block("Second-order (supply chain, competitors)", s.secondOrder)}
        {block("Mitigants", s.mitigants, "text-accent/90")}
        {s.horizons && (
          <div className="mt-3 grid grid-cols-1 gap-2 sm:grid-cols-2">
            <div className="rounded-md border border-line px-2.5 py-2">
              <div className="mb-1 label-mono text-muted">Short term</div>
              <p className="text-[12px] leading-relaxed text-fg/85">{s.horizons.shortTerm}</p>
            </div>
            <div className="rounded-md border border-line px-2.5 py-2">
              <div className="mb-1 label-mono text-muted">Long term</div>
              <p className="text-[12px] leading-relaxed text-fg/85">{s.horizons.longTerm}</p>
            </div>
          </div>
        )}
        {block("Watch for", s.watchFor, "text-warn/90")}
        {s.summary && <p className="mt-3 border-t border-line pt-2.5 text-[12.5px] leading-relaxed text-muted">{s.summary}</p>}
      </div>
    </div>
  );
}

function TypingDots() {
  return (
    <div className="flex flex-col items-start">
      <span className="mb-2 label-mono text-muted">Copilot</span>
      <div className="rounded-[4px_12px_12px_12px] border border-line bg-panel2 px-4 py-3" aria-label="Copilot is typing">
        <span className="flex gap-1">
          {[0, 0.15, 0.3].map((d, i) => (
            <span key={i} className="h-1.5 w-1.5 rounded-full bg-muted motion-safe:animate-bounce" style={{ animationDelay: `${d}s` }} />
          ))}
        </span>
      </div>
    </div>
  );
}

// `preset` lets the shell launch the drawer already pointed at a research task instead of an empty prompt
// box (guide §7.2): { mode?, personaId?, draft?, nonce }. It is applied when `nonce` changes, so launching
// the same lens twice works. Absent preset = the drawer behaves exactly as before.
//
// WAVE 4 LANE 4C adds two OPTIONAL preset fields and changes nothing else about the drawer:
//
//   context:     { kind, eventId, ticker, label, facts[] }  — what the user is looking at
//   suggestions: [ "…", "…", "…" ]                          — up to three prefilled questions
//
// Both are optional and BOTH MUST STAY OPTIONAL. The drawer's behaviour with no context is exactly
// what it was before this lane touched it — that is a regression requirement, not a nicety, because
// the `c` shortcut and the floating launcher both open it with no preset at all.
//
// Three rules govern the context, and each is visible in the code below rather than asserted here:
//
//   * OPENING SENDS NOTHING (locked decision 9). Attaching a context sets state and renders a chip.
//     No request is issued until the user sends or clicks a suggestion.
//   * THE CONTEXT IS A VISIBLE TRANSCRIPT TURN (locked decision 6). On the FIRST send it is
//     composed into a real user message and pushed into `messages`, so the user reads exactly what
//     the model was given. There is no hidden preamble anywhere in this component.
//   * IT IS COMPOSED ONCE. `contextSentRef` makes the attachment a first-turn event rather than a
//     prefix repeated on every message, which would both waste the window and make the transcript
//     lie about how many times the model was told.
export default function CopilotDrawer({ open, onClose, ticker = "NVDA", timeframe = "1D", preset = null }) {
  const reduce = useReducedMotion();
  const [messages, setMessages] = useState([]); // {role, content, modelUsed?, offline?, scenario?}
  const [input, setInput] = useState("");
  const [pending, setPending] = useState(false);
  const [mode, setMode] = useState("chat"); // chat | scenario ("what if …" structured reasoning)
  // The attached context, and the suggestions that came with it. Held in state rather than read
  // straight off `preset` so that DETACHING works: the user's removal must survive re-renders, and
  // it must not be undone by the same preset object still being in the parent's state.
  const [context, setContext] = useState(null);
  const [suggestions, setSuggestions] = useState([]);
  const contextSentRef = useRef(false);
  const [personas, setPersonas] = useState([]);
  const [personaId, setPersonaId] = useState(() => {
    try { return localStorage.getItem(PERSONA_STORAGE_KEY) || null; } catch { return null; }
  });

  // Load the persona library once (built-in presets + the user's own). Fail-silent: no personas just
  // means the picker offers the presets + Default; the chat works exactly as before.
  useEffect(() => {
    let alive = true;
    listPersonas().then((list) => { if (alive) setPersonas(list); }).catch(() => {});
    return () => { alive = false; };
  }, []);

  // Persist the active persona across sessions; drop a stale id that no longer resolves.
  useEffect(() => {
    try {
      if (personaId) localStorage.setItem(PERSONA_STORAGE_KEY, personaId);
      else localStorage.removeItem(PERSONA_STORAGE_KEY);
    } catch { /* ignore */ }
  }, [personaId]);

  // Apply a shell-supplied launch preset (a lens applied from Research, or scenario mode). Keyed on nonce
  // so repeat launches re-apply; it only sets state the user could set by hand, and sends nothing.
  useEffect(() => {
    if (!preset?.nonce) return;
    if (preset.mode) setMode(preset.mode);
    if (preset.personaId !== undefined) setPersonaId(preset.personaId);
    if (preset.draft) setInput(preset.draft);
    // A new launch replaces the attachment and re-arms the first-send composition. `context`
    // absent means "no context", which is the pre-existing behaviour and must stay reachable.
    if (preset.context !== undefined) {
      setContext(preset.context || null);
      contextSentRef.current = false;
    }
    if (preset.suggestions !== undefined) setSuggestions(preset.suggestions || []);
  }, [preset?.nonce]); // eslint-disable-line react-hooks/exhaustive-deps

  const activePersona = personas.find((p) => p.id === personaId) || null;

  const handleCreatePersona = async (data) => {
    const created = await createPersona(data);
    setPersonas((prev) => [...prev, created]);
    return created;
  };
  const handleUpdatePersona = async (id, patch) => {
    const updated = await updatePersona(id, patch);
    setPersonas((prev) => prev.map((p) => (p.id === id ? updated : p)));
    return updated;
  };
  const handleDeletePersona = async (id) => {
    await deletePersona(id);
    setPersonas((prev) => prev.filter((p) => p.id !== id));
  };

  const panelRef = useRef(null);
  const inputRef = useRef(null);
  const listRef = useRef(null);
  const restoreFocusRef = useRef(null);

  // Reset the thread when the ticker changes — a thread must not span two tickers.
  //
  // The attached context goes with it, and for the same reason: it describes an event or a read
  // belonging to the company the user just left, so carrying it into a new company's thread would
  // silently mislabel what the model is looking at.
  useEffect(() => {
    setMessages([]);
    setInput("");
    setPending(false);
    setContext(null);
    setSuggestions([]);
    contextSentRef.current = false;
  }, [ticker]);

  useEffect(() => {
    if (!open) return;
    restoreFocusRef.current = document.activeElement;
    const t = setTimeout(() => inputRef.current?.focus(), 60);
    const prevOverflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    return () => {
      clearTimeout(t);
      document.body.style.overflow = prevOverflow;
      restoreFocusRef.current?.focus?.();
    };
  }, [open]);

  useEffect(() => {
    listRef.current?.scrollTo({ top: listRef.current.scrollHeight, behavior: reduce ? "auto" : "smooth" });
  }, [messages, pending, reduce]);

  const resetThread = () => {
    setMessages([]);
    setInput("");
    // A new thread re-arms the context: the attachment is still on screen as a chip, so the next
    // first send must compose it again or the model would answer without what the chip promises.
    contextSentRef.current = false;
    inputRef.current?.focus();
  };

  const send = async (text) => {
    const content = (text ?? input).trim();
    if (!content || pending) return;

    // THE CONTEXT TURN. Composed here, on the first send only, as a REAL user message that goes
    // into the transcript the user is reading. It is not a prefix, not a system turn and not a
    // wrapper around the user's question — it is its own visible message, sent before theirs.
    // `buildContextTurn` is pure text assembly over facts already on screen (lib/askApi.js).
    const attach = !contextSentRef.current ? buildContextTurn(context) : null;
    const next = attach
      ? [...messages, attach, { role: "user", content }]
      : [...messages, { role: "user", content }];
    if (attach) contextSentRef.current = true;

    setMessages(next);
    setInput("");
    setPending(true);
    try {
      if (mode === "scenario") {
        const res = await runScenario(ticker, content);
        setMessages([...next, {
          role: "assistant", content: res.structured?.summary || "…",
          scenario: res.structured, modelUsed: res.modelUsed,
        }]);
        setPending(false);
        return;
      }
      // The transcript goes to the server AS THE USER SEES IT — the context turn included, because
      // it is a visible user turn like any other. `sendContextualChat` posts to the same
      // `/api/chat/{ticker}` route `api.js::sendChat` already used; this lane changed the adapter,
      // not the route, and `lib/api.js` is untouched.
      const res = await sendContextualChat({
        ticker,
        timeframe,
        messages: next.map(({ role, content }) => ({ role, content })),
        personaId,
      });
      setMessages([...next, { role: "assistant", content: res.reply || "…", modelUsed: res.modelUsed }]);
    } catch {
      setMessages([
        ...next,
        {
          role: "assistant",
          offline: true,
          content:
            "The assistant is offline right now — I can't reach the model. The rest of the app keeps working; try again in a moment.",
        },
      ]);
    } finally {
      setPending(false);
    }
  };

  const onKeyDown = (e) => {
    if (e.key === "Escape") {
      e.stopPropagation();
      onClose();
      return;
    }
    if (e.key !== "Tab") return;
    const focusables = panelRef.current?.querySelectorAll('button, [href], input, textarea, select, [tabindex]:not([tabindex="-1"])');
    if (!focusables || focusables.length === 0) return;
    const list = Array.from(focusables).filter((el) => !el.disabled && el.offsetParent !== null);
    if (list.length === 0) return;
    const first = list[0];
    const last = list[list.length - 1];
    if (e.shiftKey && document.activeElement === first) {
      e.preventDefault();
      last.focus();
    } else if (!e.shiftKey && document.activeElement === last) {
      e.preventDefault();
      first.focus();
    }
  };

  const onInputKeyDown = (e) => {
    if (e.key === "Enter" && !e.shiftKey) {
      e.preventDefault();
      send();
    }
  };

  const empty = messages.length === 0 && !pending;

  return (
    <AnimatePresence>
      {open && (
        <div className="fixed inset-0 z-50" role="presentation">
          <motion.div
            className="absolute inset-0 bg-black/60 backdrop-blur-[2px]"
            initial={{ opacity: 0 }}
            animate={{ opacity: 1 }}
            exit={{ opacity: 0 }}
            transition={{ duration: 0.2 }}
            onClick={onClose}
            aria-hidden="true"
          />

          <motion.aside
            ref={panelRef}
            role="dialog"
            aria-modal="true"
            aria-label={`${ticker} Copilot`}
            onKeyDown={onKeyDown}
            initial={reduce ? { opacity: 0 } : { x: "100%" }}
            animate={reduce ? { opacity: 1 } : { x: 0 }}
            exit={reduce ? { opacity: 0 } : { x: "100%" }}
            transition={{ duration: 0.22, ease: [0.16, 0.8, 0.24, 1] }}
            className="surface-deep absolute right-0 top-0 flex h-full w-full flex-col !rounded-none sm:w-[440px] sm:!rounded-l-[28px]"
          >
            {/* Header */}
            <header className="flex items-center gap-3 border-b border-line bg-panel/60 px-[18px] py-4">
              <BotAvatar />
              <div className="min-w-0 flex-1">
                <div className="text-[15px] font-semibold text-fg">{ticker} Copilot</div>
                <div className="truncate text-[12px] text-muted">
                  grounded in your live {ticker} · {timeframe} context
                </div>
              </div>
              <div className="relative flex gap-[2px] rounded-full border border-line2 p-[3px]" role="tablist" aria-label="Copilot mode">
                {["chat", "scenario"].map((m) => (
                  <button
                    key={m}
                    type="button"
                    role="tab"
                    aria-selected={mode === m}
                    onClick={() => setMode(m)}
                    className={cx(
                      "rounded-full px-2.5 py-1 label-mono transition-colors",
                      mode === m ? "bg-accent/15 text-accent" : "text-muted hover:bg-white/10 hover:text-fg"
                    )}
                  >
                    {m}
                  </button>
                ))}
              </div>
              <button
                type="button"
                onClick={resetThread}
                className="relative rounded-full border border-line2 px-3 py-1.5 font-mono text-[11px] text-fg/85 transition-colors hover:border-white/34 hover:text-fg"
              >
                New chat
              </button>
              <button
                type="button"
                onClick={onClose}
                aria-label="Close Copilot"
                className="relative flex h-11 w-11 items-center justify-center rounded-full text-muted transition-colors hover:bg-white/10 hover:text-fg sm:h-[30px] sm:w-[30px]"
              >
                <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round">
                  <path d="M6 6l12 12M18 6L6 18" />
                </svg>
              </button>
            </header>

            {/* Persona ("instructor") bar — chat mode only. Shapes tone/teaching style; never the rules. */}
            {mode === "chat" && (
              <div className="flex items-center justify-between gap-2 border-b border-line bg-panel/40 px-[18px] py-2">
                <span className="min-w-0 truncate label-mono text-muted">
                  Instructor
                </span>
                <PersonaMenu
                  personas={personas}
                  selectedId={personaId}
                  onSelect={setPersonaId}
                  onCreate={handleCreatePersona}
                  onUpdate={handleUpdatePersona}
                  onDelete={handleDeletePersona}
                />
              </div>
            )}

            {/* Body */}
            <div ref={listRef} className="flex-1 overflow-y-auto">
              {empty ? (
                <div className="flex h-full flex-col justify-center px-[22px] py-6">
                  <div className="mb-6 text-center">
                    <div className="mb-3 label-mono text-llm">● Computed context ready</div>
                    <h2 className="text-[24px] font-semibold tracking-[-0.01em] text-fg">How can I help you?</h2>
                    <p className="mt-2 text-[13px] leading-relaxed text-muted">
                      Ask about the current {ticker} read. Grounded, descriptive — never a call.
                    </p>
                    {activePersona && (
                      <p className="mt-2 inline-flex items-center gap-1.5 rounded-full border border-accent/30 bg-accent/10 px-2.5 py-1 text-[11.5px] text-fg/85">
                        <span aria-hidden="true">{activePersona.emoji || <Icon name="lens" size={13} />}</span>
                        Instructor: {activePersona.name}
                      </p>
                    )}
                  </div>
                  <div className="flex flex-col gap-2.5">
                    {STARTERS(ticker).map((s) => (
                      <button
                        key={s.text}
                        type="button"
                        onClick={() => send(s.text)}
                        className={cx(
                          "flex items-center gap-3 rounded-lg border px-4 py-3 text-left text-[13.5px] transition-colors",
                          s.highlight
                            ? "border-accent/35 bg-accent/[0.08] font-medium text-fg hover:border-accent/60"
                            : "border-line2 bg-panel2 text-fg/80 hover:border-accent/40 hover:text-fg"
                        )}
                      >
                        <PromptIcon name={s.icon} tone={s.tone} />
                        <span>{s.text}</span>
                      </button>
                    ))}
                  </div>
                </div>
              ) : (
                <div className="flex flex-col gap-4 px-[18px] py-5">
                  {messages.map((m, i) =>
                    m.scenario ? <ScenarioMessage key={i} m={m} /> : <Message key={i} m={m} ticker={ticker} />
                  )}
                  {pending && <TypingDots />}
                </div>
              )}
            </div>

            {/* Input */}
            <div className="border-t border-line bg-panel/60 px-[18px] pb-3 pt-4">
              {/* CONTEXT + SUGGESTIONS sit directly above the composer (doc §16.12), so "what I am
                  asking about" and "what I could ask" are in the same glance as the box you type
                  in. Both render only when a context was attached; with none, this block is absent
                  and the drawer looks and behaves exactly as it did before this lane. */}
              {(context || suggestions.length > 0) && (
                <div className="mb-3 flex flex-col gap-2.5">
                  {context && (
                    <AskContextChip
                      label={context.label}
                      kind={context.kind}
                      onDetach={() => {
                        setContext(null);
                        setSuggestions([]);
                        contextSentRef.current = false;
                      }}
                    />
                  )}
                  <SuggestedActions
                    suggestions={suggestionsFor(context, suggestions)}
                    disabled={pending}
                    // A suggestion is prefilled user text: clicking it sends the sentence on the
                    // button, visibly, exactly as if the user had typed it (locked decision 7).
                    onSelect={(text) => send(text)}
                  />
                </div>
              )}
              <form
                className="flex items-end gap-2 rounded-[14px] border border-line2 bg-[rgba(20,22,27,.65)] py-2 pl-4 pr-2 backdrop-blur-[10px] transition-colors focus-within:border-accent/70"
                onSubmit={(e) => {
                  e.preventDefault();
                  send();
                }}
              >
                <textarea
                  ref={inputRef}
                  value={input}
                  onChange={(e) => setInput(e.target.value)}
                  onKeyDown={onInputKeyDown}
                  rows={1}
                  placeholder={mode === "scenario" ? `What if… (hypothetical for ${ticker})` : "Ask the Copilot…"}
                  className="max-h-28 min-h-[30px] flex-1 resize-none self-center bg-transparent text-[13.5px] leading-relaxed text-fg placeholder:text-muted/70 focus:outline-none"
                />
                <button
                  type="submit"
                  disabled={pending || !input.trim()}
                  aria-label="Send message"
                  className="flex h-11 w-11 shrink-0 items-center justify-center rounded-full bg-fg text-bg transition-colors hover:bg-white disabled:opacity-40 sm:h-8 sm:w-8"
                >
                  <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round">
                    <path d="M12 20V5M6 11l6-6 6 6" />
                  </svg>
                </button>
              </form>
              <p className="mt-2.5 text-center text-[11px] leading-relaxed text-muted/60">{FOOTER}</p>
            </div>
          </motion.aside>
        </div>
      )}
    </AnimatePresence>
  );
}
