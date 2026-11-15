// App shell (PORTAL_PLAN_V2 P1): sidebar nav + topbar with the org
// switcher, theme toggle and user menu. The org context rides in the
// URL (/orgs/:orgSlug/...); pages read it via useShell().

import { useEffect, useMemo, useState, type ReactNode } from "react";
import {
  Link,
  Outlet,
  useLocation,
  useNavigate,
  useOutletContext,
  useParams,
} from "react-router";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import {
  Check,
  ChevronDown,
  Box,
  FolderKanban,
  KeyRound,
  LayoutGrid,
  LogOut,
  Menu,
  MessageSquare,
  Moon,
  Plus,
  ScrollText,
  Sun,
  Users,
  X,
  Gauge,
  BookOpen,
  Cpu,
  SlidersHorizontal,
} from "lucide-react";
import { api, type Me, type Org } from "../api";
import { useMe, useMembers } from "../lib/queries";
import { cn } from "../lib/cn";
import { Button } from "../ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogTitle } from "../ui/dialog";
import { Dropdown, DropdownItem, DropdownLabel, DropdownSeparator } from "../ui/dropdown";
import { Field, Input } from "../ui/input";
import { useToast } from "../ui/toast";

// Context handed to pages rendered inside the shell.
export type ShellContext = {
  me: Me;
  org: Org;
  /** Viewer's role in this org; undefined while members are loading. */
  role?: string;
  isAdmin: boolean;
};

export function useShell(): ShellContext {
  return useOutletContext<ShellContext>();
}

export function AppShell() {
  const { orgSlug } = useParams();
  const me = useMe();
  const navigate = useNavigate();

  // useMe() resolved already (RequireAuth gates this route), but the
  // type still carries undefined — narrow it.
  if (me.data === undefined) return null;
  const meData = me.data;
  const org = meData.orgs.find((o) => o.slug === orgSlug);

  if (!org) {
    return (
      <main className="mx-auto max-w-lg px-4 py-16">
        <div className="rounded-xl border border-border bg-surface p-6 text-center shadow-sm">
          <h1 className="text-lg font-semibold text-fg">Không tìm thấy tổ chức</h1>
          <p className="mt-1 text-sm text-muted">
            Tổ chức <code className="font-mono">{orgSlug}</code> không thuộc tài khoản của bạn.
          </p>
          <Button
            className="mt-4"
            onClick={() => navigate(`/orgs/${meData.orgs[0]?.slug ?? ""}/chat`)}
          >
            Về tổ chức của bạn
          </Button>
        </div>
      </main>
    );
  }

  return <Shell me={meData} org={org} />;
}

function Shell({ me, org }: { me: Me; org: Org }) {
  const members = useMembers(org.id);
  const role = useMemo(
    () => members.data?.find((m) => m.user_id === me.user.id)?.role,
    [members.data, me.user.id],
  );
  const isAdmin = role === "owner" || role === "admin";

  const [drawerOpen, setDrawerOpen] = useState(false);
  const location = useLocation();
  useEffect(() => setDrawerOpen(false), [location.pathname]);

  const ctx: ShellContext = { me, org, role, isAdmin };

  return (
    <div className="min-h-dvh lg:grid lg:grid-cols-[240px_1fr]">
      {/* Desktop sidebar */}
      <aside className="sticky top-0 hidden h-dvh flex-col border-r border-border bg-surface lg:flex">
        <SidebarContent org={org} role={role} />
      </aside>

      {/* Mobile drawer */}
      {drawerOpen && (
        <div className="fixed inset-0 z-50 lg:hidden" role="dialog" aria-modal="true">
          <button
            aria-label="Đóng menu"
            className="absolute inset-0 bg-black/50 backdrop-blur-[2px]"
            onClick={() => setDrawerOpen(false)}
          />
          <aside className="relative flex h-full w-64 flex-col border-r border-border bg-surface">
            <button
              aria-label="Đóng menu"
              className="absolute right-2 top-2.5 rounded-md p-1.5 text-muted hover:bg-surface-2 hover:text-fg"
              onClick={() => setDrawerOpen(false)}
            >
              <X aria-hidden className="size-4" />
            </button>
            <SidebarContent org={org} role={role} />
          </aside>
        </div>
      )}

      <div className="flex min-w-0 flex-col">
        <header className="sticky top-0 z-30 flex h-14 items-center gap-2 border-b border-border bg-surface/85 px-4 backdrop-blur">
          <Button
            variant="ghost"
            size="sm"
            className="lg:hidden"
            aria-label="Mở menu"
            onClick={() => setDrawerOpen(true)}
          >
            <Menu aria-hidden className="size-4.5" />
          </Button>
          <OrgSwitcher me={me} org={org} />
          <div className="ml-auto flex items-center gap-1.5">
            <ThemeToggle />
            <UserMenu me={me} />
          </div>
        </header>
        <main className="mx-auto w-full max-w-4xl flex-1 px-4 py-6 sm:px-6">
          <Outlet context={ctx} />
        </main>
      </div>
    </div>
  );
}

