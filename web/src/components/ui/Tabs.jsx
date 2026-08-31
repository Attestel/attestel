import { useRef } from "react";
import { cx } from "../../lib/cx.js";

// Tabs — segmented control (timeframe toggle, filters). Roving-tabindex tablist with arrow-key
// navigation for keyboard a11y. `items` = [{ value, label, title }].
export function Tabs({ items, value, onChange, size = "md", className, ariaLabel }) {
  const ref = useRef(null);

  const move = (dir) => {
    const idx = items.findIndex((i) => i.value === value);
    if (idx < 0) return;
    const next = (idx + dir + items.length) % items.length;
    onChange(items[next].value);
    // focus the newly-selected tab
    requestAnimationFrame(() => {
      const btns = ref.current?.querySelectorAll("[role=tab]");
      btns?.[next]?.focus();
    });
  };

  const onKeyDown = (e) => {
    if (e.key === "ArrowRight" || e.key === "ArrowDown") {
      e.preventDefault();
      move(1);
    } else if (e.key === "ArrowLeft" || e.key === "ArrowUp") {
      e.preventDefault();
      move(-1);
    } else if (e.key === "Home") {
      e.preventDefault();
      onChange(items[0].value);
    } else if (e.key === "End") {
      e.preventDefault();
      onChange(items[items.length - 1].value);
    }
  };

  const pad = size === "sm" ? "px-2.5 py-1 text-[11px]" : "px-3.5 py-1.5 text-xs";

  // Pill segmented control — the landing's `.glass-nav` links: a full-round track on panel2 with
  // full-round items, active state in indigo (brand = active, never green).
  return (
    <div
      ref={ref}
      role="tablist"
      aria-label={ariaLabel}
      onKeyDown={onKeyDown}
      className={cx(
        "inline-flex items-center rounded-full border border-line bg-panel2 p-[2px]",
        className
      )}
    >
      {items.map((it) => {
        const active = it.value === value;
        return (
          <button
            key={it.value}
            role="tab"
            aria-selected={active}
            title={it.title}
            tabIndex={active ? 0 : -1}
            onClick={() => onChange(it.value)}
            className={cx(
              "rounded-full font-semibold whitespace-nowrap transition-colors duration-200 ease-premium",
              pad,
              active ? "bg-accent/15 text-accent" : "text-muted hover:bg-white/10 hover:text-fg"
            )}
          >
            {it.label}
          </button>
        );
      })}
    </div>
  );
}
