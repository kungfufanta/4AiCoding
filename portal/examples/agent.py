#!/usr/bin/env python3
"""Minimal agent against the Portal public API (PORTAL_PLAN 6/S3).

Flow (plan 5 table): exchange an API key for a short-lived agent token,
create a sandbox, exec `python -c 'print(1+1)'`, read the result,
delete the sandbox.

Usage:
    export PORTAL_URL=https://app.example.com
    export PORTAL_API_KEY=sbx_live_<keyid>_<secret>
    python3 portal/examples/agent.py

Requires Python 3.9+ stdlib only. The key needs the sandbox:create,
sandbox:read and sandbox:exec scopes.
"""

import json
import os
import sys
import time
import urllib.error
import urllib.request

BASE = os.environ.get("PORTAL_URL", "http://localhost:8080").rstrip("/")
API_KEY = os.environ.get("PORTAL_API_KEY", "")


class APIError(RuntimeError):
    def __init__(self, status, code, message):
        super().__init__(f"{status} {code}: {message}")
        self.status = status
        self.code = code


def call(method, path, body=None, token=None, idempotency_key=None):
    headers = {"Content-Type": "application/json"}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    if idempotency_key:
        headers["Idempotency-Key"] = idempotency_key
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(BASE + path, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=120) as res:
            payload = res.read()
            return json.loads(payload) if payload else {}
    except urllib.error.HTTPError as e:
        try:
            err = json.loads(e.read()).get("error", {})
        except Exception:
            err = {}
        raise APIError(e.code, err.get("code", "unknown"), err.get("message", e.reason)) from None


def main() -> int:
    if not API_KEY:
        print("set PORTAL_API_KEY (and PORTAL_URL)", file=sys.stderr)
        return 2

    # 1. Exchange the long-lived key for a short-lived agent token.
    tok = call("POST", "/v1/tokens", {"scopes": ["sandbox:create", "sandbox:read", "sandbox:exec"]},
               token=API_KEY)
    token = tok["token"]
    print(f"agent token expires {tok['expires_at']}")

    # 2. Create a sandbox (retry-safe under the same Idempotency-Key).
    idem = f"agent-example-{int(time.time())}"
    sb = call("POST", "/v1/sandboxes",
              {"image": "python:3.12", "resources": {"cpus": 1, "memory_mb": 512}},
              token=token, idempotency_key=idem)
    sbx_id = sb["id"]
    print(f"sandbox {sbx_id} status={sb['status']}")

    try:
        # 3. Run code in it (a non-zero exit is still a 200).
        out = call("POST", f"/v1/sandboxes/{sbx_id}/exec",
                   {"cmd": ["python", "-c", "print(1+1)"]},
                   token=token, idempotency_key=idem + "-exec")
        print(f"stdout={out['stdout'].strip()} exit={out['exit_code']}")
        if out["stdout"].strip() != "2":
            print("unexpected result", file=sys.stderr)
            return 1

        # 4. Read it back (ownership + live status), then tear it down.
        sb = call("GET", f"/v1/sandboxes/{sbx_id}", token=token)
        print(f"before delete: status={sb['status']}")
    finally:
        call("DELETE", f"/v1/sandboxes/{sbx_id}", token=token)
        print("sandbox deleted")
    return 0


if __name__ == "__main__":
    sys.exit(main())
