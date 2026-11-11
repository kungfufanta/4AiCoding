package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tastyeffectco/sandboxd/portal/internal/apikeys"
	"github.com/tastyeffectco/sandboxd/portal/internal/db"
	"github.com/tastyeffectco/sandboxd/portal/internal/sandboxd"
)

// fakeGateway is a stub sandboxd: enough v1 surface for the gateway
// tests, plus counters and captured payloads for assertions.
type fakeGateway struct {
	mu         sync.Mutex
	creates    int
	lastID     string
	lastCreate map[string]any
	execBody   map[string]any
	deleted    []string
}

func (f *fakeGateway) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		path := r.URL.Path
		switch {
		case r.Method == http.MethodPost && path == "/v1/sandboxes":
			f.creates++
			f.lastID = fmt.Sprintf("01GW%08X", f.creates)
			f.lastCreate = map[string]any{}
			_ = json.NewDecoder(r.Body).Decode(&f.lastCreate)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"id":%q,"status":"running","image":"ubuntu:24.04",
                                "resources":{"cpus":1,"memory_mb":512},"ports":[],"egress":[],"egress_mode":"allowlist",
                                "created_at":"2026-11-11T00:00:00Z"}`, f.lastID)
		case r.Method == http.MethodGet && path == "/v1/sandboxes":
			fmt.Fprintf(w, `{"sandboxes":[{"id":%q,"status":"running","image":"ubuntu:24.04",
                                "resources":{"cpus":1,"memory_mb":512},"ports":[],"egress":[],"egress_mode":"allowlist",
                                "external":{"workspace_id":"ws"},"created_at":"2026-11-11T00:00:00Z"}]}`, f.lastID)
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/v1/sandboxes/") && !strings.Contains(path, "/exec") && !strings.HasSuffix(path, "/logs"):
			id := strings.TrimPrefix(path, "/v1/sandboxes/")
			fmt.Fprintf(w, `{"id":%q,"status":"running","image":"ubuntu:24.04",
                                "resources":{"cpus":1,"memory_mb":512},"ports":[3000],"egress":[],"egress_mode":"allowlist",
                                "created_at":"2026-11-11T00:00:00Z"}`, id)
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/exec"):
			f.execBody = map[string]any{}
			_ = json.NewDecoder(r.Body).Decode(&f.execBody)
			fmt.Fprint(w, `{"stdout":"hello-gateway","stderr":"","exit_code":0,
                                "timed_out":false,"truncated":false,"duration_ms":5}`)
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/logs"):
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprint(w, "fake-log-line-1\nfake-log-line-2\n")
		case r.Method == http.MethodDelete && strings.HasPrefix(path, "/v1/sandboxes/"):
			f.deleted = append(f.deleted, strings.TrimPrefix(path, "/v1/sandboxes/"))
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost &&
			(strings.HasSuffix(path, "/start") || strings.HasSuffix(path, "/stop")):
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"error":{"code":"not_found","message":"no such sandbox"}}`)
		}
	}
}

// setupGateway extends setupBFF with the fake sandboxd, the key
// verifier and the webhook secret.
func setupGateway(t *testing.T) (*Server, *fakeGateway) {
	t.Helper()
	srv := setupBFF(t)                                      // skips itself without PORTAL_TEST_DATABASE_URL
	fake := &fakeGateway{}
	sbx := httptest.NewServer(fake.handler())
	t.Cleanup(sbx.Close)
	srv.Sandboxd = sandboxd.New(sbx.URL, "gateway-key", 5*time.Second)
	srv.Keys = &apikeys.Verifier{Pool: srv.Pool, Log: discardLogger()}
	srv.WebhookSecret = "test-webhook-secret"
	// The dedupe + idempotency stores are global (no org scoping), and
	// the sandboxes pkey is global too — a previous run's rows would
	// turn fresh events into duplicates and clash on insert. Clean as
	// the OWNER: the pooled connections run as portal_app and RLS
	// filters every row away from them.
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

// newOrgWithKey logs a user in, creates org + project + API key and
// returns the org id, project id, the full key secret (shown once) and
// the session cookies.
func newOrgWithKey(t *testing.T, srv *Server, sub, email string) (orgID, projectID, key string, cookies []*http.Cookie) {
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
		`{"name":"gateway","scopes":["sandbox:create","sandbox:read","sandbox:exec"]}`,
		cookies...)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create api key = %d: %s", rec.Code, rec.Body.String())
	}
	key = extractString(t, rec.Body.String(), "secret")
	if !strings.HasPrefix(key, "sbx_live_") {
		t.Fatalf("key = %q", key)
	}
	return orgID, projectID, key, cookies
}

