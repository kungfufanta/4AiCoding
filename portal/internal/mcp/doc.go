// Package mcp implements a Streamable HTTP MCP server (PORTAL_PLAN_V2
// P6 / AGENT_PLAN A5) that exposes the Portal's sandbox surface as
// MCP tools an agent can call without writing HTTP. The server is a
// thin wrapper over portal/internal/httpapi's sandboxd.Client: each
// tool maps to one Portal API call, authed by the caller's Bearer
// Portal API key (passed through from the MCP request's
// Authorization header).
//
// Transport: Streamable HTTP (MCP spec 2025-06-18). One POST per
// JSON-RPC request; the server streams tool output back. There is
// no SSE / WebSocket — the transport is simple HTTP request/response,
// which keeps the server stateless and lets it run behind the same
// Traefik ingress as the Portal dashboard.
//
// Tools:
//
//	sandbox_create(image, project_id?, ttl_seconds?, resources?, ports?)
//	sandbox_exec(sandbox_id, cmd[], cwd?, env?, stdin?, timeout_s?)
//	sandbox_delete(sandbox_id)
//	read_file(sandbox_id, path)
//	write_file(sandbox_id, path, content)
//	list_files(sandbox_id, path?)
//
// Auth: the caller's Bearer token is forwarded to the Portal API as
// the Authorization header. The Portal's scope gate (sandbox:create,
// sandbox:exec, sandboxes:files) applies: a key without sandboxes:files
// gets 403 on read_file/write_file/list_files.
package mcp
