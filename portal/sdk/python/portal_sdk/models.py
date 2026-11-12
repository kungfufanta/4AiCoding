"""Dataclass models for the Portal SDK.

Each model has a ``from_dict`` classmethod that parses a raw JSON dict
returned by the Portal /v1 API. Unknown fields are ignored; required
fields default to type-appropriate empties so partial responses don't
crash callers.
"""

import base64
from dataclasses import dataclass, field
from typing import Any, Dict, List, Optional


# ---------------------------------------------------------------------------
# Sandbox
# ---------------------------------------------------------------------------


@dataclass
class Resources:
    cpus: Optional[float] = None
    memory_mb: Optional[int] = None

    @classmethod
    def from_dict(cls, d: Optional[Dict[str, Any]]) -> Optional["Resources"]:
        if d is None:
            return None
        return cls(cpus=d.get("cpus"), memory_mb=d.get("memory_mb"))


@dataclass
class Sandbox:
    id: str
    status: str
    image: str = ""
    resources: Optional[Resources] = None
    ports: List[int] = field(default_factory=list)
    egress: List[str] = field(default_factory=list)
    egress_mode: Optional[str] = None
    preview: Optional[Dict[str, Any]] = None
    project_id: Optional[str] = None
    created_at: Optional[str] = None
    updated_at: Optional[str] = None

    @classmethod
    def from_dict(cls, d: Dict[str, Any]) -> "Sandbox":
        return cls(
            id=d["id"],
            status=d.get("status", ""),
            image=d.get("image", ""),
            resources=Resources.from_dict(d.get("resources")),
            ports=list(d.get("ports", []) or []),
            egress=list(d.get("egress", []) or []),
            egress_mode=d.get("egress_mode"),
            preview=d.get("preview"),
            project_id=d.get("project_id"),
            created_at=d.get("created_at"),
            updated_at=d.get("updated_at"),
        )


# ---------------------------------------------------------------------------
# Exec
# ---------------------------------------------------------------------------


@dataclass
class ExecResult:
    stdout: str = ""
    stderr: str = ""
    exit_code: int = 0
    timed_out: bool = False
    truncated: bool = False
    duration_ms: int = 0

    @classmethod
    def from_dict(cls, d: Dict[str, Any]) -> "ExecResult":
        return cls(
            stdout=d.get("stdout", ""),
            stderr=d.get("stderr", ""),
            exit_code=d.get("exit_code", 0),
            timed_out=d.get("timed_out", False),
            truncated=d.get("truncated", False),
            duration_ms=d.get("duration_ms", 0),
        )


# ---------------------------------------------------------------------------
# Files
# ---------------------------------------------------------------------------


@dataclass
class FileEntry:
    path: str
    name: str = ""
    size: int = 0
    mode: Optional[str] = None
    is_dir: bool = False

    @classmethod
    def from_dict(cls, d: Dict[str, Any]) -> "FileEntry":
        return cls(
            path=d.get("path", ""),
            name=d.get("name", ""),
            size=d.get("size", 0),
            mode=d.get("mode"),
            is_dir=d.get("is_dir", False),
        )


@dataclass
class FileListing:
    path: str = ""
    recursive: bool = False
    entries: List[FileEntry] = field(default_factory=list)
    truncated: bool = False

    @classmethod
    def from_dict(cls, d: Dict[str, Any]) -> "FileListing":
        return cls(
            path=d.get("path", ""),
            recursive=d.get("recursive", False),
            entries=[FileEntry.from_dict(e) for e in (d.get("entries") or [])],
            truncated=d.get("truncated", False),
        )


@dataclass
class FileContent:
    path: str = ""
    size: int = 0
    offset: int = 0
    encoding: str = "utf-8"
    data: str = ""
    truncated: bool = False

    @classmethod
    def from_dict(cls, d: Dict[str, Any]) -> "FileContent":
        return cls(
            path=d.get("path", ""),
            size=d.get("size", 0),
            offset=d.get("offset", 0),
            encoding=d.get("encoding", "utf-8"),
            data=d.get("data", ""),
            truncated=d.get("truncated", False),
        )

    def as_bytes(self) -> bytes:
        """Decode ``data`` per ``encoding`` and return as raw bytes."""
        if self.encoding == "base64":
            return base64.b64decode(self.data)
        return self.data.encode("utf-8")


@dataclass
class FileWriteResult:
    path: str = ""
    size: int = 0
    sha256: str = ""

    @classmethod
    def from_dict(cls, d: Dict[str, Any]) -> "FileWriteResult":
        return cls(
            path=d.get("path", ""),
            size=d.get("size", 0),
            sha256=d.get("sha256", ""),
        )


# ---------------------------------------------------------------------------
# Usage
# ---------------------------------------------------------------------------


@dataclass
class UsagePoint:
    day: str = ""
    kind: str = ""
    quantity: int = 0

    @classmethod
    def from_dict(cls, d: Dict[str, Any]) -> "UsagePoint":
        return cls(
            day=d.get("day", ""),
            kind=d.get("kind", ""),
            quantity=d.get("quantity", 0),
        )


@dataclass
class PlanUsage:
    plan_id: str = ""
    plan_name: str = ""
    monthly_vcpu_seconds: int = 0
    used_vcpu_seconds: int = 0
    max_concurrent_sandboxes: int = 0
    running_sandboxes: int = 0

    @classmethod
    def from_dict(cls, d: Optional[Dict[str, Any]]) -> Optional["PlanUsage"]:
        if d is None:
            return None
        return cls(
            plan_id=d.get("plan_id", ""),
            plan_name=d.get("plan_name", ""),
            monthly_vcpu_seconds=d.get("monthly_vcpu_seconds", 0),
            used_vcpu_seconds=d.get("used_vcpu_seconds", 0),
            max_concurrent_sandboxes=d.get("max_concurrent_sandboxes", 0),
            running_sandboxes=d.get("running_sandboxes", 0),
        )


@dataclass
class UsageResponse:
    # `from` is a Python keyword — field name is `frm`.
    frm: str = ""
    to: str = ""
    kind: str = "all"
    series: List[UsagePoint] = field(default_factory=list)
    plan: Optional[PlanUsage] = None

    @classmethod
    def from_dict(cls, d: Dict[str, Any]) -> "UsageResponse":
        return cls(
            frm=d.get("from", ""),
            to=d.get("to", ""),
            kind=d.get("kind", "all"),
            series=[UsagePoint.from_dict(p) for p in (d.get("series") or [])],
            plan=PlanUsage.from_dict(d.get("plan")),
        )
