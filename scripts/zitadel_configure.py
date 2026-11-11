#!/usr/bin/env python3
"""Configure Zitadel (auth.devhub.io.vn) as the Portal's OIDC provider.

Authentication for the Management API comes from the iam-admin machine
user's PAT: the Zitadel chart mints it during the first-instance setup
(FirstInstance.Org.Machine with Pat) and stores it in the Kubernetes
Secret `iam-admin-pat`, which the deployer Role may read. The workflow
passes it to this script via ZITADEL_SETUP_PAT_FILE. Nothing sensitive
is ever printed: the client secret is written straight into repo
Actions secrets (PORTAL_OIDC_CLIENT_ID / PORTAL_OIDC_CLIENT_SECRET).

Fallback: with no PAT, tries the Sessions API login using
ZITADEL_ADMIN_PASSWORD (works on providers where anonymous session
creation is allowed).

Steps, all idempotent (safe to re-run):
  1. Bearer: iam-admin PAT (or session login fallback).
  2. Find or create the "Devhub Solutions" project.
  3. Find the "portal-bff" OIDC app; if it already exists, regenerate
     its client secret, otherwise create the app (Authorization Code,
     client_secret_basic, PKCE challenge tolerated, redirect URI
     https://chat.devhub.io.vn/auth/callback).
  4. Create or reset the "portal-demo" login (a human user for the
     dashboard, since the admin password is only known to CI).
  5. Promote dnt.doanngocthanh@devhub.io.vn to ORG_OWNER /
     PROJECT_OWNER so the owner can manage Zitadel from the console
     with their own password (best effort).
  6. Store PORTAL_OIDC_ISSUER / _CLIENT_ID / _CLIENT_SECRET as repo
     Actions secrets (libsodium sealed boxes, per the Secrets API).
"""
import base64
import json
import os
import sys
import time
import urllib.error
import urllib.request

import nacl.public

ISSUER = os.environ.get("ISSUER", "https://auth.devhub.io.vn").rstrip("/")
ADMIN_PASSWORD = os.environ["ZITADEL_ADMIN_PASSWORD"]
# GITHUB_TOKEN has no administration scope, so writing Actions secrets
# needs the admin PAT provided as GH_SETUP_TOKEN (repo secret).
GH_TOKEN = os.environ.get("GH_SETUP_TOKEN") or os.environ["GITHUB_TOKEN"]
GH_REPO = os.environ.get("GH_REPO", "kungfufanta/4AiCoding")

REDIRECT_URI = "https://chat.devhub.io.vn/auth/callback"
POST_LOGOUT_URI = "https://chat.devhub.io.vn"
PROJECT_NAME = "Devhub Solutions"
APP_NAME = "portal-bff"
DEMO_USER = "portal-demo"
DEMO_PASSWORD = "Devhub-Portal-2026!"
DEMO_EMAIL = "portal-demo@devhub.io.vn"
PROMOTE_USER = "dnt.doanngocthanh"

LOGIN_NAME_CANDIDATES = [
    "admin@devhub.auth.devhub.io.vn",
    "admin",
    "iam-admin@devhub.auth.devhub.io.vn",
    "iam-admin",
]


def api(method, path, body=None, token=None, ok=(200, 201, 202, 204), raw=False):
    """One HTTPS call; returns parsed JSON (or raw text). Raises on error."""
    url = path if path.startswith("http") else f"{ISSUER}{path}"
    data = json.dumps(body).encode() if body is not None else None
    headers = {"Content-Type": "application/json", "User-Agent": "portal-oidc-config"}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            payload = resp.read()
            status = resp.status
    except urllib.error.HTTPError as e:
        detail = e.read().decode("utf-8", "replace")[:500]
        raise RuntimeError(f"{method} {url} -> {e.code}: {detail}") from None
    if status not in ok:
        raise RuntimeError(f"{method} {url} -> {status} (unexpected)")
    if raw or not payload:
        return None
    return json.loads(payload)


def gh_api(method, path, body=None):
    url = f"https://api.github.com{path}"
    data = json.dumps(body).encode() if body is not None else None
    headers = {
        "Authorization": f"Bearer {GH_TOKEN}",
        "Accept": "application/vnd.github+json",
        "User-Agent": "portal-oidc-config",
    }
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            payload = resp.read()
    except urllib.error.HTTPError as e:
        detail = e.read().decode("utf-8", "replace")[:500]
        raise RuntimeError(f"{method} {url} -> {e.code}: {detail}") from None
    return json.loads(payload) if payload else None


