// Minimal dependency-free line chart for metric time series.

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
  const line = points.map((p, i) => `${i ? "L" : "M"}${x(p.t).toFixed(1)},${y(p.v).toFixed(1)}`).join("");
  const area = `${line}L${x(t1).toFixed(1)},${y(0)}L${x(t0).toFixed(1)},${y(0)}Z`;
  const ticks = [0, top / 2, top];
  const time = (t: number) => new Date(t).toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });

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
