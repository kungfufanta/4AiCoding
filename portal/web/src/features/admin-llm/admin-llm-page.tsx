// LLM connections admin (AGENT_PLAN A2 phase 3). Platform admin
// manages the platform defaults every org falls back to. The form is
// shared with the org Models page via LLMConnectionForm (exported
// here); the only difference is the API base and the SSRF rule
// (platform may point at internal LiteLLM, org may not).
//
// Checklist chất lượng UI (PORTAL_PLAN_V2 §4):
//   - errors → Toast, delete → ConfirmDialog, mutations pending.
//   - key input is type=password; after save the page shows
//     ••••1234 (key_label) — the plaintext is never kept in state.
//   - "Test connection" shows latency/model/error inline.
//   - "Load models" fills the default-model dropdown from the
//     provider's /v1/models.

import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Cpu, Pencil, Plus, Trash2 } from "lucide-react";
import {
  api,
  type LLMConnection,
  type LLMConnectionBody,
  type LLMTestResult,
} from "../../api";
import { formatDateTime } from "../../lib/format";
import { Badge } from "../../ui/badge";
import { Button } from "../../ui/button";
import { Card } from "../../ui/card";
import { useConfirm } from "../../ui/dialog";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogTitle } from "../../ui/dialog";
import { Field, Input } from "../../ui/input";
import { Select } from "../../ui/select";
import { EmptyState, SkeletonRows } from "../../ui/skeleton";
import { TBody, TD, TH, THead, TR, Table } from "../../ui/table";
import { useToast } from "../../ui/toast";

// AdminLLMPage is the platform-admin surface.
export function AdminLLMPage() {
  const toast = useToast();
  const confirm = useConfirm();
  const qc = useQueryClient();
  const [editing, setEditing] = useState<LLMConnection | null>(null);
  const [creating, setCreating] = useState(false);
  const [testResult, setTestResult] = useState<Record<string, LLMTestResult>>({});

  const list = useQuery({
    queryKey: ["platform-llm-connections"],
    queryFn: api.listPlatformLLMConnections,
  });

  const invalidate = () => qc.invalidateQueries({ queryKey: ["platform-llm-connections"] });

  const del = useMutation({
    mutationFn: api.deletePlatformLLMConnection,
    onSuccess: () => {
      toast("success", "Connection deleted.");
      invalidate();
    },
    onError: (e) => toast("error", e.message),
  });
  const test = useMutation({
    mutationFn: api.testPlatformLLMConnection,
    onSuccess: (res, cid) => {
      setTestResult((prev) => ({ ...prev, [cid]: res }));
      if (res.ok) toast("success", `Test OK — ${res.model ?? "?"} in ${res.latency_ms ?? "?"} ms`);
      else toast("error", res.error ?? "Test failed");
    },
    onError: (e) => toast("error", e.message),
  });

  const connections = list.data?.connections ?? [];

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div>
          <h1 className="text-lg font-semibold tracking-tight text-fg">LLM connections</h1>
          <p className="text-sm text-muted">
            Platform defaults. Every org falls back to the default connection here when it
            has no org-specific one. Keys are encrypted at rest and never shown again.
          </p>
        </div>
        <Button onClick={() => setCreating(true)}>
          <Plus aria-hidden className="size-4" />
          New connection
        </Button>
      </div>

      <Card className="p-0">
        {list.isPending ? (
          <div className="p-5">
            <SkeletonRows cols={5} />
          </div>
        ) : connections.length > 0 ? (
          <Table className="border-0">
            <THead>
              <TR className="hover:bg-transparent">
                <TH>Name</TH>
                <TH>Protocol</TH>
                <TH>Base URL</TH>
                <TH>Default model</TH>
                <TH>Key</TH>
                <TH>State</TH>
                <TH>Updated</TH>
                <TH aria-label="Actions" />
              </TR>
            </THead>
            <TBody>
              {connections.map((c) => {
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
                    <TD>
                      <Badge variant="neutral">{c.protocol}</Badge>{" "}
                      {c.flavor === "webhub" && <Badge variant="brand">webhub</Badge>}
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
                    <TD className="whitespace-nowrap text-muted">{formatDateTime(c.updated_at ?? "")}</TD>
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
                              description: `Delete "${c.name}"? Orgs falling back to it lose chat access until another default is set.`,
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
                  </TR>
                );
              })}
            </TBody>
          </Table>
        ) : (
          <div className="p-5">
            <EmptyState
              icon={<Cpu aria-hidden className="size-8" />}
              title="No platform connections"
              description="Add the WebHub connection so orgs get a working chat out of the box."
            />
          </div>
        )}
      </Card>

      <LLMConnectionDialog
        open={creating || editing !== null}
        existing={editing}
        scope="platform"
        onClose={() => {
          setCreating(false);
          setEditing(null);
        }}
        onSaved={invalidate}
      />
    </div>
  );
}

