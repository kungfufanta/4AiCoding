"""Error types for the Portal SDK."""

from dataclasses import dataclass
from typing import Any, Optional


@dataclass
class PortalErrorDetail:
    """The ``error`` object embedded in a Portal error response."""

    code: str
    message: str
    # Present on 422 plan_limit responses (per OpenAPI).
    field: Optional[str] = None
    limit: Any = None
    used: Any = None


class PortalError(Exception):
    """Raised when the Portal API returns a non-2xx response.

    Carries the HTTP status, error code, message, and (for 429/422)
    extra fields like ``Retry-After`` (header) or ``field/limit/used``
    (body, on 422 plan_limit).
    """

    def __init__(
        self,
        status_code: int,
        code: str,
        message: str,
        *,
        field: Optional[str] = None,
        limit: Any = None,
        used: Any = None,
        retry_after: Optional[str] = None,
    ) -> None:
        super().__init__(f"[{status_code}] {code}: {message}")
        self.status_code = status_code
        self.code = code
        self.message = message
        self.field = field
        self.limit = limit
        self.used = used
        self.retry_after = retry_after

    @property
    def detail(self) -> PortalErrorDetail:
        return PortalErrorDetail(
            code=self.code,
            message=self.message,
            field=self.field,
            limit=self.limit,
            used=self.used,
        )

    def __repr__(self) -> str:
        return (
            f"PortalError(status_code={self.status_code!r}, "
            f"code={self.code!r}, message={self.message!r})"
        )
