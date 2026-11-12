// API keys (PORTAL_PLAN_V2 P2): create with scoped permissions, show
// the secret exactly once, revoke with confirmation. The quickstart
// block mirrors the plan's curl flow so an agent can start calling
// the public API right away.

import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { KeyRound, Plus, TriangleAlert } from "lucide-react";
import { api, ApiError, type ApiKey } from "../../api";
import { useApiKeys, useProjects } from "../../lib/queries";
import { formatDateTime } from "../../lib/format";
import { useShell } from "../../layout/app-shell";
import { Badge } from "../../ui/badge";
import { Button } from "../../ui/button";
import { Card } from "../../ui/card";
import { CodeBlock } from "../../ui/code-block";
import { useConfirm } from "../../ui/dialog";
import { CopyField } from "../../ui/copy-field";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogTitle } from "../../ui/dialog";
import { Field, Input } from "../../ui/input";
import { Select } from "../../ui/select";
import { EmptyState, SkeletonRows } from "../../ui/skeleton";
import { TBody, TD, TH, THead, TR, Table } from "../../ui/table";
import { useToast } from "../../ui/toast";

const SCOPES = [
  "sandbox:create",
  "sandbox:read",
  "sandbox:exec",
  "usage:read",
  "agent:run",
  "agent:read",
] as const;

const EXPIRY_OPTIONS = [
  { value: "0", label: "Never expires" },
  { value: "7", label: "7 days" },
  { value: "30", label: "30 days" },
  { value: "90", label: "90 days" },
];

type KeyStatus = { label: string; variant: "success" | "danger" | "warning" };

function keyStatus(key: ApiKey): KeyStatus {
  if (key.revoked_at) return { label: "revoked", variant: "danger" };
  if (key.expires_at && new Date(key.expires_at).getTime() < Date.now()) {
    return { label: "expired", variant: "warning" };
  }
  return { label: "active", variant: "success" };
}

