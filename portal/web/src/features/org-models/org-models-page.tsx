// Org Models page (AGENT_PLAN A2 phase 3). Org owner/admin sees the
// platform default (metadata only — never the key), adds org-specific
// connections (BYO key, SSRF-checked), and picks the org default. A
// member gets a read-only view. The Chat page resolves the effective
// connection from this data (org default → platform default).

import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Cpu, Pencil, Plus, Trash2 } from "lucide-react";
import { api, type LLMConnection, type LLMTestResult } from "../../api";
import { useMembers } from "../../lib/queries";
import { Badge } from "../../ui/badge";
import { Button } from "../../ui/button";
import { Card } from "../../ui/card";
import { useConfirm } from "../../ui/dialog";
import { EmptyState, SkeletonRows } from "../../ui/skeleton";
import { TBody, TD, TH, THead, TR, Table } from "../../ui/table";
import { useToast } from "../../ui/toast";
import { useShell } from "../../layout/app-shell";
import { LLMConnectionDialog } from "../admin-llm/admin-llm-page";

export function OrgModelsPage() {
  const { org, me } = useShell();
  const toast = useToast();
  const confirm = useConfirm();
  const qc = useQueryClient();
  const [editing, setEditing] = useState<LLMConnection | null>(null);
  const [creating, setCreating] = useState(false);
  const [testResult, setTestResult] = useState<Record<string, LLMTestResult>>({});

  // The viewer's role decides edit vs read-only (member sees data,
  // cannot mutate).
  const members = useMembers(org.id);
  const myRole = members.data?.find((m) => m.user_id === me.user.id)?.role;
  const canEdit = myRole === "owner" || myRole === "admin";

  const list = useQuery({
    queryKey: ["org-llm-connections", org.id],
    queryFn: () => api.listLLMConnections(org.id),
  });

  const invalidate = () => qc.invalidateQueries({ queryKey: ["org-llm-connections", org.id] });

  const del = useMutation({
    mutationFn: (cid: string) => api.deleteLLMConnection(org.id, cid),
    onSuccess: () => {
      toast("success", "Connection deleted.");
      invalidate();
    },
    onError: (e) => toast("error", e.message),
  });
  const test = useMutation({
    mutationFn: (cid: string) => api.testLLMConnection(org.id, cid),
    onSuccess: (res, cid) => {
      setTestResult((prev) => ({ ...prev, [cid]: res }));
      if (res.ok) toast("success", `Test OK — ${res.model ?? "?"} in ${res.latency_ms ?? "?"} ms`);
      else toast("error", res.error ?? "Test failed");
    },
    onError: (e) => toast("error", e.message),
  });

  const connections = list.data?.connections ?? [];
  const platform = connections.filter((c) => !c.org_id);
  const orgOwn = connections.filter((c) => !!c.org_id);

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div>
          <h1 className="text-lg font-semibold tracking-tight text-fg">Models</h1>
          <p className="text-sm text-muted">
            {canEdit
              ? "Org connections take priority over the platform default. Bring your own key; private base URLs are refused."
              : "Read-only: ask an org admin to change model connections."}
          </p>
        </div>
        {canEdit && (
          <Button onClick={() => setCreating(true)}>
            <Plus aria-hidden className="size-4" />
            New org connection
          </Button>
        )}
      </div>

      <Card className="p-5">
        <div className="mb-2 text-sm font-medium text-fg">Platform default</div>
        {platform.length === 0 ? (
          <p className="text-sm text-muted">
            No platform connection configured — chat falls back to the deployment's
            PORTAL_DEFAULT_LLM_* env.
          </p>
        ) : (
          <ul className="space-y-1 text-sm">
            {platform.map((c) => (
              <li key={c.id} className="flex flex-wrap items-center gap-2">
                <span className="font-medium text-fg">{c.name}</span>
                {c.is_default && <Badge variant="brand">default</Badge>}
                <Badge variant="neutral">{c.protocol}</Badge>
                <span className="font-mono text-xs text-muted">{c.base_url}</span>
                <span className="font-mono text-xs text-muted">{c.default_model}</span>
                {c.has_api_key && <span className="text-xs text-muted">key ••••{c.key_label}</span>}
              </li>
            ))}
          </ul>
        )}
      </Card>

      <Card className="p-0">
        <div className="border-b border-fg/5 p-4 text-sm font-medium text-fg">
          Org connections
        </div>
        {list.isPending ? (
          <div className="p-5">
            <SkeletonRows cols={5} />
          </div>
        ) : orgOwn.length > 0 ? (
          <Table className="border-0">
            <THead>
              <TR className="hover:bg-transparent">
                <TH>Name</TH>
                <TH>Base URL</TH>
                <TH>Default model</TH>
                <TH>Key</TH>
                <TH>State</TH>
                {canEdit && <TH aria-label="Actions" />}
              </TR>
            </THead>
            <TBody>
              {orgOwn.map((c) => {
                const tr = testResult[c.id];
                return (
                  <TR key={c.id}>
                    <TD>
                      <span className="font-medium text-fg">{c.name}</span>
                      {c.is_default && (
                        <Badge variant="brand" className="ml-2">
                          default
                        </Badge>
                      )}
                    </TD>
                    <TD className="max-w-56 truncate font-mono text-xs text-muted">{c.base_url}</TD>
                    <TD className="font-mono text-xs">{c.default_model}</TD>
                    <TD className="font-mono text-xs text-muted">
                      {c.has_api_key ? `••••${c.key_label ?? ""}` : "—"}
                    </TD>
                    <TD>
                      {c.enabled ? <Badge variant="success">enabled</Badge> : <Badge variant="neutral">disabled</Badge>}
                      {tr && (
                        <span className="ml-2 text-xs text-muted">
                          {tr.ok ? `ok ${tr.latency_ms}ms` : "failed"}
                        </span>
                      )}
                    </TD>
                    {canEdit && (
                      <TD>
                        <div className="flex justify-end gap-1">
                          <Button variant="ghost" size="sm" onClick={() => test.mutate(c.id)} disabled={test.isPending}>
                            Test
                          </Button>
                          <Button variant="ghost" size="sm" onClick={() => setEditing(c)} aria-label={`Edit ${c.name}`}>
                            <Pencil aria-hidden className="size-3.5" />
                          </Button>
                          <Button
                            variant="ghost"
                            size="sm"
                            aria-label={`Delete ${c.name}`}
                            onClick={async () => {
                              const ok = await confirm({
                                title: "Delete connection",
                                description: `Delete "${c.name}"? Chat will fall back to the platform default.`,
                                confirmLabel: "Delete",
                              });
                              if (ok) del.mutate(c.id);
                            }}
                            disabled={del.isPending}
                          >
                            <Trash2 aria-hidden className="size-3.5 text-danger" />
                          </Button>
                        </div>
                      </TD>
                    )}
                  </TR>
                );
              })}
            </TBody>
          </Table>
        ) : (
          <div className="p-5">
            <EmptyState
              icon={<Cpu aria-hidden className="size-8" />}
              title="No org connections"
              description={
                canEdit
                  ? "Add one with your own provider key, or rely on the platform default."
                  : "This org uses the platform default connection."
              }
            />
          </div>
        )}
      </Card>

      <LLMConnectionDialog
        open={creating || editing !== null}
        existing={editing}
        scope="org"
        orgID={org.id}
        onClose={() => {
          setCreating(false);
          setEditing(null);
        }}
        onSaved={invalidate}
      />
    </div>
  );
}
