// Minimal dependency-free line chart for metric time series.

// The periods metric charts can show. Samples older than 6 hours are
// 5-minute averages on the server; 30 days is as far back as they go.
export const RANGES: { minutes: number; label: string; title: string }[] = [
  { minutes: 60, label: "1h", title: "last hour" },
  { minutes: 360, label: "6h", title: "last 6 hours" },
  { minutes: 1440, label: "24h", title: "last 24 hours" },
  { minutes: 10080, label: "7d", title: "last 7 days" },
  { minutes: 43200, label: "30d", title: "last 30 days" },
];

export const rangeTitle = (minutes: number) => RANGES.find((r) => r.minutes === minutes)?.title ?? "";

// How often a chart over this period is refreshed: a new sample matters
// less the longer the period.
export const rangeRefresh = (minutes: number) => (minutes <= 360 ? 10_000 : 60_000);

export function RangePicker({ value, onChange }: { value: number; onChange: (minutes: number) => void }) {
  return (
    <div className="segmented" role="group" aria-label="Period">
      {RANGES.map((r) => (
        <button
          key={r.minutes}
          type="button"
          className={value === r.minutes ? "active" : ""}
          title={r.title}
          onClick={() => onChange(r.minutes)}
        >
          {r.label}
        </button>
      ))}
    </div>
  );
}

// GAP: samples further apart than this are not joined (raw samples are
// 10 seconds apart, averaged ones 5 minutes).
const GAP = 15 * 60_000;

export function Chart({
  points,
  max,
  format,
}: {
  points: { t: number; v: number }[];
  max?: number;
  format: (v: number) => string;
}) {
  const W = 600;
  const H = 120;
  const pad = { l: 44, r: 8, t: 8, b: 18 };
  if (points.length < 2) {
    return <div className="faint small" style={{ height: H, display: "grid", placeItems: "center" }}>Collecting data…</div>;
  }
  const t0 = points[0].t;
  const t1 = points[points.length - 1].t;
  const top = max ?? (Math.max(...points.map((p) => p.v)) * 1.15 || 1);
  const x = (t: number) => pad.l + ((t - t0) / Math.max(1, t1 - t0)) * (W - pad.l - pad.r);
  const y = (v: number) => pad.t + (1 - Math.min(v, top) / top) * (H - pad.t - pad.b);
  // A gap in the samples (the server or the panel was down) breaks the
  // line instead of bridging it.
  const runs: { t: number; v: number }[][] = [];
  points.forEach((p, i) => {
    if (i === 0 || p.t - points[i - 1].t > GAP) runs.push([]);
    runs[runs.length - 1].push(p);
  });
  const path = (run: { t: number; v: number }[]) =>
    run.map((p, i) => `${i ? "L" : "M"}${x(p.t).toFixed(1)},${y(p.v).toFixed(1)}`).join("");
  const line = runs.map(path).join("");
  const area = runs
    .map((run) => `${path(run)}L${x(run[run.length - 1].t).toFixed(1)},${y(0)}L${x(run[0].t).toFixed(1)},${y(0)}Z`)
    .join("");
  const ticks = [0, top / 2, top];
  // Over more than a day, the axis shows dates.
  const time = (t: number) =>
    t1 - t0 > 20 * 3600_000
      ? new Date(t).toLocaleString(undefined, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" })
      : new Date(t).toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });

  return (
    <svg className="chart" viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none" role="img">
      {ticks.map((v) => (
        <g key={v}>
          <line className="grid" x1={pad.l} x2={W - pad.r} y1={y(v)} y2={y(v)} />
          <text x={pad.l - 6} y={y(v) + 3} textAnchor="end">
            {format(v)}
          </text>
        </g>
      ))}
      <path className="area" d={area} />
      <path className="line" d={line} vectorEffect="non-scaling-stroke" />
      <text x={pad.l} y={H - 4}>
        {time(t0)}
      </text>
      <text x={W - pad.r} y={H - 4} textAnchor="end">
        {time(t1)}
      </text>
    </svg>
  );
}
