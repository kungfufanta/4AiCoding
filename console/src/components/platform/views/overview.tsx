import { useEffect, useState } from "react";
import {
  Boxes,
  ChevronRight,
  Container,
  Plus,
  ShieldAlert,
  TriangleAlert,
  UserPlus,
  Users,
} from "lucide-react";
import { get as apiGet } from "@/console/api";
import { AreaChart } from "../charts";
import {
  KpiCard,
  Mono,
  Panel,
  SectionTitle,
  StatusPill,
  VI,
} from "../primitives";
import { type ViewKey } from "@/lib/platform-data";
import { navigate as go } from "@/console/ui";
import { usePlatform } from "@/lib/platform-store";
import { cn } from "@/lib/utils";

const KIND_DOT: Record<string, string> = {
  deploy: "bg-neon",
  info: "bg-info",
  warn: "bg-warn",
  error: "bg-danger",
  access: "bg-admin",
};

export function OverviewView({
  onNavigate,
}: {
  onNavigate: (v: ViewKey) => void;
}) {
  const users = usePlatform((s) => s.users);
  const sandboxes = usePlatform((s) => s.sandboxes);
  const activity = usePlatform((s) => s.activity);
  const me = usePlatform((s) => s.me);
  const fleet = usePlatform((s) => s.fleet);
  const canWrite = me?.role === "admin" || me?.role === "operator";
  const nums = usePlatform((s) => s.overviewNums);

  const [healthCpu, setHealthCpu] = useState<number[]>([]);

  const activeSandboxes = sandboxes.filter(
    (s) => s.status === "Running",
  ).length;
  const provisioning = sandboxes.filter((s) => s.status === "Starting").length;
  const alerts = activity.filter(
    (a) => a.kind === "warn" || a.kind === "error",
  ).length;
  const firstName = (me?.username ?? "operator")
    .split(/[._\-\s]/)[0]
    .replace(/^./, (c) => c.toUpperCase());

  /* real 24h fleet-cpu history behind the health chart (operator role) */
  useEffect(() => {
    let live = true;
    apiGet<{ fleet?: { mem_limit_mb: number }; history: { cpu: number }[] }>(
      "/v1/monitoring?range=24h",
    )
      .then((d) => {
        if (live) setHealthCpu(d.history.map((h) => h.cpu));
      })
      .catch(() => {
        /* viewer role — the chart stays empty */
      });
    return () => {
      live = false;
    };
  }, []);

  const onlinePct = sandboxes.length
    ? Math.round((activeSandboxes / sandboxes.length) * 100)
    : 0;
  const errorRate =
    nums && nums.netTotal > 0
      ? ((nums.netBlocked / nums.netTotal) * 100).toFixed(2)
      : "0.00";
  const health = healthCpu.length ? healthCpu : [0];

  return (
    <div className="space-y-6">
      {/* ── hero strip ─────────────────────────────────────────── */}
      <div className="flex flex-col gap-4 md:flex-row md:items-end md:justify-between">
        <div>
          <h1 className="text-[28px] leading-tight font-bold tracking-tight text-ink">
            Welcome back, {firstName}
          </h1>
          <p className="mt-1.5 text-sm text-gray-400">
            Here's what's happening across your sandbox fleet.
            <span className="text-gray-600">
              <span className="mx-2 text-line">·</span>
              Chào mừng trở lại
            </span>
          </p>
        </div>
        <div className="flex flex-wrap items-center gap-2.5">
          {canWrite && (
            <button
              onClick={() => onNavigate("create")}
              className="inline-flex items-center gap-2 rounded-lg bg-neon px-4 py-2.5 text-sm font-semibold text-navy transition-all hover:bg-neon/90 hover:shadow-[0_0_20px_var(--glow-mid)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
            >
              <Plus className="size-4" /> New Sandbox
            </button>
          )}
          {me?.role === "admin" && (
            <button
              onClick={() => onNavigate("users")}
              className="inline-flex items-center gap-2 rounded-lg border border-line bg-surface px-4 py-2.5 text-sm font-medium text-gray-300 transition-colors hover:bg-fg/5 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
            >
              <UserPlus className="size-4" /> Manage users
            </button>
          )}
        </div>
      </div>

      {/* ── KPI row ────────────────────────────────────────────── */}
      <div className="grid grid-cols-2 gap-4 xl:grid-cols-4">
        <KpiCard
          icon={<Boxes className="size-4.5" />}
          label="Active Sandboxes"
          vi="Đang chạy"
          value={activeSandboxes}
          trend={
            provisioning > 0
              ? { delta: `+${provisioning}`, up: true }
              : undefined
          }
          accent="text-neon"
        />
        <KpiCard
          icon={<Container className="size-4.5" />}
          label="Processes"
          vi="PID đang chạy"
          value={fleet ? fleet.pids : "—"}
          accent="text-info"
        />
        <KpiCard
          icon={<Users className="size-4.5" />}
          label="Users"
          vi="Người dùng"
          value={users.length}
        />
        <KpiCard
          icon={<TriangleAlert className="size-4.5" />}
          label="Warnings"
          vi="Cảnh báo gần đây"
          value={alerts}
          accent="text-warn"
        />
      </div>

      {/* ── main grid ──────────────────────────────────────────── */}
      <div className="grid gap-4 xl:grid-cols-3">
        {/* recent sandboxes */}
        <Panel className="xl:col-span-2 overflow-hidden">
          <div className="p-6 pb-4">
            <SectionTitle
              title="Recent Sandboxes"
              vi="Môi trường gần đây"
              right={
                <button
                  onClick={() => onNavigate("sandboxes")}
                  className="inline-flex items-center gap-1 text-xs text-gray-400 hover:text-neon"
                >
                  view all <ChevronRight className="size-3.5" />
                </button>
              }
            />
          </div>
          <div className="overflow-x-auto">
            <table className="w-full min-w-[480px] text-sm">
              <thead>
                <tr className="border-y border-line-soft text-left text-[11px] tracking-wider text-gray-500 uppercase">
                  <th className="px-6 py-3 font-medium">Sandbox</th>
                  <th className="hidden px-4 py-3 font-medium 2xl:table-cell">Image</th>
                  <th className="px-4 py-3 font-medium">Status</th>
                  <th className="px-4 py-3 font-medium">Resources</th>
                  <th className="px-4 py-3 font-medium">Uptime</th>
                  <th className="px-4 py-3" />
                </tr>
              </thead>
              <tbody>
                {sandboxes.slice(0, 6).map((s) => (
                  <tr
                    key={s.id}
                    onClick={() => {
                      go(`/sandboxes/${s.id}`);
                    }}
                    className="group cursor-pointer border-b border-line-soft last:border-0 transition-colors hover:bg-line/40"
                  >
                    <td className="max-w-[220px] px-6 py-3.5">
                      <div className="truncate text-gray-200">{s.name}</div>
                      {s.name !== s.id && <Mono className="block truncate text-[11px] text-info">{s.id}</Mono>}
                    </td>
                    <td className="hidden max-w-[220px] px-4 py-3.5 2xl:table-cell">
                      <Mono className="block truncate text-xs text-gray-400">
                        {s.image}
                      </Mono>
                    </td>
                    <td className="px-4 py-3.5">
                      <StatusPill status={s.status} />
                    </td>
                    <td className="px-4 py-3.5">
                      <Mono className="whitespace-nowrap text-xs text-gray-400">
                        {s.vcpu} vCPU · {s.memGb} GB
                      </Mono>
                    </td>
                    <td className="px-4 py-3.5">
                      <Mono className="whitespace-nowrap text-xs text-gray-500">{s.uptime}</Mono>
                    </td>
                    <td className="px-4 py-3.5 text-right">
                      <ChevronRight className="ml-auto size-4 text-gray-600 transition-colors group-hover:text-neon" />
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </Panel>

        {/* right column */}
        <div className="flex flex-col gap-4">
          <Panel className="p-6">
            <SectionTitle
              title="System Health"
              vi="Sức khỏe hệ thống"
              right={
                <span className="inline-flex items-center gap-1.5 rounded-md border border-neon/25 bg-neon/5 px-2 py-1 font-mono text-[11px] text-neon">
                  {onlinePct}% fleet online
                </span>
              }
            />
            <AreaChart
              data={health}
              tone="cyan"
              height={130}
              yMax={100}
              gridLines={3}
              showXLabels={false}
              className="mt-4"
            />
            <div className="mt-3 grid grid-cols-3 gap-3 border-t border-line-soft pt-3">
              <div>
                <div className="font-mono text-sm text-ink">
                  {fleet ? `${fleet.avgCpu.toFixed(1)}%` : "—"}
                </div>
                <div className="mt-0.5 text-[10px] text-gray-500">
                  fleet cpu
                </div>
              </div>
              <div>
                <div className="font-mono text-sm text-ink">{errorRate}%</div>
                <div className="mt-0.5 text-[10px] text-gray-500">
                  egress blocked
                </div>
              </div>
              <div>
                <div className="font-mono text-sm text-ink">
                  {nums && nums.nodesTotal
                    ? `${nums.nodesOnline}/${nums.nodesTotal}`
                    : "—"}
                </div>
                <div className="mt-0.5 text-[10px] text-gray-500">
                  nodes healthy
                </div>
              </div>
            </div>
          </Panel>

          <Panel className="flex-1 p-6">
            <SectionTitle title="Recent Activity" vi="Hoạt động gần đây" />
            <div className="mt-4 max-h-[320px] space-y-3 overflow-y-auto pr-1">
              {activity.length === 0 && (
                <p className="text-xs text-gray-500">No audit entries yet.</p>
              )}
              {activity.map((a, i) => (
                <div key={i} className="flex items-start gap-3">
                  <span
                    className={cn(
                      "mt-1.5 size-1.5 shrink-0 rounded-full",
                      KIND_DOT[a.kind],
                    )}
                  />
                  <div className="min-w-0 flex-1">
                    <p className="text-xs leading-relaxed text-gray-300">
                      {a.text}
                    </p>
                    <Mono className="mt-0.5 block text-[10px] text-gray-600">
                      {a.time}
                    </Mono>
                  </div>
                </div>
              ))}
            </div>
          </Panel>
        </div>
      </div>

      {/* audit strip */}
      <Panel className="flex flex-col gap-3 p-4 sm:flex-row sm:items-center sm:justify-between">
        <div className="flex items-center gap-3">
          <div className="grid size-9 place-items-center rounded-lg border border-neon/25 bg-neon/5 text-neon">
            <ShieldAlert className="size-4.5" />
          </div>
          <div>
            <div className="text-sm text-gray-200">
              Every change to sandboxes, users and policy is written to the
              hash-chained audit log.
            </div>
            <VI>Mọi thay đổi đều được ghi vào audit log</VI>
          </div>
        </div>
        <Mono className="text-[11px] text-gray-500">
          last audit: {activity[0]?.time ?? "—"} · {activity.length} recent
          events
        </Mono>
      </Panel>
    </div>
  );
}
