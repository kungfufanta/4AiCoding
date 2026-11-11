

import { useMemo, useState } from "react";
import {
  ChevronDown,
  Container,
  Pause,
  Play,
  Plus,
  RotateCw,
  Search,
  SlidersHorizontal,
} from "lucide-react";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { toast } from "sonner";
import { type SandboxStatus, type ViewKey } from "@/lib/platform-data";
import { usePlatform } from "@/lib/platform-store";
import { cn } from "@/lib/utils";
import { Mono, PageHeader, Panel, Segmented, StatusPill, VI } from "../primitives";

type StatusFilter = "All" | SandboxStatus;

const FILTERS: Array<{ value: StatusFilter; label: string }> = [
  { value: "All", label: "All" },
  { value: "Running", label: "Running" },
  { value: "Starting", label: "Starting" },
  { value: "Stopped", label: "Stopped" },
  { value: "Error", label: "Error" },
];

/* tiny inline meter for CPU / MEM columns */
function UsageMeter({ pct, tone }: { pct: number; tone: "neon" | "info" }) {
  return (
    <div className="flex items-center gap-2">
      <div className="h-1.5 w-14 overflow-hidden rounded-full bg-line">
        <div
          className={cn(
            "h-full rounded-full bg-gradient-to-r transition-all",
            tone === "neon" ? "from-neon/50 to-neon" : "from-info/50 to-info"
          )}
          style={{ width: `${Math.min(100, pct)}%` }}
        />
      </div>
      <Mono className={cn("text-[11px]", pct >= 80 ? "text-danger" : "text-gray-400")}>
        {pct}%
      </Mono>
    </div>
  );
}