function SidebarContent({ org, role }: { org: Org; role?: string }) {
  const base = `/orgs/${org.slug}`;
  const isOwnerOrAdmin = role === "owner" || role === "admin";
  // Z1 #81: Chat first. Everything a developer works with sits under
  // the "Developer" label; the org-management pages follow under
  // "Manage".
  const chatItem = { to: `${base}/chat`, label: "Chat", icon: MessageSquare };
  const devItems = [
    { to: `${base}/projects`, label: "Dự án", icon: FolderKanban },
    { to: `${base}/sandboxes`, label: "Sandbox", icon: Box },
    { to: `${base}/api-keys`, label: "API key", icon: KeyRound },
    { to: `${base}/quickstart`, label: "Hướng dẫn nhanh", icon: BookOpen },
    { to: `${base}/usage`, label: "Sử dụng", icon: Gauge },
  ];
  const manageItems = [
    { to: `${base}/models`, label: "Mô hình", icon: Cpu },
    ...(isOwnerOrAdmin
      ? [{ to: `${base}/admin/llm`, label: "Quản trị LLM", icon: SlidersHorizontal }]
      : []),
    { to: `${base}/members`, label: "Thành viên", icon: Users },
    { to: `${base}/audit`, label: "Nhật ký", icon: ScrollText },
  ];

  return (
    <>
      <div className="flex items-center gap-2 px-4 pb-3 pt-4">
        <span className="flex size-7 items-center justify-center rounded-lg bg-brand text-brand-fg">
          <LayoutGrid aria-hidden className="size-4" />
        </span>
        <span className="text-base font-semibold tracking-tight text-fg">Portal</span>
        <span className="ml-auto text-xs text-muted">sandbox cho agent</span>
      </div>
      <nav className="flex flex-col gap-0.5 px-2 py-2" aria-label="Điều hướng chính">
        <NavLink
          key={chatItem.to}
          to={chatItem.to}
          label={chatItem.label}
          icon={<MessageSquare aria-hidden className="size-4" />}
        />
        <SidebarGroup label="Nhà phát triển" />
        {devItems.map(({ to, label, icon: Icon }) => (
          <NavLink key={to} to={to} label={label} icon={<Icon aria-hidden className="size-4" />} />
        ))}
        <SidebarGroup label="Quản lý" />
        {manageItems.map(({ to, label, icon: Icon }) => (
          <NavLink key={to} to={to} label={label} icon={<Icon aria-hidden className="size-4" />} />
        ))}
      </nav>
      <div className="mt-auto px-4 pb-4 text-[11px] leading-relaxed text-muted">
        4AiCoding Portal
      </div>
    </>
  );
}

function SidebarGroup({ label }: { label: string }) {
  return (
    <p className="px-2.5 pb-1 pt-3 text-[11px] font-medium tracking-wide text-muted uppercase">
      {label}
    </p>
  );
}

function NavLink({ to, label, icon }: { to: string; label: string; icon: ReactNode }) {
  const location = useLocation();
  const active = location.pathname.startsWith(to);
  return (
    <Link
      to={to}
      aria-current={active ? "page" : undefined}
      className={cn(
        "flex items-center gap-2.5 rounded-[var(--radius)] px-2.5 py-2 text-sm transition-colors",
        active ? "bg-brand-soft font-medium text-brand" : "text-muted hover:bg-surface-2 hover:text-fg",
      )}
    >
      {icon}
      {label}
    </Link>
  );
}

