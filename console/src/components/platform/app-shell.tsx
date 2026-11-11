

import { useEffect, useState } from "react";
import { AnimatePresence, motion } from "framer-motion";
import { Network, Settings } from "lucide-react";
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle } from "@/components/ui/sheet";
import { type ViewKey } from "@/lib/platform-data";
import { usePlatform } from "@/lib/platform-store";
import { useTheme } from "@/lib/theme-store";
import { Panel, SectionTitle } from "./primitives";
import { SidebarContent } from "./sidebar";
import { Topbar } from "./topbar";
import { CommandPalette } from "./command-palette";
import { OverviewView } from "./views/overview";
import { UsersView } from "./views/users";
import { CreateSandboxView } from "./views/create-sandbox";
import { MonitoringView } from "./views/monitoring";
import { SandboxListView } from "./views/sandbox-list";
import { SandboxDetailView } from "./views/sandbox-detail";
import { NodeDetailView, NodesView } from "./views/nodes";
import { MeContext, NoticeProvider, navigate as go, usePath } from "@/console/ui";
import { AdminPage } from "@/console/pages/Admin";
import { NetworkPage } from "@/console/pages/Network";
import { SandboxDetailPage } from "@/console/pages/SandboxDetail";
import "@/console/console.css";

/* Ops pages that only exist in the sandboxd console (instance policy,
   approvals, nodes, API keys, audit, domain; a sandbox's live network
   panel; exec + logs). They render inside this shell, styled by the
   .legacy-scoped console.css. */
type OpsRoute =
  | { kind: "admin"; tab: string }
  | { kind: "network"; id: string }
  | { kind: "terminal"; id: string }
  | { kind: "nodes"; id?: string };

function opsFromPath(pathname: string): OpsRoute | null {
  const seg = pathname.split("/").filter(Boolean);
  if (seg[0] === "nodes") return { kind: "nodes", id: seg[1] };
  if (seg[0] === "admin") return { kind: "admin", tab: seg[1] || "policy" };
  if (seg[0] === "settings") return { kind: "admin", tab: "policy" };
  if (seg[0] === "network") return { kind: "admin", tab: "domain" };
  if (seg[0] === "sandboxes" && seg[1] && seg[2] === "network") return { kind: "network", id: seg[1] };
  if (seg[0] === "sandboxes" && seg[1] && seg[2] === "terminal") return { kind: "terminal", id: seg[1] };
  return null;
}

/** sidebar entry an ops page belongs to */
function opsNav(o: OpsRoute): ViewKey {
  if (o.kind === "nodes") return "nodes";
  if (o.kind !== "admin") return "sandboxes";
  if (o.tab === "nodes") return "nodes";
  return o.tab === "domain" ? "network" : "settings";
}

function ModulePlaceholder({
  title,
  vi,
  icon,
  onBack,
  onAction,
  actionLabel,
}: {
  title: string;
  vi: string;
  icon: typeof Network;
  onBack: () => void;
  onAction?: () => void;
  actionLabel?: string;
}) {
  const Icon = icon;
  return (
    <Panel className="flex min-h-[420px] flex-col items-center justify-center gap-4 p-10 text-center">
      <div className="grid size-14 place-items-center rounded-xl border border-line bg-navy/60 text-gray-500">
        <Icon className="size-6" />
      </div>
      <SectionTitle title={title} vi={vi} className="justify-center" />
      <p className="max-w-sm text-sm text-gray-500">
        This module is part of the platform roadmap. The design system, layout grid and
        access model are ready — implementation is scheduled for the next release cycle.
      </p>
      <div className="flex flex-wrap items-center justify-center gap-2">
        {onAction && actionLabel && (
          <button
            onClick={onAction}
            className="mt-2 inline-flex items-center gap-2 rounded-lg border border-neon/30 bg-neon/10 px-4 py-2 text-sm text-neon transition-colors hover:bg-neon/20"
          >
            {actionLabel} →
          </button>
        )}
        <button
          onClick={onBack}
          className="mt-2 inline-flex items-center gap-2 rounded-lg border border-line px-4 py-2 text-sm text-gray-300 hover:bg-fg/5"
        >
          ← Back to Overview
        </button>
      </div>
    </Panel>
  );
}

/* map a URL path to the template view so deep links survive a
   hard refresh — everything renders the same shell, no legacy UI */
function viewFromPath(pathname: string): { view: ViewKey; sandboxId: string | null } {
  const seg = pathname.split("/").filter(Boolean);
  const [head, second] = seg;
  switch (head) {
    case undefined:
    case "overview":
      return { view: "overview", sandboxId: null };
    case "users":
      return { view: "users", sandboxId: null };
    case "monitoring":
      return { view: "monitoring", sandboxId: null };
    case "network":
      return { view: "network", sandboxId: null };
    case "create":
      return { view: "create", sandboxId: null };
    case "sandboxes":
      if (second === "new") return { view: "create", sandboxId: null };
      return { view: "sandboxes", sandboxId: second ?? null };
    default:
      // legacy ops-console paths (/admin/<tab>, /settings, …) land on Overview
      return { view: "overview", sandboxId: null };
  }
}

const viewPath = (v: ViewKey) => (v === "create" ? "/sandboxes/new" : `/${v}`);

