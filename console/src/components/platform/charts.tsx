

import { useCallback, useEffect, useId, useMemo, useRef, useState } from "react";
import { cn } from "@/lib/utils";

/* ── tone → CSS variable mapping (theme-aware) ────────────────── */
export type ChartTone = "neon" | "cyan" | "amber" | "danger";
const TONE_VAR: Record<ChartTone, string> = {
  neon: "var(--neon)",
  cyan: "var(--info)",
  amber: "var(--warn)",
  danger: "var(--danger)",
};

/* ── container size hook (crisp 1:1 pixel rendering) ──────────── */
function useSize<T extends HTMLElement>() {
  const ref = useRef<T | null>(null);
  const [w, setW] = useState(0);
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    const ro = new ResizeObserver((entries) => {
      const cr = entries[0]?.contentRect;
      if (cr) setW(Math.round(cr.width));
    });
    ro.observe(el);
    setW(Math.round(el.getBoundingClientRect().width));
    return () => ro.disconnect();
  }, []);
  return { ref, width: w };
}

/* ── Catmull-Rom → cubic Bézier smoothing ─────────────────────── */
interface Pt {
  x: number;
  y: number;
}
function smoothPath(pts: Pt[]): string {
  if (pts.length === 0) return "";
  if (pts.length === 1) return `M ${pts[0].x},${pts[0].y}`;
  let d = `M ${pts[0].x.toFixed(2)},${pts[0].y.toFixed(2)}`;
  for (let i = 0; i < pts.length - 1; i++) {
    const p0 = pts[Math.max(0, i - 1)];
    const p1 = pts[i];
    const p2 = pts[i + 1];
    const p3 = pts[Math.min(pts.length - 1, i + 2)];
    const c1x = p1.x + (p2.x - p0.x) / 6;
    const c1y = p1.y + (p2.y - p0.y) / 6;
    const c2x = p2.x - (p3.x - p1.x) / 6;
    const c2y = p2.y - (p3.y - p1.y) / 6;
    d += ` C ${c1x.toFixed(2)},${c1y.toFixed(2)} ${c2x.toFixed(2)},${c2y.toFixed(2)} ${p2.x.toFixed(2)},${p2.y.toFixed(2)}`;
  }
  return d;
}

