#!/usr/bin/env python3
"""Reset the Zitadel console admin password and/or configure SMTP.

Run by .github/workflows/zitadel-admin-smtp.yml. Authenticates with the
iam-admin machine user's PAT (Kubernetes Secret `iam-admin-pat`, passed
in via ZITADEL_SETUP_PAT_FILE), so it works even when nobody knows the
current admin password. Secret values are never printed.

Env:
  ISSUER                  default https://auth.devhub.io.vn
  ZITADEL_SETUP_PAT_FILE  file holding the iam-admin PAT (required)
  DO_RESET_PASSWORD       "true" to set the admin password
  ZITADEL_ADMIN_PASSWORD  the new password (8+ chars, upper, lower,
                          digit, symbol)
  ADMIN_USERNAME          default "admin" (login admin@devhub.auth.devhub.io.vn)
  DO_CONFIGURE_SMTP       "true" to create/update + activate SMTP
  ZITADEL_SMTP_HOST       host:port, e.g. smtp.gmail.com:587
  ZITADEL_SMTP_USER       SMTP login (optional for relays without auth)
  ZITADEL_SMTP_PASSWORD   SMTP password / app password
  ZITADEL_SMTP_FROM       sender address, e.g. no-reply@devhub.io.vn
  ZITADEL_SMTP_FROM_NAME  sender name (default "DevHub")
  ZITADEL_SMTP_REPLY_TO   reply-to address (optional)
  SMTP_TLS                "true" (default) / "false"
  SMTP_TEST_TO            if set, send a test mail there after activating

The SMTP provider is identified by its description "devhub-smtp", so a
re-run updates the same provider instead of adding another one.
"""
import json
import os
import re
import sys
import urllib.error
import urllib.request

ISSUER = os.environ.get("ISSUER", "https://auth.devhub.io.vn").rstrip("/")
SMTP_DESCRIPTION = "devhub-smtp"


def env(name, default=""):
    # GitHub passes an unset secret as an empty string, so an empty
    # value falls back to the default too.
    return os.environ.get(name, "").strip() or default


def truthy(name):
    return env(name).lower() in ("1", "true", "yes")


class APIError(RuntimeError):
    def __init__(self, msg, code):
        super().__init__(msg)
        self.code = code


def api(method, path, token, body=None):
    url = f"{ISSUER}{path}"
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(url, data=data, method=method, headers={
        "Content-Type": "application/json",
        "Authorization": f"Bearer {token}",
        "User-Agent": "zitadel-admin-smtp",
    })
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            payload = resp.read()
    except urllib.error.HTTPError as e:
        detail = e.read().decode("utf-8", "replace")[:400]
        raise APIError(f"{method} {path} -> {e.code}: {detail}", e.code) from None
    return json.loads(payload) if payload else {}


def fail(msg):
    print(f"ERROR: {msg}", file=sys.stderr)
    sys.exit(1)


# ---------------------------------------------------------------- password

def check_password(pw):
    problems = []
    if len(pw) < 8:
        problems.append("at least 8 characters")
    if not re.search(r"[a-z]", pw):
        problems.append("a lowercase letter")
    if not re.search(r"[A-Z]", pw):
        problems.append("an uppercase letter")
    if not re.search(r"[0-9]", pw):
        problems.append("a digit")
    if not re.search(r"[^A-Za-z0-9]", pw):
        problems.append("a symbol")
    if problems:
        fail("ZITADEL_ADMIN_PASSWORD needs " + ", ".join(problems))


def find_user(token, username):
    r = api("POST", "/management/v1/users/_search", token, {"query": {"limit": 500}})
    for u in r.get("result", []):
        login_names = u.get("loginNames") or []
        if u.get("userName") == username or any(
                n.split("@", 1)[0] == username for n in login_names):
            return u["id"], u.get("preferredLoginName") or username
    fail(f"user '{username}' not found in the iam-admin's org")


def reset_password(token):
    pw = os.environ.get("ZITADEL_ADMIN_PASSWORD", "")
    if not pw:
        fail("ZITADEL_ADMIN_PASSWORD is empty")
    check_password(pw)
    username = env("ADMIN_USERNAME", "admin")
    uid, login = find_user(token, username)
    try:
        api("POST", f"/v2/users/{uid}/password", token,
            {"newPassword": {"password": pw, "changeRequired": False}})
    except APIError as e:
        if e.code not in (404, 405, 501):
            raise
        api("POST", f"/management/v1/users/{uid}/password", token,
            {"password": pw, "noChangeRequired": True})
    print(f"  password set for {login} (user id {uid})")


