import { useEffect, useRef, useState } from "react";
import { getMonitoring, sse, type Monitoring } from "../api";
import { fmtBytes, Loading, Section, useLoad } from "../ui";

// Merged from the SBX Console template ("Monitoring" view): fleet metric
// cards, aggregate CPU / memory charts and a live log stream — all fed by
// /v1/monitoring and the per-sandbox SSE log endpoint. The original console
// ships no chart component, so the two area charts are tiny inline SVGs.

function Area({ points, max, unit }: { points: number[]; max: number; unit: string }) {
  const w = 560;
  const h = 120;
  const step = points.length > 1 ? w / (points.length - 1) : w;
  const y = (v: number) => h - (Math.max(0, Math.min(max, v)) / max) * (h - 6) - 3;
  const line = points.map((v, i) => `${i * step},${y(v)}`).join(" ");
  const area = `0,${h} ${line} ${w},${h}`;
  const peak = Math.max(...points, 0);
  return (
    <div>
      <svg viewBox={`0 0 ${w} ${h}`} className="area-chart" preserveAspectRatio="none" role="img" aria-label={unit}>
        <polygon points={area} fill="var(--chart-fill)" />
        <polyline points={line} fill="none" stroke="var(--ok)" strokeWidth="1.5" />
      </svg>
      <div className="row small muted" style={{ justifyContent: "space-between" }}>
        <span>
          peak {peak.toFixed(1)}
          {unit}
        </span>
        <span>{unit} now: {points.at(-1)?.toFixed(1) ?? "—"}</span>
      </div>
    </div>
  );
}

export function MonitoringPage() {
  const { data, error, reload } = useLoad<Monitoring>(() => getMonitoring(), []);
  const [pick, setPick] = useState<string>("");
  const [lines, setLines] = useState<string[]>([]);
  const [follow, setFollow] = useState(true);
  const box = useRef<HTMLPreElement>(null);

  const running = data?.sandboxes.map((s) => s.id) ?? [];
  const streamId = pick && running.includes(pick) ? pick : running[0] ?? "";

  useEffect(() => {
    setLines([]);
    if (!streamId || !follow) return;
    return sse(`/v1/sandboxes/${streamId}/logs?tail=100&follow=true`, (l) =>
      setLines((xs) => (xs.length > 1000 ? [...xs.slice(-800), l] : [...xs, l])),
    );
  }, [streamId, follow]);

  useEffect(() => {
    if (box.current) box.current.scrollTop = box.current.scrollHeight;
  }, [lines]);

  if (error) return <Loading error={error} />;
  if (!data) return <Loading />;

  return (
    <>
      <div className="row wrap" style={{ justifyContent: "space-between" }}>
        <h2 style={{ margin: 0 }}>Monitoring</h2>
        <button className="ghost" onClick={reload}>
          Refresh
        </button>
      </div>

      <div className="kpis">
        <div className="kpi">
          <b className="ok-text">{data.fleet.running}</b>
          <span>running of {data.fleet.total}</span>
        </div>
        <div className="kpi">
          <b>{data.fleet.avg_cpu}%</b>
          <span>avg CPU</span>
        </div>
        <div className="kpi">
          <b>
            {data.fleet.mem_used_mb} / {Math.round(data.fleet.mem_limit_mb)} MiB
          </b>
          <span>memory</span>
        </div>
      </div>

      <div className="cols-2">
        <Section title="Fleet CPU (last 8 min)">
          <Area points={data.history.map((h) => h.cpu)} max={100} unit="%" />
        </Section>
        <Section title="Fleet memory (last 8 min)">
          <Area points={data.history.map((h) => h.mem_mb)} max={Math.max(64, data.fleet.mem_limit_mb)} unit=" MiB" />
        </Section>
      </div>

      <div className="cols-2">
        <Section title="Per-sandbox stats">
          {data.sandboxes.length === 0 && <p className="muted">Nothing running.</p>}
          <table>
            <thead>
              <tr>
                <th>Sandbox</th>
                <th>CPU</th>
                <th>Memory</th>
                <th>PIDs</th>
              </tr>
            </thead>
            <tbody>
              {data.sandboxes.map((s) => (
                <tr key={s.id}>
                  <td className="mono">{s.id}</td>
                  <td>{s.cpu_percent.toFixed(1)}%</td>
                  <td>
                    {fmtBytes(s.mem_usage_bytes)} <span className="muted">/ {fmtBytes(s.mem_limit_bytes)}</span>
                  </td>
                  <td>{s.pids}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </Section>

        <Section
          title="Live logs"
          actions={
            <>
              <select value={streamId} onChange={(e) => setPick(e.target.value)}>
                {running.map((id) => (
                  <option key={id} value={id}>
                    {id}
                  </option>
                ))}
              </select>
              <label className="row small">
                <input type="checkbox" checked={follow} disabled={!streamId} onChange={(e) => setFollow(e.target.checked)} />{" "}
                Follow
              </label>
            </>
          }
        >
          {!streamId ? (
            <p className="muted">No running sandbox to attach to.</p>
          ) : (
            <pre className="term" ref={box}>
              {lines.length ? lines.join("\n") : "waiting for output…"}
            </pre>
          )}
        </Section>
      </div>
    </>
  );
}
