// Sign-in page (issue Z1 #83): fully Vietnamese, friendly to visitors
// arriving from the landing. The server decides which surfaces render
// via /auth/methods: SSO when an OIDC provider is configured, password
// for the bootstrap admin — collapsed under "Đăng nhập quản trị" when
// SSO exists, because regular users sign in with SSO — and the signup
// button only when the backend reports signup=true (older backends
// without the field keep it hidden). The SSO callback may bounce back
// here with ?error=<code>; the codes are explained in Vietnamese.

import { Navigate, useLocation, useSearchParams, Link } from "react-router";
import { AlertCircle, ArrowRight, Bot, ChevronDown } from "lucide-react";
import { useAuthMethods, useMe } from "../../lib/queries";
import { FullPageSpinner } from "../../ui/skeleton";
import { ButtonLink } from "../../ui/button";
import { Card, CardTitle } from "../../ui/card";
import { PasswordForm } from "./password-form";

// Vietnamese explanations for the ?error= codes the SSO callback can
// append to /login. Unknown codes fall back to the generic message.
const SSO_ERRORS: Record<string, string> = {
  sso_not_configured: "Đăng nhập SSO chưa được cấu hình, vui lòng thử lại sau.",
  login_flow: "Phiên đăng nhập không còn hiệu lực, vui lòng thử lại.",
  state_mismatch: "Phiên đăng nhập không hợp lệ, vui lòng thử lại.",
  login_failed: "Đăng nhập không thành công, vui lòng thử lại.",
  email_unverified:
    "Email chưa được xác nhận ở nhà cung cấp định danh — hãy xác nhận rồi đăng nhập lại.",
  email_in_use: "Email này đã được dùng cho cách đăng nhập khác, hãy đăng nhập bằng cách đó.",
  internal: "Hệ thống đang gặp sự cố, vui lòng thử lại sau.",
};

const GENERIC_SSO_ERROR = "Đăng nhập không thành công, vui lòng thử lại.";

export function LoginPage() {
  const me = useMe();
  const methods = useAuthMethods();
  const location = useLocation();
  const [params] = useSearchParams();
  const from = (location.state as { from?: string } | null)?.from ?? "/";

  if (me.isPending) return <FullPageSpinner />;
  if (me.data) return <Navigate to={from} replace />;

  const ssoError =
    params.get("error") === null
      ? null
      : (SSO_ERRORS[params.get("error") ?? ""] ?? GENERIC_SSO_ERROR);

  return (
    <main className="flex min-h-dvh flex-col items-center justify-center px-4 py-10">
      {/* Brand block links back to the landing. */}
      <Link to="/" className="mb-6 flex items-center gap-2.5">
        <span className="flex size-9 items-center justify-center rounded-xl bg-brand text-brand-fg">
          <Bot aria-hidden className="size-5" />
        </span>
        <div>
          <h1 className="text-xl font-semibold tracking-tight text-fg">4AiCoding</h1>
          <p className="text-xs text-muted">Chat với AI agent trong sandbox</p>
        </div>
      </Link>

      <Card className="w-full max-w-sm">
        <CardTitle>Đăng nhập</CardTitle>

        {ssoError && (
          <p className="mt-3 flex items-start gap-1.5 text-sm text-danger" role="alert">
            <AlertCircle aria-hidden className="mt-0.5 size-4 shrink-0" />
            {ssoError}
          </p>
        )}

        {methods.isPending && <p className="mt-3 text-sm text-muted">Đang tải…</p>}

        {methods.isError && (
          <p className="mt-3 text-sm text-danger">Không thể kết nối máy chủ. Vui lòng thử lại.</p>
        )}

        {methods.data?.sso && (
          <div className="mt-4">
            <p className="text-sm text-muted">
              Đăng nhập một lần qua nhà cung cấp định danh của tổ chức bạn.
            </p>
            <ButtonLink
              href={`/auth/login?next=${encodeURIComponent(from)}`}
              className="mt-3 w-full"
            >
              Đăng nhập bằng tài khoản Devhub
              <ArrowRight aria-hidden className="size-4" />
            </ButtonLink>
            {/* Only when the backend enables signup (Z2 #80); older
                backends without the `signup` field keep it hidden. */}
            {methods.data.signup && (
              <p className="mt-3 text-center text-sm text-muted">
                Chưa có tài khoản?{" "}
                {/* Server route (SSO redirect), never a SPA <Link>. */}
                <a
                  href={`/auth/login?signup=1&next=${encodeURIComponent(from)}`}
                  className="font-medium text-brand hover:underline"
                >
                  Đăng ký
                </a>
              </p>
            )}
          </div>
        )}

        {methods.data?.sso && methods.data?.password && (
          <div className="my-4 flex items-center gap-3 text-xs text-muted">
            <span className="h-px flex-1 bg-border" />
            hoặc
            <span className="h-px flex-1 bg-border" />
          </div>
        )}

        {methods.data?.password &&
          (methods.data.sso ? (
            // Regular users use SSO; the local admin form stays
            // collapsed until an admin needs it.
            <details className="mt-1">
              <summary className="flex cursor-pointer list-none select-none items-center justify-center gap-1 text-sm font-medium text-fg [&::-webkit-details-marker]:hidden">
                Đăng nhập quản trị
                <ChevronDown aria-hidden className="size-4 text-muted" />
              </summary>
              <div className="mt-3">
                <PasswordForm onAuthenticated={() => undefined} className="space-y-0.5" />
              </div>
            </details>
          ) : (
            // Password is the only surface — show it outright.
            <div className="mt-4">
              <h2 className="text-sm font-medium text-fg">Đăng nhập quản trị</h2>
              <div className="mt-3">
                <PasswordForm onAuthenticated={() => undefined} className="space-y-0.5" />
              </div>
            </div>
          ))}

        {methods.data && !methods.data.sso && !methods.data.password && (
          <p className="mt-3 text-sm text-muted">Đăng nhập tạm thời chưa mở, thử lại sau.</p>
        )}
      </Card>

      <p className="mt-6 text-xs text-muted">© Devhub Solutions</p>
    </main>
  );
}
