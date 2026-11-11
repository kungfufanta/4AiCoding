// Members (PORTAL_PLAN_V2 P1): table with role management (confirm
// before changing), invite dialog, and the once-shown invite URL in a
// CopyField. Owner rows and the viewer's own row show a pill — the
// server refuses self-role changes and non-owner grants.

import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { UserPlus, Users } from "lucide-react";
import { api, type Member } from "../../api";
import { useMembers } from "../../lib/queries";
import { useShell } from "../../layout/app-shell";
import { Badge } from "../../ui/badge";
import { Button } from "../../ui/button";
import { Card } from "../../ui/card";
import { useConfirm } from "../../ui/dialog";
import { CopyField } from "../../ui/copy-field";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogTitle } from "../../ui/dialog";
import { Dropdown, DropdownItem, DropdownLabel, DropdownSeparator } from "../../ui/dropdown";
import { Field, Input } from "../../ui/input";
import { Select } from "../../ui/select";
import { EmptyState, SkeletonRows } from "../../ui/skeleton";
import { TBody, TD, TH, THead, TR, Table } from "../../ui/table";
import { useToast } from "../../ui/toast";

const INVITE_ROLES = ["admin", "member", "billing"];

const roleOptions = INVITE_ROLES.map((r) => ({ value: r, label: r }));

function roleBadgeVariant(role: string): "brand" | "neutral" {
  return role === "owner" || role === "admin" ? "brand" : "neutral";
}

export function MembersPage() {
  const { org, me, role } = useShell();
  const members = useMembers(org.id);
  const isAdmin = role === "owner" || role === "admin";
  const confirm = useConfirm();
  const toast = useToast();
  const qc = useQueryClient();

  const [inviting, setInviting] = useState(false);
  const [inviteUrl, setInviteUrl] = useState<string | null>(null);

  const setRole = useMutation({
    mutationFn: ({ userID, next }: { userID: string; next: string }) =>
      api.setMemberRole(org.id, userID, next),
    onSuccess: async (updated) => {
      await qc.invalidateQueries({ queryKey: ["members", org.id] });
      toast("success", `Role changed to ${updated.role}`);
    },
    onError: (err) => toast("error", err instanceof Error ? err.message : String(err)),
  });

  const createInvite = useMutation({
    mutationFn: (body: { email: string; role: string }) => api.createInvite(org.id, body.email, body.role),
    onSuccess: ({ url }) => {
      setInviting(false);
      setInviteUrl(url);
      qc.invalidateQueries({ queryKey: ["members", org.id] });
    },
    onError: (err) => toast("error", err instanceof Error ? err.message : String(err)),
  });

  async function changeRole(member: Member, next: string) {
    if (next === member.role) return;
    const ok = await confirm({
      title: `Change role of ${member.name || member.email}?`,
      description: `${member.email} will become ${next} of ${org.name}. This takes effect immediately.`,
      confirmLabel: "Change role",
    });
    if (ok) setRole.mutate({ userID: member.user_id, next });
  }

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between gap-3">
        <div>
          <h1 className="text-lg font-semibold tracking-tight text-fg">Members</h1>
          <p className="text-sm text-muted">People in {org.name} and what they can do.</p>
        </div>
        {isAdmin && (
          <Button onClick={() => setInviting(true)}>
            <UserPlus aria-hidden className="size-4" />
            Invite
          </Button>
        )}
      </div>

      <Card className="p-0">
        {members.isPending ? (
          <div className="p-5">
            <SkeletonRows cols={3} />
          </div>
        ) : members.data && members.data.length > 0 ? (
          <Table className="border-0">
            <THead>
              <TR className="hover:bg-transparent">
                <TH>Member</TH>
                <TH>Email</TH>
                <TH>Role</TH>
              </TR>
            </THead>
            <TBody>
              {members.data.map((m) => {
                const editable =
                  isAdmin && m.role !== "owner" && m.user_id !== me.user.id;
                return (
                  <TR key={m.user_id}>
                    <TD className="font-medium text-fg">{m.name || "—"}</TD>
                    <TD className="text-muted">{m.email}</TD>
                    <TD>
                      {editable ? (
                        <Dropdown
                          trigger={
                            <button
                              aria-label={`Change role of ${m.email}`}
                              className="inline-flex items-center gap-1.5"
                            >
                              <Badge variant={roleBadgeVariant(m.role)}>{m.role}</Badge>
                            </button>
                          }
                        >
                          <DropdownLabel>Set role</DropdownLabel>
                          {roleOptions.map((r) => (
                            <DropdownItem
                              key={r.value}
                              onSelect={() => void changeRole(m, r.value)}
                            >
                              <span className="flex-1">{r.label}</span>
                              {r.value === m.role && <Badge variant="brand">current</Badge>}
                            </DropdownItem>
                          ))}
                          <DropdownSeparator />
                        </Dropdown>
                      ) : (
                        <Badge variant={roleBadgeVariant(m.role)}>{m.role}</Badge>
                      )}
                    </TD>
                  </TR>
                );
              })}
            </TBody>
          </Table>
        ) : (
          <div className="p-5">
            <EmptyState
              icon={<Users aria-hidden className="size-8" />}
              title="No members yet"
              description="Invite teammates to collaborate on this organisation."
            />
          </div>
        )}
      </Card>

      <InviteDialog
        open={inviting}
        onOpenChange={setInviting}
        pending={createInvite.isPending}
        onInvite={(email, invRole) => createInvite.mutate({ email, role: invRole })}
      />

      <Dialog open={inviteUrl !== null} onOpenChange={(o) => !o && setInviteUrl(null)}>
        <DialogContent>
          <DialogTitle>Invite link — shown only once</DialogTitle>
          <DialogDescription>
            Share this link with your teammate now. It will not be shown again and expires
            automatically.
          </DialogDescription>
          <CopyField value={inviteUrl ?? ""} className="mt-4" />
          <DialogFooter>
            <Button variant="secondary" onClick={() => setInviteUrl(null)}>
              Done
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}

function InviteDialog({
  open,
  onOpenChange,
  pending,
  onInvite,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  pending: boolean;
  onInvite: (email: string, role: string) => void;
}) {
  const [email, setEmail] = useState("");
  const [role, setRole] = useState("member");

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        if (!o) setEmail("");
        onOpenChange(o);
      }}
    >
      <DialogContent>
        <DialogTitle>Invite a member</DialogTitle>
        <DialogDescription>
          They receive a one-time link; the invite expires after 7 days.
        </DialogDescription>
        <form
          className="mt-4 space-y-3"
          onSubmit={(e) => {
            e.preventDefault();
            if (email.trim()) onInvite(email.trim(), role);
          }}
        >
          <Field label="Email">
            <Input
              type="email"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              placeholder="teammate@company.com"
              required
              autoFocus
            />
          </Field>
          <Field label="Role">
            <Select
              ariaLabel="Invite role"
              value={role}
              onValueChange={setRole}
              options={roleOptions}
            />
          </Field>
          <DialogFooter>
            <Button variant="secondary" onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button type="submit" loading={pending}>
              Send invite
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
