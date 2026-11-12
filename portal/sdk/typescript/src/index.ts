/**
 * @4aicoding/portal-sdk — public entry point.
 *
 * Re-exports the client class, error type, and all model interfaces.
 */

export { PortalClient } from "./client.js";
export { PortalError } from "./errors.js";
export { execStream, logsFollow } from "./stream.js";
export type {
  CreateSandboxOptions,
  CreateTokenOptions,
  ExecOptions,
  ExecResult,
  ExecStreamChunk,
  FileContent,
  FileEntry,
  FileListing,
  FileWriteResult,
  ListFilesOptions,
  LogsOptions,
  PlanUsage,
  PortalClientOptions,
  PortalErrorDetail,
  ReadFileOptions,
  Sandbox,
  SandboxPreview,
  SandboxResources,
  SandboxStatus,
  UsageOptions,
  UsagePoint,
  UsageResponse,
  WriteFileOptions,
} from "./types.js";

export const VERSION = "0.1.0";