export function AppShell() {
  const [boot] = useState(() => viewFromPath(window.location.pathname));
  const [view, setView] = useState<ViewKey>(boot.view);
  const path = usePath();
  const ops = opsFromPath(path);
  const [paletteOpen, setPaletteOpen] = useState(false);
  const [mobileNav, setMobileNav] = useState(false);
  const selectedSandboxId = usePlatform((s) => s.selectedSandboxId);
  const me = usePlatform((s) => s.me);

  /* sync theme store with the class set by the no-flash boot script */
  useEffect(() => {
    useTheme.getState().hydrate();
  }, []);

  /* live metric polling — KPIs, charts and status pills follow /v1 */
  useEffect(() => {
    const t = setInterval(() => void usePlatform.getState().pollLive(), 5000);
    return () => clearInterval(t);
  }, []);

  /* deep link /sandboxes/<id> — open the detail when the id exists
     (the store is already populated by PlatformRoot before we render) */
  useEffect(() => {
    if (boot.sandboxId && usePlatform.getState().sandboxes.some((b) => b.id === boot.sandboxId))
      usePlatform.getState().openSandbox(boot.sandboxId);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  /* keep the URL in sync so a refresh reopens the same view */
  useEffect(() => {
    if (opsFromPath(location.pathname)) return; // an ops page owns the URL
    if (view !== "sandboxes") {
      history.replaceState(null, "", viewPath(view));
      return;
    }
    history.replaceState(null, "", selectedSandboxId ? `/sandboxes/${selectedSandboxId}` : "/sandboxes");
  }, [view, selectedSandboxId, path]);

  /* back/forward between template views */
  useEffect(() => {
    if (ops) return;
    const v = viewFromPath(path);
    setView(v.view);
    if (v.view === "sandboxes" && v.sandboxId) usePlatform.getState().openSandbox(v.sandboxId);
    if (v.view === "sandboxes" && !v.sandboxId) usePlatform.getState().closeSandbox();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [path]);

  const navigate = (v: ViewKey) => {
    if (v !== "sandboxes") usePlatform.getState().closeSandbox();
    setView(v);
    setMobileNav(false);
    if (v === "nodes") go("/nodes");
    else if (v === "network" || v === "settings") go(v === "network" ? "/admin/domain" : "/admin/policy");
    else go(viewPath(v));
  };

  /* leaving the sandboxes section also closes the detail */
  const activeNav: ViewKey = ops ? opsNav(ops) : view === "create" ? "sandboxes" : view;

  return (
    <div className="min-h-screen bg-navy">
      {/* desktop sidebar */}
      <aside className="fixed inset-y-0 left-0 z-40 hidden w-[220px] border-r border-line bg-navy lg:block">
        <SidebarContent active={activeNav} onNavigate={navigate} />
      </aside>

      {/* mobile sidebar */}
      <Sheet open={mobileNav} onOpenChange={setMobileNav}>
        <SheetContent side="left" className="w-[240px] p-0 bg-navy border-line">
          <SheetHeader className="sr-only">
            <SheetTitle>Navigation</SheetTitle>
            <SheetDescription>Platform navigation</SheetDescription>
          </SheetHeader>
          <SidebarContent active={activeNav} onNavigate={navigate} />
        </SheetContent>
      </Sheet>

      <div className="flex min-h-screen flex-col lg:pl-[220px]">
        <Topbar
          onOpenPalette={() => setPaletteOpen(true)}
          onOpenMobileNav={() => setMobileNav(true)}
          onNavigate={navigate}
        />

        <main className="grid-bg flex-1">
          <div className="mx-auto w-full max-w-[1440px] p-4 md:p-6 lg:p-8">
            <AnimatePresence mode="wait">
              <motion.div
                key={ops ? path : view}
                initial={{ opacity: 0, y: 10 }}
                animate={{ opacity: 1, y: 0 }}
                exit={{ opacity: 0, y: -6 }}
                transition={{ duration: 0.18, ease: "easeOut" }}
              >
                {ops?.kind === "nodes" && (ops.id ? <NodeDetailView id={ops.id} /> : <NodesView />)}
                {ops && ops.kind !== "nodes" && (
                  <MeContext.Provider value={me ? { kind: "user", user: me.username, role: me.role } : null}>
                    <NoticeProvider>
                      <div className="legacy">
                        {ops.kind === "admin" && <AdminPage tab={ops.tab} />}
                        {ops.kind === "network" && <NetworkPage id={ops.id} />}
                        {ops.kind === "terminal" && <SandboxDetailPage id={ops.id} />}
                      </div>
                    </NoticeProvider>
                  </MeContext.Provider>
                )}
                {!ops && view === "overview" && <OverviewView onNavigate={navigate} />}
                {!ops && view === "users" && <UsersView />}
                {!ops && view === "sandboxes" &&
                  (selectedSandboxId ? (
                    <SandboxDetailView />
                  ) : (
                    <SandboxListView onNavigate={navigate} />
                  ))}
                {!ops && view === "create" && <CreateSandboxView onNavigate={navigate} />}
                {!ops && view === "monitoring" && <MonitoringView />}
                {!ops && view === "network" && (
                  <ModulePlaceholder
                    title="Network"
                    vi="Quản lý mạng"
                    icon={Network}
                    onBack={() => navigate("overview")}
                    onAction={() => navigate("sandboxes")}
                    actionLabel="Per-sandbox firewall & egress live in each sandbox detail"
                  />
                )}
                {!ops && view === "settings" && (
                  <ModulePlaceholder
                    title="Settings"
                    vi="Cài đặt hệ thống"
                    icon={Settings}
                    onBack={() => navigate("overview")}
                  />
                )}
              </motion.div>
            </AnimatePresence>
          </div>
        </main>
      </div>

      <CommandPalette open={paletteOpen} setOpen={setPaletteOpen} onNavigate={navigate} />
    </div>
  );
}
