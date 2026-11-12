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

export function useApiKeys(orgID: string) {
  return useQuery({
    queryKey: ["api-keys", orgID],
    queryFn: () => api.listApiKeys(orgID),
    enabled: orgID !== "",
  });
}

export function useChatStatus() {
  return useQuery({ queryKey: ["chat-status"], queryFn: api.chatStatus, staleTime: 60_000 });
}

export function useChats(orgID: string) {
  return useQuery({
    queryKey: ["chats", orgID],
    queryFn: () => api.listChats(orgID),
    enabled: orgID !== "",
  });
}

export function useChatMessages(orgID: string, chatID: string | undefined) {
  return useQuery({
    queryKey: ["chat-messages", orgID, chatID],
    queryFn: () => api.chatMessages(orgID, chatID!),
    enabled: orgID !== "" && !!chatID,
    staleTime: Infinity,
    refetchOnWindowFocus: false,
  });
}

// P5: usage rollup for the dashboard page. Window defaults to the
// last 30 days; refetch every 60s.
export function useUsage(orgID: string) {
  return useQuery({
    queryKey: ["usage", orgID],
    queryFn: () => api.getUsage(orgID),
    enabled: orgID !== "",
    staleTime: 60_000,
    refetchInterval: 60_000,
  });
}