def login_admin():
    """Fallback: return (bearer_token, admin_user_id) via the Sessions API."""
    for name in LOGIN_NAME_CANDIDATES:
        try:
            s = api("POST", "/v2/sessions", body={
                "checks": {
                    "user": {"loginName": name},
                    "password": {"password": ADMIN_PASSWORD},
                },
            })
        except RuntimeError as e:
            print(f"  login attempt {name}: rejected ({str(e).split(':', 2)[-1].strip()[:120]})")
            continue
        token = s["sessionToken"]
        session_id = s["sessionId"]
        user_id = ""
        try:
            detail = api("GET", f"/v2/sessions/{session_id}", token=token)
            factors = (detail.get("session") or {}).get("factors") or {}
            user = factors.get("user") or {}
            user_id = user.get("id", "")
        except RuntimeError:
            pass
        print(f"  logged in as {name} (session {session_id[:8]}..., user_id={user_id or '?'})")
        return token, user_id
    print("ERROR: no login-name variant accepted ZITADEL_ADMIN_PASSWORD", file=sys.stderr)
    sys.exit(1)


class MgmtAuth:
    """Bearer management. Prefers the session token; if Zitadel rejects
    session tokens on the management API, mints a personal access token
    for the admin user and uses that instead."""

    def __init__(self, token, user_id):
        self.token = token
        self.user_id = user_id

    def _mgmt(self, method, path, body=None):
        return api(method, path, body=body, token=self.token)

    def _mint_pat(self):
        paths = [
            f"/management/v1/users/{self.user_id}/personal_access_tokens",
            f"/management/v1/users/{self.user_id}/pat",
        ]
        expiry = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time() + 90 * 86400))
        for path in paths:
            try:
                r = self._mgmt("POST", path, {"expirationDate": expiry})
                pat = r.get("token", "")
                if pat:
                    print("  session token rejected by management API — using a fresh PAT")
                    self.token = pat
                    return True
            except RuntimeError as e:
                print(f"  PAT mint via {path} failed: {str(e)[:120]}")
        return False

    def call(self, method, path, body=None):
        try:
            return self._mgmt(method, path, body)
        except RuntimeError as e:
            if "-> 401" not in str(e) and "-> 403" not in str(e):
                raise
            if not self._mint_pat():
                raise RuntimeError(
                    "management API rejected the session token and PAT minting "
                    "failed — see the errors above"
                ) from None
            return self._mgmt(method, path, body)


def find_or_create_project(mgmt):
    r = mgmt.call("POST", "/management/v1/projects/_search", {})
    for p in r.get("result", []):
        if p.get("name") == PROJECT_NAME:
            print(f"  project '{PROJECT_NAME}' exists (id {p['id']})")
            return p["id"]
    r = mgmt.call("POST", "/management/v1/projects", {"name": PROJECT_NAME})
    pid = r["id"]
    print(f"  created project '{PROJECT_NAME}' (id {pid})")
    return pid


def find_or_create_app(mgmt, pid):
    body = {
        "name": APP_NAME,
        "redirectUris": [REDIRECT_URI],
        "postLogoutRedirectUris": [POST_LOGOUT_URI],
        "responseTypes": ["OIDC_RESPONSE_TYPE_CODE"],
        "grantTypes": ["OIDC_GRANT_TYPE_AUTHORIZATION_CODE"],
        "appType": "OIDC_APP_TYPE_WEB",
        "authMethodType": "OIDC_AUTH_METHOD_TYPE_BASIC",
        "devMode": False,
        "accessTokenType": "OIDC_TOKEN_TYPE_BEARER",
        "idTokenUserinfoAssertion": True,
    }
    r = mgmt.call("POST", f"/management/v1/projects/{pid}/apps/_search", {})
    for a in r.get("result", []):
        if a.get("name") == APP_NAME:
            # No reliable regenerate endpoint on this build — delete and
            # recreate so every run ends with a fresh client secret that
            # is stored below.
            app_id = a["id"]
            print(f"  app '{APP_NAME}' exists (id {app_id}) — recreating it")
            mgmt.call("DELETE", f"/management/v1/projects/{pid}/apps/{app_id}")
    r = mgmt.call("POST", f"/management/v1/projects/{pid}/apps/oidc", body)
    print(f"  created OIDC app '{APP_NAME}' (id {r.get('appId')})")
    return r["clientId"], r["clientSecret"]


def search_user(mgmt, user_name):
    try:
        r = mgmt.call("POST", "/management/v1/users/_search", {
            "queries": [{"userNameQuery": {"userName": user_name,
                                           "method": "TEXT_QUERY_METHOD_EQUALS"}}],
        })
        for u in r.get("result", []):
            if u.get("userName") == user_name or u.get("preferredLoginName", "").startswith(user_name):
                return u["id"]
    except RuntimeError as e:
        print(f"  user search for {user_name} failed: {str(e)[:150]}")
    return None


