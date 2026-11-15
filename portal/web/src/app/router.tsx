// Router + providers (PORTAL_PLAN_V2 §3; Z1 #79). Route tree:
//
//   /                         — public landing for guests; signed-in users
//                               go to the first org's Chat (#81)
//   /login                    — sign-in (SSO + password)
//   /invites/:orgId?token=…   — accept an invite (signs in first)
//   /orgs/:orgSlug/…          — app shell: chat, projects, members, api keys, audit
//
// The org context rides in the URL so refresh and deep links keep it.
// Z1 #81: sign-in and "/" land in the first org's Chat, not Projects.

import { BrowserRouter, Navigate, Outlet, Route, Routes, useLocation } from "react-router";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { ApiError } from "../api";
import { useMe } from "../lib/queries";
import { AppShell } from "../layout/app-shell";
import { ConfirmProvider } from "../ui/dialog";
import { ToastProvider } from "../ui/toast";
import { FullPageSpinner } from "../ui/skeleton";
import { Card } from "../ui/card";
import { LandingPage } from "../features/landing/landing-page";
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
import { QuickstartPage } from "../features/quickstart/quickstart-page";
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
              <Route path="/" element={<HomeGate />} />
              <Route path="/login" element={<LoginPage />} />
              <Route path="/invites/:orgId" element={<AcceptInvitePage />} />
              <Route element={<RequireAuth />}>
                <Route path="/quickstart" element={<OrgShortRedirect target="quickstart" />} />
                <Route path="/orgs/:orgSlug" element={<AppShell />}>
                  <Route index element={<Navigate to="chat" replace />} />
                  <Route path="chat" element={<ChatPage />} />
                  <Route path="chat/:chatId" element={<ChatPage />} />
                  <Route path="projects" element={<ProjectsPage />} />
                  <Route path="sandboxes" element={<SandboxesPage />} />
                  <Route path="sandboxes/:sandboxId" element={<SandboxDetailPage />} />
                  <Route path="usage" element={<UsagePage />} />
                  <Route path="quickstart" element={<QuickstartPage />} />
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

// "/" is public (Z1 #79): guests get the landing page instead of a
// redirect to /login. Signed-in users go to the first org's Chat
// (Z1 #81). bootstrapLogin always
// leaves at least the personal org, so the no-org card is a repair
// hint, not a dead end.
function HomeGate() {
  const me = useMe();

  if (me.isPending) return <FullPageSpinner />;
  if (me.data) {
    const slug = me.data.orgs[0]?.slug;
    if (slug) return <Navigate to={`/orgs/${slug}/chat`} replace />;
    return <NoOrgCard />;
  }
  if (me.error instanceof ApiError && me.error.status === 401) {
    return <LandingPage />;
  }
  return (
    <main className="flex min-h-dvh items-center justify-center px-4">
      <Card className="max-w-md text-center">
        <h1 className="text-base font-semibold text-fg">Đã xảy ra lỗi</h1>
        <p className="mt-2 text-sm text-muted">
          {me.error instanceof Error ? me.error.message : "Không tải được thông tin của bạn."}
        </p>
      </Card>
    </main>
  );
}

// Short links like /quickstart (the landing feature card) resolve into
// the first org's page — guests are bounced through RequireAuth to
// /login and come back here after signing in.
function OrgShortRedirect({ target }: { target: string }) {
  const me = useMe();
  const slug = me.data?.orgs[0]?.slug;
  if (slug) return <Navigate to={`/orgs/${slug}/${target}`} replace />;
  return <NoOrgCard />;
}

function NoOrgCard() {
  return (
    <main className="flex min-h-dvh items-center justify-center px-4">
      <Card className="max-w-md text-center">
        <h1 className="text-base font-semibold text-fg">Chưa có tổ chức nào</h1>
        <p className="mt-2 text-sm text-muted">
          Đăng xuất rồi đăng nhập lại để tái tạo tổ chức cá nhân của bạn.
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
