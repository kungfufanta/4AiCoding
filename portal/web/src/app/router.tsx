// Router + providers (PORTAL_PLAN_V2 §3). Route tree:
//
//   /login                    — sign-in (SSO + password)
//   /invites/:orgId?token=…   — accept an invite (signs in first)
//   /orgs/:orgSlug/…          — app shell: chat, projects, members, api keys, audit
//
// The org context rides in the URL so refresh and deep links keep it.

import { BrowserRouter, Navigate, Outlet, Route, Routes, useLocation } from "react-router";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ApiError } from "../api";
import { useMe } from "../lib/queries";
import { AppShell } from "../layout/app-shell";
import { ConfirmProvider } from "../ui/dialog";
import { ToastProvider } from "../ui/toast";
import { FullPageSpinner } from "../ui/skeleton";
import { Card } from "../ui/card";
import { LoginPage } from "../features/auth/login-page";
import { AcceptInvitePage } from "../features/invites/accept-invite-page";
import { ProjectsPage } from "../features/projects/projects-page";
import { MembersPage } from "../features/members/members-page";
import { ApiKeysPage } from "../features/api-keys/api-keys-page";
import { AuditPage } from "../features/audit/audit-page";
import { ChatPage } from "../features/chat/chat-page";
import { SandboxesPage } from "../features/sandboxes/sandboxes-page";
import { SandboxDetailPage } from "../features/sandboxes/sandbox-detail-page";
import { UsagePage } from "../features/usage/usage-page";
import { OrgModelsPage } from "../features/org-models/org-models-page";
import { AdminLLMPage } from "../features/admin-llm/admin-llm-page";

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      retry: false,
      staleTime: 15_000,
      refetchOnWindowFocus: true,
    },
  },
});

export function PortalApp() {
  return (
    <QueryClientProvider client={queryClient}>
      <ToastProvider>
        <ConfirmProvider>
          <BrowserRouter>
            <Routes>
              <Route path="/login" element={<LoginPage />} />
              <Route path="/invites/:orgId" element={<AcceptInvitePage />} />
              <Route element={<RequireAuth />}>
                <Route path="/" element={<OrgIndexRedirect />} />
                <Route path="/orgs/:orgSlug" element={<AppShell />}>
                  <Route index element={<Navigate to="projects" replace />} />
                  <Route path="chat" element={<ChatPage />} />
                  <Route path="chat/:chatId" element={<ChatPage />} />
                  <Route path="projects" element={<ProjectsPage />} />
                  <Route path="sandboxes" element={<SandboxesPage />} />
                  <Route path="sandboxes/:sandboxId" element={<SandboxDetailPage />} />
                  <Route path="usage" element={<UsagePage />} />
                  <Route path="models" element={<OrgModelsPage />} />
                  <Route path="admin/llm" element={<AdminLLMPage />} />
                  <Route path="members" element={<MembersPage />} />
                  <Route path="api-keys" element={<ApiKeysPage />} />
                  <Route path="audit" element={<AuditPage />} />
                </Route>
              </Route>
              <Route path="*" element={<NotFound />} />
            </Routes>
          </BrowserRouter>
        </ConfirmProvider>
      </ToastProvider>
    </QueryClientProvider>
  );
}

// Gates every shell route on a live session; 401 redirects to the
// login page carrying the deep link back.
function RequireAuth() {
  const me = useMe();
  const location = useLocation();

  if (me.isPending) return <FullPageSpinner />;
  if (me.isError) {
    if (me.error instanceof ApiError && me.error.status === 401) {
      const from = location.pathname + location.search;
      return <Navigate to="/login" replace state={{ from }} />;
    }
    return (
      <main className="flex min-h-dvh items-center justify-center px-4">
        <Card className="max-w-md text-center">
          <h1 className="text-base font-semibold text-fg">Something went wrong</h1>
          <p className="mt-2 text-sm text-muted">
            {me.error instanceof Error ? me.error.message : "Could not load your profile."}
          </p>
        </Card>
      </main>
    );
  }
  return <Outlet />;
}

// "/" lands on the first org's projects (bootstrapLogin always leaves
// the user with at least their personal org).
function OrgIndexRedirect() {
  const me = useMe();
  const slug = me.data?.orgs[0]?.slug;
  if (slug) return <Navigate to={`/orgs/${slug}/projects`} replace />;
  return (
    <main className="flex min-h-dvh items-center justify-center px-4">
      <Card className="max-w-md text-center">
        <h1 className="text-base font-semibold text-fg">No organisation yet</h1>
        <p className="mt-2 text-sm text-muted">
          Sign out and in again to recreate your personal organisation.
        </p>
      </Card>
    </main>
  );
}

function NotFound() {
  return (
    <main className="flex min-h-dvh items-center justify-center px-4">
      <Card className="max-w-md text-center">
        <h1 className="text-base font-semibold text-fg">Page not found</h1>
        <p className="mt-2 text-sm text-muted">The address does not match anything in the portal.</p>
      </Card>
    </main>
  );
}