function OrgSwitcher({ me, org }: { me: Me; org: Org }) {
  const navigate = useNavigate();
  const toast = useToast();
  const qc = useQueryClient();
  const [creating, setCreating] = useState(false);

  const createOrg = useMutation({
    mutationFn: (name: string) => api.createOrg(name),
    onSuccess: async (created) => {
      setCreating(false);
      await qc.invalidateQueries({ queryKey: ["me"] });
      toast("success", `Đã tạo tổ chức “${created.name}”`);
      navigate(`/orgs/${created.slug}/chat`);
    },
    onError: (err) => {
      toast("error", err instanceof Error ? err.message : String(err));
    },
  });

  return (
    <>
      <Dropdown
        trigger={
          <Button variant="secondary" size="sm" className="max-w-52">
            <FolderKanban aria-hidden className="size-4 shrink-0 text-muted" />
            <span className="truncate">{org.name}</span>
            <ChevronDown aria-hidden className="size-3.5 shrink-0 text-muted" />
          </Button>
        }
      >
        <DropdownLabel>Tổ chức</DropdownLabel>
        {me.orgs.map((o) => (
          <DropdownItem
            key={o.id}
            onSelect={() => navigate(`/orgs/${o.slug}/chat`)}
          >
            <span className="flex-1 truncate">{o.name}</span>
            {o.id === org.id && <Check aria-hidden className="size-4 text-brand" />}
          </DropdownItem>
        ))}
        <DropdownSeparator />
        <DropdownItem onSelect={() => setCreating(true)}>
          <Plus aria-hidden className="size-4 text-muted" />
          Tạo tổ chức mới
        </DropdownItem>
      </Dropdown>

      <CreateOrgDialog
        open={creating}
        onOpenChange={setCreating}
        pending={createOrg.isPending}
        onCreate={(name) => createOrg.mutate(name)}
      />
    </>
  );
}

function CreateOrgDialog({
  open,
  onOpenChange,
  pending,
  onCreate,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  pending: boolean;
  onCreate: (name: string) => void;
}) {
  const [name, setName] = useState("");
  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        if (!o) setName("");
        onOpenChange(o);
      }}
    >
      <DialogContent>
        <DialogTitle>Tạo tổ chức mới</DialogTitle>
        <DialogDescription>
          Bạn sẽ là chủ sở hữu; mời đồng đội ở trang Thành viên.
        </DialogDescription>
        <form
          className="mt-4"
          onSubmit={(e) => {
            e.preventDefault();
            if (name.trim()) onCreate(name.trim());
          }}
        >
          <Field label="Tên tổ chức">
            <Input
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="Công ty ABC"
              required
              autoFocus
            />
          </Field>
          <DialogFooter>
            <Button variant="secondary" onClick={() => onOpenChange(false)}>
              Huỷ
            </Button>
            <Button type="submit" loading={pending} disabled={!name.trim()}>
              Tạo
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}

function UserMenu({ me }: { me: Me }) {
  const navigate = useNavigate();
  const qc = useQueryClient();
  const initial = (me.user.name || me.user.email).charAt(0).toUpperCase();

  async function signOut() {
    await api.logout();
    qc.clear();
    navigate("/login");
  }

  return (
    <Dropdown
      trigger={
        <button
          aria-label="Menu tài khoản"
          className="flex h-8 items-center gap-2 rounded-full border border-border bg-surface pl-1 pr-2.5 transition-colors hover:bg-surface-2"
        >
          <span className="flex size-6 items-center justify-center rounded-full bg-brand text-xs font-semibold text-brand-fg">
            {initial}
          </span>
          <span className="hidden max-w-40 truncate text-xs text-muted sm:block">
            {me.user.email}
          </span>
        </button>
      }
    >
      <DropdownLabel>{me.user.email}</DropdownLabel>
      <DropdownSeparator />
      <DropdownItem
        danger
        onSelect={() => void signOut()}
      >
        <LogOut aria-hidden className="size-4" />
        Đăng xuất
      </DropdownItem>
    </Dropdown>
  );
}

export function ThemeToggle() {
  const [dark, setDark] = useState(() => document.documentElement.classList.contains("dark"));

  function toggle() {
    const next = !dark;
    document.documentElement.classList.toggle("dark", next);
    localStorage.setItem("portal-theme", next ? "dark" : "light");
    setDark(next);
  }

  return (
    <Button variant="ghost" size="sm" aria-label="Bật/tắt giao diện tối" onClick={toggle}>
      {dark ? <Sun aria-hidden className="size-4.5" /> : <Moon aria-hidden className="size-4.5" />}
    </Button>
  );
}
