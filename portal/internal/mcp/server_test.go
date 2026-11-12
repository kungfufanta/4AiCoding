// MCP server tests. The fake Portal is an httptest.Server that
// records the Authorization header and request body of each call so
// tests can assert the MCP server forwarded the caller's token and
// the right arguments.
package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakePortal records the last request it received and returns a
// canned response. Each handler sets lastAuth, lastMethod, lastPath,
// lastBody so tests can assert the MCP server forwarded correctly.
type fakePortal struct {
	lastAuth   string
	lastMethod string
	lastPath   string
	lastBody   string
	// Canned responses keyed by path prefix.
	responses map[string]fakeResponse
}

type fakeResponse struct {
	status int
	body   string
}

func (f *fakePortal) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.lastAuth = r.Header.Get("Authorization")
		f.lastMethod = r.Method
		f.lastPath = r.URL.Path
		if r.URL.RawQuery != "" {
			f.lastPath += "?" + r.URL.RawQuery
		}
		if r.Body != nil {
			b, _ := io.ReadAll(r.Body)
			f.lastBody = string(b)
		}
		// Find the canned response: longest matching prefix.
		var best string
		for k := range f.responses {
			if strings.HasPrefix(r.URL.Path, k) && len(k) > len(best) {
				best = k
			}
		}
		if best == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		resp := f.responses[best]
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.status)
		_, _ = w.Write([]byte(resp.body))
	}
}

func newFakePortal(t *testing.T) (*fakePortal, *httptest.Server) {
	t.Helper()
	fp := &fakePortal{responses: map[string]fakeResponse{}}
	srv := httptest.NewServer(fp.handler())
	t.Cleanup(srv.Close)
	return fp, srv
}

// callMCP sends one JSON-RPC request to the MCP server and returns the
// response. The Bearer token is set on the request so tests can
// verify it is forwarded.
func callMCP(t *testing.T, s *Server, token, method string, params any) *rpcResponse {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  params,
	})
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(string(body)))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.Handle(rec, req)
	var resp rpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode rpc response: %v (body=%s)", err, rec.Body.String())
	}
	return &resp
}

func TestInitialize(t *testing.T) {
	s := &Server{PortalURL: "http://unused"}
	resp := callMCP(t, s, "key_abc", "initialize", map[string]any{})
	if resp.Error != nil {
		t.Fatalf("initialize error: %+v", resp.Error)
	}
	result, _ := resp.Result.(map[string]any)
	if result["protocolVersion"] != "2025-06-18" {
		t.Errorf("protocolVersion = %v", result["protocolVersion"])
	}
}

func TestToolsList(t *testing.T) {
	s := &Server{PortalURL: "http://unused"}
	resp := callMCP(t, s, "key_abc", "tools/list", map[string]any{})
	if resp.Error != nil {
		t.Fatalf("tools/list error: %+v", resp.Error)
	}
	result := resp.Result.(map[string]any)
	tools := result["tools"].([]any)
	names := map[string]bool{}
	for _, t := range tools {
		tool := t.(map[string]any)
		names[tool["name"].(string)] = true
	}
	for _, want := range []string{"sandbox_create", "sandbox_exec", "sandbox_delete", "read_file", "write_file", "list_files"} {
		if !names[want] {
			t.Errorf("missing tool %q in tools/list", want)
		}
	}
}

