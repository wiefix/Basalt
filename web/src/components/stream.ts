import { useEffect, useRef, useState } from "react";

export type StreamState = "connecting" | "open" | "ended" | "failed";

// useEventStream follows a server-sent event stream while url is set. Each
// handler receives the parsed JSON data of events with its name. An "end"
// event closes the stream (otherwise EventSource would reconnect).
export function useEventStream(url: string | null, handlers: Record<string, (data: any) => void>): StreamState {
  const ref = useRef(handlers);
  ref.current = handlers;
  const [state, setState] = useState<StreamState>("connecting");

  useEffect(() => {
    if (!url) return;
    setState("connecting");
    const es = new EventSource(url);
    const names = new Set([...Object.keys(ref.current), "end"]);
    for (const name of names) {
      es.addEventListener(name, (e) => {
        let data: unknown = null;
        try {
          data = JSON.parse((e as MessageEvent).data);
        } catch {}
        ref.current[name]?.(data);
        if (name === "end") {
          es.close();
          setState("ended");
        }
      });
    }
    es.onopen = () => setState("open");
    // A refused stream (4xx) is not retried by the browser; a dropped one is.
    es.onerror = () => setState(es.readyState === EventSource.CLOSED ? "failed" : "connecting");
    return () => es.close();
  }, [url]);

  return state;
}
