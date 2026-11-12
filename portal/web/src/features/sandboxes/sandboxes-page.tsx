// Sandboxes list (PORTAL_PLAN_V2 P4): live table with status pills,
// 5 s auto-refresh (hooks.ts), "New sandbox" dialog and a mobile-first
// card list under the table breakpoint.

import { useState } from "react";
import { Link } from "react-router";
import { useQueryClient } from "@tanstack/react-query";
import { Box, ExternalLink, Plus, RadioTower } from "lucide-react";
import { type Sandbox } from "../../api";
import { useProjects } from "../../lib/queries";
import { formatAge } from "../../lib/format";
import { useShell } from "../../layout/app-shell";
import { Badge } from "../../ui/badge";
import { Button } from "../../ui/button";
import { Card } from "../../ui/card";
import { EmptyState, SkeletonRows } from "../../ui/skeleton";
import { TBody, TD, TH, THead, TR, Table } from "../../ui/table";
import { useToast } from "../../ui/toast";
import { statusTone, useSandboxes } from "./hooks";
import { NewSandboxDialog } from "./new-sandbox-dialog";

export function SandboxesPage() {
  const { org } = useShell();
  const projects = useProjects(org.id);
  const list = useSandboxes(org.id);

  const [creating, setCreating] = useState(false);
  const projectName = (sb: Sandbox) =>
    projects.data?.find((p) => p.id === sb.project_id)?.name;

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between gap-3">
        <div>
          <h1 className="text-lg font-semibold tracking-tight text-fg">Sandboxes</h1>
          <p className="text-sm text-muted">
            Live view — refreshes every 5 seconds.
          </p>
        </div>
        <Button onClick={() => setCreating(true)}>
          <Plus aria-hidden className="size-4" />
          New sandbox
        </Button>
      </div>

      {list.data?.unreachable_nodes && list.data.unreachable_nodes.length > 0 && (
        <Card className="flex items-start gap-2.5 border-warning/40 bg-warning/5 p-3.5">
          <RadioTower aria-hidden className="mt-0.5 size-4 shrink-0 text-warning" />
          <p className="text-sm text-fg">
            Some fleet nodes are unreachable, so their sandboxes may be missing:
            <span className="ml-1 font-mono text-xs text-muted">
              {list.data.unreachable_nodes.join(", ")}
            </span>
          </p>
        </Card>
      )}

      <Card className="p-0">
        {list.isPending ? (
          <div className="p-5">
            <SkeletonRows cols={5} />
          </div>
        ) : list.isError ? (
          <div className="p-5">
            <EmptyState
              icon={<Box aria-hidden className="size-8" />}
              title="Could not load sandboxes"
              description={list.error instanceof Error ? list.error.message : "The sandbox backend is unreachable."}
            />
          </div>
        ) : list.data && !list.data.configured ? (
          <div className="p-5">
            <EmptyState
              icon={<Box aria-hidden className="size-8" />}
              title="Sandbox backend not connected"
              description="This deployment has no sandboxd configured (PORTAL_SANDBOXD_URL). Ask your administrator to wire one up to create and run sandboxes."
            />
          </div>
        ) : list.data && list.data.sandboxes.length > 0 ? (
          <>
            {/* Desktop table */}
            <div className="hidden overflow-x-auto md:block">
              <Table className="border-0">
                <THead>
                  <TR className="hover:bg-transparent">
                    <TH>Sandbox</TH>
                    <TH>Status</TH>
                    <TH>Project</TH>
                    <TH>Image</TH>
                    <TH>Resources</TH>
                    <TH>Age</TH>
                    <TH>{/* preview */}</TH>
                  </TR>
                </THead>
                <TBody>
                  {list.data.sandboxes.map((sb) => (
                    <TR key={sb.id}>
                      <TD>
                        <Link
                          to={`/orgs/${org.slug}/sandboxes/${encodeURIComponent(sb.id)}`}
                          className="font-mono text-xs font-medium text-brand hover:underline"
                        >
                          {sb.id}
                        </Link>
                      </TD>
                      <TD>
                        <Badge variant={statusTone(sb.status)}>{sb.status}</Badge>
                      </TD>
                      <TD className="text-muted">{projectName(sb) ?? "—"}</TD>
                      <TD className="max-w-44 truncate font-mono text-xs text-muted">
                        <span title={sb.image}>{sb.image}</span>
                      </TD>
                      <TD className="whitespace-nowrap text-muted">
                        {sb.resources.cpus} vCPU · {sb.resources.memory_mb} MB
                      </TD>
                      <TD className="whitespace-nowrap text-muted">{formatAge(sb.created_at)}</TD>
                      <TD className="text-right">
                        {sb.preview && sb.status === "running" && (
                          <a
                            href={sb.preview.url}
                            target="_blank"
                            rel="noreferrer"
                            aria-label={`Open preview for sandbox ${sb.id}`}
                            className="inline-flex items-center gap-1 text-xs text-brand hover:underline"
                            onClick={(e) => e.stopPropagation()}
                          >
                            <ExternalLink aria-hidden className="size-3.5" />
                            Preview
                          </a>
                        )}
                      </TD>
                    </TR>
                  ))}
                </TBody>
              </Table>
            </div>

            {/* Mobile cards (390 px): rows stack vertically */}
            <ul className="divide-y divide-border md:hidden">
              {list.data.sandboxes.map((sb) => (
                <li key={sb.id}>
                  <Link
                    to={`/orgs/${org.slug}/sandboxes/${encodeURIComponent(sb.id)}`}
                    className="block px-4 py-3 hover:bg-surface-2"
                  >
                    <div className="flex items-center justify-between gap-2">
                      <span className="font-mono text-xs font-medium text-brand">{sb.id}</span>
                      <Badge variant={statusTone(sb.status)}>{sb.status}</Badge>
                    </div>
                    <p className="mt-1 truncate text-xs text-muted">
                      {sb.image} · {sb.resources.cpus} vCPU · {sb.resources.memory_mb} MB ·{" "}
                      {formatAge(sb.created_at)}
                    </p>
                    {projectName(sb) && (
                      <p className="mt-0.5 text-xs text-muted">Project: {projectName(sb)}</p>
                    )}
                  </Link>
                </li>
              ))}
            </ul>
          </>
        ) : (
          <div className="p-5">
            <EmptyState
              icon={<Box aria-hidden className="size-8" />}
              title="No sandboxes yet"
              description="Spin up your first sandbox, then exec commands, stream logs and watch its resources from here."
              action={
                <div className="flex flex-wrap gap-2">
                  <Button onClick={() => setCreating(true)}>
                    <Plus aria-hidden className="size-4" />
                    New sandbox
                  </Button>
                  <Link to={`/orgs/${org.slug}/quickstart`}>
                    <Button variant="ghost">
                      Read the quickstart
                    </Button>
                  </Link>
                </div>
              }
            />
          </div>
        )}
      </Card>

      <NewSandboxDialog open={creating} onOpenChange={setCreating} />
    </div>
  );
}

export function useSandboxCreatedToast() {
  const toast = useToast();
  const qc = useQueryClient();
  return () => {
    void qc.invalidateQueries({ queryKey: ["sandboxes"] });
    toast("success", "Sandbox created");
  };
}