def create_human_user(mgmt, username, first_name, last_name, email,
                      password):
    """Try the endpoints this Zitadel build may expose for creating a
    human user, in order."""
    bodies = [
        # v2 API "AddHumanUser"
        (f"{ISSUER}/v2/users/human", {
            "username": username,
            "organization": {},
            "profile": {"givenName": first_name, "familyName": last_name},
            "email": {"email": email, "isEmailVerified": True},
            "password": {"password": password, "changeRequired": False},
        }),
        # older management v1 import
        (f"{ISSUER}/management/v1/users/_import", {
            "userName": username,
            "profile": {"firstName": first_name, "lastName": last_name,
                        "displayName": f"{first_name} {last_name}",
                        "preferredLanguage": "en"},
            "email": {"email": email, "isEmailVerified": True},
            "password": password,
            "passwordChangeRequired": False,
        }),
        # older management v1 human create
        (f"{ISSUER}/management/v1/users/human", {
            "userName": username,
            "profile": {"firstName": first_name, "lastName": last_name},
            "email": {"email": email, "isEmailVerified": True},
            "password": password,
            "passwordChangeRequired": False,
        }),
    ]
    last_err = None
    for url, body in bodies:
        try:
            r = api("POST", url, body=body, token=mgmt.token)
            return r.get("userId") or r.get("id", "")
        except RuntimeError as e:
            last_err = e
            print(f"  create user via {url.split('.vn')[1]} failed: {str(e)[:110]}")
    raise RuntimeError(f"could not create human user {username}: {last_err}")


def set_password(mgmt, uid, password):
    for path, body in (
        (f"/v2/users/{uid}/password",
         {"password": password, "changeRequired": False}),
        (f"/management/v1/users/{uid}/password",
         {"password": password, "noVerification": True}),
    ):
        try:
            mgmt.call("POST", path, body)
            return True
        except RuntimeError as e:
            print(f"  set password via {path} failed: {str(e)[:110]}")
    return False


def ensure_demo_user(mgmt):
    uid = search_user(mgmt, DEMO_USER)
    if uid:
        print(f"  demo user '{DEMO_USER}' exists (id {uid}) — resetting password")
        if set_password(mgmt, uid, DEMO_PASSWORD):
            print("  demo user password reset OK")
        else:
            print("  demo user password NOT reset (continuing)")
        return
    uid = create_human_user(mgmt, DEMO_USER, "Portal", "Demo", DEMO_EMAIL,
                            DEMO_PASSWORD)
    print(f"  created demo user '{DEMO_USER}' (id {uid})")


def promote_owner(mgmt, pid):
    uid = search_user(mgmt, PROMOTE_USER)
    if not uid:
        print(f"  user '{PROMOTE_USER}' not found — skip promotion")
        return
    for label, path, body in (
        ("ORG_OWNER", "/management/v1/orgs/me/members",
         {"userId": uid, "roles": ["ORG_OWNER"]}),
        ("PROJECT_OWNER", f"/management/v1/projects/{pid}/members",
         {"userId": uid, "roles": ["PROJECT_OWNER"]}),
    ):
        try:
            mgmt.call("POST", path, body)
            print(f"  granted {label} to {PROMOTE_USER}")
        except RuntimeError as e:
            print(f"  {label} grant for {PROMOTE_USER} skipped: {str(e)[:120]}")


def store_repo_secret(name, value):
    pk = gh_api("GET", f"/repos/{GH_REPO}/actions/secrets/public-key")
    sealed = nacl.public.SealedBox(nacl.public.PublicKey(pk["key"].encode("utf-8")))
    encrypted = base64.b64encode(sealed.encrypt(value.encode("utf-8"))).decode("ascii")
    gh_api("PUT", f"/repos/{GH_REPO}/actions/secrets/{name}",
           body={"encrypted_value": encrypted, "key_id": pk["key_id"]})
    print(f"  stored repo secret {name}")


def main():
    pat = os.environ.get("ZITADEL_SETUP_PAT", "").strip()
    if not pat:
        pat_file = os.environ.get("ZITADEL_SETUP_PAT_FILE", "").strip()
        if pat_file:
            with open(pat_file) as f:
                pat = f.read().strip()
    if pat:
        print("1/5 bearer: iam-admin PAT from the cluster secret")
        mgmt = MgmtAuth(pat, "")
    else:
        print("1/5 no PAT available — logging in via the Sessions API")
        token, user_id = login_admin()
        mgmt = MgmtAuth(token, user_id)

    print("2/5 ensuring project")
    pid = find_or_create_project(mgmt)

    print("3/5 ensuring OIDC app")
    client_id, client_secret = find_or_create_app(mgmt, pid)

    print("4/5 users")
    ensure_demo_user(mgmt)
    promote_owner(mgmt, pid)

    print("5/5 storing PORTAL_OIDC_* repo secrets")
    store_repo_secret("PORTAL_OIDC_ISSUER", ISSUER)
    store_repo_secret("PORTAL_OIDC_CLIENT_ID", client_id)
    store_repo_secret("PORTAL_OIDC_CLIENT_SECRET", client_secret)
    print(f"done — issuer={ISSUER} clientId={client_id} (redacted above if masked)")


if __name__ == "__main__":
    main()
