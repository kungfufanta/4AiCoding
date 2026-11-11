

import { useMemo, useState } from "react";
import { AnimatePresence, motion } from "framer-motion";
import {
  Ban,
  CircleUserRound,
  MoreHorizontal,
  PlayCircle,
  Search,
  ShieldCheck,
  Trash2,
  UserPlus,
  Users,
  X,
} from "lucide-react";
import { toast } from "sonner";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
} from "@/components/ui/alert-dialog";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import { type Role, type User } from "@/lib/platform-data";
import { usePlatform } from "@/lib/platform-store";
import { cn } from "@/lib/utils";
import {
  KpiCard,
  Mono,
  PageHeader,
  Panel,
  RoleBadge,
  SectionTitle,
  Segmented,
  StatusPill,
  UserAvatar,
  VI,
} from "../primitives";

const PAGE_SIZE = 8;
const ROLES: Role[] = ["Admin", "Developer", "Viewer"];

/* deterministic per-user activity feed */
export function UsersView() {
  const users = usePlatform((s) => s.users);
  const setUserRole = usePlatform((s) => s.setUserRole);
  const setUserStatus = usePlatform((s) => s.setUserStatus);
  const deleteUser = usePlatform((s) => s.deleteUser);
  const addUser = usePlatform((s) => s.addUser);

  const [query, setQuery] = useState("");
  const [roleFilter, setRoleFilter] = useState<string>("all");
  const [statusFilter, setStatusFilter] = useState<string>("all");
  const [segment, setSegment] = useState<"all" | "active" | "suspended">("all");
  const [page, setPage] = useState(1);
  const [drawerUser, setDrawerUser] = useState<User | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<User | null>(null);
  const [inviteOpen, setInviteOpen] = useState(false);

  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase();
    return users.filter((u) => {
      if (q && !u.name.toLowerCase().includes(q)) return false;
      if (roleFilter !== "all" && u.role !== roleFilter) return false;
      if (segment === "active" && u.status !== "Active") return false;
      if (segment === "suspended" && u.status !== "Suspended") return false;
      if (statusFilter === "active" && u.status !== "Active") return false;
      if (statusFilter === "suspended" && u.status !== "Suspended") return false;
      return true;
    });
  }, [users, query, roleFilter, statusFilter, segment]);

  const totalPages = Math.max(1, Math.ceil(filtered.length / PAGE_SIZE));
  const safePage = Math.min(page, totalPages);
  const pageRows = filtered.slice((safePage - 1) * PAGE_SIZE, safePage * PAGE_SIZE);

  const stats = {
    total: users.length,
    active: users.filter((u) => u.status === "Active").length,
    suspended: users.filter((u) => u.status === "Suspended").length,
    admins: users.filter((u) => u.role === "Admin").length,
  };

  const openDrawer = (u: User) => setDrawerUser(u);

  return (
    <div className="space-y-6">
      <PageHeader
        title="User Management"
        subtitle="Manage access, roles and account status"
        vi="Quản lý người dùng & phân quyền"
        actions={
          <button
            onClick={() => setInviteOpen(true)}
            className="inline-flex items-center gap-2 rounded-lg bg-neon px-4 py-2.5 text-sm font-semibold text-navy transition-all hover:bg-neon/90 hover:shadow-[0_0_20px_rgba(0,255,157,0.25)] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
          >
            <UserPlus className="size-4" /> Add user
          </button>
        }
      />

      {/* ── KPI strip ──────────────────────────────────────────── */}
      <div className="grid grid-cols-2 gap-4 xl:grid-cols-4">
        <KpiCard
          icon={<Users className="size-4.5" />}
          label="Total Users"
          vi="Tổng số"
          value={stats.total}
        />
        <KpiCard
          icon={<CircleUserRound className="size-4.5" />}
          label="Active"
          vi="Hoạt động"
          value={stats.active}
          accent="text-neon"
        />
        <KpiCard
          icon={<Ban className="size-4.5" />}
          label="Suspended"
          vi="Tạm khóa"
          value={stats.suspended}
          accent="text-danger"
        />
        <KpiCard
          icon={<ShieldCheck className="size-4.5" />}
          label="Admins"
          vi="Quản trị"
          value={stats.admins}
          accent="text-admin"
        />
      </div>

      {/* ── filter bar ─────────────────────────────────────────── */}
      <Panel className="flex flex-col gap-3 p-4 lg:flex-row lg:items-center">
        <div className="relative flex-1 lg:max-w-sm">
          <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-gray-500" />
          <Input
            value={query}
            onChange={(e) => {
              setQuery(e.target.value);
              setPage(1);
            }}
            placeholder="Search username…"
            className="border-line bg-navy/60 pl-9 text-sm placeholder:text-gray-600 focus-visible:ring-ring"
          />
        </div>

        <div className="flex flex-wrap items-center gap-3">
          <Select value={roleFilter} onValueChange={(v) => { setRoleFilter(v); setPage(1); }}>
            <SelectTrigger className="w-[140px] border-line bg-navy/60 text-sm focus-visible:ring-ring">
              <SelectValue placeholder="Role" />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all">All roles</SelectItem>
              {ROLES.map((r) => (
                <SelectItem key={r} value={r}>{r}</SelectItem>
              ))}
            </SelectContent>
          </Select>

          <Select value={statusFilter} onValueChange={(v) => { setStatusFilter(v); setPage(1); }}>
            <SelectTrigger className="w-[150px] border-line bg-navy/60 text-sm focus-visible:ring-ring">
              <SelectValue placeholder="Status" />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all">All statuses</SelectItem>
              <SelectItem value="active">Active</SelectItem>
              <SelectItem value="suspended">Suspended</SelectItem>
            </SelectContent>
          </Select>

          <div className="ml-auto">
            <Segmented
              options={[
                { value: "all", label: "All" },
                { value: "active", label: "Active" },
                { value: "suspended", label: "Suspended" },
              ]}
              value={segment}
              onChange={(v) => { setSegment(v); setPage(1); }}
            />
          </div>
        </div>
      </Panel>

      {/* ── data table ─────────────────────────────────────────── */}
      <Panel className="overflow-hidden">
        <div className="overflow-x-auto">
          <table className="w-full min-w-[640px] text-sm">
            <thead>
              <tr className="border-b border-line text-left text-[11px] tracking-wider text-gray-500 uppercase">
                <th className="px-6 py-3.5 font-medium">User</th>
                <th className="px-4 py-3.5 font-medium">Role</th>
                <th className="px-4 py-3.5 font-medium">Status</th>
                <th className="px-4 py-3.5 font-medium">Created</th>
                <th className="px-4 py-3.5 text-right font-medium">Actions</th>
              </tr>
            </thead>
            <tbody>
              {pageRows.map((u) => (
                <tr
                  key={u.id}
                  className="group cursor-pointer border-b border-line-soft transition-colors last:border-0 hover:bg-line/40"
                  onClick={() => openDrawer(u)}
                >
                  <td className="px-6 py-3.5">
                    <div className="flex items-center gap-3">
                      <UserAvatar name={u.name} colorIdx={u.colorIdx} />
                      <div className="min-w-0">
                        <div className="text-gray-100 font-medium">{u.name}</div>
                        <Mono className="text-[11px] text-gray-500">{u.id}</Mono>
                      </div>
                    </div>
                  </td>
                  <td className="px-4 py-3.5">
                    <RoleBadge role={u.role} />
                  </td>
                  <td className="px-4 py-3.5">
                    <StatusPill status={u.status} />
                  </td>
                  <td className="px-4 py-3.5">
                    <Mono className="text-xs text-gray-500">{u.joined || "—"}</Mono>
                  </td>
                  <td className="px-4 py-3.5 text-right" onClick={(e) => e.stopPropagation()}>
                    <DropdownMenu>
                      <DropdownMenuTrigger asChild>
                        <button
                          className="inline-grid size-8 place-items-center rounded-md text-gray-500 outline-none transition-colors hover:bg-fg/5 hover:text-gray-200 focus-visible:ring-2 focus-visible:ring-ring"
                          aria-label={`Actions for ${u.name}`}
                        >
                          <MoreHorizontal className="size-4" />
                        </button>
                      </DropdownMenuTrigger>
                      <DropdownMenuContent align="end" className="w-44">
                        <DropdownMenuItem onClick={() => openDrawer(u)}>
                          <ShieldCheck className="size-4" /> Edit role
                        </DropdownMenuItem>
                        {u.status === "Active" ? (
                          <DropdownMenuItem
                            onClick={() => {
                              setUserStatus(u.id, "Suspended");
                              toast.warning(`${u.name} suspended`);
                            }}
                          >
                            <Ban className="size-4" /> Suspend
                          </DropdownMenuItem>
                        ) : (
                          <DropdownMenuItem
                            onClick={() => {
                              setUserStatus(u.id, "Active");
                              toast.success(`${u.name} reactivated`);
                            }}
                          >
                            <PlayCircle className="size-4" /> Reactivate
                          </DropdownMenuItem>
                        )}
                        <DropdownMenuSeparator />
                        <DropdownMenuItem
                          className="text-danger data-[highlighted]:text-danger data-[highlighted]:bg-danger/10"
                          onClick={() => setDeleteTarget(u)}
                        >
                          <Trash2 className="size-4" /> Delete
                        </DropdownMenuItem>
                      </DropdownMenuContent>
                    </DropdownMenu>
                  </td>
                </tr>
              ))}
              {pageRows.length === 0 && (
                <tr>
                  <td colSpan={6} className="px-6 py-16 text-center">
                    <div className="text-sm text-gray-400">No users match your filters.</div>
                    <VI>Không có người dùng phù hợp bộ lọc</VI>
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </div>

        {/* pagination footer */}
        <div className="flex items-center justify-between border-t border-line px-6 py-3.5">
          <Mono className="text-xs text-gray-500">
            showing {filtered.length === 0 ? 0 : (safePage - 1) * PAGE_SIZE + 1}–
            {Math.min(safePage * PAGE_SIZE, filtered.length)} of {filtered.length}
          </Mono>
          <div className="flex items-center gap-2">
            <button
              disabled={safePage <= 1}
              onClick={() => setPage(safePage - 1)}
              className="rounded-md border border-line px-3 py-1.5 text-xs text-gray-300 transition-colors hover:bg-fg/5 disabled:pointer-events-none disabled:opacity-40"
            >
              ← Prev
            </button>
            <Mono className="px-1 text-xs text-gray-400">
              {safePage} / {totalPages}
            </Mono>
            <button
              disabled={safePage >= totalPages}
              onClick={() => setPage(safePage + 1)}
              className="rounded-md border border-line px-3 py-1.5 text-xs text-gray-300 transition-colors hover:bg-fg/5 disabled:pointer-events-none disabled:opacity-40"
            >
              Next →
            </button>
          </div>
        </div>
      </Panel>

      {/* ── user detail drawer ─────────────────────────────────── */}
      <AnimatePresence>
        {drawerUser && (
          <UserDrawer
            key={drawerUser.id}
            user={drawerUser}
            onClose={() => setDrawerUser(null)}
            onSave={(role, status) => {
              setUserRole(drawerUser.id, role);
              setUserStatus(drawerUser.id, status);
              setDrawerUser(null);
              toast.success(`${drawerUser.name} updated`, {
                description: `Role: ${role} · Status: ${status}`,
              });
            }}
          />
        )}
      </AnimatePresence>

      {/* ── delete confirm ─────────────────────────────────────── */}
      <AlertDialog open={!!deleteTarget} onOpenChange={(o) => !o && setDeleteTarget(null)}>
        <AlertDialogContent>
          <AlertDialogHeader>
            <AlertDialogTitle className="text-ink">Delete {deleteTarget?.name}?</AlertDialogTitle>
            <AlertDialogDescription>
              This permanently removes the account <Mono className="text-gray-300">{deleteTarget?.name}</Mono>{" "}
              and signs out its sessions. This action cannot be undone.
            </AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>Cancel</AlertDialogCancel>
            <AlertDialogAction
              className="bg-danger text-white hover:bg-danger/90"
              onClick={() => {
                if (deleteTarget) {
                  deleteUser(deleteTarget.id);
                  toast.error(`${deleteTarget.name} deleted`);
                }
                setDeleteTarget(null);
              }}
            >
              Delete user
            </AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>

      {/* ── invite dialog ──────────────────────────────────────── */}
      <InviteDialog
        open={inviteOpen}
        onOpenChange={setInviteOpen}
        onInvite={(u) => {
          addUser(u);
          setInviteOpen(false);
        }}
      />
    </div>
  );
}

/* ══ Slide-over user drawer ═════════════════════════════════════ */
function UserDrawer({
  user,
  onClose,
  onSave,
}: {
  user: User;
  onClose: () => void;
  onSave: (role: Role, status: User["status"]) => void;
}) {
  const [role, setRole] = useState<Role>(user.role);
  const [active, setActive] = useState(user.status === "Active");
  const dirty = role !== user.role || active !== (user.status === "Active");

  return (
    <motion.div
      className="fixed inset-0 z-50"
      initial={{ opacity: 0 }}
      animate={{ opacity: 1 }}
      exit={{ opacity: 0 }}
      transition={{ duration: 0.15 }}
    >
      <div className="absolute inset-0 bg-black/60 backdrop-blur-[2px]" onClick={onClose} />
      <motion.aside
        role="dialog"
        aria-label={`User details — ${user.name}`}
        className="absolute inset-y-0 right-0 flex w-full max-w-[440px] flex-col border-l border-line bg-surface panel-shadow"
        initial={{ x: "100%" }}
        animate={{ x: 0 }}
        exit={{ x: "100%" }}
        transition={{ type: "spring", damping: 30, stiffness: 300 }}
      >
        {/* header */}
        <div className="flex items-start justify-between gap-4 border-b border-line p-6">
          <div className="flex items-center gap-4">
            <UserAvatar name={user.name} colorIdx={user.colorIdx} size="lg" />
            <div>
              <div className="text-base font-semibold text-ink">{user.name}</div>
              <Mono className="text-xs text-gray-400">{user.id}</Mono>
              <div className="mt-1 flex items-center gap-2">
                <Mono className="text-[10px] text-gray-600">created {user.joined || "—"}</Mono>
              </div>
            </div>
          </div>
          <button
            onClick={onClose}
            className="grid size-8 place-items-center rounded-md text-gray-500 hover:bg-fg/5 hover:text-gray-200"
            aria-label="Close panel"
          >
            <X className="size-4" />
          </button>
        </div>

        <div className="flex-1 space-y-6 overflow-y-auto p-6">
          {/* role selector */}
          <section>
            <SectionTitle title="Role" vi="Vai trò" />
            <div className="mt-3 grid grid-cols-3 gap-2">
              {ROLES.map((r) => (
                <button
                  key={r}
                  onClick={() => setRole(r)}
                  className={cn(
                    "rounded-lg border px-2 py-2.5 text-xs font-medium transition-all outline-none focus-visible:ring-2 focus-visible:ring-ring",
                    role === r
                      ? "border-neon/40 bg-neon/10 text-neon"
                      : "border-line bg-navy/40 text-gray-400 hover:text-gray-200"
                  )}
                >
                  {r}
                </button>
              ))}
            </div>
          </section>

          {/* status toggle */}
          <section className="flex items-center justify-between rounded-lg border border-line bg-navy/40 p-4">
            <div>
              <div className="text-sm text-gray-200">Account active</div>
              <VI>Tài khoản hoạt động</VI>
            </div>
            <Switch
              checked={active}
              onCheckedChange={setActive}
              className="data-[state=checked]:bg-neon data-[state=unchecked]:bg-line"
            />
          </section>

        </div>

        {/* footer */}
        <div className="flex items-center justify-end gap-3 border-t border-line p-5">
          <button
            onClick={onClose}
            className="rounded-lg border border-line px-4 py-2 text-sm text-gray-300 hover:bg-fg/5"
          >
            Cancel
          </button>
          <button
            disabled={!dirty}
            onClick={() => onSave(role, active ? "Active" : "Suspended")}
            className="rounded-lg bg-neon px-5 py-2 text-sm font-semibold text-navy transition-all hover:bg-neon/90 disabled:pointer-events-none disabled:opacity-40"
          >
            Save changes
          </button>
        </div>
      </motion.aside>
    </motion.div>
  );
}

/* ══ Add-user dialog (POST /v1/users: username + password + role) ══ */
function InviteDialog({
  open,
  onOpenChange,
  onInvite,
}: {
  open: boolean;
  onOpenChange: (v: boolean) => void;
  onInvite: (u: { name: string; password: string; role: Role }) => void;
}) {
  const [name, setName] = useState("");
  const [password, setPassword] = useState("");
  const [role, setRole] = useState<Role>("Developer");

  // sandboxd requires ≥ 8 characters; empty means "generate one for me"
  const valid = /^[A-Za-z0-9._-]{2,64}$/.test(name.trim()) && (password === "" || password.length >= 8);

  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent className="border-line bg-surface sm:max-w-md">
        <DialogHeader>
          <DialogTitle className="text-ink">Add user</DialogTitle>
          <DialogDescription className="text-gray-500">
            Creates a console account. Leave the password empty to generate a temporary one.
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-4 py-2">
          <div className="space-y-1.5">
            <label className="text-xs text-gray-400" htmlFor="nu-name">Username</label>
            <Input
              id="nu-name"
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="e.g. lan.vu"
              autoComplete="off"
              className="border-line bg-navy/60 font-mono text-sm focus-visible:ring-ring"
            />
          </div>
          <div className="space-y-1.5">
            <label className="text-xs text-gray-400" htmlFor="nu-pass">Password</label>
            <Input
              id="nu-pass"
              type="password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              placeholder="at least 8 characters (optional)"
              autoComplete="new-password"
              className="border-line bg-navy/60 font-mono text-sm focus-visible:ring-ring"
            />
          </div>
          <div className="space-y-1.5">
            <label className="text-xs text-gray-400">Role</label>
            <Select value={role} onValueChange={(v) => setRole(v as Role)}>
              <SelectTrigger className="border-line bg-navy/60 focus-visible:ring-ring">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {ROLES.map((r) => (
                  <SelectItem key={r} value={r}>{r}</SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
        </div>
        <DialogFooter>
          <button
            onClick={() => onOpenChange(false)}
            className="rounded-lg border border-line px-4 py-2 text-sm text-gray-300 hover:bg-fg/5"
          >
            Cancel
          </button>
          <button
            disabled={!valid}
            onClick={() => {
              onInvite({ name: name.trim(), password, role });
              setName("");
              setPassword("");
            }}
            className="rounded-lg bg-neon px-5 py-2 text-sm font-semibold text-navy transition-all hover:bg-neon/90 disabled:pointer-events-none disabled:opacity-40"
          >
            Create user
          </button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
