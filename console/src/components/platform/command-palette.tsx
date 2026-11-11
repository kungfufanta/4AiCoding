

import { useEffect } from "react";
import {
  Activity,
  Box,
  LayoutDashboard,
  Moon,
  Plus,
  Sun,
  UserPlus,
  Users,
} from "lucide-react";
import {
  CommandDialog,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
  CommandSeparator,
} from "@/components/ui/command";
import { type ViewKey } from "@/lib/platform-data";
import { navigate as goPath } from "@/console/ui";
import { usePlatform } from "@/lib/platform-store";
import { useTheme } from "@/lib/theme-store";

export function CommandPalette({
  open,
  setOpen,
  onNavigate,
}: {
  open: boolean;
  setOpen: (v: boolean) => void;
  onNavigate: (v: ViewKey) => void;
}) {
  const sandboxes = usePlatform((s) => s.sandboxes);
  const users = usePlatform((s) => s.users);
  const theme = useTheme((s) => s.theme);
  const toggleTheme = useTheme((s) => s.toggle);

  useEffect(() => {
    const down = (e: KeyboardEvent) => {
      if ((e.key === "k" || e.key === "K") && (e.metaKey || e.ctrlKey)) {
        e.preventDefault();
        setOpen(!open);
      }
    };
    document.addEventListener("keydown", down);
    return () => document.removeEventListener("keydown", down);
  }, [open, setOpen]);

  const go = (v: ViewKey) => {
    setOpen(false);
    onNavigate(v);
  };

  return (
    <CommandDialog open={open} onOpenChange={setOpen}>
      <CommandInput placeholder="Type a command or search…" />
      <CommandList>
        <CommandEmpty>No results found.</CommandEmpty>
        <CommandGroup heading="Navigation">
          <CommandItem onSelect={() => go("overview")}>
            <LayoutDashboard className="size-4" /> Overview
          </CommandItem>
          <CommandItem onSelect={() => go("sandboxes")}>
            <Box className="size-4" /> Sandboxes
          </CommandItem>
          <CommandItem onSelect={() => go("users")}>
            <Users className="size-4" /> User Management
          </CommandItem>
          <CommandItem onSelect={() => go("create")}>
            <Plus className="size-4" /> Create Sandbox
          </CommandItem>
          <CommandItem onSelect={() => go("monitoring")}>
            <Activity className="size-4" /> Sandbox Monitoring
          </CommandItem>
        </CommandGroup>
        <CommandSeparator />
        <CommandGroup heading="Quick actions">
          <CommandItem onSelect={() => go("create")}>
            <Plus className="size-4 text-neon" /> New Sandbox
          </CommandItem>
          <CommandItem onSelect={() => go("users")}>
            <UserPlus className="size-4 text-neon" /> Add User
          </CommandItem>
          <CommandItem
            onSelect={() => {
              toggleTheme();
              setOpen(false);
            }}
          >
            {theme === "dark" ? (
              <Sun className="size-4 text-warn" />
            ) : (
              <Moon className="size-4 text-info" />
            )}
            {theme === "dark" ? "Switch to Light Mode" : "Switch to Dark Mode"}
          </CommandItem>
        </CommandGroup>
        <CommandSeparator />
        <CommandGroup heading="Sandboxes">
          {sandboxes.slice(0, 5).map((s) => (
            <CommandItem
              key={s.id}
              onSelect={() => {
                setOpen(false);
                goPath(`/sandboxes/${s.id}`);
              }}
            >
              <span className="font-mono text-xs text-info">{s.id}</span>
              <span className="text-gray-400">{s.name}</span>
            </CommandItem>
          ))}
        </CommandGroup>
        <CommandSeparator />
        <CommandGroup heading="Users">
          {users.slice(0, 5).map((u) => (
            <CommandItem key={u.id} onSelect={() => go("users")}>
              <Users className="size-3.5" />
              <span>{u.name}</span>
              <span className="ml-auto font-mono text-[10px] text-gray-500">{u.role}</span>
            </CommandItem>
          ))}
        </CommandGroup>
      </CommandList>
    </CommandDialog>
  );
}
