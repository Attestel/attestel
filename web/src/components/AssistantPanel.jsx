import { useEffect, useRef, useState } from "react";
import { AnimatePresence, motion, useReducedMotion } from "framer-motion";
import { Panel, Badge, Button, SkeletonText } from "./ui/index.js";
import { cx } from "../lib/cx.js";
import { fmtPrice } from "../lib/format.js";
import { fetchAssistant, sendChat } from "../lib/api.js";

// AssistantPanel — beside the Directional Signal. Three parts:
//   1. Situational note (auto): situation · what to watch · what would change it (grounded AI).
//   2. Expected-close RANGE: a volatility band around the current price (COMPUTED, not a prediction),
//      with the AI's one-line explanation.
//   3. Chat: grounded follow-ups; fail-silent "assistant offline".
// Guardrails on screen: model badge, "informational — your decision, not financial advice", and the
// range is always labelled "volatility range, not a prediction".

const NOT_ADVICE = "Informational — your decision, not financial advice.";

// Small band viz: low —•— high with the current price marked. mid == current price (no drift claim).
function RangeBand({ ec }) {
  if (!ec || ec.low == null || ec.high == null) return null;
  const { low, mid, high } = ec;
  const span = high - low || 1;
  const midPct = Math.max(0, Math.min(100, ((mid - low) / span) * 100));
  const collapsed = high - low < 1e-9; // session over → band collapses to the price
  return (
    <div className="mt-1">
      <div className="relative h-2.5 rounded-full bg-gradient-to-r from-down/25 via-panel2 to-up/25">
        {!collapsed && (
          <motion.span
            initial={{ opacity: 0 }}
            animate={{ opacity: 1 }}
            className="absolute top-1/2 h-3.5 w-[3px] -translate-x-1/2 -translate-y-1/2 rounded-full bg-accent"
            style={{ left: `${midPct}%` }}
            aria-hidden="true"
          />
        )}
      </div>
      <div className="mt-1 flex items-center justify-between text-[11px]">
        <span className="num text-down">{fmtPrice(low)}</span>
        <span className="num font-semibold text-fg">{fmtPrice(mid)}</span>
        <span className="num text-up">{fmtPrice(high)}</span>
      </div>
      <div className="mt-0.5 flex items-center justify-between text-[9px] uppercase tracking-wide text-muted">
        <span>low</span>
        <span>now</span>
        <span>high</span>
      </div>
    </div>
  );
}

function Bullets({ items, marker = "•", tone = "fg" }) {
  if (!items || items.length === 0) return null;
  return (
    <ul className="flex flex-col gap-1 text-[12px] leading-snug">
      {items.map((x, i) => (
        <li key={i} className={cx("flex gap-1.5", tone === "muted" ? "text-muted" : "text-fg")}>
          <span aria-hidden="true" className="mt-[1px] shrink-0 text-muted">{marker}</span>
          <span>{typeof x === "string" ? x : JSON.stringify(x)}</span>
        </li>
      ))}
    </ul>
  );
}