export function SandboxListView({ onNavigate }: { onNavigate: (v: ViewKey) => void }) {
  const sandboxes = usePlatform((s) => s.sandboxes);
  const openSandbox = usePlatform((s) => s.openSandbox);
  const setSandboxStatus = usePlatform((s) => s.setSandboxStatus);

  const [filter, setFilter] = useState<StatusFilter>("All");
  const [query, setQuery] = useState("");

  const counts = useMemo(() => {
    const c: Record<StatusFilter, number> = { All: sandboxes.length, Running: 0, Starting: 0, Suspended: 0, Stopped: 0, Error: 0 };
    for (const b of sandboxes) c[b.status] = (c[b.status] ?? 0) + 1;
    return c;
  }, [sandboxes]);

  const rows = useMemo(() => {
    const q = query.trim().toLowerCase();
    return sandboxes.filter((b) => {
      if (filter !== "All" && b.status !== filter) return false;
      if (!q) return true;
      return (
        b.name.toLowerCase().includes(q) ||
        b.id.toLowerCase().includes(q) ||
        b.image.toLowerCase().includes(q) ||
        b.host.toLowerCase().includes(q)
      );
    });
  }, [sandboxes, filter, query]);

  const resume = (id: string, name: string) => {
    setSandboxStatus(id, "Starting");
    toast.info(`${id} resuming…`, { description: "Allocating resources and probing health." });
    setTimeout(() => {
      setSandboxStatus(id, "Running");
      toast.success(`${name} is live`, { description: "Status changed to Running." });
    }, 1800);
  };

  const suspend = (id: string) => {
    setSandboxStatus(id, "Suspended");
    toast.warning(`${id} suspended`, { description: "Compute released — snapshot retained." });
  };

  const restart = (id: string, name: string) => {
    setSandboxStatus(id, "Starting");
    setTimeout(() => {
      setSandboxStatus(id, "Running");
      toast.success(`${name} restarted`, { description: "Healthcheck OK — back to Running." });
    }, 2200);
  };

  return (
    <div className="space-y-5">
      <PageHeader
        title="Sandboxes"
        subtitle="Every isolated environment in this workspace"
        vi="Danh sách môi trường thực thi"
        actions={
          <button
            onClick={() => onNavigate("create")}
            className="inline-flex items-center gap-2 rounded-lg bg-neon px-4 py-2.5 text-sm font-semibold text-navy transition-all hover:bg-neon/90 hover:shadow-[0_0_20px_var(--glow-mid)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
          >
            <Plus className="size-4" /> New Sandbox
          </button>
        }
      />

      {/* status filter + search */}
      <div className="flex flex-col gap-3 lg:flex-row lg:items-center lg:justify-between">
        <Segmented
          options={FILTERS.map((f) => ({
            value: f.value,
            label: `${f.label} · ${counts[f.value]}`,
          }))}
          value={filter}
          onChange={setFilter}
        />
        <div className="relative lg:w-72">
          <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-gray-500" />
          <input
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            placeholder="Name, ID, image, IP…"
            aria-label="Search sandboxes"
            className="h-10 w-full rounded-lg border border-line bg-navy/60 pl-9 pr-3 text-sm text-gray-100 placeholder:text-gray-600 outline-none focus-visible:ring-2 focus-visible:ring-ring"
          />
        </div>
      </div>

      {/* list table */}
      <Panel className="overflow-hidden">
        <div className="overflow-x-auto">
          <table className="w-full min-w-[900px] text-sm">
            <thead>
              <tr className="border-b border-line text-left text-[11px] tracking-wider text-gray-500 uppercase">
                <th className="px-6 py-3 font-medium">Sandbox</th>
                <th className="px-4 py-3 font-medium">Image</th>
                <th className="px-4 py-3 font-medium">
                  Trạng thái <VI className="ml-1">Status</VI>
                </th>
                <th className="px-4 py-3 font-medium">Network</th>
                <th className="px-4 py-3 font-medium">CPU</th>
                <th className="px-4 py-3 font-medium">MEM</th>
                <th className="px-4 py-3 font-medium">Uptime</th>
                <th className="px-4 py-3" />
              </tr>
            </thead>
            <tbody>
              {rows.map((b) => (
                <tr
                  key={b.id}
                  onClick={() => openSandbox(b.id)}
                  className="group cursor-pointer border-b border-line-soft last:border-0 transition-colors hover:bg-line/40"
                >
                  <td className="px-6 py-3.5">
                    <div className="flex items-center gap-3">
                      <span className="grid size-8 shrink-0 place-items-center rounded-lg border border-line bg-navy/60 text-gray-500 group-hover:text-neon">
                        <Container className="size-4" />
                      </span>
                      <div className="min-w-0">
                        <div className="font-medium text-gray-200">{b.name}</div>
                        <Mono className="text-[11px] text-info">{b.id}</Mono>
                      </div>
                    </div>
                  </td>
                  <td className="px-4 py-3.5">
                    <Mono className="text-xs text-gray-400">{b.image}</Mono>
                  </td>
                  <td className="px-4 py-3.5">
                    <StatusPill status={b.status} />
                  </td>
                  <td className="px-4 py-3.5">
                    <Mono className="text-xs text-gray-300">{b.host}</Mono>
                    <Mono className="mt-0.5 block text-[10px] text-gray-500">
                      {b.ports.length ? b.ports.join(" · ") : "no published ports"}
                    </Mono>
                  </td>
                  <td className="px-4 py-3.5">
                    <UsageMeter pct={b.status === "Running" ? b.cpuPct : 0} tone="neon" />
                  </td>
                  <td className="px-4 py-3.5">
                    <UsageMeter pct={b.status === "Running" ? b.memPct : 0} tone="info" />
                  </td>
                  <td className="px-4 py-3.5">
                    <Mono className="text-xs text-gray-500">{b.uptime}</Mono>
                  </td>
                  <td className="px-4 py-3.5 text-right" onClick={(e) => e.stopPropagation()}>
                    <div className="flex items-center justify-end gap-1">
                      <button
                        onClick={() => openSandbox(b.id)}
                        className="rounded-md px-2.5 py-1.5 text-[11px] font-medium text-gray-400 opacity-0 transition-all hover:bg-fg/5 hover:text-neon focus-visible:opacity-100 group-hover:opacity-100"
                      >
                        Open
                      </button>
                      <DropdownMenu>
                        <DropdownMenuTrigger asChild>
                          <button
                            className="grid size-8 place-items-center rounded-md border border-line text-gray-500 outline-none transition-colors hover:text-gray-200 focus-visible:ring-2 focus-visible:ring-ring"
                            aria-label={`Actions for ${b.id}`}
                          >
                            <ChevronDown className="size-3.5" />
                          </button>
                        </DropdownMenuTrigger>
                        <DropdownMenuContent align="end" className="w-44">
                          <DropdownMenuItem className="gap-2" onSelect={() => openSandbox(b.id)}>
                            <SlidersHorizontal className="size-3.5" /> Details & Network
                          </DropdownMenuItem>
                          <DropdownMenuItem
                            className="gap-2"
                            onSelect={() => restart(b.id, b.name)}
                            disabled={b.status === "Starting"}
                          >
                            <RotateCw className="size-3.5" /> Restart
                          </DropdownMenuItem>
                          <DropdownMenuSeparator />
                          {b.status === "Running" ? (
                            <DropdownMenuItem className="gap-2 text-warn data-[highlighted]:text-warn" onSelect={() => suspend(b.id)}>
                              <Pause className="size-3.5" /> Suspend
                            </DropdownMenuItem>
                          ) : (
                            <DropdownMenuItem className="gap-2 text-neon data-[highlighted]:text-neon" onSelect={() => resume(b.id, b.name)}>
                              <Play className="size-3.5" /> Resume
                            </DropdownMenuItem>
                          )}
                        </DropdownMenuContent>
                      </DropdownMenu>
                    </div>
                  </td>
                </tr>
              ))}
              {rows.length === 0 && (
                <tr>
                  <td colSpan={8} className="px-6 py-16 text-center">
                    <div className="mx-auto flex max-w-xs flex-col items-center gap-2 text-gray-500">
                      <Search className="size-6" />
                      <p className="text-sm">
                        No sandbox matches <span className="text-gray-300">“{query}”</span>
                        {filter !== "All" && (
                          <>
                            {" "}
                            in <span className="text-gray-300">{filter}</span>
                          </>
                        )}
                        .
                      </p>
                      <button
                        onClick={() => {
                          setQuery("");
                          setFilter("All");
                        }}
                        className="mt-1 text-xs text-neon hover:underline"
                      >
                        Clear filters
                      </button>
                    </div>
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </div>
        <div className="flex items-center justify-between border-t border-line px-6 py-2.5">
          <Mono className="text-[10px] text-gray-600">
            {rows.length} / {sandboxes.length} sandboxes
          </Mono>
          <Mono className="hidden text-[10px] text-gray-600 sm:block">
            click a row → details · network · firewall
          </Mono>
        </div>
      </Panel>
    </div>
  );
}