// --- shared dialog + form (also used by the org Models page) ----------

export function LLMConnectionDialog({
  open,
  existing,
  scope, // "platform" | "org"
  orgID,
  onClose,
  onSaved,
}: {
  open: boolean;
  existing: LLMConnection | null;
  scope: "platform" | "org";
  orgID?: string;
  onClose: () => void;
  onSaved: () => void;
}) {
  const toast = useToast();
  const [models, setModels] = useState<string[]>([]);
  const [loadingModels, setLoadingModels] = useState(false);

  const save = useMutation({
    mutationFn: (body: LLMConnectionBody) => {
      if (existing) {
        return scope === "platform"
          ? api.updatePlatformLLMConnection(existing.id, body)
          : api.updateLLMConnection(orgID!, existing.id, body);
      }
      return scope === "platform"
        ? api.createPlatformLLMConnection(body)
        : api.createLLMConnection(orgID!, body);
    },
    onSuccess: () => {
      toast("success", existing ? "Connection updated." : "Connection created.");
      onSaved();
      onClose();
    },
    onError: (e) => toast("error", e.message),
  });

  const loadModels = async (baseURL: string) => {
    if (!baseURL) {
      toast("error", "Enter a base URL first.");
      return;
    }
    setLoadingModels(true);
    try {
      // The models call goes through a throwaway connection-less
      // probe: we call the provider from the browser is blocked by
      // CORS in most cases, so we reuse the connection's /models
      // endpoint when editing, or ask the user to paste the model id
      // when creating. Simpler: the backend exposes /models only on
      // an existing connection — for a new one we disable the button.
      if (existing) {
        const res =
          scope === "platform"
            ? await api.listPlatformLLMModels(existing.id)
            : await api.listLLMModels(orgID!, existing.id);
        setModels(res.models);
      } else {
        toast("error", "Save the connection first, then use Load models.");
      }
    } catch (e) {
      toast("error", (e as Error).message);
    } finally {
      setLoadingModels(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={(o) => !o && onClose()}>
      <DialogContent className="max-w-xl">
        <DialogTitle>{existing ? "Edit connection" : "New LLM connection"}</DialogTitle>
        <DialogDescription>
          {existing
            ? "Leave the key blank to keep the stored key."
            : "The key is encrypted at rest and never shown again."}
        </DialogDescription>
        <LLMConnectionForm
          existing={existing}
          models={models}
          loadingModels={loadingModels}
          onLoadModels={loadModels}
          onCancel={onClose}
          onSubmit={(body) => save.mutate(body)}
          saving={save.isPending}
        />
      </DialogContent>
    </Dialog>
  );
}

// LLMConnectionForm is the shared new/edit form.
export function LLMConnectionForm({
  existing,
  models,
  loadingModels,
  onLoadModels,
  onSubmit,
  onCancel,
  saving,
}: {
  existing: LLMConnection | null;
  models: string[];
  loadingModels: boolean;
  onLoadModels: (baseURL: string) => void;
  onSubmit: (body: LLMConnectionBody) => void;
  onCancel: () => void;
  saving: boolean;
}) {
  const [name, setName] = useState(existing?.name ?? "");
  const [protocol, setProtocol] = useState<"openai" | "anthropic">(existing?.protocol ?? "openai");
  const [flavor, setFlavor] = useState<"generic" | "webhub">(existing?.flavor ?? "generic");
  const [baseURL, setBaseURL] = useState(existing?.base_url ?? "");
  const [defaultModel, setDefaultModel] = useState(existing?.default_model ?? "");
  const [keyMode, setKeyMode] = useState<"shared" | "per_org">(existing?.key_mode ?? "shared");
  const [apiKey, setApiKey] = useState("");
  const [timeoutS, setTimeoutS] = useState(String(existing?.timeout_s ?? 60));
  const [maxTokens, setMaxTokens] = useState(String(existing?.max_tokens ?? 4096));
  const [enabled, setEnabled] = useState(existing?.enabled ?? true);
  const [isDefault, setIsDefault] = useState(existing?.is_default ?? false);
  const [manualModel, setManualModel] = useState("");

  const modelOptions = useMemo(() => {
    const fromList = models.map((m) => ({ value: m, label: m }));
    const fromConn = (existing?.models ?? []).map((m) => ({ value: m.id, label: m.id }));
    const seen = new Set(fromList.map((o) => o.value));
    return [...fromList, ...fromConn.filter((o) => !seen.has(o.value))];
  }, [models, existing]);

  const submit = () => {
    const body: LLMConnectionBody = {
      name: name.trim(),
      protocol,
      flavor,
      base_url: baseURL.trim(),
      default_model: (defaultModel || manualModel).trim(),
      models: (existing?.models ?? []).map((m) =>
        m.id === defaultModel ? { ...m } : m,
      ),
      key_mode: keyMode,
      timeout_s: Number(timeoutS) || 60,
      max_tokens: Number(maxTokens) || 4096,
      enabled,
      is_default: isDefault,
    };
    if (apiKey.trim()) body.api_key = apiKey.trim();
    onSubmit(body);
  };

  return (
    <form
      className="mt-3 space-y-3"
      onSubmit={(e) => {
        e.preventDefault();
        submit();
      }}
    >
      <Field label="Name">
        <Input value={name} onChange={(e) => setName(e.target.value)} required placeholder="webhub" />
      </Field>
      <div className="grid grid-cols-2 gap-3">
        <Field label="Protocol">
          <Select
            ariaLabel="Protocol"
            value={protocol}
            onValueChange={(v) => setProtocol(v as "openai" | "anthropic")}
            options={[
              { value: "openai", label: "OpenAI-compatible" },
              { value: "anthropic", label: "Anthropic Messages" },
            ]}
          />
        </Field>
        <Field label="Flavor">
          <Select
            ariaLabel="Flavor"
            value={flavor}
            onValueChange={(v) => setFlavor(v as "generic" | "webhub")}
            options={[
              { value: "generic", label: "Generic" },
              { value: "webhub", label: "WebHub" },
            ]}
          />
        </Field>
      </div>
      <Field label="Base URL">
        <Input
          value={baseURL}
          onChange={(e) => setBaseURL(e.target.value)}
          required
          placeholder="https://h11hp9dd1y90-d.space-z.ai/v1"
        />
      </Field>
      <div className="grid grid-cols-2 gap-3">
        <Field label="Key mode">
          <Select
            ariaLabel="Key mode"
            value={keyMode}
            onValueChange={(v) => setKeyMode(v as "shared" | "per_org")}
            options={[
              { value: "shared", label: "Shared" },
              { value: "per_org", label: "Per org (WebHub)" },
            ]}
          />
        </Field>
        <Field label={existing?.has_api_key ? `Key (stored ••••${existing.key_label ?? ""})` : "API key"}>
          <Input
            type="password"
            value={apiKey}
            onChange={(e) => setApiKey(e.target.value)}
            placeholder={existing?.has_api_key ? "Leave blank to keep" : "wh_… or sk_…"}
          />
        </Field>
      </div>
      <div className="grid grid-cols-2 gap-3">
        <Field label="Timeout (s)">
          <Input
            type="number"
            min={1}
            max={600}
            value={timeoutS}
            onChange={(e) => setTimeoutS(e.target.value)}
          />
        </Field>
        <Field label="Max tokens">
          <Input
            type="number"
            min={1}
            max={32768}
            value={maxTokens}
            onChange={(e) => setMaxTokens(e.target.value)}
          />
        </Field>
      </div>
      <Field label="Default model">
        <div className="flex gap-2">
          {modelOptions.length > 0 ? (
            <Select
              ariaLabel="Default model"
              value={defaultModel}
              onValueChange={setDefaultModel}
              options={modelOptions}
              placeholder="Pick a model"
            />
          ) : (
            <Input
              value={manualModel || defaultModel}
              onChange={(e) => {
                setManualModel(e.target.value);
                setDefaultModel(e.target.value);
              }}
              placeholder="glm-4.6"
            />
          )}
          <Button
            type="button"
            variant="ghost"
            onClick={() => onLoadModels(baseURL)}
            disabled={loadingModels || !existing}
            aria-label="Load models from provider"
          >
            {loadingModels ? "Loading…" : "Load models"}
          </Button>
        </div>
      </Field>
      <div className="flex flex-wrap gap-4 pt-1">
        <label className="flex items-center gap-2 text-sm text-fg">
          <input type="checkbox" checked={enabled} onChange={(e) => setEnabled(e.target.checked)} />
          Enabled
        </label>
        <label className="flex items-center gap-2 text-sm text-fg">
          <input type="checkbox" checked={isDefault} onChange={(e) => setIsDefault(e.target.checked)} />
          Set as default
        </label>
      </div>
      <DialogFooter>
        <Button type="button" variant="ghost" onClick={onCancel}>
          Cancel
        </Button>
        <Button type="submit" disabled={saving}>
          {saving ? "Saving…" : "Save connection"}
        </Button>
      </DialogFooter>
    </form>
  );
}
