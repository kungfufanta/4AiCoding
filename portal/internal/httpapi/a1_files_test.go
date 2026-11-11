// A1 gateway tests. Real Postgres via PORTAL_TEST_DATABASE_URL (the
// same gate as the rest of the portal httpapi tests). The fake
// sandboxd grows a files surface so the gateway can proxy it.
package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tastyeffectco/sandboxd/portal/internal/apikeys"
	"github.com/tastyeffectco/sandboxd/portal/internal/db"
	"github.com/tastyeffectco/sandboxd/portal/internal/sandboxd"
)

// filesFakeGateway extends fakeGateway with the 5 files routes.
// Files writes are recorded so tests can assert the Portal forwarded
// the body byte-for-byte (not JSON-wrapped).
type filesFakeGateway struct {
	*fakeGateway
	putBody     []byte
	putPath     string
	moveBody    []byte
	listCalls   int
	readCalls   int
	deleteCalls int
	execCalls   int
}

func newFilesFakeGateway() *filesFakeGateway {
	return &filesFakeGateway{fakeGateway: &fakeGateway{}}
}

func (f *filesFakeGateway) handler() http.HandlerFunc {
	base := f.fakeGateway.handler()
	return func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/files"):
			f.listCalls++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"path":".","entries":[{"path":"hello.txt","name":"hello.txt","size":5,"mode":"-rw-r--r--","is_dir":false}],"truncated":false}`))
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/files/content"):
			f.readCalls++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"path":"hello.txt","size":5,"offset":0,"encoding":"utf-8","data":"hello","truncated":false}`))
		case r.Method == http.MethodPut && strings.HasSuffix(path, "/files/content"):
			body, _ := io.ReadAll(r.Body)
			f.putBody = body
			f.putPath = r.URL.Query().Get("path")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"path":"hello.txt","size":` + itoa(len(body)) + `,"sha256":"deadbeef"}`))
		case r.Method == http.MethodDelete && strings.HasSuffix(path, "/files"):
			f.deleteCalls++
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/files/move"):
			body, _ := io.ReadAll(r.Body)
			f.moveBody = body
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"from":"a.txt","to":"b.txt"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/exec"):
			f.execCalls++
			if r.URL.Query().Get("stream") == "ndjson" {
				w.Header().Set("Content-Type", "application/x-ndjson")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"stream":"stdout","data":"hello"}` + "\n"))
				_, _ = w.Write([]byte(`{"exit_code":0,"timed_out":false,"truncated":false,"duration_ms":3}` + "\n"))
				return
			}
			bodyBytes, _ := io.ReadAll(r.Body)
			f.putBody = bodyBytes // reused for debug inspection
			var body map[string]any
			_ = json.Unmarshal(bodyBytes, &body)
			out := map[string]any{
				"stdout":      "hello-gateway",
				"stderr":      "",
				"exit_code":   0,
				"timed_out":   false,
				"truncated":   false,
				"duration_ms": 5,
				"debug_body":  string(bodyBytes),
			}
			if _, ok := body["timeout_s"]; ok {
				out["stdin_received"] = body["stdin"]
				out["timeout_s"] = body["timeout_s"]
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(out)
		default:
			base(w, r)
		}
	}
}

// setupA1 mirrors setupGateway with the files-aware fake.
func setupA1(t *testing.T) (*Server, *filesFakeGateway) {
	t.Helper()
	srv := setupBFF(t) // skips without PORTAL_TEST_DATABASE_URL
	fake := newFilesFakeGateway()
	sbx := httptest.NewServer(fake.handler())
	t.Cleanup(sbx.Close)
	srv.Sandboxd = sandboxd.New(sbx.URL, "gateway-key", 5*time.Second)
	srv.Keys = &apikeys.Verifier{Pool: srv.Pool, Log: discardLogger()}
	srv.WebhookSecret = "test-webhook-secret"
	// Clean the shared tables so a prior run cannot turn fresh events
	// into duplicates. Same pattern as setupGateway.
	ctx := context.Background()
	url := os.Getenv("PORTAL_TEST_DATABASE_URL")
	admin, err := db.Open(ctx, discardLogger(), url, "")
	if err != nil {
		t.Fatalf("open admin for cleanup: %v", err)
	}
	defer admin.Close()
	for _, table := range []string{"webhook_events", "idempotency_keys", "sandboxes"} {
		if _, err := admin.Exec(ctx, "DELETE FROM "+table); err != nil {
			t.Fatalf("clean %s: %v", table, err)
		}
	}
	return srv, fake
}

