import { useLayoutEffect, useRef } from "react";
import type { StreamState } from "./stream";

// LogView shows log text and keeps it scrolled to the end unless the user
// scrolled up to read.
export function LogView({ text, tall }: { text: string; tall?: boolean }) {
  const ref = useRef<HTMLPreElement>(null);
  const stick = useRef(true);
  useLayoutEffect(() => {
    const el = ref.current;
    if (el && stick.current) el.scrollTop = el.scrollHeight;
  }, [text]);
  return (
    <pre
      ref={ref}
      className={tall ? "logbox tall" : "logbox"}
      onScroll={(e) => {
        const el = e.currentTarget;
        stick.current = el.scrollHeight - el.scrollTop - el.clientHeight < 24;
      }}
    >
      {text}
    </pre>
  );
}

const MAX_LOG_CHARS = 1_000_000;

// trimLog drops the oldest lines once a live view holds too much text.
export function trimLog(t: string): string {
  if (t.length <= MAX_LOG_CHARS) return t;
  const cut = t.indexOf("\n", t.length - MAX_LOG_CHARS * 0.8);
  return cut < 0 ? t.slice(-MAX_LOG_CHARS) : t.slice(cut + 1);
}

// StreamBadge shows the state of a followed log stream.
export function StreamBadge({ state, ended }: { state: StreamState; ended: string | null }) {
  if (ended !== null)
    return (
      <span className="badge err" title={ended}>
        stopped
      </span>
    );
  if (state === "open") return <span className="badge ok">live</span>;
  if (state === "failed") return <span className="badge err">unavailable</span>;
  return <span className="badge warn">connecting</span>;
}