// v1do issues a Bearer request against the gateway.
func v1do(t *testing.T, srv *Server, method, target, body, key string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *strings.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	} else {
		rdr = strings.NewReader("")
	}
	req := httptest.NewRequest(method, target, rdr)
	req.Header.Set("Authorization", "Bearer "+key)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// TestSandboxGatewayV1: key create → index row → replay → reuse
// refused → cross-org 404 → list → exec.
func TestSandboxGatewayV1(t *testing.T) {
	srv, fake := setupGateway(t)
	orgA, _, keyA, _ := newOrgWithKey(t, srv, "gw-alice", "gw-alice@example.test")
	_, _, keyB, _ := newOrgWithKey(t, srv, "gw-bob", "gw-bob@example.test")

	// 1. Create with an Idempotency-Key; the tenant tag rides along.
	hdr := map[string]string{"Idempotency-Key": "idem-1", "Content-Type": "application/json"}
	body := `{"image":"ubuntu:24.04"}`
	rec := v1do(t, srv, http.MethodPost, "/v1/sandboxes", body, keyA, hdr)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	sbxID := extractString(t, rec.Body.String(), "id")
	if ext := fake.lastCreate["external"].(map[string]any); ext["workspace_id"] != orgA {
		t.Errorf("workspace tag = %v, want %s", ext, orgA)
	}

	// 2. Same key + same body → the stored response replays; no second
	// create call reaches sandboxd.
	rec = v1do(t, srv, http.MethodPost, "/v1/sandboxes", body, keyA, hdr)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), sbxID) {
		t.Fatalf("replay = %d: %s", rec.Code, rec.Body.String())
	}
	if fake.creates != 1 {
		t.Errorf("sandboxd creates = %d, want 1 (replay must not re-create)", fake.creates)
	}

	// 3. Same key, different body → 422, still no new create.
	rec = v1do(t, srv, http.MethodPost, "/v1/sandboxes", `{"image":"node:22"}`, keyA, hdr)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("key reuse = %d, want 422", rec.Code)
	}
	if fake.creates != 1 {
		t.Errorf("sandboxd creates = %d after reuse, want 1", fake.creates)
	}

	// 4. Cross-org: Bob's key against Alice's sandbox is 404 (both the
	// detail read and the exec), never 403.
	rec = v1do(t, srv, http.MethodGet, "/v1/sandboxes/"+sbxID, "", keyB, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("cross-org get = %d, want 404", rec.Code)
	}
	rec = v1do(t, srv, http.MethodPost, "/v1/sandboxes/"+sbxID+"/exec", `{"cmd":["true"]}`, keyB, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("cross-org exec = %d, want 404", rec.Code)
	}

	// 5. Alice lists: exactly her sandbox, tagged.
	rec = v1do(t, srv, http.MethodGet, "/v1/sandboxes", "", keyA, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), sbxID) {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}

	// 6. Exec answers with the passthrough result.
	rec = v1do(t, srv, http.MethodPost, "/v1/sandboxes/"+sbxID+"/exec", `{"cmd":["echo","hi"]}`, keyA, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "hello-gateway") {
		t.Fatalf("exec = %d: %s", rec.Code, rec.Body.String())
	}
	if cmd := fake.execBody["cmd"].([]any); len(cmd) != 2 {
		t.Errorf("exec argv = %v", fake.execBody)
	}

	// 7. Logs proxy keeps the upstream content type + body.
	rec = v1do(t, srv, http.MethodGet, "/v1/sandboxes/"+sbxID+"/logs", "", keyA, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "fake-log-line-2") {
		t.Fatalf("logs = %d: %s", rec.Code, rec.Body.String())
	}

	// 8. No key → 401; wrong key → 401.
	rec = v1do(t, srv, http.MethodGet, "/v1/sandboxes", "", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous = %d, want 401", rec.Code)
	}
	rec = v1do(t, srv, http.MethodGet, "/v1/sandboxes", "", "sbx_live_AAAAAAAAAAAA_wrongwrongwrongwrongwrongwrong12", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("bogus key = %d, want 401", rec.Code)
	}
}

