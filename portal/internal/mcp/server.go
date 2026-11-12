// MCP server core (PORTAL_PLAN_V2 P6). The JSON-RPC 2.0 handler
// dispatches initialize / tools/list / tools/call. Tool calls forward
// to the Portal /v1 API via the embedded Portal client (or via HTTP
// to a separate Portal binary in production).
//
// The server is transport-agnostic: the caller wraps it in an
// http.Handler that reads JSON-RPC from the request body and writes
// the JSON-RPC response. SSE-style streaming responses (for
// notifications/progress) are out of scope for the first cut — every
// tool call returns one JSON-RPC response.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Server is the MCP JSON-RPC handler. One instance per Portal
// deployment; the Bearer token of the caller is read from each
// request's Authorization header and forwarded to the Portal API.
type Server struct {
	// PortalURL is the base URL of the Portal /v1 API (e.g.
	// https://chat.devhub.io.vn). The MCP server forwards every tool
	// call here as an HTTP request with the caller's Bearer token.
	PortalURL string
	// HTTP is the client used for upstream Portal calls. nil = default
	// with a 60s timeout (longer than a typical exec but shorter than
	// the 120s gateway cap; the caller can override for long streams).
	HTTP *http.Client
}

// Handle is the http.HandlerFunc that serves MCP JSON-RPC. It reads
// one JSON-RPC request object from the body and writes one response.
// Batch requests (an array) are not supported in the first cut.
func (s *Server) Handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeRPCError(w, nil, -32700, "parse error: "+err.Error())
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPCError(w, nil, -32700, "parse error: "+err.Error())
		return
	}
	// A notification (no id, e.g. notifications/initialized) gets no
	// JSON-RPC response; Streamable HTTP answers it with 202.
	if len(req.ID) == 0 || string(req.ID) == "null" {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	// The caller's Bearer token rides in the Authorization header;
	// forward it to every Portal API call.
	token := r.Header.Get("Authorization")
	resp := s.dispatch(r.Context(), token, &req)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// dispatch routes one JSON-RPC method. Returns the response object
// (never nil unless the request is a notification, in which case the
// caller should not write a body — but MCP tool calls are not
// notifications, so we always return a response).
func (s *Server) dispatch(ctx context.Context, token string, req *rpcRequest) *rpcResponse {
	switch req.Method {
	case "initialize":
		return &rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"protocolVersion": "2025-06-18",
				"serverInfo": map[string]any{
					"name":    "4aicoding-portal-mcp",
					"version": "0.1.0",
				},
				"capabilities": map[string]any{
					"tools": map[string]any{},
				},
			},
		}
	case "tools/list":
		return &rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"tools": allTools(),
			},
		}
	case "tools/call":
		return s.handleToolCall(ctx, token, req)
	case "ping":
		return &rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}}
	default:
		return &rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &rpcError{Code: -32601, Message: "method not found: " + req.Method},
		}
	}
}

// rpcRequest / rpcResponse / rpcError are the JSON-RPC 2.0 envelope.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	resp := rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// httpClient returns the upstream client (default 60s timeout).
func (s *Server) httpClient() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Timeout: 60 * time.Second}
}

// portalCall forwards one request to the Portal /v1 API and returns
// the response body + status. The caller's Bearer token is sent as
// Authorization; the body is JSON-encoded if non-nil.
func (s *Server) portalCall(ctx context.Context, token, method, path string, body any) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("encode body: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method,
		strings.TrimRight(s.PortalURL, "/")+path, rdr)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		// Forward verbatim — token may be "Bearer sbx_live_…" or just
		// the key; normalise to "Bearer …".
		if strings.HasPrefix(token, "Bearer ") || token == "" {
			req.Header.Set("Authorization", token)
		} else {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
	res, err := s.httpClient().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	return res.StatusCode, data, nil
}

// decodeError turns a Portal error envelope into an MCP error result.
func decodeError(status int, body []byte) *rpcError {
	var env struct {
		Error struct {
			Code, Message string
			Field         string
			Limit, Used   any
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err == nil && env.Error.Code != "" {
		return &rpcError{
			Code:    status,
			Message: env.Error.Code + ": " + env.Error.Message,
			Data:    env.Error,
		}
	}
	return &rpcError{Code: status, Message: strings.TrimSpace(string(body))}
}

// urlPathEscape is a tiny wrapper to keep call sites readable.
func urlPathEscape(s string) string { return url.PathEscape(s) }