// newOrgWithFilesKey creates an org + API key with the
// sandboxes:files scope (plus the sandbox:create/exec/read scopes the
// exec tests need).
func newOrgWithFilesKey(t *testing.T, srv *Server, sub, email string) (orgID, projectID, key string, cookies []*http.Cookie) {
	t.Helper()
	_, sessionCookie, csrf := login(t, srv, sub, email, sub)
	cookies = []*http.Cookie{sessionCookie, csrf}
	rec := do(t, srv, http.MethodPost, "/api/orgs", `{"name":"`+sub+` org"}`, cookies...)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create org = %d: %s", rec.Code, rec.Body.String())
	}
	orgID = extractString(t, rec.Body.String(), "id")
	rec = do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/projects", `{"name":"proj"}`, cookies...)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create project = %d: %s", rec.Code, rec.Body.String())
	}
	projectID = extractString(t, rec.Body.String(), "id")
	rec = do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/api-keys",
		`{"name":"a1","scopes":["sandbox:create","sandbox:read","sandbox:exec","sandboxes:files"]}`,
		cookies...)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create api key = %d: %s", rec.Code, rec.Body.String())
	}
	key = extractString(t, rec.Body.String(), "secret")
	return orgID, projectID, key, cookies
}

// seedSandboxRow inserts an ownership row so keyOwnsSandbox passes.
// The fake gateway returns 201 on create, so the natural create path
// already inserts one; this helper is for tests that skip create.
func seedSandboxRow(t *testing.T, srv *Server, orgID, id string) {
	t.Helper()
	ctx := context.Background()
	if err := db.WithOrg(ctx, srv.Pool, orgID, func(tx db.Tx) error {
		_, err := tx.Exec(ctx, `
                        INSERT INTO sandboxes
                                (id, org_id, status, image, vcpu, memory_mb, ports, created_at, updated_at)
                        VALUES ($1, $2::uuid, 'running', 'ubuntu:24.04', 1.0, 512, '{}', now(), now())`,
			id, orgID)
		return err
	}); err != nil {
		t.Fatalf("seed sandbox: %v", err)
	}
}

// TestFilesGatewayListAndRead — GET /files and /files/content proxy
// the sandboxd response verbatim and increment the right counters.
func TestFilesGatewayListAndRead(t *testing.T) {
	srv, fake := setupA1(t)
	orgID, _, key, _ := newOrgWithFilesKey(t, srv, "files-alice", "files-alice@example.test")
	seedSandboxRow(t, srv, orgID, "01FILES000000000000000001")

	r := doV1(t, srv, http.MethodGet, "/v1/sandboxes/01FILES000000000000000001/files?path=.", "", key)
	if r.Code != http.StatusOK {
		t.Fatalf("GET /files = %d, want 200; body=%s", r.Code, r.Body.String())
	}
	if fake.listCalls != 1 {
		t.Errorf("listCalls = %d, want 1 (gateway must proxy to sandboxd)", fake.listCalls)
	}
	if !strings.Contains(r.Body.String(), `"hello.txt"`) {
		t.Errorf("list response missing hello.txt entry: %s", r.Body.String())
	}

	r = doV1(t, srv, http.MethodGet, "/v1/sandboxes/01FILES000000000000000001/files/content?path=hello.txt", "", key)
	if r.Code != http.StatusOK {
		t.Fatalf("GET /files/content = %d, want 200; body=%s", r.Code, r.Body.String())
	}
	if fake.readCalls != 1 {
		t.Errorf("readCalls = %d, want 1", fake.readCalls)
	}
	if !strings.Contains(r.Body.String(), `"data":"hello"`) {
		t.Errorf("content response missing data=hello: %s", r.Body.String())
	}
}

