// Data hooks (PORTAL_PLAN_V2 §3): one query key family per feature.
// retry is off — the BFF answers with meaningful status codes, and a
// retry would double-submit mutations' GET cousins for no benefit.

import { useQuery } from "@tanstack/react-query";
import { api, type Me } from "../api";

export function useMe() {
  return useQuery<Me>({
    queryKey: ["me"],
    queryFn: api.me,
    retry: false,
    staleTime: 30_000,
  });
}

export function useAuthMethods() {
  return useQuery({
    queryKey: ["auth-methods"],
    queryFn: api.authMethods,
    staleTime: 60_000,
  });
}

export function useProjects(orgID: string) {
  return useQuery({
    queryKey: ["projects", orgID],
    queryFn: () => api.listProjects(orgID),
    enabled: orgID !== "",
  });
}

export function useMembers(orgID: string) {
  return useQuery({
    queryKey: ["members", orgID],
    queryFn: () => api.listMembers(orgID),
    enabled: orgID !== "",
  });
}

export function useAudit(orgID: string) {
  return useQuery({
    queryKey: ["audit", orgID],
    queryFn: () => api.audit(orgID),
    enabled: orgID !== "",
  });
}