# -------------------------------------------------------------------- smtp

def smtp_body():
    host = env("ZITADEL_SMTP_HOST")
    sender = env("ZITADEL_SMTP_FROM")
    if not host or not sender:
        fail("ZITADEL_SMTP_HOST and ZITADEL_SMTP_FROM must be set")
    if ":" not in host:
        fail("ZITADEL_SMTP_HOST must include the port, e.g. smtp.gmail.com:587")
    body = {
        "senderAddress": sender,
        "senderName": env("ZITADEL_SMTP_FROM_NAME", "DevHub"),
        "host": host,
        "tls": env("SMTP_TLS", "true").lower() != "false",
        "description": SMTP_DESCRIPTION,
    }
    reply_to = env("ZITADEL_SMTP_REPLY_TO")
    if reply_to:
        body["replyToAddress"] = reply_to
    user = env("ZITADEL_SMTP_USER")
    password = os.environ.get("ZITADEL_SMTP_PASSWORD", "")
    if user:
        body["user"] = user
        body["password"] = password
    return body, user, password


def with_auth_variants(body, user, password):
    """Newer builds take the auth as a oneof (plain/none); older ones take
    flat user/password. Yield the newer shape first."""
    if user:
        newer = {k: v for k, v in body.items() if k not in ("user", "password")}
        newer["plain"] = {"user": user, "password": password}
    else:
        newer = dict(body, none={})
    return [newer, body]


def try_shapes(method, path, token, bodies):
    last = None
    for b in bodies:
        try:
            return api(method, path, token, b)
        except APIError as e:
            last = e
            if e.code != 400:
                raise
            print(f"  {method} {path}: body shape rejected, trying the older one")
    raise last


def find_provider(token):
    try:
        r = api("POST", "/admin/v1/email/_search", token, {})
        legacy = False
    except APIError as e:
        if e.code not in (404, 405, 501):
            raise
        r = api("POST", "/admin/v1/smtp/_search", token, {})
        legacy = True
    for p in r.get("result", []):
        if p.get("description") == SMTP_DESCRIPTION:
            return p.get("id"), p.get("state", ""), legacy
    return None, "", legacy


def configure_smtp(token):
    body, user, password = smtp_body()
    pid, state, legacy = find_provider(token)
    base = "/admin/v1/smtp" if legacy else "/admin/v1/email/smtp"
    bodies = [body] if legacy else with_auth_variants(body, user, password)
    if pid:
        try_shapes("PUT", f"{base}/{pid}", token, bodies)
        if user:
            # some builds ignore the password on update
            api("PUT", f"{base}/{pid}/password", token, {"password": password})
        print(f"  updated SMTP provider {pid} (host {body['host']})")
    else:
        r = try_shapes("POST", base, token, bodies)
        pid = r["id"]
        print(f"  added SMTP provider {pid} (host {body['host']})")

    if "ACTIVE" in state and "INACTIVE" not in state:
        print("  provider already active")
    else:
        act = f"/admin/v1/smtp/{pid}/_activate" if legacy else f"/admin/v1/email/{pid}/_activate"
        try:
            api("POST", act, token, {})
            print("  provider activated")
        except APIError as e:
            # 400/409 when it is already the active provider
            if e.code not in (400, 409, 412):
                raise
            print(f"  activate skipped: {str(e)[:160]}")

    to = env("SMTP_TEST_TO")
    if to:
        test = f"{base}/{pid}/_test"
        api("POST", test, token, {"receiverAddress": to})
        print(f"  test mail sent to {to}")


def main():
    pat_file = env("ZITADEL_SETUP_PAT_FILE")
    if not pat_file:
        fail("ZITADEL_SETUP_PAT_FILE is not set")
    with open(pat_file) as f:
        token = f.read().strip()

    if not truthy("DO_RESET_PASSWORD") and not truthy("DO_CONFIGURE_SMTP"):
        fail("nothing to do: enable reset_admin_password and/or configure_smtp")
    if truthy("DO_RESET_PASSWORD"):
        print("resetting the admin password")
        reset_password(token)
    if truthy("DO_CONFIGURE_SMTP"):
        print("configuring SMTP")
        configure_smtp(token)
    print("done")


if __name__ == "__main__":
    main()
