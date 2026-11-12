"""PortalClient — thin stdlib-only HTTP client for the Portal /v1 API."""

import json
import urllib.error
import urllib.parse
import urllib.request
import uuid
from typing import Any, Dict, List, Optional, Union

from .errors import PortalError
from .models import (
    ExecResult,
    FileContent,
    FileListing,
    FileWriteResult,
    Sandbox,
    UsageResponse,
)
from .stream import exec_stream as _exec_stream_impl
from .stream import logs_follow as _logs_follow_impl

DEFAULT_TIMEOUT = 30
USER_AGENT = "portal-sdk-python/0.1.0"
RETRY_STATUSES = {502, 503, 504}


class PortalClient:
    """Thin client for the 4AiCoding Portal /v1 API.

    Args:
        base_url: e.g. ``https://chat.devhub.io.vn`` (no trailing slash).
        api_key: Portal API key (``sbx_live_...``) or agent token.
        timeout: per-request socket timeout in seconds.

    Idempotency: ``create_sandbox`` and ``exec`` auto-inject an
    ``Idempotency-Key`` (UUID4) header when the caller doesn't pass one.
    Retry: GET requests are retried once on HTTP 502/503/504.
    """

    def __init__(
        self,
        base_url: str,
        api_key: str,
        timeout: int = DEFAULT_TIMEOUT,
    ) -> None:
        if not base_url:
            raise ValueError("base_url is required")
        if not api_key:
            raise ValueError("api_key is required")
        self.base_url = base_url.rstrip("/")
        self.api_key = api_key
        self.timeout = timeout

    # ====================================================================
    # Sandboxes
    # ====================================================================

    def create_sandbox(
        self,
        *,
        image: str,
        project_id: Optional[str] = None,
        command: Optional[List[str]] = None,
        env: Optional[Dict[str, str]] = None,
        resources: Optional[Dict[str, Any]] = None,
        ports: Optional[List[int]] = None,
        egress: Optional[Dict[str, Any]] = None,
        workspace: Optional[Dict[str, str]] = None,
        ttl_seconds: Optional[int] = None,
        idempotency_key: Optional[str] = None,
    ) -> Sandbox:
        body: Dict[str, Any] = {"image": image}
        if project_id is not None:
            body["project_id"] = project_id
        if command is not None:
            body["command"] = command
        if env is not None:
            body["env"] = env
        if resources is not None:
            body["resources"] = resources
        if ports is not None:
            body["ports"] = ports
        if egress is not None:
            body["egress"] = egress
        if workspace is not None:
            body["workspace"] = workspace
        if ttl_seconds is not None:
            body["ttl_seconds"] = ttl_seconds

        if idempotency_key is None:
            idempotency_key = str(uuid.uuid4())
        result = self._request(
            "POST",
            "/v1/sandboxes",
            body=body,
            headers={"Idempotency-Key": idempotency_key},
        )
        return Sandbox.from_dict(result)

    def get_sandbox(self, sandbox_id: str) -> Sandbox:
        result = self._request(
            "GET", f"/v1/sandboxes/{sandbox_id}", retry_get=True
        )
        return Sandbox.from_dict(result)

    def list_sandboxes(
        self, project_id: Optional[str] = None
    ) -> Dict[str, Any]:
        result = self._request(
            "GET",
            "/v1/sandboxes",
            query={"project_id": project_id},
            retry_get=True,
        )
        if isinstance(result, dict):
            sbs = result.get("sandboxes") or []
            result["sandboxes"] = [Sandbox.from_dict(s) for s in sbs]
        return result

    def start_sandbox(self, sandbox_id: str) -> None:
        self._request("POST", f"/v1/sandboxes/{sandbox_id}/start")

    def stop_sandbox(self, sandbox_id: str) -> None:
        self._request("POST", f"/v1/sandboxes/{sandbox_id}/stop")

    def delete_sandbox(self, sandbox_id: str) -> None:
        self._request("DELETE", f"/v1/sandboxes/{sandbox_id}")

    # ====================================================================
    # Exec
    # ====================================================================

    def exec(
        self,
        sandbox_id: str,
        *,
        cmd: List[str],
        cwd: Optional[str] = None,
        env: Optional[Dict[str, str]] = None,
        stdin: Optional[str] = None,
        timeout_s: Optional[int] = None,
        max_output_bytes: Optional[int] = None,
        idempotency_key: Optional[str] = None,
    ) -> ExecResult:
        body: Dict[str, Any] = {"cmd": cmd}
        if cwd is not None:
            body["cwd"] = cwd
        if env is not None:
            body["env"] = env
        # Field contract: `stdin` is a plain string (review #47).
        if stdin is not None:
            body["stdin"] = stdin
        if timeout_s is not None:
            body["timeout_s"] = timeout_s
        if max_output_bytes is not None:
            body["max_output_bytes"] = max_output_bytes

        if idempotency_key is None:
            idempotency_key = str(uuid.uuid4())
        result = self._request(
            "POST",
            f"/v1/sandboxes/{sandbox_id}/exec",
            body=body,
            headers={"Idempotency-Key": idempotency_key},
        )
        return ExecResult.from_dict(result)

    def exec_stream(
        self,
        sandbox_id: str,
        *,
        cmd: List[str],
        cwd: Optional[str] = None,
        env: Optional[Dict[str, str]] = None,
        stdin: Optional[str] = None,
        timeout_s: Optional[int] = None,
        max_output_bytes: Optional[int] = None,
        idempotency_key: Optional[str] = None,
    ):
        """Stream exec output as NDJSON. See ``portal_sdk.stream.exec_stream``."""
        return _exec_stream_impl(
            self,
            sandbox_id,
            cmd=cmd,
            cwd=cwd,
            env=env,
            stdin=stdin,
            timeout_s=timeout_s,
            max_output_bytes=max_output_bytes,
            idempotency_key=idempotency_key,
        )

    # ====================================================================
    # Files
    # ====================================================================

    def list_files(
        self,
        sandbox_id: str,
        *,
        path: Optional[str] = None,
        recursive: Optional[bool] = None,
        max_entries: Optional[int] = None,
    ) -> FileListing:
        query = {
            "path": path,
            "recursive": recursive,
            "max_entries": max_entries,
        }
        result = self._request(
            "GET",
            f"/v1/sandboxes/{sandbox_id}/files",
            query=query,
            retry_get=True,
        )
        return FileListing.from_dict(result)

    def read_file(
        self,
        sandbox_id: str,
        *,
        path: str,
        offset: Optional[int] = None,
        limit: Optional[int] = None,
    ) -> FileContent:
        query = {"path": path, "offset": offset, "limit": limit}
        result = self._request(
            "GET",
            f"/v1/sandboxes/{sandbox_id}/files/content",
            query=query,
            retry_get=True,
        )
        return FileContent.from_dict(result)

    def write_file(
        self,
        sandbox_id: str,
        *,
        path: str,
        data: Union[bytes, str],
        if_match: Optional[str] = None,
    ) -> FileWriteResult:
        """PUT raw bytes to a sandbox file.

        Per OpenAPI contract, the body is ``application/octet-stream``
        (raw bytes), NOT JSON-wrapped. ``data`` may be ``str`` or
        ``bytes`` — strings are UTF-8 encoded.
        """
        raw = data.encode("utf-8") if isinstance(data, str) else data
        query: Dict[str, Any] = {"path": path}
        if if_match is not None:
            query["if_match"] = if_match
        result = self._request(
            "PUT",
            f"/v1/sandboxes/{sandbox_id}/files/content",
            raw_body=raw,
            query=query,
        )
        return FileWriteResult.from_dict(result)

    def delete_file(self, sandbox_id: str, *, path: str) -> None:
        self._request(
            "DELETE",
            f"/v1/sandboxes/{sandbox_id}/files",
            query={"path": path},
        )

    def move_file(
        self, sandbox_id: str, *, frm: str, to: str
    ) -> Dict[str, str]:
        body = {"from": frm, "to": to}
        result = self._request(
            "POST",
            f"/v1/sandboxes/{sandbox_id}/files/move",
            body=body,
        )
        return result

    # ====================================================================
    # Logs
    # ====================================================================

    def get_logs(
        self,
        sandbox_id: str,
        *,
        tail: Optional[int] = None,
        follow: bool = False,
    ) -> bytes:
        """Fetch container logs (text/plain). For SSE follow, use
        ``logs_follow`` instead."""
        query: Dict[str, Any] = {"follow": follow}
        if tail is not None:
            query["tail"] = tail
        result = self._request(
            "GET",
            f"/v1/sandboxes/{sandbox_id}/logs",
            query=query,
            accept="text/plain",
            retry_get=True,
            parse_json=False,
        )
        if isinstance(result, bytes):
            return result
        if isinstance(result, str):
            return result.encode("utf-8")
        return b""

    def logs_follow(
        self, sandbox_id: str, *, tail: Optional[int] = None
    ):
        """Follow logs as SSE. See ``portal_sdk.stream.logs_follow``."""
        return _logs_follow_impl(self, sandbox_id, tail=tail)

    # ====================================================================
    # Usage
    # ====================================================================

    def get_usage(
        self,
        *,
        frm: Optional[str] = None,
        to: Optional[str] = None,
        kind: Optional[str] = None,
    ) -> UsageResponse:
        query = {"from": frm, "to": to, "kind": kind}
        result = self._request(
            "GET", "/v1/usage", query=query, retry_get=True
        )
        return UsageResponse.from_dict(result)

    # ====================================================================
    # Tokens
    # ====================================================================

    def create_token(
        self,
        *,
        scopes: List[str],
        ttl_seconds: Optional[int] = None,
        agent_run_id: Optional[str] = None,
    ) -> Dict[str, str]:
        body: Dict[str, Any] = {"scopes": scopes}
        if ttl_seconds is not None:
            body["ttl_seconds"] = ttl_seconds
        if agent_run_id is not None:
            body["agent_run_id"] = agent_run_id
        return self._request("POST", "/v1/tokens", body=body)

    # ====================================================================
    # Internal HTTP plumbing
    # ====================================================================

    def _request(
        self,
        method: str,
        path: str,
        *,
        body: Optional[Dict[str, Any]] = None,
        raw_body: Optional[bytes] = None,
        query: Optional[Dict[str, Any]] = None,
        headers: Optional[Dict[str, str]] = None,
        accept: str = "application/json",
        retry_get: bool = False,
        parse_json: bool = True,
    ) -> Any:
        url = self.base_url + path
        if query:
            qs = urllib.parse.urlencode(
                {k: v for k, v in query.items() if v is not None}
            )
            if qs:
                url += "?" + qs

        if raw_body is not None:
            data: Optional[bytes] = raw_body
        elif body is not None:
            data = json.dumps(body).encode("utf-8")
        else:
            data = None

        h: Dict[str, str] = {
            "Authorization": f"Bearer {self.api_key}",
            "Accept": accept,
            "User-Agent": USER_AGENT,
        }
        if data is not None and raw_body is None:
            h["Content-Type"] = "application/json"
        elif raw_body is not None:
            h["Content-Type"] = "application/octet-stream"
        if headers:
            h.update(headers)

        attempts = 2 if retry_get else 1
        last_error: Optional[Exception] = None
        for attempt in range(attempts):
            req = urllib.request.Request(
                url, data=data, method=method, headers=h
            )
            try:
                resp = urllib.request.urlopen(req, timeout=self.timeout)
                try:
                    return self._parse_response(resp, parse_json)
                finally:
                    resp.close()
            except urllib.error.HTTPError as e:
                if (
                    retry_get
                    and e.code in RETRY_STATUSES
                    and attempt == 0
                ):
                    last_error = e
                    continue
                raise _http_error(e) from None
            except urllib.error.URLError as e:
                # Per spec, only retry on 502/503/504, not network errors.
                raise PortalError(0, "network_error", str(e.reason)) from None

        # Should be unreachable; if we got here, the retry loop exited
        # without returning or raising.
        if last_error is not None:
            raise _http_error(last_error)
        raise PortalError(0, "unknown", "request failed without error")

    def _parse_response(self, resp, parse_json: bool) -> Any:
        status = resp.status
        if status == 204:
            return None
        body = resp.read()
        if not body:
            return None
        ctype = ""
        if resp.headers:
            ctype = resp.headers.get("Content-Type", "")
        if parse_json or "application/json" in ctype:
            try:
                return json.loads(body.decode("utf-8"))
            except (json.JSONDecodeError, UnicodeDecodeError):
                # Fall through to returning raw bytes for non-JSON bodies.
                pass
        return body


# ---------------------------------------------------------------------------
# Module-level helpers (shared with stream.py)
# ---------------------------------------------------------------------------


def _http_error(e: urllib.error.HTTPError) -> PortalError:
    try:
        raw = e.read()
        payload = json.loads(raw.decode("utf-8")) if raw else {}
    except Exception:
        payload = {}
    err = payload.get("error", {}) if isinstance(payload, dict) else {}
    retry_after = None
    if e.headers:
        retry_after = e.headers.get("Retry-After")
    return PortalError(
        status_code=e.code,
        code=err.get("code", "http_error"),
        message=err.get("message", e.reason or "HTTP error"),
        field=err.get("field"),
        limit=err.get("limit"),
        used=err.get("used"),
        retry_after=retry_after,
    )
