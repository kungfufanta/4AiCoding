// Usage page (P5): daily series + plan rollup. The dashboard BFF
// returns both the daily totals (sandbox_seconds / vcpu_seconds) and
// the org's plan ceiling, so the page can show a single "this month
// vs. quota" gauge plus a 30-day bar chart.

import { Gauge } from "lucide-react";
import { useShell } from "../../layout/app-shell";
import { useUsage } from "../../lib/queries";
import { Badge } from "../../ui/badge";
import { Card } from "../../ui/card";
import { EmptyState } from "../../ui/skeleton";

export function UsagePage() {
  const { org } = useShell();
  const usage = useUsage(org.id);

  if (usage.isPending) {
    return (
      <div className="space-y-4">
        <h1 className="text-lg font-semibold tracking-tight text-fg">Usage</h1>
        <Card className="p-5">
          <div className="h-32 animate-pulse rounded bg-fg/5" />
        </Card>
      </div>
    );
  }
  if (usage.isError) {
    return (
      <div className="space-y-4">
        <h1 className="text-lg font-semibold tracking-tight text-fg">Usage</h1>
        <Card className="p-5">
          <EmptyState
            icon={<Gauge aria-hidden className="size-8" />}
            title="Could not load usage"
            description={String(usage.error?.message ?? "unknown error")}
          />
        </Card>
      </div>
    );
  }

  const data = usage.data;
  if (!data) return null;

  const plan = data.plan;
  const monthPct =
    plan.monthly_vcpu_seconds > 0
      ? Math.min(100, Math.round((plan.used_vcpu_seconds / plan.monthly_vcpu_seconds) * 100))
      : 0;
  const runningPct =
    plan.max_concurrent_sandboxes > 0
      ? Math.min(100, Math.round((plan.running_sandboxes / plan.max_concurrent_sandboxes) * 100))
      : 0;

  // Daily bar chart: group by day, show vcpu_seconds (or sandbox_seconds
  // when vcpu is missing). 30 days, stacked bars where both series exist.
  const days = aggregateDays(data.series);

  return (
    <div className="space-y-4">
      <div>
        <h1 className="text-lg font-semibold tracking-tight text-fg">Usage</h1>
        <p className="text-sm text-muted">
          {org.name} on the <Badge variant="brand">{plan.plan_name}</Badge> plan.
        </p>
      </div>

      <div className="grid gap-4 md:grid-cols-2">
        <Card className="space-y-3 p-5">
          <div className="text-sm text-muted">Monthly vCPU budget</div>
          <div className="flex items-baseline gap-2">
            <span className="text-2xl font-semibold text-fg">{fmtSeconds(plan.used_vcpu_seconds)}</span>
            <span className="text-sm text-muted">/ {fmtSeconds(plan.monthly_vcpu_seconds)}</span>
          </div>
          <ProgressBar pct={monthPct} />
          <p className="text-xs text-muted">{monthPct}% of the plan's monthly vCPU-seconds used this month.</p>
        </Card>
        <Card className="space-y-3 p-5">
          <div className="text-sm text-muted">Concurrent sandboxes</div>
          <div className="flex items-baseline gap-2">
            <span className="text-2xl font-semibold text-fg">{plan.running_sandboxes}</span>
            <span className="text-sm text-muted">/ {plan.max_concurrent_sandboxes} running</span>
          </div>
          <ProgressBar pct={runningPct} />
          <p className="text-xs text-muted">Quota enforced at sandbox create time — overflow returns 429 quota_exceeded.</p>
        </Card>
      </div>

      <Card className="p-5">
        <div className="mb-3 flex items-center justify-between">
          <div>
            <div className="text-sm font-medium text-fg">Daily vCPU-seconds</div>
            <div className="text-xs text-muted">Last 30 days. Bars stack vCPU-seconds over sandbox-seconds.</div>
          </div>
        </div>
        {days.length === 0 ? (
          <EmptyState
            icon={<Gauge aria-hidden className="size-8" />}
            title="No usage yet"
            description="Run a sandbox and stop it — the webhook bills the elapsed window."
          />
        ) : (
          <BarChart days={days} />
        )}
      </Card>
    </div>
  );
}

// aggregateDays groups the BFF's flat series list by day, summing each
// kind. Returns days sorted ascending.
function aggregateDays(series: { day: string; kind: string; quantity: number }[]): {
  day: string;
  vcpu: number;
  sandbox: number;
}[] {
  const byDay = new Map<string, { vcpu: number; sandbox: number }>();
  for (const p of series) {
    const key = p.day.slice(0, 10); // YYYY-MM-DD
    const e = byDay.get(key) ?? { vcpu: 0, sandbox: 0 };
    if (p.kind === "vcpu_seconds") e.vcpu += p.quantity;
    if (p.kind === "sandbox_seconds") e.sandbox += p.quantity;
    byDay.set(key, e);
  }
  return [...byDay.entries()]
    .map(([day, v]) => ({ day, ...v }))
    .sort((a, b) => a.day.localeCompare(b.day));
}

function BarChart({ days }: { days: { day: string; vcpu: number; sandbox: number }[] }) {
  const max = Math.max(1, ...days.map((d) => d.vcpu + d.sandbox));
  return (
    <div className="flex h-40 items-end gap-[3px]">
      {days.map((d) => {
        const total = d.vcpu + d.sandbox;
        const totalH = (total / max) * 100;
        const vcpuH = total > 0 ? (d.vcpu / total) * totalH : 0;
        const sbH = total > 0 ? (d.sandbox / total) * totalH : 0;
        return (
          <div
            key={d.day}
            className="group relative flex-1"
            title={`${d.day}: ${fmtSeconds(d.vcpu)} vCPU, ${fmtSeconds(d.sandbox)} sandbox`}
          >
            <div className="flex h-40 flex-col justify-end">
              <div
                className="w-full rounded-t bg-brand"
                style={{ height: `${vcpuH}%` }}
                aria-label={`${fmtSeconds(d.vcpu)} vCPU-seconds on ${d.day}`}
              />
              <div
                className="w-full bg-brand/40"
                style={{ height: `${sbH}%` }}
                aria-label={`${fmtSeconds(d.sandbox)} sandbox-seconds on ${d.day}`}
              />
            </div>
            <div
              className="absolute inset-x-0 top-0 h-full opacity-0 group-hover:opacity-100"
              aria-hidden
            />
            <span className="sr-only">{d.day}</span>
            <div style={{ height: `${100 - totalH}%` }} />
          </div>
        );
      })}
    </div>
  );
}

function ProgressBar({ pct }: { pct: number }) {
  return (
    <div className="h-2 w-full overflow-hidden rounded-full bg-fg/10" aria-hidden>
      <div
        className="h-full rounded-full bg-brand transition-all"
        style={{ width: `${pct}%` }}
      />
    </div>
  );
}

function fmtSeconds(s: number): string {
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.round(s / 60)}m`;
  if (s < 86400) return `${(s / 3600).toFixed(1)}h`;
  return `${(s / 86400).toFixed(1)}d`;
}