func TestSandboxCreateForwardsTokenAndBody(t *testing.T) {
	fp, srv := newFakePortal(t)
	fp.responses["/v1/sandboxes"] = fakeResponse{
		status: http.StatusCreated,
		body:   `{"id":"01HMCP0000000000000000001","status":"pending","image":"ubuntu:24.04","resources":{"cpus":1,"memory_mb":512},"ports":[],"egress":[],"created_at":"2026-11-12T00:00:00Z"}`,
	}
	s := &Server{PortalURL: srv.URL, HTTP: srv.Client()}
	resp := callMCP(t, s, "sbx_live_abc", "tools/call", map[string]any{
		"name":      "sandbox_create",
		"arguments": map[string]any{"image": "ubuntu:24.04", "cpus": 1.0, "memory_mb": 512.0},
	})
	if resp.Error != nil {
		t.Fatalf("sandbox_create error: %+v", resp.Error)
	}
	if fp.lastAuth != "Bearer sbx_live_abc" {
		t.Errorf("forwarded Authorization = %q, want 'Bearer sbx_live_abc'", fp.lastAuth)
	}
	if fp.lastMethod != http.MethodPost {
		t.Errorf("forwarded method = %q, want POST", fp.lastMethod)
	}
	if !strings.Contains(fp.lastBody, `"image":"ubuntu:24.04"`) {
		t.Errorf("forwarded body missing image: %s", fp.lastBody)
	}
	if !strings.Contains(fp.lastBody, `"cpus":1`) {
		t.Errorf("forwarded body missing cpus: %s", fp.lastBody)
	}
	// Result is the Portal response wrapped as MCP content.
	result := resp.Result.(map[string]any)
	content := result["content"].([]any)
	block := content[0].(map[string]any)
	text := block["text"].(string)
	if !strings.Contains(text, "01HMCP0000000000000000001") {
		t.Errorf("result text missing sandbox id: %s", text)
	}
}

func TestSandboxExecForwardsCmd(t *testing.T) {
	fp, srv := newFakePortal(t)
	fp.responses["/v1/sandboxes/"] = fakeResponse{
		status: http.StatusOK,
		body:   `{"stdout":"hello\n","stderr":"","exit_code":0,"timed_out":false,"truncated":false,"duration_ms":3}`,
	}
	s := &Server{PortalURL: srv.URL, HTTP: srv.Client()}
	resp := callMCP(t, s, "key", "tools/call", map[string]any{
		"name": "sandbox_exec",
		"arguments": map[string]any{
			"sandbox_id": "01HMCP0000000000000000001",
			"cmd":        []any{"echo", "hello"},
			"stdin":      "input-data",
		},
	})
	if resp.Error != nil {
		t.Fatalf("exec error: %+v", resp.Error)
	}
	if !strings.Contains(fp.lastPath, "/v1/sandboxes/01HMCP0000000000000000001/exec") {
		t.Errorf("forwarded path = %q", fp.lastPath)
	}
	if !strings.Contains(fp.lastBody, `"cmd":["echo","hello"]`) {
		t.Errorf("forwarded body missing cmd: %s", fp.lastBody)
	}
	if !strings.Contains(fp.lastBody, `"stdin":"input-data"`) {
		t.Errorf("forwarded body missing stdin: %s", fp.lastBody)
	}
}

func TestReadFileForwardsPath(t *testing.T) {
	fp, srv := newFakePortal(t)
	fp.responses["/v1/sandboxes/"] = fakeResponse{
		status: http.StatusOK,
		body:   `{"path":"hello.txt","size":5,"offset":0,"encoding":"utf-8","data":"hello","truncated":false}`,
	}
	s := &Server{PortalURL: srv.URL, HTTP: srv.Client()}
	resp := callMCP(t, s, "key", "tools/call", map[string]any{
		"name": "read_file",
		"arguments": map[string]any{
			"sandbox_id": "01HMCP0000000000000000001",
			"path":       "hello.txt",
		},
	})
	if resp.Error != nil {
		t.Fatalf("read_file error: %+v", resp.Error)
	}
	if !strings.Contains(fp.lastPath, "/files/content?path=hello.txt") {
		t.Errorf("forwarded path missing ?path=hello.txt: %s", fp.lastPath)
	}
}

func TestWriteFileSendsRawBytes(t *testing.T) {
	fp, srv := newFakePortal(t)
	fp.responses["/v1/sandboxes/"] = fakeResponse{
		status: http.StatusOK,
		body:   `{"path":"hello.txt","size":11,"sha256":"deadbeef"}`,
	}
	s := &Server{PortalURL: srv.URL, HTTP: srv.Client()}
	resp := callMCP(t, s, "key", "tools/call", map[string]any{
		"name": "write_file",
		"arguments": map[string]any{
			"sandbox_id": "01HMCP0000000000000000001",
			"path":       "hello.txt",
			"content":    "hello world",
		},
	})
	if resp.Error != nil {
		t.Fatalf("write_file error: %+v", resp.Error)
	}
	// The body sent to the Portal must be the raw file content, NOT
	// a JSON wrapper (sandboxd's A0 files API expects raw bytes).
	if fp.lastBody != "hello world" {
		t.Errorf("forwarded body = %q, want 'hello world' (raw bytes, not JSON)", fp.lastBody)
	}
}

