import { createContext, useCallback, useContext, useRef, useState } from "react";
import { AnimatePresence, motion } from "framer-motion";
import { cx } from "../../lib/cx.js";
import { Icon } from "../shell/icons.jsx";

const ToastCtx = createContext(() => {});

// useToast() → push({ title, message, tone, ttl }). tone ∈ error | warn | success | info.
export function useToast() {
  return useContext(ToastCtx);
}

const TONE = {
  error: { border: "!border-down/50", accent: "text-down", icon: "close" },
  warn: { border: "!border-warn/60", accent: "text-warn", icon: "warning" },
  success: { border: "!border-up/50", accent: "text-up", icon: "check" },
  info: { border: "!border-info/50", accent: "text-info", icon: "info" },
};

export function ToastProvider({ children }) {
  const [toasts, setToasts] = useState([]);
  const idRef = useRef(0);

  const remove = useCallback((id) => setToasts((t) => t.filter((x) => x.id !== id)), []);

  const push = useCallback(
    ({ title, message, tone = "info", ttl = 4500 }) => {
      const id = ++idRef.current;
      setToasts((t) => [...t, { id, title, message, tone }]);
      if (ttl) setTimeout(() => remove(id), ttl);
      return id;
    },
    [remove]
  );

  return (
    <ToastCtx.Provider value={push}>
      {children}
      <div className="pointer-events-none fixed bottom-4 right-4 z-[100] flex w-[min(360px,calc(100vw-2rem))] flex-col gap-2">
        <AnimatePresence initial={false}>
          {toasts.map((t) => {
            const tone = TONE[t.tone] || TONE.info;
            return (
              <motion.div
                key={t.id}
                layout
                initial={{ opacity: 0, y: 12, scale: 0.98 }}
                animate={{ opacity: 1, y: 0, scale: 1 }}
                exit={{ opacity: 0, x: 24 }}
                transition={{ duration: 0.18, ease: [0.16, 0.8, 0.24, 1] }}
                role="status"
                className={cx(
                  "surface-deep pointer-events-auto flex items-start gap-2.5 !rounded-xl px-4 py-3.5",
                  tone.border
                )}
              >
                <span className={cx("relative mt-px flex-none", tone.accent)}>
                  <Icon name={tone.icon} size={15} />
                </span>
                <div className="relative min-w-0 flex-1">
                  {t.title && <div className="text-[13px] font-semibold text-fg">{t.title}</div>}
                  {t.message && <div className="text-xs text-muted">{t.message}</div>}
                </div>
                <button
                  onClick={() => remove(t.id)}
                  aria-label="Dismiss"
                  className="relative -mr-1.5 -mt-1 flex-none rounded-full p-1.5 text-muted transition-colors hover:bg-white/10 hover:text-fg"
                >
                  <Icon name="close" size={13} />
                </button>
              </motion.div>
            );
          })}
        </AnimatePresence>
      </div>
    </ToastCtx.Provider>
  );
}
