import { useLayoutEffect, useRef } from "react";

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
