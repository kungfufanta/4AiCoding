"""Streaming helpers for the Portal SDK.

``exec_stream`` — async-style generator yielding parsed NDJSON lines
from ``POST /v1/sandboxes/{id}/exec?stream=ndjson``. Each chunk is
``{"stream":"stdout"|"stderr","data":"..."}``; the final line is
``{"exit_code":...,"timed_out":...,"duration_ms":...}``.

``logs_follow`` — generator yielding SSE ``data:`` payloads from
``GET /v1/sandboxes/{id}/logs?follow=true``.

Both functions take ``client`` as the first positional arg so they can
be wired as methods on ``PortalClient`` (see ``client.py``).
"""

import json
import urllib.error
import urllib.parse
import urllib.request
import uuid
from typing import Any, Dict, Iterator, List, Optional

from .errors import PortalError

USER_AGENT = "portal-sdk-python/0.1.0"


def exec_stream(
    client,
    sandbox_id: str,
    *,
    cmd: List[str],
    cwd: Optional[str] = None,
    env: Optional[Dict[str, str]] = None,
    stdin: Optional[str] = None,
    timeout_s: Optional[int] = None,
    max_output_bytes: Optional[int] = None,
    idempotency_key: Optional[str] = None,
) -> Iterator[Dict[str, Any]]:
    """Yield parsed NDJSON lines from a streaming exec run.

    Yields dicts. Each non-final line has shape ``{"stream": "stdout"
    | "stderr", "data": "..."}``. The terminal line has shape
    ``{"exit_code": int, "timed_out": bool, "duration_ms": int}``.
    """
    body: Dict[str, Any] = {"cmd": cmd}
    if cwd is not None:
        body["cwd"] = cwd
    if env is not None:
        body["env"] = env
    # Field contract (review #47): `stdin` is a plain string, NOT base64.
    if stdin is not None:
        body["stdin"] = stdin
    if timeout_s is not None:
        body["timeout_s"] = timeout_s
    if max_output_bytes is not None:
        body["max_output_bytes"] = max_output_bytes

    if idempotency_key is None:
        idempotency_key = str(uuid.uuid4())

    url = (
        client.base_url
        + f"/v1/sandboxes/{sandbox_id}/exec"
        + "?"
        + urllib.parse.urlencode({"stream": "ndjson"})
    )
    data = json.dumps(body).encode("utf-8")
    headers = {
        "Authorization": f"Bearer {client.api_key}",
        "Accept": "application/x-ndjson",
        "Content-Type": "application/json",
        "Idempotency-Key": idempotency_key,
        "User-Agent": USER_AGENT,
    }
    req = urllib.request.Request(url, data=data, method="POST", headers=headers)
    try:
        resp = urllib.request.urlopen(req, timeout=client.timeout)
    except urllib.error.HTTPError as e:
        raise _http_error(e) from None
    except urllib.error.URLError as e:
        raise PortalError(0, "network_error", str(e.reason)) from None

    try:
        while True:
            line = resp.readline()
            if not line:
                break
            stripped = line.strip()
            if not stripped:
                continue
            try:
                yield json.loads(stripped.decode("utf-8"))
            except (json.JSONDecodeError, UnicodeDecodeError):
                # Skip malformed lines — should not happen with a
                # conforming server, but don't crash the consumer.
                continue
    finally:
        resp.close()


def logs_follow(
    client,
    sandbox_id: str,
    *,
    tail: Optional[int] = None,
) -> Iterator[str]:
    """Yield SSE ``data:`` payloads from a following logs request.

    Each yielded value is the data payload of one ``data:`` line, with
    the leading ``data: `` stripped and the trailing newline removed.
    Other SSE fields (``event:``, ``id:``, ``retry:``) and blank lines
    are ignored.
    """
    query: Dict[str, str] = {"follow": "true"}
    if tail is not None:
        query["tail"] = str(tail)
    url = (
        client.base_url
        + f"/v1/sandboxes/{sandbox_id}/logs?"
        + urllib.parse.urlencode(query)
    )
    headers = {
        "Authorization": f"Bearer {client.api_key}",
        "Accept": "text/event-stream",
        "User-Agent": USER_AGENT,
    }
    req = urllib.request.Request(url, method="GET", headers=headers)
    try:
        resp = urllib.request.urlopen(req, timeout=client.timeout)
    except urllib.error.HTTPError as e:
        raise _http_error(e) from None
    except urllib.error.URLError as e:
        raise PortalError(0, "network_error", str(e.reason)) from None

    try:
        while True:
            line = resp.readline()
            if not line:
                break
            try:
                text = line.decode("utf-8")
            except UnicodeDecodeError:
                continue
            if text.startswith("data:"):
                payload = text[5:]
                if payload.startswith(" "):
                    payload = payload[1:]
                yield payload.rstrip("\r\n")
            # other SSE fields (event:, id:, retry:) and blank lines ignored
    finally:
        resp.close()


# ---------------------------------------------------------------------------
# internal
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
