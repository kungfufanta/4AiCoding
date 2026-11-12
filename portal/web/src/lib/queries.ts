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

export function useChatStatus(orgID: string) {
  return useQuery({
    queryKey: ["chat-status", orgID],
    queryFn: () => api.chatStatus(orgID),
    enabled: orgID !== "",
    staleTime: 60_000,
  });
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

// A3.4: agent runs of one conversation, newest first. Polled lightly
// so a run that finished in another tab still lands here; the live
// view itself rides the SSE stream, not this query.
export function useAgentRuns(orgID: string, chatID: string | undefined) {
  return useQuery({
    queryKey: ["agent-runs", orgID, chatID],
    queryFn: () => api.listChatAgentRuns(orgID, chatID!),
    enabled: orgID !== "" && !!chatID,
    refetchInterval: 5_000,
  });
}

// A3.4: all runs of the attached sandbox (any surface). The composer
// locks when a run from /v1 or another chat holds the sandbox.
export function useSandboxAgentRuns(orgID: string, sandboxID: string | undefined) {
  return useQuery({
    queryKey: ["agent-runs-sandbox", orgID, sandboxID],
    queryFn: () => api.listSandboxAgentRuns(orgID, sandboxID!),
    enabled: orgID !== "" && !!sandboxID,
    refetchInterval: 5_000,
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
