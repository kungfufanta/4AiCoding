// Password sign-in form (bootstrap admin). Shared by the login page
// and the accept-invite page (sign in where you landed, then the
// invite accepts in place). Labels and errors are Vietnamese per the
// portal language rule (issue Z1 #83); server error details stay out
// of the UI — only the friendly mapping is shown.

import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { AlertCircle } from "lucide-react";
import { api, ApiError } from "../../api";
import { Button } from "../../ui/button";
import { Field, Input } from "../../ui/input";

export function PasswordForm({
  onAuthenticated,
  submitLabel = "Đăng nhập",
  className,
}: {
  onAuthenticated?: () => void;
  submitLabel?: string;
  className?: string;
}) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | null>(null);
  const qc = useQueryClient();

  const login = useMutation({
    mutationFn: () => api.passwordLogin(username, password),
    onSuccess: async () => {
      setUsername("");
      setPassword("");
      await qc.invalidateQueries({ queryKey: ["me"] });
      onAuthenticated?.();
    },
    onError: (err) => {
      setError(
        err instanceof ApiError && err.code === "invalid_credentials"
          ? "Tên đăng nhập hoặc mật khẩu không đúng."
          : "Không thể đăng nhập, vui lòng thử lại.",
      );
    },
  });

  return (
    <form
      className={className}
      onSubmit={(e) => {
        e.preventDefault();
        setError(null);
        login.mutate();
      }}
    >
      <div className="space-y-3">
        <Field label="Tên đăng nhập">
          <Input
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            placeholder="admin"
            autoComplete="username"
            required
          />
        </Field>
        <Field label="Mật khẩu">
          <Input
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            autoComplete="current-password"
            required
          />
        </Field>
        <Button type="submit" className="w-full" loading={login.isPending}>
          {login.isPending ? "Đang đăng nhập…" : submitLabel}
        </Button>
      </div>
      {error && (
        <p className="mt-3 flex items-start gap-1.5 text-sm text-danger" role="alert">
          <AlertCircle aria-hidden className="mt-0.5 size-4 shrink-0" />
          {error}
        </p>
      )}
    </form>
  );
}