/* ══ AreaChart ══════════════════════════════════════════════════ */
export function AreaChart({
  data,
  tone = "neon",
  height = 220,
  yMax = 100,
  yUnit = "%",
  xTicks = [],
  xLabelFor,
  gridLines = 4,
  showXLabels = true,
  className,
}: {
  data: number[];
  tone?: ChartTone;
  height?: number;
  yMax?: number;
  yUnit?: string;
  xTicks?: string[];
  xLabelFor?: (i: number) => string;
  gridLines?: number;
  showXLabels?: boolean;
  className?: string;
}) {
  const { ref, width } = useSize<HTMLDivElement>();
  const gid = useId().replace(/[:]/g, "");
  const [hover, setHover] = useState<number | null>(null);

  const padL = 38;
  const padR = 10;
  const padT = 12;
  const padB = showXLabels ? 24 : 8;
  const innerW = Math.max(0, width - padL - padR);
  const innerH = height - padT - padB;

  const pts: Pt[] = useMemo(() => {
    if (!data.length || innerW <= 0) return [];
    const step = data.length > 1 ? innerW / (data.length - 1) : 0;
    return data.map((v, i) => ({
      x: padL + i * step,
      y: padT + innerH - (Math.min(v, yMax) / yMax) * innerH,
    }));
  }, [data, innerW, innerH, yMax, padL, padT]);

  const linePath = useMemo(() => smoothPath(pts), [pts]);
  const areaPath = useMemo(() => {
    if (!pts.length) return "";
    const bottom = padT + innerH;
    return `${linePath} L ${pts[pts.length - 1].x.toFixed(2)},${bottom} L ${pts[0].x.toFixed(2)},${bottom} Z`;
  }, [linePath, pts, padT, innerH]);

  const color = TONE_VAR[tone];

  const onMove = useCallback(
    (e: React.MouseEvent<SVGSVGElement>) => {
      if (!pts.length) return;
      const rect = e.currentTarget.getBoundingClientRect();
      const x = e.clientX - rect.left;
      let best = 0;
      let bestD = Infinity;
      for (let i = 0; i < pts.length; i++) {
        const d = Math.abs(pts[i].x - x);
        if (d < bestD) {
          bestD = d;
          best = i;
        }
      }
      setHover(best);
    },
    [pts]
  );

  const label = (i: number) => (xLabelFor ? xLabelFor(i) : xTicks[i] ?? "");

  return (
    <div ref={ref} className={cn("relative w-full select-none", className)} style={{ height }}>
      {width > 0 && pts.length > 0 && (
        <svg
          width={width}
          height={height}
          className="overflow-visible block"
          onMouseMove={onMove}
          onMouseLeave={() => setHover(null)}
        >
          <defs>
            <linearGradient id={`fill-${gid}`} x1="0" y1="0" x2="0" y2="1">
              <stop offset="0%" style={{ stopColor: color }} stopOpacity="0.32" />
              <stop offset="55%" style={{ stopColor: color }} stopOpacity="0.10" />
              <stop offset="100%" style={{ stopColor: color }} stopOpacity="0" />
            </linearGradient>
          </defs>

          {/* horizontal grid + y labels */}
          {Array.from({ length: gridLines + 1 }, (_, i) => {
            const y = padT + (innerH * i) / gridLines;
            const v = Math.round(yMax * (1 - i / gridLines));
            return (
              <g key={i}>
                <line
                  x1={padL}
                  x2={width - padR}
                  y1={y}
                  y2={y}
                  style={{ stroke: "var(--line)" }}
                  strokeWidth="1"
                  strokeDasharray={i === gridLines ? undefined : "2 6"}
                  opacity={i === gridLines ? 0.9 : 0.55}
                />
                <text
                  x={padL - 8}
                  y={y + 3.5}
                  textAnchor="end"
                  className="fill-gray-500"
                  fontSize="9.5"
                  fontFamily="var(--font-jetbrains), monospace"
                >
                  {v}
                  {yUnit}
                </text>
              </g>
            );
          })}

          {/* area + line (soft glow underneath) */}
          <path d={areaPath} fill={`url(#fill-${gid})`} />
          <path
            d={linePath}
            fill="none"
            style={{ stroke: color }}
            strokeOpacity="0.22"
            strokeWidth="5"
            strokeLinecap="round"
          />
          <path
            d={linePath}
            fill="none"
            style={{ stroke: color }}
            strokeWidth="1.8"
            strokeLinecap="round"
          />

          {/* x labels */}
          {showXLabels &&
            xTicks.map((t, i) => {
              const x =
                pts.length < 2
                  ? padL
                  : padL + (innerW * i) / (xTicks.length - 1);
              return (
                <text
                  key={i}
                  x={x}
                  y={height - 6}
                  textAnchor={i === 0 ? "start" : i === xTicks.length - 1 ? "end" : "middle"}
                  className="fill-gray-500"
                  fontSize="9.5"
                  fontFamily="var(--font-jetbrains), monospace"
                >
                  {t}
                </text>
              );
            })}

          {/* hover crosshair */}
          {hover !== null && pts[hover] && (
            <g>
              <line
                x1={pts[hover].x}
                x2={pts[hover].x}
                y1={padT}
                y2={padT + innerH}
                style={{ stroke: color }}
                strokeOpacity="0.4"
                strokeDasharray="3 3"
              />
              <circle cx={pts[hover].x} cy={pts[hover].y} r="6" style={{ fill: color }} fillOpacity="0.18" />
              <circle
                cx={pts[hover].x}
                cy={pts[hover].y}
                r="3"
                style={{ fill: "var(--bg)", stroke: color }}
                strokeWidth="1.6"
              />
            </g>
          )}
        </svg>
      )}

      {/* tooltip */}
      {hover !== null && pts[hover] && (
        <div
          className="pointer-events-none absolute z-10 -translate-x-1/2 rounded-lg border border-line bg-sunken/95 px-2.5 py-1.5 panel-shadow whitespace-nowrap"
          style={{
            left: Math.min(Math.max(pts[hover].x, 56), width - 56),
            top: Math.max(pts[hover].y - 46, 0),
          }}
        >
          <div className="font-mono text-[10px] text-gray-500">{label(hover)}</div>
          <div className="font-mono text-xs font-semibold" style={{ color }}>
            {data[hover]}
            {yUnit}
          </div>
        </div>
      )}
    </div>
  );
}

