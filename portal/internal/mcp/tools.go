// MCP tool definitions and the tools/call dispatch. Each tool maps
// to one Portal /v1 API call; the Bearer token from the MCP request
// is forwarded as Authorization.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// tool describes one MCP tool the server exposes.
type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// allTools returns the tool catalogue the server advertises on
// tools/list. Each tool's inputSchema is a JSON Schema describing the
// parameters; the Portal's OpenAPI spec (portal/openapi.yaml) is the
// source of truth for the underlying request shapes.
func allTools() []tool {
	return []tool{
		{
			Name:        "sandbox_create",
			Description: "Create a sandbox. Returns the sandbox id (ULID).",
			InputSchema: schemaObject("Create a sandbox.", map[string]any{
				"image":       schemaString("repo[:tag], max 200 chars."),
				"project_id":  schemaString("Optional project UUID."),
				"ttl_seconds": schemaInt("Sandbox idle timeout. 0 = sandboxd default. Clamped to the org's plan ceiling.", 0, 604800),
				"cpus":        schemaNum("vCPU count (clamped to plan.max_vcpu_per_sandbox)."),
				"memory_mb":   schemaInt("Memory in MiB.", 0, 65536),
			}, []string{"image"}),
		},
		{
			Name:        "sandbox_exec",
			Description: "Run a command in a running sandbox. Returns stdout, stderr, exit_code.",
			InputSchema: schemaObject("Run a command.", map[string]any{
				"sandbox_id": schemaString("Sandbox ULID."),
				"cmd": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"minItems":    1,
					"maxItems":    64,
					"description": "Argv, e.g. [\"bash\",\"-lc\",\"echo hi\"].",
				},
				"cwd":       schemaString("Working directory (under /home/sandbox)."),
				"stdin":     schemaString("Plain string input (not base64). Max 1 MiB."),
				"timeout_s": schemaInt("In-container timeout. Clamped to 600.", 1, 600),
			}, []string{"sandbox_id", "cmd"}),
		},
		{
			Name:        "sandbox_delete",
			Description: "Delete a sandbox (container + workspace + index row).",
			InputSchema: schemaObject("Delete a sandbox.", map[string]any{
				"sandbox_id": schemaString("Sandbox ULID."),
			}, []string{"sandbox_id"}),
		},
		{
			Name:        "read_file",
			Description: "Read a file from the sandbox workspace (text UTF-8 or base64 for binary).",
			InputSchema: schemaObject("Read a file.", map[string]any{
				"sandbox_id": schemaString("Sandbox ULID."),
				"path":       schemaString("Workspace-relative path."),
			}, []string{"sandbox_id", "path"}),
		},
		{
			Name:        "write_file",
			Description: "Write a file to the sandbox workspace (raw bytes, not JSON).",
			InputSchema: schemaObject("Write a file.", map[string]any{
				"sandbox_id": schemaString("Sandbox ULID."),
				"path":       schemaString("Workspace-relative path."),
				"content":    schemaString("File content (UTF-8 text)."),
			}, []string{"sandbox_id", "path", "content"}),
		},
		{
			Name:        "list_files",
			Description: "List workspace files (relative to /home/sandbox).",
			InputSchema: schemaObject("List files.", map[string]any{
				"sandbox_id": schemaString("Sandbox ULID."),
				"path":       schemaString("Directory to list (default root)."),
			}, []string{"sandbox_id"}),
		},
	}
}

// --- JSON Schema helpers ----------------------------------------------

func schemaObject(desc string, props map[string]any, required []string) map[string]any {
	return map[string]any{
		"type":        "object",
		"description": desc,
		"properties":  props,
		"required":    required,
	}
}