func TestDeleteSandbox(t *testing.T) {
	fp, srv := newFakePortal(t)
	fp.responses["/v1/sandboxes/"] = fakeResponse{status: http.StatusNoContent, body: ""}
	s := &Server{PortalURL: srv.URL, HTTP: srv.Client()}
	resp := callMCP(t, s, "key", "tools/call", map[string]any{
		"name":      "sandbox_delete",
		"arguments": map[string]any{"sandbox_id": "01HMCP0000000000000000001"},
	})
	if resp.Error != nil {
		t.Fatalf("delete error: %+v", resp.Error)
	}
	if fp.lastMethod != http.MethodDelete {
		t.Errorf("forwarded method = %q, want DELETE", fp.lastMethod)
	}
}

func TestListFiles(t *testing.T) {
	fp, srv := newFakePortal(t)
	fp.responses["/v1/sandboxes/"] = fakeResponse{
		status: http.StatusOK,
		body:   `{"path":".","entries":[{"path":"hello.txt","name":"hello.txt","size":5,"mode":"-rw-r--r--","is_dir":false}],"truncated":false}`,
	}
	s := &Server{PortalURL: srv.URL, HTTP: srv.Client()}
	resp := callMCP(t, s, "key", "tools/call", map[string]any{
		"name":      "list_files",
		"arguments": map[string]any{"sandbox_id": "01HMCP0000000000000000001", "path": "."},
	})
	if resp.Error != nil {
		t.Fatalf("list_files error: %+v", resp.Error)
	}
	if !strings.Contains(fp.lastPath, "/files?path=.") {
		t.Errorf("forwarded path missing ?path=.: %s", fp.lastPath)
	}
}

func TestPortalErrorPropagates(t *testing.T) {
	fp, srv := newFakePortal(t)
	fp.responses["/v1/sandboxes"] = fakeResponse{
		status: http.StatusTooManyRequests,
		body:   `{"error":{"code":"quota_exceeded","message":"org has reached its concurrent sandbox quota"}}`,
	}
	s := &Server{PortalURL: srv.URL, HTTP: srv.Client()}
	resp := callMCP(t, s, "key", "tools/call", map[string]any{
		"name":      "sandbox_create",
		"arguments": map[string]any{"image": "ubuntu:24.04"},
	})
	// A Portal refusal is a tool execution error: a result with
	// isError, so the model sees the message (MCP tools spec).
	if resp.Error != nil {
		t.Fatalf("got JSON-RPC error, want isError result: %+v", resp.Error)
	}
	res, _ := json.Marshal(resp.Result)
	if !strings.Contains(string(res), `"isError":true`) {
		t.Errorf("result = %s, want isError true", res)
	}
	if !strings.Contains(string(res), "HTTP 429") || !strings.Contains(string(res), "quota_exceeded") {
		t.Errorf("result = %s, want HTTP 429 and quota_exceeded", res)
	}
}

func TestNotificationGetsNoBody(t *testing.T) {
	s := &Server{PortalURL: "http://unused"}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	rec := httptest.NewRecorder()
	s.Handle(rec, req)
	if rec.Code != http.StatusAccepted || rec.Body.Len() != 0 {
		t.Errorf("notification: status %d body %q, want 202 with no body", rec.Code, rec.Body.String())
	}
}

func TestUnknownToolReturnsError(t *testing.T) {
	s := &Server{PortalURL: "http://unused"}
	resp := callMCP(t, s, "key", "tools/call", map[string]any{
		"name":      "nonexistent_tool",
		"arguments": map[string]any{},
	})
	if resp.Error == nil {
		t.Fatalf("expected error for unknown tool")
	}
	if resp.Error.Code != -32601 {
		t.Errorf("error code = %d, want -32601", resp.Error.Code)
	}
}

// keep imports honest
var _ = context.Background
