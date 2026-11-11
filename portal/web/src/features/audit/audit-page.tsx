// Audit trail (PORTAL_PLAN_V2 P1): table with a client-side filter.
// The BFF returns the latest 50 events (no cursor yet); pagination
// lands with the P2+ backend work.

import { useState } from "react";
import { ScrollText, Search } from "lucide-react";
import { useAudit } from "../../lib/queries";
import { formatDateTime, shortID } from "../../lib/format";
import { useShell } from "../../layout/app-shell";
import { Badge } from "../../ui/badge";
import { Card } from "../../ui/card";
import { Input } from "../../ui/input";
import { EmptyState, SkeletonRows } from "../../ui/skeleton";
import { TBody, TD, TH, THead, TR, Table } from "../../ui/table";

export function AuditPage() {
  const { org } = useShell();
  const audit = useAudit(org.id);
  const [filter, setFilter] = useState("");

  const events = (audit.data ?? []).filter(
    (e) =>
      e.action.toLowerCase().includes(filter.toLowerCase()) ||
      e.target.toLowerCase().includes(filter.toLowerCase()),
  );

  return (
    <div className="space-y-4">
      <div>
        <h1 className="text-lg font-semibold tracking-tight text-fg">Audit trail</h1>
        <p className="text-sm text-muted">
          Who did what in {org.name}, latest 50 events.
        </p>
      </div>

      <div className="relative max-w-sm">
        <Search aria-hidden className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted" />
        <Input
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
          placeholder="Filter by action or target…"
          className="pl-9"
          aria-label="Filter audit events"
        />
      </div>

      <Card className="p-0">
        {audit.isPending ? (
          <div className="p-5">
            <SkeletonRows cols={4} />
          </div>
        ) : events.length > 0 ? (
          <Table className="border-0">
            <THead>
              <TR className="hover:bg-transparent">
                <TH>Time</TH>
                <TH>Action</TH>
                <TH>Actor</TH>
                <TH>Target</TH>
              </TR>
            </THead>
            <TBody>
              {events.map((e) => (
                <TR key={e.id}>
                  <TD className="whitespace-nowrap text-muted">{formatDateTime(e.occurred_at)}</TD>
                  <TD>
                    <Badge variant="brand">{e.action}</Badge>
                  </TD>
                  <TD className="text-muted">
                    {e.actor_type} {shortID(e.actor_id)}
                  </TD>
                  <TD className="font-mono text-xs text-muted">{shortID(e.target)}</TD>
                </TR>
              ))}
            </TBody>
          </Table>
        ) : (
          <div className="p-5">
            <EmptyState
              icon={<ScrollText aria-hidden className="size-8" />}
              title={filter ? "No matching events" : "Nothing recorded yet"}
              description={
                filter
                  ? "Try a different action or target."
                  : "Actions like creating orgs, projects and invites appear here."
              }
            />
          </div>
        )}
      </Card>
    </div>
  );
}