export default function AssistantPanel({ ticker, timeframe, expectedClose }) {
  const reduce = useReducedMotion();
  const [note, setNote] = useState(null);
  const [loading, setLoading] = useState(true);

  const [messages, setMessages] = useState([]); // {role, content, offline?}
  const [input, setInput] = useState("");
  const [sending, setSending] = useState(false);
  const listRef = useRef(null);

  // (Re)load the situational note when ticker/timeframe changes.
  useEffect(() => {
    let alive = true;
    setLoading(true);
    setNote(null);
    fetchAssistant(ticker, timeframe)
      .then((n) => alive && setNote(n))
      .catch(() => alive && setNote({ error: true }))
      .finally(() => alive && setLoading(false));
    return () => {
      alive = false;
    };
  }, [ticker, timeframe]);

  // Reset the chat thread on context switch (the old thread was grounded in a different context).
  useEffect(() => {
    setMessages([]);
    setInput("");
  }, [ticker, timeframe]);

  useEffect(() => {
    listRef.current?.scrollTo({ top: listRef.current.scrollHeight });
  }, [messages, sending]);

  const s = note?.structured || {};
  const stub = String(note?.modelUsed || "").includes("stub");
  const ec = expectedClose || null;

  const send = async () => {
    const q = input.trim();
    if (!q || sending) return;
    const next = [...messages, { role: "user", content: q }];
    setMessages(next);
    setInput("");
    setSending(true);
    try {
      const res = await sendChat(ticker, timeframe, next.map(({ role, content }) => ({ role, content })));
      setMessages([...next, { role: "assistant", content: res.reply, modelUsed: res.modelUsed }]);
    } catch {
      setMessages([...next, {
        role: "assistant", offline: true,
        content: "Assistant offline — the computed context and the volatility range above still hold.",
      }]);
    } finally {
      setSending(false);
    }
  };

  const badges = (
    <>
      <Badge variant={stub ? "warn" : "ok"} title="Model behind the assistant">
        {note?.modelUsed || "assistant"}
      </Badge>
      <Badge variant="info" title="Grounded AI + a computed volatility range — not a prediction">AI assistant</Badge>
    </>
  );

  return (
    <Panel title="Assistant" badges={badges}>
      {/* 1. Situational note */}
      {loading ? (
        <SkeletonText lines={4} />
      ) : note?.error ? (
        <p className="text-[12px] text-muted">Assistant offline — the volatility range below still renders.</p>
      ) : (
        <motion.div
          initial={reduce ? false : { opacity: 0, y: 6 }}
          animate={{ opacity: 1, y: 0 }}
          transition={{ duration: 0.2 }}
        >
          <p className="text-[13px] leading-relaxed text-fg">{s.situation || "—"}</p>
          {s.whatToWatch?.length > 0 && (
            <div className="mt-2.5">
              <div className="mb-1 text-[10px] font-semibold uppercase tracking-wide text-accent">What to watch</div>
              <Bullets items={s.whatToWatch} marker="›" />
            </div>
          )}
          {s.wouldChangeIt?.length > 0 && (
            <div className="mt-2.5">
              <div className="mb-1 text-[10px] font-semibold uppercase tracking-wide text-accent">What would change it</div>
              <Bullets items={s.wouldChangeIt} marker="~" tone="muted" />
            </div>
          )}
        </motion.div>
      )}

      {/* 2. Expected-close range (COMPUTED, always renders if available) */}
      {ec && (
        <div className="mt-3 rounded-lg border border-line bg-panel2/40 p-3">
          <div className="flex items-center justify-between">
            <span className="text-[11px] font-semibold uppercase tracking-wide text-fg">Expected close · {timeframe}</span>
            <Badge variant="warn" title={ec.label}>volatility range, not a prediction</Badge>
          </div>
          <RangeBand ec={ec} />
          <p className="mt-2 text-[11px] leading-relaxed text-muted">
            {s.rangeComment ||
              `${ec.method || "±1 ATR × time-remaining"} (${ec.confidence || "~68% band"}). A statistical band from volatility — price can gap through it.`}
          </p>
        </div>
      )}

      {/* 3. Chat */}
      <div className="mt-3 border-t border-line pt-2.5">
        <div className="mb-1.5 text-[10px] font-semibold uppercase tracking-wide text-accent">Ask about {ticker}</div>
        <div
          ref={listRef}
          className="flex max-h-56 flex-col gap-1.5 overflow-y-auto"
          aria-live="polite"
        >
          {messages.length === 0 && !sending && (
            <p className="text-[11px] italic text-muted">
              e.g. “what’s the setup?”, “where’s support?”, “what would flip this bearish?” — answers are grounded in the live context above.
            </p>
          )}
          <AnimatePresence initial={false}>
            {messages.map((m, i) => (
              <motion.div
                key={i}
                initial={reduce ? false : { opacity: 0, y: 4 }}
                animate={{ opacity: 1, y: 0 }}
                className={cx(
                  "max-w-[92%] rounded-lg px-2.5 py-1.5 text-[12px] leading-snug",
                  m.role === "user"
                    ? "self-end bg-accent/12 text-fg"
                    : cx("self-start bg-panel2 text-fg", m.offline && "border border-warn/30 text-warn")
                )}
              >
                {m.content}
              </motion.div>
            ))}
          </AnimatePresence>
          {sending && (
            <div className="self-start rounded-lg bg-panel2 px-2.5 py-1.5 text-[12px] text-muted">
              <span className="inline-flex gap-1">
                <span className="motion-safe:animate-bounce">·</span>
                <span className="motion-safe:animate-bounce [animation-delay:0.15s]">·</span>
                <span className="motion-safe:animate-bounce [animation-delay:0.3s]">·</span>
              </span>
            </div>
          )}
        </div>
        <form
          className="mt-2 flex items-center gap-1.5"
          onSubmit={(e) => {
            e.preventDefault();
            send();
          }}
        >
          <input
            value={input}
            onChange={(e) => setInput(e.target.value)}
            placeholder="Ask a follow-up…"
            disabled={sending}
            className="min-w-0 flex-1 rounded-md border border-line bg-panel2 px-2.5 py-1.5 text-[12px] text-fg placeholder:text-muted/70 focus:border-accent/60 focus:outline-none"
          />
          <Button type="submit" size="sm" disabled={sending || !input.trim()}>
            Send
          </Button>
        </form>
      </div>

      <p className="mt-2.5 text-[11px] leading-relaxed text-muted">
        {NOT_ADVICE} {note?.disclaimer && !String(note.disclaimer).includes("decision") ? note.disclaimer : ""}
      </p>
    </Panel>
  );
}