// TestSandboxGatewayProjectPinnedKey: a key pinned to one project
// cannot read, exec, stop or delete a sandbox of another project in the
// same org (404, as for a foreign org), but still reaches its own.
func TestSandboxGatewayProjectPinnedKey(t *testing.T) {
	srv, fake := setupGateway(t)
	orgID, projA, keyAll, cookies := newOrgWithKey(t, srv, "gw-pin", "gw-pin@example.test")

	rec := do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/projects", `{"name":"proj-b"}`, cookies...)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create project b = %d: %s", rec.Code, rec.Body.String())
	}
	projB := extractString(t, rec.Body.String(), "id")
	rec = do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/api-keys",
		`{"name":"pinned-b","scopes":["sandbox:create","sandbox:read","sandbox:exec"],"project_id":"`+projB+`"}`,
		cookies...)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create pinned key = %d: %s", rec.Code, rec.Body.String())
	}
	keyB := extractString(t, rec.Body.String(), "secret")

	hdr := map[string]string{"Content-Type": "application/json"}
	rec = v1do(t, srv, http.MethodPost, "/v1/sandboxes", `{"image":"ubuntu:24.04","project_id":"`+projA+`"}`, keyAll, hdr)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create in project a = %d: %s", rec.Code, rec.Body.String())
	}
	sbxA := extractString(t, rec.Body.String(), "id")

	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/sandboxes/" + sbxA, ""},
		{http.MethodPost, "/v1/sandboxes/" + sbxA + "/exec", `{"cmd":["true"]}`},
		{http.MethodGet, "/v1/sandboxes/" + sbxA + "/logs", ""},
		{http.MethodPost, "/v1/sandboxes/" + sbxA + "/stop", ""},
		{http.MethodDelete, "/v1/sandboxes/" + sbxA, ""},
	} {
		if rec := v1do(t, srv, c.method, c.path, c.body, keyB, hdr); rec.Code != http.StatusNotFound {
			t.Errorf("pinned key %s %s = %d, want 404", c.method, c.path, rec.Code)
		}
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("pinned key deleted another project's sandbox: %v", fake.deleted)
	}

	// Its own project still works.
	rec = v1do(t, srv, http.MethodPost, "/v1/sandboxes", `{"image":"ubuntu:24.04"}`, keyB, hdr)
	if rec.Code != http.StatusCreated {
		t.Fatalf("pinned create = %d: %s", rec.Code, rec.Body.String())
	}
	sbxB := extractString(t, rec.Body.String(), "id")
	if rec := v1do(t, srv, http.MethodGet, "/v1/sandboxes/"+sbxB, "", keyB, nil); rec.Code != http.StatusOK {
		t.Errorf("pinned get own = %d: %s", rec.Code, rec.Body.String())
	}
}