// TestFilesGatewayPutForwardsRawBody — the Portal must NOT JSON-wrap
// the PUT body; sandboxd's A0 expects raw bytes. The fake records the
// forwarded body, so we assert it matches the request body verbatim.
func TestFilesGatewayPutForwardsRawBody(t *testing.T) {
	srv, fake := setupA1(t)
	orgID, _, key, _ := newOrgWithFilesKey(t, srv, "put-alice", "put-alice@example.test")
	seedSandboxRow(t, srv, orgID, "01PUT0000000000000000001")

	body := "raw file bytes, not JSON"
	r := doV1(t, srv, http.MethodPut,
		"/v1/sandboxes/01PUT0000000000000000001/files/content?path=hello.txt", body, key)
	if r.Code != http.StatusOK {
		t.Fatalf("PUT /files/content = %d, want 200; body=%s", r.Code, r.Body.String())
	}
	if string(fake.putBody) != body {
		t.Errorf("forwarded body = %q, want %q (Portal must not JSON-wrap)", string(fake.putBody), body)
	}
	if fake.putPath != "hello.txt" {
		t.Errorf("forwarded path = %q, want hello.txt", fake.putPath)
	}
}

// TestFilesGatewayDeleteAndMove — DELETE removes the file and POST
// /files/move forwards the JSON body verbatim.
func TestFilesGatewayDeleteAndMove(t *testing.T) {
	srv, fake := setupA1(t)
	orgID, _, key, _ := newOrgWithFilesKey(t, srv, "delmove-alice", "delmove-alice@example.test")
	seedSandboxRow(t, srv, orgID, "01DELMOVE0000000000000001")

	r := doV1(t, srv, http.MethodDelete, "/v1/sandboxes/01DELMOVE0000000000000001/files?path=hello.txt", "", key)
	if r.Code != http.StatusNoContent {
		t.Fatalf("DELETE /files = %d, want 204; body=%s", r.Code, r.Body.String())
	}
	if fake.deleteCalls != 1 {
		t.Errorf("deleteCalls = %d, want 1", fake.deleteCalls)
	}

	moveBody := `{"from":"a.txt","to":"b.txt"}`
	r = doV1(t, srv, http.MethodPost, "/v1/sandboxes/01DELMOVE0000000000000001/files/move", moveBody, key)
	if r.Code != http.StatusOK {
		t.Fatalf("POST /files/move = %d, want 200; body=%s", r.Code, r.Body.String())
	}
	if string(fake.moveBody) != moveBody {
		t.Errorf("forwarded move body = %q, want %q", string(fake.moveBody), moveBody)
	}
}

// TestFilesGatewayCrossOrgReturns404 — the headline A1 regression: a
// key for org B cannot read files of a sandbox owned by org A. The
// Portal returns 404 (not 403) to avoid revealing the sandbox exists.
func TestFilesGatewayCrossOrgReturns404(t *testing.T) {
	srv, _ := setupA1(t)
	orgA, _, _, _ := newOrgWithFilesKey(t, srv, "crossfiles-a", "crossfiles-a@example.test")
	_, _, keyB, _ := newOrgWithFilesKey(t, srv, "crossfiles-b", "crossfiles-b@example.test")
	seedSandboxRow(t, srv, orgA, "01CROSSFILES00000000000001")

	// keyB trying to read orgA's sandbox: 404.
	r := doV1(t, srv, http.MethodGet,
		"/v1/sandboxes/01CROSSFILES00000000000001/files?path=.", "", keyB)
	if r.Code != http.StatusNotFound {
		t.Fatalf("cross-org GET /files = %d, want 404; body=%s", r.Code, r.Body.String())
	}
	// And PUT, DELETE, move too.
	r = doV1(t, srv, http.MethodPut,
		"/v1/sandboxes/01CROSSFILES00000000000001/files/content?path=x", "x", keyB)
	if r.Code != http.StatusNotFound {
		t.Errorf("cross-org PUT /files = %d, want 404", r.Code)
	}
	r = doV1(t, srv, http.MethodDelete,
		"/v1/sandboxes/01CROSSFILES00000000000001/files?path=x", "", keyB)
	if r.Code != http.StatusNotFound {
		t.Errorf("cross-org DELETE /files = %d, want 404", r.Code)
	}
	r = doV1(t, srv, http.MethodPost,
		"/v1/sandboxes/01CROSSFILES00000000000001/files/move", `{"from":"a","to":"b"}`, keyB)
	if r.Code != http.StatusNotFound {
		t.Errorf("cross-org POST /files/move = %d, want 404", r.Code)
	}
}

