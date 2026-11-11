

import { useState } from "react";
import { Bell, Building2, LogOut, Menu, Moon, Search, Settings, Sun, UserCircle } from "lucide-react";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { type ViewKey } from "@/lib/platform-data";
import { usePlatform } from "@/lib/platform-store";
import { useTheme } from "@/lib/theme-store";
import { cn } from "@/lib/utils";
import { UserAvatar } from "./primitives";

const TONE_DOT: Record<string, string> = {
  info: "bg-info",
  warn: "bg-warn",
  error: "bg-danger",
  success: "bg-neon",
};

/* dark / light switch — brand default is dark */
function ThemeToggle() {
  const theme = useTheme((s) => s.theme);
  const toggle = useTheme((s) => s.toggle);
  const isDark = theme === "dark";
  return (
    <button
      onClick={toggle}
      aria-label={isDark ? "Switch to light mode" : "Switch to dark mode"}
      title={isDark ? "Light mode (Sáng)" : "Dark mode (Tối)"}
      className="grid size-9 place-items-center rounded-lg border border-line text-gray-400 outline-none transition-colors hover:bg-surface hover:text-warn focus-visible:ring-2 focus-visible:ring-ring"
    >
      {isDark ? <Sun className="size-4" /> : <Moon className="size-4" />}
    </button>
  );
}

export function Topbar({
  onOpenPalette,
  onOpenMobileNav,
  onNavigate,
}: {
  onOpenPalette: () => void;
  onOpenMobileNav: () => void;
  onNavigate: (v: ViewKey) => void;
}) {
  const [read, setRead] = useState(false);
  const me = usePlatform((s) => s.me);
  const notifications = usePlatform((s) => s.notifications);
  const signOut = usePlatform((s) => s.signOut);
  const displayName = me?.username ?? "operator";
  const unread = read ? 0 : notifications.length;

  return (
    <header className="sticky top-0 z-30 flex h-[60px] items-center gap-3 border-b border-line bg-navy/85 px-4 backdrop-blur-md md:px-6">
      {/* mobile nav trigger */}
      <button
        onClick={onOpenMobileNav}
        className="grid size-9 place-items-center rounded-lg border border-line text-gray-400 hover:text-gray-200 hover:bg-fg/5 lg:hidden"
        aria-label="Open navigation"
      >
        <Menu className="size-4.5" />
      </button>

      {/* instance badge (one control plane per console) */}
      <div className="flex items-center gap-2.5 rounded-lg border border-line bg-surface/70 px-3 py-1.5 text-sm">
        <span className="grid size-6 place-items-center rounded-md bg-neon/10 text-neon">
          <Building2 className="size-3.5" />
        </span>
        <span className="hidden sm:block font-medium text-ink">{location.hostname}</span>
        <span className="hidden md:inline rounded border border-line px-1.5 py-0.5 font-mono text-[10px] uppercase text-gray-500">
          {me?.role ?? "—"}
        </span>
      </div>

      <div className="flex-1" />

      {/* global search trigger */}
      <button
        onClick={onOpenPalette}
        className="hidden sm:flex h-9 w-64 lg:w-80 items-center gap-2.5 rounded-lg border border-line bg-surface/70 px-3 text-sm text-gray-500 outline-none transition-colors hover:bg-surface hover:text-gray-300 focus-visible:ring-2 focus-visible:ring-ring"
      >
        <Search className="size-4 shrink-0" />
        <span className="flex-1 text-left">Search sandboxes, users…</span>
        <kbd className="rounded border border-line bg-navy px-1.5 py-0.5 font-mono text-[10px] text-gray-500">
          ⌘K
        </kbd>
      </button>

      {/* mobile search icon */}
      <button
        onClick={onOpenPalette}
        className="grid size-9 place-items-center rounded-lg border border-line text-gray-400 hover:text-gray-200 sm:hidden"
        aria-label="Search"
      >
        <Search className="size-4" />
      </button>

      {/* theme switch */}
      <ThemeToggle />

      {/* notifications */}
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <button
            className="relative grid size-9 place-items-center rounded-lg border border-line text-gray-400 outline-none transition-colors hover:bg-surface hover:text-gray-200 focus-visible:ring-2 focus-visible:ring-ring"
            aria-label={`Notifications (${unread} unread)`}
          >
            <Bell className="size-4" />
            {unread > 0 && (
              <span className="absolute -top-1 -right-1 grid size-4 place-items-center rounded-full bg-danger font-mono text-[9px] font-bold text-white">
                {unread}
              </span>
            )}
          </button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end" className="w-80 p-0">
          <div className="flex items-center justify-between border-b border-line px-4 py-3">
            <span className="text-sm font-semibold text-ink">Notifications</span>
            <button
              className="font-mono text-[10px] text-gray-500 hover:text-neon"
              onClick={() => setRead(true)}
            >
              mark all read
            </button>
          </div>
          <div className="max-h-80 overflow-y-auto">
            {notifications.length === 0 && (
              <div className="px-4 py-6 text-center text-xs text-gray-500">No notifications.</div>
            )}
            {notifications.map((n) => (
              <div key={n.id} className="flex gap-3 border-b border-line-soft px-4 py-3 last:border-0 hover:bg-fg/[0.03]">
                <span className={cn("mt-1.5 size-1.5 shrink-0 rounded-full", TONE_DOT[n.tone])} />
                <div className="min-w-0 flex-1">
                  <div className="flex items-baseline justify-between gap-2">
                    <span className="text-xs font-medium text-gray-200">{n.title}</span>
                    <span className="font-mono text-[10px] text-gray-600">{n.time}</span>
                  </div>
                  <p className="mt-0.5 text-xs leading-relaxed text-gray-500">{n.body}</p>
                </div>
              </div>
            ))}
          </div>
        </DropdownMenuContent>
      </DropdownMenu>

      {/* avatar menu */}
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <button className="ml-1 rounded-full outline-none focus-visible:ring-2 focus-visible:ring-ring" aria-label="Account menu">
            <UserAvatar name={displayName} colorIdx={0} />
          </button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end" className="w-56">
          <DropdownMenuLabel>
            <div className="text-sm text-ink">{displayName}</div>
            <div className="font-mono text-[11px] text-gray-500">{me?.role}</div>
          </DropdownMenuLabel>
          <DropdownMenuSeparator />
          <DropdownMenuItem className="gap-2" onClick={() => onNavigate("users")}>
            <UserCircle className="size-4" /> Profile
          </DropdownMenuItem>
          <DropdownMenuItem className="gap-2" onClick={() => onNavigate("settings")}>
            <Settings className="size-4" /> Settings
          </DropdownMenuItem>
          <DropdownMenuSeparator />
          <DropdownMenuItem className="gap-2 text-danger data-[highlighted]:text-danger" onClick={() => void signOut()}>
            <LogOut className="size-4" /> Sign out
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
    </header>
  );
}