func schemaString(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func schemaInt(desc string, min, max int) map[string]any {
	return map[string]any{"type": "integer", "description": desc, "minimum": min, "maximum": max}
}

func schemaNum(desc string) map[string]any {
	return map[string]any{"type": "number", "description": desc}
}

// --- tools/call dispatch ---------------------------------------------

// handleToolCall parses the tool name + arguments and forwards the
// call to the Portal /v1 API. The result is wrapped as MCP tool
// content (a list of {type, text} blocks).
func (s *Server) handleToolCall(ctx context.Context, token string, req *rpcRequest) *rpcResponse {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return rpcErr(req.ID, -32602, "invalid params: "+err.Error(), nil)
	}
	var args map[string]any
	if len(params.Arguments) > 0 {
		if err := json.Unmarshal(params.Arguments, &args); err != nil {
			return rpcErr(req.ID, -32602, "invalid arguments: "+err.Error(), nil)
		}
	}
	result, mcpErr := s.callTool(ctx, token, params.Name, args)
	if mcpErr != nil {
		// Protocol problems (unknown tool, bad arguments) are JSON-RPC
		// errors. A refusal from the Portal (HTTP status as the code)
		// is a tool execution error: MCP wants it in the result with
		// isError so the model sees the message and can react to it.
		if mcpErr.Code < 0 {
			return rpcErr(req.ID, mcpErr.Code, mcpErr.Message, mcpErr.Data)
		}
		return &rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"content": []map[string]string{
					{"type": "text", "text": fmt.Sprintf("HTTP %d: %s", mcpErr.Code, mcpErr.Message)},
				},
				"isError": true,
			},
		}
	}
	// MCP tool result: {content: [{type:"text", text:…}], isError:false}
	return &rpcResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result: map[string]any{
			"content": []map[string]string{
				{"type": "text", "text": result},
			},
			"isError": false,
		},
	}
}

// callTool forwards to the Portal API and returns the text result.
func (s *Server) callTool(ctx context.Context, token, name string, args map[string]any) (string, *rpcError) {
	switch name {
	case "sandbox_create":
		return s.toolCreateSandbox(ctx, token, args)
	case "sandbox_exec":
		return s.toolExecSandbox(ctx, token, args)
	case "sandbox_delete":
		return s.toolDeleteSandbox(ctx, token, args)
	case "read_file":
		return s.toolReadFile(ctx, token, args)
	case "write_file":
		return s.toolWriteFile(ctx, token, args)
	case "list_files":
		return s.toolListFiles(ctx, token, args)
	default:
		return "", &rpcError{Code: -32601, Message: "unknown tool: " + name}
	}
}

// --- tool implementations --------------------------------------------

func (s *Server) toolCreateSandbox(ctx context.Context, token string, args map[string]any) (string, *rpcError) {
	body := map[string]any{"image": args["image"]}
	if v, ok := args["project_id"].(string); ok && v != "" {
		body["project_id"] = v
	}
	if v, ok := args["ttl_seconds"].(float64); ok {
		body["ttl_seconds"] = int(v)
	}
	resources := map[string]any{}
	if v, ok := args["cpus"].(float64); ok {
		resources["cpus"] = v
	}
	if v, ok := args["memory_mb"].(float64); ok {
		resources["memory_mb"] = int(v)
	}
	if len(resources) > 0 {
		body["resources"] = resources
	}
	status, data, err := s.portalCall(ctx, token, http.MethodPost, "/v1/sandboxes", body)
	if err != nil {
		return "", portalCallErr(err)
	}
	if status != http.StatusCreated {
		return "", decodeError(status, data)
	}
	return string(data), nil
}

func (s *Server) toolExecSandbox(ctx context.Context, token string, args map[string]any) (string, *rpcError) {
	id, ok := args["sandbox_id"].(string)
	if !ok || id == "" {
		return "", &rpcError{Code: -32602, Message: "sandbox_id is required"}
	}
	cmdRaw, ok := args["cmd"]
	if !ok {
		return "", &rpcError{Code: -32602, Message: "cmd is required"}
	}
	cmd, err := toStringSlice(cmdRaw)
	if err != nil {
		return "", &rpcError{Code: -32602, Message: "cmd: " + err.Error()}
	}
	body := map[string]any{"cmd": cmd}
	if v, ok := args["cwd"].(string); ok {
		body["cwd"] = v
	}
	if v, ok := args["stdin"].(string); ok {
		body["stdin"] = v
	}
	if v, ok := args["timeout_s"].(float64); ok {
		body["timeout_s"] = int(v)
	}
	status, data, err := s.portalCall(ctx, token, http.MethodPost,
		"/v1/sandboxes/"+urlPathEscape(id)+"/exec", body)
	if err != nil {
		return "", portalCallErr(err)
	}
	if status != http.StatusOK {
		return "", decodeError(status, data)
	}
	return string(data), nil
}

