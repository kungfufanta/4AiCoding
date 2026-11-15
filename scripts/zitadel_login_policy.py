#!/usr/bin/env python3
"""Turn on self sign-up in Zitadel's default login policy.

Run by .github/workflows/zitadel-login-policy.yml with the iam-admin PAT
(Kubernetes Secret `iam-admin-pat`, passed in via ZITADEL_SETUP_PAT_FILE).
Sets allowRegister=true and allowUsernamePassword=true and keeps every
other field as it is. Safe to re-run: "no changes" counts as success.
Secret values are never printed.

Env:
  ISSUER                  default https://auth.devhub.io.vn
  ZITADEL_SETUP_PAT_FILE  file holding the iam-admin PAT (required)
"""
import json
import os
import sys
import urllib.error
import urllib.request

ISSUER = os.environ.get("ISSUER", "https://auth.devhub.io.vn").rstrip("/")

# Fields accepted by PUT /admin/v1/policies/login (UpdateLoginPolicyRequest).
UPDATE_FIELDS = (
    "allowUsernamePassword", "allowRegister", "allowExternalIdp", "forceMfa",
    "passwordlessType", "hidePasswordReset", "ignoreUnknownUsernames",
    "defaultRedirectUri", "passwordCheckLifetime", "externalLoginCheckLifetime",
    "mfaInitSkipLifetime", "secondFactorCheckLifetime", "multiFactorCheckLifetime",
    "allowDomainDiscovery", "disableLoginWithEmail", "disableLoginWithPhone",
    "forceMfaLocalOnly",
)
SHOWN = ("allowRegister", "allowUsernamePassword", "allowExternalIdp",
         "forceMfa", "passwordlessType", "isDefault")


def api(method, path, token, body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(f"{ISSUER}{path}", data=data, method=method, headers={
        "Content-Type": "application/json",
        "Authorization": f"Bearer {token}",
        "User-Agent": "zitadel-login-policy",
    })
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            payload = resp.read()
    except urllib.error.HTTPError as e:
        detail = e.read().decode("utf-8", "replace")[:400]
        return e.code, detail
    return 200, json.loads(payload) if payload else {}


def main():
    pat_file = os.environ.get("ZITADEL_SETUP_PAT_FILE", "")
    if not pat_file or not os.path.exists(pat_file):
        sys.exit("ERROR: ZITADEL_SETUP_PAT_FILE is not set or missing")
    token = open(pat_file).read().strip()

    code, cur = api("GET", "/admin/v1/policies/login", token)
    if code != 200:
        sys.exit(f"ERROR: GET /admin/v1/policies/login -> {code}: {cur}")
    policy = cur.get("policy", {})
    print("before:", json.dumps({k: policy.get(k) for k in SHOWN}))

    body = {k: policy[k] for k in UPDATE_FIELDS if k in policy}
    body["allowRegister"] = True
    body["allowUsernamePassword"] = True
    code, res = api("PUT", "/admin/v1/policies/login", token, body)
    if code != 200:
        # Zitadel answers 400 "No changes" (COMMAND-...) when the policy
        # already matches: that is the idempotent success case.
        if code == 400 and "no changes" in str(res).lower():
            print("policy already allows self sign-up (no changes)")
        else:
            sys.exit(f"ERROR: PUT /admin/v1/policies/login -> {code}: {res}")

    code, after = api("GET", "/admin/v1/policies/login", token)
    if code != 200:
        sys.exit(f"ERROR: GET /admin/v1/policies/login -> {code}: {after}")
    p = after.get("policy", {})
    print("after: ", json.dumps({k: p.get(k) for k in SHOWN}))
    if not p.get("allowRegister") or not p.get("allowUsernamePassword"):
        sys.exit("ERROR: policy still does not allow self sign-up")
    print(f"OK: open {ISSUER}/ui/v2/login/register to check the sign-up page")


if __name__ == "__main__":
    main()
