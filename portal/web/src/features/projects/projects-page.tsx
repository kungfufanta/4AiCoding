// Projects (PORTAL_PLAN_V2 P1): real table + create dialog. The
// create button mirrors the server's admin gate; members still see
// the list (POST /api/orgs/{id}/projects answers 404 for them).

import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { FolderKanban, Plus } from "lucide-react";
import { api, ApiError } from "../../api";
import { useProjects } from "../../lib/queries";
import { useShell } from "../../layout/app-shell";
import { Badge } from "../../ui/badge";
import { Button } from "../../ui/button";
import { Card } from "../../ui/card";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogTitle } from "../../ui/dialog";
import { Field, Input } from "../../ui/input";
import { EmptyState, SkeletonRows } from "../../ui/skeleton";
import { TBody, TD, TH, THead, TR, Table } from "../../ui/table";
import { useToast } from "../../ui/toast";

export function ProjectsPage() {
  const { org, isAdmin } = useShell();
  const projects = useProjects(org.id);
  const [creating, setCreating] = useState(false);

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between gap-3">
        <div>
          <h1 className="text-lg font-semibold tracking-tight text-fg">Projects</h1>
          <p className="text-sm text-muted">
            Group sandboxes and API keys per project inside {org.name}.
          </p>
        </div>
        {isAdmin && (
          <Button onClick={() => setCreating(true)}>
            <Plus aria-hidden className="size-4" />
            New project
          </Button>
        )}
      </div>

      <Card className="p-0">
        {projects.isPending ? (
          <div className="p-5">
            <SkeletonRows cols={2} />
          </div>
        ) : projects.data && projects.data.length > 0 ? (
          <Table className="border-0">
            <THead>
              <TR className="hover:bg-transparent">
                <TH>Name</TH>
                <TH>Slug</TH>
              </TR>
            </THead>
            <TBody>
              {projects.data.map((p) => (
                <TR key={p.id}>
                  <TD className="font-medium text-fg">{p.name}</TD>
                  <TD>
                    <Badge>/ {p.slug}</Badge>
                  </TD>
                </TR>
              ))}
            </TBody>
          </Table>
        ) : (
          <div className="p-5">
            <EmptyState
              icon={<FolderKanban aria-hidden className="size-8" />}
              title="No projects yet"
              description="Projects scope sandboxes and keys, so different workstreams stay separate."
              action={
                isAdmin ? (
                  <Button onClick={() => setCreating(true)}>
                    <Plus aria-hidden className="size-4" />
                    Create your first project
                  </Button>
                ) : undefined
              }
            />
          </div>
        )}
      </Card>

      <CreateProjectDialog open={creating} onOpenChange={setCreating} />
    </div>
  );
}

function CreateProjectDialog({
  open,
  onOpenChange,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
}) {
  const { org } = useShell();
  const [name, setName] = useState("");
  const toast = useToast();
  const qc = useQueryClient();

  const create = useMutation({
    mutationFn: () => api.createProject(org.id, name.trim()),
    onSuccess: async (project) => {
      onOpenChange(false);
      setName("");
      await qc.invalidateQueries({ queryKey: ["projects", org.id] });
      toast("success", `Project “${project.name}” created`);
    },
    onError: (err) => {
      toast(
        "error",
        err instanceof ApiError && err.status === 409
          ? "A project with a similar name already exists."
          : err instanceof Error
            ? err.message
            : String(err),
      );
    },
  });

  return (
    <Dialog
      open={open}
      onOpenChange={(o) => {
        if (!o) setName("");
        onOpenChange(o);
      }}
    >
      <DialogContent>
        <DialogTitle>New project</DialogTitle>
        <DialogDescription>
          A slug is derived from the name; it must be unique in this org.
        </DialogDescription>
        <form
          className="mt-4"
          onSubmit={(e) => {
            e.preventDefault();
            if (name.trim()) create.mutate();
          }}
        >
          <Field label="Project name">
            <Input
              value={name}
              onChange={(e) => setName(e.target.value)}
              placeholder="Agent evaluation"
              required
              autoFocus
            />
          </Field>
          <DialogFooter>
            <Button variant="secondary" onClick={() => onOpenChange(false)}>
              Cancel
            </Button>
            <Button type="submit" loading={create.isPending} disabled={!name.trim()}>
              Create project
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  );
}
