// Accept-invite page — the destination of the once-shown invite URL
// (POST /api/orgs/{orgID}/invites returns /invites/{orgID}?token=…).
// Signs the visitor in first if needed, then accepts in place and
// routes to the org. Closes the S1 gap where the API existed but had
// no UI (PORTAL_PLAN_V2 §0.2).

import { useNavigate, useParams, useSearchParams } from "react-router";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { CircleCheckBig, Inbox } from "lucide-react";
import { api, ApiError } from "../../api";
import { useAuthMethods, useMe } from "../../lib/queries";
import { FullPageSpinner } from "../../ui/skeleton";
import { Button, ButtonLink } from "../../ui/button";
import { Card, CardTitle } from "../../ui/card";
import { PasswordForm } from "../auth/password-form";

export function AcceptInvitePage() {
  const { orgId } = useParams();
  const [params] = useSearchParams();
  const token = params.get("token") ?? "";

  const me = useMe();
  const methods = useAuthMethods();
  const navigate = useNavigate();
  const qc = useQueryClient();

  const accept = useMutation({
    mutationFn: () => api.acceptInvite(orgId ?? "", token),
    onSuccess: async ({ org_id }) => {
      await qc.invalidateQueries({ queryKey: ["me"] });
      const orgs = qc.getQueryData<{ orgs: { id: string; slug: string }[] }>(["me"])?.orgs ?? [];
      const org = orgs.find((o) => o.id === org_id);
      navigate(org ? `/orgs/${org.slug}/projects` : "/", { replace: true });
    },
  });

  if (!token || !orgId) {
    return <InviteCard icon={<Inbox aria-hidden className="size-8" />} title="Invalid invite link">
      This link is missing its token. Ask the sender for a fresh invite.
    </InviteCard>;
  }

  if (me.isPending || methods.isPending) return <FullPageSpinner />;

  if (!me.data) {
    // Not signed in yet: sign in on the spot, then accept in place.
    return (
      <InviteCard icon={<Inbox aria-hidden className="size-8" />} title="You've been invited to join an organisation">
        <p className="text-sm text-muted">
          Sign in with the email the invite was sent to, then the invitation is applied
          automatically.
        </p>
        {methods.data?.sso && (
          <ButtonLink
            href={`/auth/login?next=${encodeURIComponent(`/invites/${orgId}?token=${token}`)}`}
            className="mt-4 w-full"
          >
            Sign in with SSO
          </ButtonLink>
        )}
        {methods.data?.sso && methods.data?.password && (
          <div className="my-4 flex items-center gap-3 text-xs text-muted">
            <span className="h-px flex-1 bg-border" />
            or
            <span className="h-px flex-1 bg-border" />
          </div>
        )}
        {methods.data?.password && <PasswordForm className="space-y-0.5" />}
        {accept.isError && <AcceptError error={accept.error} />}
      </InviteCard>
    );
  }

  if (accept.isPending) return <FullPageSpinner label="Accepting invite…" />;

  if (accept.isSuccess) {
    // onSuccess already invalidated “me” and navigated once the orgs
    // arrived; this is the transient frame between the two.
    return <FullPageSpinner label="Opening your org…" />;
  }

  return (
    <InviteCard icon={<Inbox aria-hidden className="size-8" />} title="Your invitation">
      <p className="text-sm text-muted">
        Click accept to join the organisation with the role stated in your invite.
      </p>
      <Button className="mt-4 w-full" onClick={() => accept.mutate()}>
        Accept invite
      </Button>
      {accept.isError && <AcceptError error={accept.error} />}
    </InviteCard>
  );
}

function AcceptError({ error }: { error: unknown }) {
  const message =
    error instanceof ApiError && error.status === 403
      ? error.message // invite_email_mismatch — actionable, show verbatim
      : error instanceof ApiError && error.status === 404
        ? "This invite is not valid anymore — it expired or was already used."
        : error instanceof ApiError && error.status === 409
          ? "You are already a member of this organisation."
          : error instanceof Error
            ? error.message
            : String(error);
  return (
    <p className="mt-3 text-sm text-danger" role="alert">
      {message}
    </p>
  );
}

function InviteCard({
  icon,
  title,
  children,
}: {
  icon: ReactNode;
  title: string;
  children: ReactNode;
}) {
  return (
    <main className="flex min-h-dvh flex-col items-center justify-center px-4 py-10">
      <Card className="w-full max-w-sm text-center">
        <div className="mb-3 flex justify-center text-brand">{icon}</div>
        <CardTitle className="text-center">{title}</CardTitle>
        <div className="mt-2 text-left">{children}</div>
      </Card>
      <p className="mt-6 flex items-center gap-1.5 text-xs text-muted">
        <CircleCheckBig aria-hidden className="size-3.5" /> Invites are shown once — keep this link
        safe.
      </p>
    </main>
  );
}