// TestFilesGatewayScopeGate — a key with sandbox:exec but NOT
// sandboxes:files gets 403 scope_exceeded on every files route.
func TestFilesGatewayScopeGate(t *testing.T) {
	srv, _ := setupA1(t)
	orgID, _, _, cookies := newOrgWithFilesKey(t, srv, "scope-gate", "scope-gate@example.test")
	// Mint a key with only sandbox:exec (no sandboxes:files).
	rec := do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/api-keys",
		`{"name":"exec-only","scopes":["sandbox:exec"]}`, cookies...)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create exec-only key = %d: %s", rec.Code, rec.Body.String())
	}
	execOnlyKey := extractString(t, rec.Body.String(), "secret")
	seedSandboxRow(t, srv, orgID, "01SCOPEGATE00000000000001")

	for _, tt := range []struct {
		method, path string
		body         string
	}{
		{http.MethodGet, "/v1/sandboxes/01SCOPEGATE00000000000001/files", ""},
		{http.MethodGet, "/v1/sandboxes/01SCOPEGATE00000000000001/files/content?path=x", ""},
		{http.MethodPut, "/v1/sandboxes/01SCOPEGATE00000000000001/files/content?path=x", "x"},
		{http.MethodDelete, "/v1/sandboxes/01SCOPEGATE00000000000001/files?path=x", ""},
		{http.MethodPost, "/v1/sandboxes/01SCOPEGATE00000000000001/files/move", `{"from":"a","to":"b"}`},
	} {
		r := doV1(t, srv, tt.method, tt.path, tt.body, execOnlyKey)
		if r.Code != http.StatusForbidden {
			t.Errorf("%s %s = %d, want 403 (key lacks sandboxes:files)", tt.method, tt.path, r.Code)
		}
	}
}

// TestExecV2ForwardsFields — POST /v1/sandboxes/{id}/exec forwards the
// v2 fields (timeout_s, cwd, env, stdin, max_output_bytes) to
// sandboxd. The fake records the body and we assert each field arrives.
func TestExecV2ForwardsFields(t *testing.T) {
	srv, fake := setupA1(t)
	orgID, _, key, _ := newOrgWithFilesKey(t, srv, "execv2-alice", "execv2-alice@example.test")
	seedSandboxRow(t, srv, orgID, "01EXECV200000000000000001")

	body := `{"cmd":["bash","-lc","cat"],"cwd":"/home/sandbox","env":{"CI":"1"},"stdin":"hello","timeout_s":30,"max_output_bytes":1024}`
	r := doV1(t, srv, http.MethodPost, "/v1/sandboxes/01EXECV200000000000000001/exec", body, key)
	if r.Code != http.StatusOK {
		t.Fatalf("exec v2 = %d, want 200; body=%s", r.Code, r.Body.String())
	}
	// The gateway forwards the request to sandboxd; the request body
	// the fake sees is what sandboxd.Client.Exec built. Assert it
	// carries the v2 fields (the gateway did not strip them).
	// ExecResult is decoded by sandboxd.Client.Exec and re-marshalled
	// by the gateway, so the response shape is the standard exec
	// result — we cannot inspect the forwarded body through the
	// response. Instead, the fake recorded what it received.
	var forwarded map[string]any
	if err := json.Unmarshal(fake.putBody, &forwarded); err != nil {
		t.Fatalf("decode forwarded body: %v (raw=%q)", err, string(fake.putBody))
	}
	if forwarded["stdin"] != "hello" {
		t.Errorf("forwarded stdin = %v, want hello (body=%s)", forwarded["stdin"], string(fake.putBody))
	}
	if v, ok := forwarded["timeout_s"].(float64); !ok || v != 30 {
		t.Errorf("forwarded timeout_s = %v, want 30 (body=%s)", forwarded["timeout_s"], string(fake.putBody))
	}
	if forwarded["cwd"] != "/home/sandbox" {
		t.Errorf("forwarded cwd = %v, want /home/sandbox", forwarded["cwd"])
	}
	if env, ok := forwarded["env"].(map[string]any); !ok || env["CI"] != "1" {
		t.Errorf("forwarded env.CI = %v, want 1", forwarded["env"])
	}
	if v, ok := forwarded["max_output_bytes"].(float64); !ok || v != 1024 {
		t.Errorf("forwarded max_output_bytes = %v, want 1024", forwarded["max_output_bytes"])
	}
	_ = fake
}