func (s *Server) toolDeleteSandbox(ctx context.Context, token string, args map[string]any) (string, *rpcError) {
	id, ok := args["sandbox_id"].(string)
	if !ok || id == "" {
		return "", &rpcError{Code: -32602, Message: "sandbox_id is required"}
	}
	status, data, err := s.portalCall(ctx, token, http.MethodDelete,
		"/v1/sandboxes/"+urlPathEscape(id), nil)
	if err != nil {
		return "", portalCallErr(err)
	}
	if status != http.StatusNoContent {
		return "", decodeError(status, data)
	}
	return `{"deleted":` + fmt.Sprintf("%q", id) + `}`, nil
}

func (s *Server) toolReadFile(ctx context.Context, token string, args map[string]any) (string, *rpcError) {
	id, _ := args["sandbox_id"].(string)
	path, _ := args["path"].(string)
	if id == "" || path == "" {
		return "", &rpcError{Code: -32602, Message: "sandbox_id and path are required"}
	}
	q := url.Values{"path": {path}}
	status, data, err := s.portalCall(ctx, token, http.MethodGet,
		"/v1/sandboxes/"+urlPathEscape(id)+"/files/content?"+q.Encode(), nil)
	if err != nil {
		return "", portalCallErr(err)
	}
	if status != http.StatusOK {
		return "", decodeError(status, data)
	}
	return string(data), nil
}

func (s *Server) toolWriteFile(ctx context.Context, token string, args map[string]any) (string, *rpcError) {
	id, _ := args["sandbox_id"].(string)
	path, _ := args["path"].(string)
	content, _ := args["content"].(string)
	if id == "" || path == "" {
		return "", &rpcError{Code: -32602, Message: "sandbox_id and path are required"}
	}
	// Write_file sends raw bytes (application/octet-stream), not JSON.
	// We build the request manually so the body is the file content.
	body := []byte(content)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		strings.TrimRight(s.PortalURL, "/")+"/v1/sandboxes/"+urlPathEscape(id)+
			"/files/content?"+url.Values{"path": {path}}.Encode(),
		bytesReader(body))
	if err != nil {
		return "", portalCallErr(err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if token != "" {
		if strings.HasPrefix(token, "Bearer ") {
			req.Header.Set("Authorization", token)
		} else {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
	res, err := s.httpClient().Do(req)
	if err != nil {
		return "", portalCallErr(err)
	}
	defer res.Body.Close()
	data, _ := ioReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		return "", decodeError(res.StatusCode, data)
	}
	return string(data), nil
}

func (s *Server) toolListFiles(ctx context.Context, token string, args map[string]any) (string, *rpcError) {
	id, _ := args["sandbox_id"].(string)
	path, _ := args["path"].(string)
	if id == "" {
		return "", &rpcError{Code: -32602, Message: "sandbox_id is required"}
	}
	q := url.Values{}
	if path != "" {
		q.Set("path", path)
	}
	status, data, err := s.portalCall(ctx, token, http.MethodGet,
		"/v1/sandboxes/"+urlPathEscape(id)+"/files?"+q.Encode(), nil)
	if err != nil {
		return "", portalCallErr(err)
	}
	if status != http.StatusOK {
		return "", decodeError(status, data)
	}
	return string(data), nil
}

// --- helpers ----------------------------------------------------------

func rpcErr(id json.RawMessage, code int, msg string, data any) *rpcResponse {
	return &rpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &rpcError{Code: code, Message: msg, Data: data},
	}
}

func portalCallErr(err error) *rpcError {
	return &rpcError{Code: -32603, Message: "portal call failed: " + err.Error()}
}

// toStringSlice coerces a JSON-decoded cmd ([]any of strings) into a
// []string, rejecting non-string entries.
func toStringSlice(v any) ([]string, error) {
	arr, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("must be an array of strings")
	}
	out := make([]string, 0, len(arr))
	for i, e := range arr {
		s, ok := e.(string)
		if !ok {
			return nil, fmt.Errorf("entry %d is not a string", i)
		}
		out = append(out, s)
	}
	return out, nil
}
