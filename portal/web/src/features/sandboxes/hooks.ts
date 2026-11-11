// Sandboxes feature hooks (PORTAL_PLAN_V2 §3: each feature owns its
// query keys). The list and the detail poll every 5 s — plan P4:
// "auto-refresh 5 s bằng TanStack Query" — and pause while the tab is
// hidden (TanStack's default).

import { useQuery } from "@tanstack/react-query";
import { ApiError, api } from "../../api";

export function useSandboxes(orgID: string, projectID?: string) {
  return useQuery({
    queryKey: ["sandboxes", orgID, projectID ?? "all"],
    queryFn: () => api.listSandboxes(orgID, projectID),
    enabled: orgID !== "",
    refetchInterval: 5_000,
  });
}

export function useSandbox(orgID: string, id: string) {
  return useQuery({
    queryKey: ["sandbox", orgID, id],
    queryFn: () => api.getSandbox(orgID, id),
    enabled: orgID !== "" && id !== "",
    refetchInterval: 5_000,
    retry: (failures, error) => {
      // A purged sandbox (404) is final — stop polling instead of
      // hammering a dead id.
      if (error instanceof ApiError && error.status === 404) return false;
      return failures < 2;
    },
  });
}

// statusTone maps a sandbox status to the Badge variant the two pages
// share. Unknown statuses stay neutral.
export function statusTone(status: string): "success" | "warning" | "danger" | "neutral" {
  switch (status) {
    case "running":
    case "created":
      return "success";
    case "stopped":
    case "exited":
    case "creating":
    case "starting":
    case "sleeping":
      return "warning";
    case "failed":
    case "dead":
      return "danger";
    default:
      return "neutral";
  }
}
