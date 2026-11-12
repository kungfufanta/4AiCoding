/**
 * PortalError — raised on any non-2xx Portal API response.
 *
 * Carries the HTTP status, the `error.code`, the human-readable
 * `error.message`, and (for 429 / 422 plan_limit responses) extra
 * fields like `Retry-After` (header) or `field/limit/used` (body).
 */

import type { PortalErrorDetail } from "./types.js";

export class PortalError extends Error {
  readonly statusCode: number;
  readonly code: string;
  readonly detail: Partial<PortalErrorDetail>;
  /** Value of the `Retry-After` response header (set on 429 responses). */
  readonly retryAfter?: string;

  constructor(
    statusCode: number,
    code: string,
    message: string,
    detail: Partial<PortalErrorDetail> = {},
    retryAfter?: string,
  ) {
    super(message);
    this.name = "PortalError";
    this.statusCode = statusCode;
    this.code = code;
    this.detail = detail;
    this.retryAfter = retryAfter;
  }

  override toString(): string {
    return `PortalError [${this.statusCode}] ${this.code}: ${this.message}`;
  }
}
