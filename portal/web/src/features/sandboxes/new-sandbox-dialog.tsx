// "New sandbox" dialog (PORTAL_PLAN_V2 P4): maps the form onto the
// sandboxspec fields the gateway accepts — allowlist-checked image,
// project, resources, preview ports, egress preset and an optional
// workspace repo. Validation errors from sandboxd surface as toasts.

import { useState } from "react";
import { useNavigate } from "react-router";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api, type Sandbox } from "../../api";
import { useProjects } from "../../lib/queries";
import { useShell } from "../../layout/app-shell";
import { Button } from "../../ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogTitle } from "../../ui/dialog";
import { Field, Input } from "../../ui/input";
import { Select } from "../../ui/select";
import { useToast } from "../../ui/toast";

// Suggested images. The authoritative allowlist lives in sandboxd's
// instance policy; a rejected image comes back as a clear 400 toast.
const IMAGE_SUGGESTIONS = ["ubuntu:24.04", "node:22", "python:3.12", "golang:1.26"];

const RESOURCE_OPTIONS = [
  { value: "1:512", label: "1 vCPU · 512 MB" },
  { value: "2:1024", label: "2 vCPU · 1 GB" },
  { value: "4:2048", label: "4 vCPU · 2 GB" },
];

const EGRESS_OPTIONS = [
  { value: "allowlist", label: "Allowlist (recommended)" },
  { value: "none", label: "No network" },
  { value: "open", label: "Open (no egress filter)" },
];

export function NewSandboxDialog({
  open,
  onOpenChange,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const { org } = useShell();
  const projects = useProjects(org.id);
  const toast = useToast();
  const qc = useQueryClient();
  const navigate = useNavigate();

  const [image, setImage] = useState("");
  const [projectID, setProjectID] = useState("");
  const [resources, setResources] = useState("1:512");
  const [ports, setPorts] = useState("");
  const [egress, setEgress] = useState("allowlist");
  const [repoURL, setRepoURL] = useState("");

  const create = useMutation({
    mutationFn: (): Promise<Sandbox> => {
      const [cpus, memoryMB] = resources.split(":").map(Number);
      const portList = ports
        .split(/[,\s]+/)
        .map((p) => Number.parseInt(p, 10))
        .filter((p) => Number.isInteger(p) && p >= 1 && p <= 65535);
      return api.createSandbox(org.id, {
        image: image.trim(),
        ...(projectID ? { project_id: projectID } : {}),
        resources: { cpus, memory_mb: memoryMB },
        ...(portList.length > 0 ? { ports: portList } : {}),
        ...(egress !== "allowlist" ? { egress: { mode: egress } } : {}),
        ...(repoURL.trim() ? { workspace: { repo_url: repoURL.trim() } } : {}),
      });
    },
    onSuccess: async (sb) => {
      await qc.invalidateQueries({ queryKey: ["sandboxes", org.id] });
      onOpenChange(false);
      setImage("");
      setPorts("");
      setRepoURL("");
      toast("success", `Sandbox ${sb.id} created`);
      navigate(`/orgs/${org.slug}/sandboxes/${encodeURIComponent(sb.id)}`);
    },
    onError: (err) => toast("error", err instanceof Error ? err.message : String(err)),
  });

  const imageValid = IMAGE_SUGGESTIONS.includes(image.trim()) || /^[a-zA-Z0-9][a-zA-Z0-9._/:@-]*$/.test(image.trim());

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        if (!o) setImage("");
        onOpenChange(o);
      }}
    >
      <DialogContent>
        <DialogTitle>New sandbox</DialogTitle>
        <DialogDescription>
          The image must be on the deployment's allowlist. Resources are clamped to the
          instance limits by the backend.
        </DialogDescription>
        <form
          className="mt-4 space-y-3.5"
          onSubmit={(e) => {
            e.preventDefault();
            if (imageValid) create.mutate();
          }}
        >
          <Field label="Image" hint="repo[:tag] — e.g. ubuntu:24.04">
            <Input
              value={image}
              onChange={(e) => setImage(e.target.value)}
              placeholder="ubuntu:24.04"
              list="sandbox-image-suggestions"
              required
              autoFocus
              aria-invalid={image !== "" && !imageValid}
            />
            <datalist id="sandbox-image-suggestions">
              {IMAGE_SUGGESTIONS.map((img) => (
                <option key={img} value={img} />
              ))}
            </datalist>
          </Field>
          <Field label="Project (optional)">
            <Select
              ariaLabel="Sandbox project"
              value={projectID}
              onValueChange={setProjectID}
              placeholder="No project"
              options={[
                { value: "", label: "No project" },
                ...(projects.data ?? []).map((p) => ({ value: p.id, label: p.name })),
              ]}
            />
          </Field>
          <div className="grid grid-cols-1 gap-3.5 sm:grid-cols-2">
            <Field label="Resources">
              <Select
                ariaLabel="Sandbox resources"
                value={resources}
                onValueChange={setResources}
                options={RESOURCE_OPTIONS}
              />
            </Field>
            <Field label="Egress">
              <Select
                ariaLabel="Egress policy"
                value={egress}
                onValueChange={setEgress}
                options={EGRESS_OPTIONS}
              />
            </Field>
          </div>
          <Field label="Preview ports (optional)" hint="comma separated, max 8 — e.g. 3000, 5173">
            <Input
              value={ports}
              onChange={(e) => setPorts(e.target.value)}
              placeholder="3000"
              inputMode="numeric"
            />
          </Field>
          <Field label="Workspace repo (optional)" hint="cloned into the sandbox at start">
            <Input
              value={repoURL}
              onChange={(e) => setRepoURL(e.target.value)}
              placeholder="https://github.com/you/your-repo.git"
              type="url"
            />
          </Field>
          <DialogFooter>
            <Button variant="secondary" onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button type="submit" loading={create.isPending} disabled={!imageValid}>
              Create sandbox
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