export function ApiKeysPage() {
  const { org, isAdmin } = useShell();
  const projects = useProjects(org.id);
  const list = useApiKeys(org.id);
  const toast = useToast();
  const qc = useQueryClient();
  const confirm = useConfirm();

  const [creating, setCreating] = useState(false);
  const [issuedSecret, setIssuedSecret] = useState<string | null>(null);

  const revoke = useMutation({
    mutationFn: (keyID: string) => api.revokeApiKey(org.id, keyID),
    onSuccess: async ({ key_id }) => {
      await qc.invalidateQueries({ queryKey: ["api-keys", org.id] });
      toast("success", `Key sbx_live_${key_id} revoked`);
    },
    onError: (err) => toast("error", err instanceof Error ? err.message : String(err)),
  });

  async function revokeKey(key: ApiKey) {
    const ok = await confirm({
      title: `Revoke “${key.name}”?`,
      description:
        "Agents using sbx_live_" +
        key.key_id +
        " lose access within 60 seconds. This cannot be undone — create a new key instead.",
      confirmLabel: "Revoke key",
      danger: true,
    });
    if (ok) revoke.mutate(key.key_id);
  }

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between gap-3">
        <div>
          <h1 className="text-lg font-semibold tracking-tight text-fg">API keys</h1>
          <p className="text-sm text-muted">
            How agents call the Portal API. Secrets are shown once at creation.
          </p>
        </div>
        {isAdmin && (
          <Button onClick={() => setCreating(true)}>
            <Plus aria-hidden className="size-4" />
            Create key
          </Button>
        )}
      </div>

      <Card className="p-0">
        {list.isPending ? (
          <div className="p-5">
            <SkeletonRows cols={4} />
          </div>
        ) : list.data && list.data.length > 0 ? (
          <Table className="border-0">
            <THead>
              <TR className="hover:bg-transparent">
                <TH>Name</TH>
                <TH>Key</TH>
                <TH>Scopes</TH>
                <TH>Last used</TH>
                <TH>Status</TH>
                <TH>{/* actions */}</TH>
              </TR>
            </THead>
            <TBody>
              {list.data.map((k) => {
                const st = keyStatus(k);
                const project = projects.data?.find((p) => p.id === k.project_id);
                return (
                  <TR key={k.id}>
                    <TD className="font-medium text-fg">{k.name}</TD>
                    <TD>
                      <code className="font-mono text-xs text-muted">
                        sbx_live_{k.key_id}
                      </code>
                      {project && (
                        <span className="ml-2 text-xs text-muted">· {project.name}</span>
                      )}
                    </TD>
                    <TD>
                      <div className="flex flex-wrap gap-1">
                        {k.scopes.map((s) => (
                          <Badge key={s} variant="neutral">
                            {s}
                          </Badge>
                        ))}
                      </div>
                    </TD>
                    <TD className="whitespace-nowrap text-muted">
                      {k.last_used_at ? formatDateTime(k.last_used_at) : "never"}
                    </TD>
                    <TD>
                      <Badge variant={st.variant}>{st.label}</Badge>
                    </TD>
                    <TD className="text-right">
                      {!k.revoked_at && isAdmin && (
                        <Button
                          variant="ghost"
                          size="sm"
                          className="text-danger hover:bg-danger-soft"
                          onClick={() => void revokeKey(k)}
                        >
                          Revoke
                        </Button>
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
              icon={<KeyRound aria-hidden className="size-8" />}
              title="No API keys yet"
              description="Create a key, exchange it for short-lived agent tokens, and let your agents call the sandbox API."
              action={
                isAdmin ? (
                  <Button onClick={() => setCreating(true)}>
                    <Plus aria-hidden className="size-4" />
                    Create your first key
                  </Button>
                ) : undefined
              }
            />
          </div>
        )}
      </Card>

      <Card>
        <h2 className="text-base font-semibold text-fg">Quickstart</h2>
        <p className="mt-1 text-sm text-muted">
          Exchange the key for a short-lived agent token, then call the API with it.
        </p>
        <div className="mt-3">
          <CodeBlock ariaLabel="API quickstart snippet" code={quickstartSnippet()} />
        </div>
      </Card>

      <CreateKeyDialog
        open={creating}
        onOpenChange={setCreating}
        onIssued={(secret) => {
          setCreating(false);
          setIssuedSecret(secret);
        }}
      />

      <Dialog open={issuedSecret !== null} onOpenChange={(o) => !o && setIssuedSecret(null)}>
        <DialogContent>
          <DialogTitle className="flex items-center gap-2">
            <TriangleAlert aria-hidden className="size-4.5 text-warning" />
            Save your API key now
          </DialogTitle>
          <DialogDescription>
            This is the only time the full key is shown. Copy it into your secret store —
            closing this dialog means it is gone forever.
          </DialogDescription>
          <CopyField value={issuedSecret ?? ""} className="mt-4" />
          <DialogFooter>
            <Button variant="secondary" onClick={() => setIssuedSecret(null)}>
              I saved it
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}

function quickstartSnippet(): string {
  const origin = window.location.origin;
  return (
    `# 1) Exchange your API key for a short-lived agent token (default 15 min)\n` +
    `curl -s ${origin}/v1/tokens \\\n` +
    `  -H "Authorization: Bearer sbx_live_<keyid>_<secret>" \\\n` +
    `  -H "Content-Type: application/json" \\\n` +
    `  -d '{"scopes":["sandbox:read"]}'\n` +
    `\n` +
    `# 2) Call the public API with the agent token\n` +
    `curl -s ${origin}/v1/sandboxes \\\n` +
    `  -H "Authorization: Bearer <agent token>"`
  );
}

function CreateKeyDialog({
  open,
  onOpenChange,
  onIssued,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onIssued: (secret: string) => void;
}) {
  const { org } = useShell();
  const projects = useProjects(org.id);
  const toast = useToast();
  const qc = useQueryClient();

  const [name, setName] = useState("");
  const [scopes, setScopes] = useState<string[]>(["sandbox:read"]);
  const [projectID, setProjectID] = useState("");
  const [expiry, setExpiry] = useState("0");

  const create = useMutation({
    mutationFn: () =>
      api.createApiKey(org.id, {
        name: name.trim(),
        scopes,
        ...(projectID ? { project_id: projectID } : {}),
        ...(expiry !== "0" ? { expires_in_days: Number(expiry) } : {}),
      }),
    onSuccess: async ({ secret }) => {
      setName("");
      setScopes(["sandbox:read"]);
      setProjectID("");
      setExpiry("0");
      await qc.invalidateQueries({ queryKey: ["api-keys", org.id] });
      onIssued(secret);
    },
    onError: (err) => {
      toast(
        "error",
        err instanceof ApiError && err.status === 400
          ? err.message
          : err instanceof Error
            ? err.message
            : String(err),
      );
    },
  });

  function toggleScope(scope: string, on: boolean) {
    setScopes((cur) => (on ? [...cur, scope] : cur.filter((s) => s !== scope)));
  }

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        if (!o) setName("");
        onOpenChange(o);
      }}
    >
      <DialogContent>
        <DialogTitle>Create API key</DialogTitle>
        <DialogDescription>
          Give the key only the scopes it needs; agents receive short-lived tokens derived
          from it.
        </DialogDescription>
        <form
          className="mt-4 space-y-3.5"
          onSubmit={(e) => {
            e.preventDefault();
            if (name.trim() && scopes.length > 0) create.mutate();
          }}
        >
          <Field label="Key name">
            <Input
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="CI pipeline"
              required
              autoFocus
            />
          </Field>
          <fieldset>
            <legend className="mb-1.5 text-sm font-medium text-fg">Scopes</legend>
            <div className="space-y-1.5">
              {SCOPES.map((s) => (
                <label key={s} className="flex items-center gap-2 text-sm">
                  <input
                    type="checkbox"
                    className="size-4 accent-[var(--brand)]"
                    checked={scopes.includes(s)}
                    onChange={(e) => toggleScope(s, e.target.checked)}
                  />
                  <span className="font-mono text-xs">{s}</span>
                </label>
              ))}
            </div>
          </fieldset>
          <Field label="Project (optional)">
            <Select
              ariaLabel="Project scope"
              value={projectID}
              onValueChange={setProjectID}
              placeholder="All projects"
              options={[
                { value: "", label: "All projects" },
                ...(projects.data ?? []).map((p) => ({ value: p.id, label: p.name })),
              ]}
            />
          </Field>
          <Field label="Expiry">
            <Select
              ariaLabel="Key expiry"
              value={expiry}
              onValueChange={setExpiry}
              options={EXPIRY_OPTIONS}
            />
          </Field>
          <DialogFooter>
            <Button variant="secondary" onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button type="submit" loading={create.isPending} disabled={!name.trim() || scopes.length === 0}>
              Create key
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
