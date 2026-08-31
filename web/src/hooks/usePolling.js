import { useEffect, useRef } from "react";

// usePolling — run `callback` immediately then every `intervalMs`. The callback lives in a ref so
// the interval isn't torn down every render; it only re-inits when interval/enabled change. Async
// rejections are swallowed (services fail silent). Pass enabled:false to pause (e.g. panel closed).
export function usePolling(callback, intervalMs, { enabled = true, immediate = true } = {}) {
  const saved = useRef(callback);
  useEffect(() => {
    saved.current = callback;
  });

  useEffect(() => {
    if (!enabled || !intervalMs) return;
    let alive = true;
    const run = () => {
      try {
        const r = saved.current();
        if (r && typeof r.catch === "function") r.catch(() => {});
      } catch {
        /* ignore */
      }
    };
    if (immediate) run();
    const id = setInterval(() => alive && run(), intervalMs);
    return () => {
      alive = false;
      clearInterval(id);
    };
  }, [intervalMs, enabled, immediate]);
}
