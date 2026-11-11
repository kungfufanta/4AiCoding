// Sign-in page (PORTAL_PLAN_V2 P1). The server decides which surfaces
// to render via /auth/methods: SSO when an OIDC provider is
// configured, password for the local bootstrap admin. Deep links land
// back here after sign-in via the router state.

import { Navigate, useLocation } from "react-router";
import { ArrowRight, KeyRound } from "lucide-react";
import { useAuthMethods, useMe } from "../../lib/queries";
import { FullPageSpinner } from "../../ui/skeleton";
import { ButtonLink } from "../../ui/button";
import { Card, CardTitle } from "../../ui/card";
import { PasswordForm } from "./password-form";

export function LoginPage() {
  const me = useMe();
  const methods = useAuthMethods();
  const location = useLocation();
  const from = (location.state as { from?: string } | null)?.from ?? "/";

  if (me.isPending) return <FullPageSpinner />;
  if (me.data) return <Navigate to={from} replace />;

  return (
    <main className="flex min-h-dvh flex-col items-center justify-center px-4 py-10">
      <div className="mb-6 flex items-center gap-2.5">
        <span className="flex size-9 items-center justify-center rounded-xl bg-brand text-brand-fg">
          <KeyRound aria-hidden className="size-5" />
        </span>
        <div>
          <h1 className="text-xl font-semibold tracking-tight text-fg">Portal</h1>
          <p className="text-xs text-muted">sandboxes for agents</p>
        </div>
      </div>

      <Card className="w-full max-w-sm">
        <CardTitle>Sign in</CardTitle>

        {methods.isPending && <p className="mt-3 text-sm text-muted">Loading…</p>}

        {methods.data?.sso && (
          <div className="mt-4">
            <p className="text-sm text-muted">
              Single sign-on via your organisation's identity provider.
            </p>
            <ButtonLink
              href={`/auth/login?next=${encodeURIComponent(from)}`}
              className="mt-3 w-full"
            >
              Sign in with SSO <ArrowRight aria-hidden className="size-4" />
            </ButtonLink>
          </div>
        )}

        {methods.data?.sso && methods.data?.password && (
          <div className="my-4 flex items-center gap-3 text-xs text-muted">
            <span className="h-px flex-1 bg-border" />
            or
            <span className="h-px flex-1 bg-border" />
          </div>
        )}

        {methods.data?.password && (
          <div className={methods.data.sso ? "mt-1" : "mt-4"}>
            <PasswordForm onAuthenticated={() => undefined} className="space-y-0.5" />
          </div>
        )}

        {methods.isError && (
          <p className="mt-3 text-sm text-danger">Could not reach the server. Try again.</p>
        )}
      </Card>

      <p className="mt-6 text-xs text-muted">4AiCoding Portal</p>
    </main>
  );
}