// TestBFFSandboxes: the dashboard surface — configured flag, member
// create, detail with read-through, delete drops the index row.
func TestBFFSandboxes(t *testing.T) {
	srv, fake := setupGateway(t)
	orgID, projectID, _, cookies := newOrgWithKey(t, srv, "bff-sbx", "bff-sbx@example.test")

	// Not-configured servers answer a soft 200 so the page can render.
	rec := do(t, srv, http.MethodGet, "/api/orgs/"+orgID+"/sandboxes", "", cookies...)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}

	// Create through the BFF.
	body := `{"image":"ubuntu:24.04","project_id":"` + projectID + `"}`
	rec = do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/sandboxes", body, cookies...)
	if rec.Code != http.StatusCreated {
		t.Fatalf("bff create = %d: %s", rec.Code, rec.Body.String())
	}
	sbxID := extractString(t, rec.Body.String(), "id")

	// Detail syncs the index row (read-through).
	rec = do(t, srv, http.MethodGet, "/api/orgs/"+orgID+"/sandboxes/"+sbxID, "", cookies...)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ports":[3000]`) {
		t.Fatalf("bff detail = %d: %s", rec.Code, rec.Body.String())
	}

	// List reports the sandbox with the project pointer.
	rec = do(t, srv, http.MethodGet, "/api/orgs/"+orgID+"/sandboxes", "", cookies...)
	if rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"configured":true`) ||
		!strings.Contains(rec.Body.String(), `"project_id":"`+projectID+`"`) {
		t.Fatalf("bff list = %d: %s", rec.Code, rec.Body.String())
	}

	// Delete → 204, index row gone, detail becomes 404.
	rec = do(t, srv, http.MethodDelete, "/api/orgs/"+orgID+"/sandboxes/"+sbxID, "", cookies...)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("bff delete = %d: %s", rec.Code, rec.Body.String())
	}
	if len(fake.deleted) != 1 {
		t.Errorf("sandboxd deletes = %d, want 1", len(fake.deleted))
	}
	rec = do(t, srv, http.MethodGet, "/api/orgs/"+orgID+"/sandboxes/"+sbxID, "", cookies...)
	if rec.Code != http.StatusNotFound {
		t.Errorf("detail after delete = %d, want 404", rec.Code)
	}
}

// TestSandboxdWebhook: signature gate, freshness window, dedupe and
// the status fold into the index.
func TestSandboxdWebhook(t *testing.T) {
	srv, _ := setupGateway(t)
	orgID, _, _, cookies := newOrgWithKey(t, srv, "hook-user", "hook-user@example.test")

	// Seed a sandbox through the BFF so the index has the row.
	rec := do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/sandboxes",
		`{"image":"ubuntu:24.04"}`, cookies...)
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed create = %d: %s", rec.Code, rec.Body.String())
	}
	sbxID := extractString(t, rec.Body.String(), "id")

	send := func(payload string, sign bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/webhooks/sandboxd", strings.NewReader(payload))
		if sign {
			mac := hmac.New(sha256.New, []byte("test-webhook-secret"))
			mac.Write([]byte(payload))
			req.Header.Set("X-Sandboxd-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		}
		req.Header.Set("X-Sandboxd-Event-Id", "evt-1")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	// Event ids are unique per run — the dedupe store persists across
	// runs, and a stale id would read as a replay.
	run := time.Now().UnixNano()
	evt := func(n int) string { return fmt.Sprintf("evt-%d-%d", run, n) }

	// Unsigned / wrong signature → 401.
	bad := `{"id":"` + evt(0) + `","event":"started","sandbox_id":"` + sbxID + `","occurred_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}`
	if rec := send(bad, false); rec.Code != http.StatusUnauthorized {
		t.Errorf("unsigned = %d, want 401", rec.Code)
	}

	// Fresh + signed → applied (status → running).
	good := `{"id":"` + evt(1) + `","event":"started","sandbox_id":"` + sbxID + `","external":{"workspace_id":"` + orgID + `"},"occurred_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}`
	if rec := send(good, true); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "applied") {
		t.Errorf("signed fresh = %d: %s", rec.Code, rec.Body.String())
	}
	if status := sandboxStatus(t, srv, orgID, sbxID); status != "running" {
		t.Errorf("status after started webhook = %q, want running", status)
	}

	// Replay (same event id) → duplicate, still 200.
	if rec := send(good, true); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "duplicate") {
		t.Errorf("replay = %d: %s", rec.Code, rec.Body.String())
	}

	// Stale beyond ±5m → 400 even with a valid signature.
	stale := `{"id":"` + evt(2) + `","event":"stopped","sandbox_id":"` + sbxID + `","external":{"workspace_id":"` + orgID + `"},"occurred_at":"` + time.Now().Add(-10*time.Minute).UTC().Format(time.RFC3339) + `"}`
	if rec := send(stale, true); rec.Code != http.StatusBadRequest {
		t.Errorf("stale = %d, want 400", rec.Code)
	}

	// Foreign workspace tag → ignored (unknown sandbox).
	foreign := `{"id":"` + evt(3) + `","event":"started","sandbox_id":"` + sbxID + `","external":{"workspace_id":"00000000-0000-0000-0000-000000000000"},"occurred_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}`
	if rec := send(foreign, true); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ignored") {
		t.Errorf("foreign = %d: %s", rec.Code, rec.Body.String())
	}

	// deleted → the index row is gone.
	del := `{"id":"` + evt(4) + `","event":"deleted","sandbox_id":"` + sbxID + `","external":{"workspace_id":"` + orgID + `"},"occurred_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}`
	if rec := send(del, true); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "applied") {
		t.Errorf("deleted = %d: %s", rec.Code, rec.Body.String())
	}
	if status := sandboxStatus(t, srv, orgID, sbxID); status != "" {
		t.Errorf("row survived the deleted webhook: %q", status)
	}
}

// sandboxStatus reads the index row straight from the database (the
// BFF detail endpoint would read-through-sync from the fake sandboxd
// and mask whatever the webhook wrote).
func sandboxStatus(t *testing.T, srv *Server, orgID, id string) string {
	t.Helper()
	var status string
	err := db.WithOrg(context.Background(), srv.Pool, orgID, func(tx db.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT status FROM sandboxes WHERE id = $1`, id).Scan(&status)
	})
	if err != nil {
		return ""
	}
	return status
}