// TestExecV2StreamRelaysNDJSON — ?stream=ndjson makes the gateway
// relay the NDJSON stream line-by-line (not buffer it).
func TestExecV2StreamRelaysNDJSON(t *testing.T) {
	srv, _ := setupA1(t)
	orgID, _, key, _ := newOrgWithFilesKey(t, srv, "execstream-alice", "execstream-alice@example.test")
	seedSandboxRow(t, srv, orgID, "01EXECSTREAM00000000000001")

	body := `{"cmd":["bash","-lc","echo hello"],"timeout_s":5}`
	r := doV1(t, srv, http.MethodPost,
		"/v1/sandboxes/01EXECSTREAM00000000000001/exec?stream=ndjson", body, key)
	if r.Code != http.StatusOK {
		t.Fatalf("exec stream = %d, want 200; body=%s", r.Code, r.Body.String())
	}
	if ct := r.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Errorf("Content-Type = %q, want application/x-ndjson", ct)
	}
	// Two NDJSON lines: stdout + final exit.
	lines := strings.Split(strings.TrimSpace(r.Body.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected at least 2 NDJSON lines, got %d: %q", len(lines), r.Body.String())
	}
	if !strings.Contains(lines[0], `"stream":"stdout"`) {
		t.Errorf("first line missing stream=stdout: %s", lines[0])
	}
	if !strings.Contains(lines[len(lines)-1], `"exit_code":0`) {
		t.Errorf("last line missing exit_code=0: %s", lines[len(lines)-1])
	}
}

// TestExecV2GatewayClampsTimeout — the gateway clamps timeout_s to
// maxExecSeconds (120) regardless of what the caller sends.
func TestExecV2GatewayClampsTimeout(t *testing.T) {
	srv, fake := setupA1(t)
	orgID, _, key, _ := newOrgWithFilesKey(t, srv, "execclamp-alice", "execclamp-alice@example.test")
	seedSandboxRow(t, srv, orgID, "01EXECCLAMP00000000000001")

	// Request a 9999s timeout; the gateway must clamp to maxExecSeconds.
	body := `{"cmd":["true"],"timeout_s":9999}`
	r := doV1(t, srv, http.MethodPost, "/v1/sandboxes/01EXECCLAMP00000000000001/exec", body, key)
	if r.Code != http.StatusOK {
		t.Fatalf("exec clamp = %d, want 200; body=%s", r.Code, r.Body.String())
	}
	// The fake recorded what sandboxd received: timeout_s must be 120,
	// not 9999 (the gateway clamped before forwarding).
	var forwarded map[string]any
	if err := json.Unmarshal(fake.putBody, &forwarded); err != nil {
		t.Fatalf("decode forwarded body: %v (raw=%q)", err, string(fake.putBody))
	}
	if v, ok := forwarded["timeout_s"].(float64); !ok || v != float64(maxExecSeconds) {
		t.Errorf("clamped timeout_s = %v, want %d (body=%s)",
			forwarded["timeout_s"], maxExecSeconds, string(fake.putBody))
	}
}

// TestExecV2LegacyCmdStillWorks — a bare {"cmd":[...]} request (no v2
// fields) still works, preserving the P4 contract.
func TestExecV2LegacyCmdStillWorks(t *testing.T) {
	srv, _ := setupA1(t)
	orgID, _, key, _ := newOrgWithFilesKey(t, srv, "execlegacy-alice", "execlegacy-alice@example.test")
	seedSandboxRow(t, srv, orgID, "01EXECLEGACY00000000000001")

	r := doV1(t, srv, http.MethodPost,
		"/v1/sandboxes/01EXECLEGACY00000000000001/exec", `{"cmd":["echo","hi"]}`, key)
	if r.Code != http.StatusOK {
		t.Fatalf("legacy exec = %d, want 200; body=%s", r.Code, r.Body.String())
	}
}

// --- helpers ------------------------------------------------------------

// doV1 sends a request to the public /v1 surface with a Bearer key.
func doV1(t *testing.T, srv *Server, method, target, body, apiKey string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rdr)
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}
