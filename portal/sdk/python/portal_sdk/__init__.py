"""Thin Python SDK for the 4AiCoding Portal /v1 API.

Pure stdlib (urllib + http.client), no third-party deps. Provides:

- ``PortalClient`` — main entry point, configurable base URL + API key.
- Dataclasses for Sandbox, ExecResult, FileListing, FileContent,
  FileWriteResult, UsageResponse.
- Streaming helpers ``exec_stream`` (NDJSON) and ``logs_follow`` (SSE).

Auth: Bearer Portal API key (``sbx_live_...``) or agent token.
Idempotency: POST /v1/sandboxes and POST /v1/sandboxes/{id}/exec
auto-inject an ``Idempotency-Key`` (UUID4) if the caller omits one.
Retry: GET requests are retried once on HTTP 502/503/504.
"""

from .client import PortalClient
from .errors import PortalError, PortalErrorDetail
from .models import (
    ExecResult,
    FileContent,
    FileEntry,
    FileListing,
    FileWriteResult,
    PlanUsage,
    Resources,
    Sandbox,
    UsagePoint,
    UsageResponse,
)
from .stream import exec_stream, logs_follow

__version__ = "0.1.0"

__all__ = [
    "PortalClient",
    "PortalError",
    "PortalErrorDetail",
    "Sandbox",
    "Resources",
    "ExecResult",
    "FileEntry",
    "FileListing",
    "FileContent",
    "FileWriteResult",
    "UsagePoint",
    "PlanUsage",
    "UsageResponse",
    "exec_stream",
    "logs_follow",
    "__version__",
]