/* ══ Sparkline ══════════════════════════════════════════════════ */
export function Sparkline({
  data,
  tone = "neon",
  height = 34,
  className,
}: {
  data: number[];
  tone?: ChartTone;
  height?: number;
  className?: string;
}) {
  const gid = useId().replace(/[:]/g, "");
  const color = TONE_VAR[tone];
  const W = 100;
  const H = 36;
  const min = Math.min(...data);
  const max = Math.max(...data);
  const span = max - min || 1;
  const pts: Pt[] = data.map((v, i) => ({
    x: (i / (data.length - 1)) * W,
    y: 3 + (H - 6) * (1 - (v - min) / span),
  }));
  const line = smoothPath(pts);
  const area = `${line} L ${W},${H} L 0,${H} Z`;
  return (
    <svg
      viewBox={`0 0 ${W} ${H}`}
      preserveAspectRatio="none"
      style={{ height }}
      className={cn("w-full block", className)}
    >
      <defs>
        <linearGradient id={`spark-${gid}`} x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" style={{ stopColor: color }} stopOpacity="0.30" />
          <stop offset="100%" style={{ stopColor: color }} stopOpacity="0" />
        </linearGradient>
      </defs>
      <path d={area} fill={`url(#spark-${gid})`} />
      <path
        d={line}
        fill="none"
        style={{ stroke: color }}
        strokeWidth="1.4"
        vectorEffect="non-scaling-stroke"
        strokeLinecap="round"
      />
    </svg>
  );
}

/* ══ Donut progress (disk usage) ════════════════════════════════ */
export function DonutProgress({
  value,
  tone = "neon",
  size = 88,
  label,
}: {
  value: number;
  tone?: ChartTone;
  size?: number;
  label?: string;
}) {
  const gid = useId().replace(/[:]/g, "");
  const color = TONE_VAR[tone];
  const stroke = 7;
  const r = (size - stroke) / 2;
  const c = 2 * Math.PI * r;
  const filled = (Math.min(value, 100) / 100) * c;
  return (
    <div className="relative inline-grid place-items-center" style={{ width: size, height: size }}>
      <svg width={size} height={size} className="-rotate-90">
        <defs>
          <linearGradient id={`donut-${gid}`} x1="0" y1="0" x2="1" y2="1">
            <stop offset="0%" style={{ stopColor: color }} stopOpacity="0.55" />
            <stop offset="100%" style={{ stopColor: color }} />
          </linearGradient>
        </defs>
        <circle cx={size / 2} cy={size / 2} r={r} fill="none" style={{ stroke: "var(--line)" }} strokeWidth={stroke} />
        <circle
          cx={size / 2}
          cy={size / 2}
          r={r}
          fill="none"
          stroke={`url(#donut-${gid})`}
          strokeWidth={stroke}
          strokeLinecap="round"
          strokeDasharray={`${filled} ${c - filled}`}
          className="transition-[stroke-dasharray] duration-700 ease-out"
        />
      </svg>
      <div className="absolute inset-0 grid place-items-center">
        <span className="font-mono text-sm font-semibold" style={{ color }}>
          {Math.round(value)}%
        </span>
        {label && <span className="text-[9px] text-gray-500 absolute bottom-1.5">{label}</span>}
      </div>
    </div>
  );
}

/* ══ MiniBars (network up/down) ═════════════════════════════════ */
export function MiniBars({
  data,
  tone = "cyan",
  height = 40,
  className,
}: {
  data: number[];
  tone?: ChartTone;
  height?: number;
  className?: string;
}) {
  const color = TONE_VAR[tone];
  const max = Math.max(...data, 0.001);
  return (
    <div className={cn("flex items-end gap-[3px]", className)} style={{ height }}>
      {data.map((v, i) => (
        <div
          key={i}
          className="flex-1 rounded-sm transition-all duration-500"
          style={{
            height: `${Math.max(8, (v / max) * 100)}%`,
            background: color,
            opacity: 0.25 + (v / max) * 0.75,
          }}
        />
      ))}
    </div>
  );
}
