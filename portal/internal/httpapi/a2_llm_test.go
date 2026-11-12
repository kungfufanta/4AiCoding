// A2 phase 2 e2e tests. Real Postgres via PORTAL_TEST_DATABASE_URL.
// Covers: key never in response (grep), SSRF block on org base_url,
// member cannot edit, org A cannot see org B's connections, test
// endpoint calls the provider with the decrypted key.
package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tastyeffectco/sandboxd/portal/internal/db"
	"github.com/tastyeffectco/sandboxd/portal/internal/passwordauth"
)

// setupA2 returns (srv, fakeLLM, adminSub). adminSub is the bootstrap
// admin's oidc_sub (unique per test run) so loginA2Admin can log in
// without colliding with parallel runs.
func setupA2(t *testing.T) (*Server, *fakeLLMProbe, string) {
	t.Helper()
	srv := setupBFF(t)
	sfx := fmt.Sprintf("%d", time.Now().UnixNano())
	username := "a2-admin-" + sfx
	adminEmail := username + "@example.test"
	adminID, err := passwordauth.EnsureBootstrap(context.Background(), srv.Pool, discardLogger(),
		username, "correct-horse-1", adminEmail)
	if err != nil {
		t.Fatalf("EnsureBootstrap: %v", err)
	}
	srv.PlatformAdminEnabled = true
	srv.BootstrapAdminID = adminID
	if os.Getenv("PORTAL_SECRETS_KEY") == "" {
		_ = os.Setenv("PORTAL_SECRETS_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	}
	fake := newFakeLLMProbe()
	t.Cleanup(fake.Close)
	srv.LLMHTTP = &http.Client{Timeout: 5 * time.Second}
	// passwordauth uses SubjectPrefix "password:" + username.
	adminSub := "password:" + username
	return srv, fake, adminSub
}

// fakeLLMProbe is a tiny OpenAI-compatible server that records the headers
// it received and returns a canned completion.
type fakeLLMProbe struct {
	*httptest.Server
	mu      sync.Mutex
	headers http.Header
	body    string
}

func newFakeLLMProbe() *fakeLLMProbe {
	f := &fakeLLMProbe{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.headers = r.Header.Clone()
		body, _ := io.ReadAll(r.Body)
		f.body = string(body)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/chat/completions" && strings.Contains(f.body, `"stream":true`):
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
		case r.URL.Path == "/v1/chat/completions":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"model":"glm-4.6","choices":[{"message":{"content":"ok"}}]}`))
		case r.URL.Path == "/v1/models":
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"data":[{"id":"glm-4.6"},{"id":"glm-4.6-mini"}]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	return f
}

func (f *fakeLLMProbe) lastHeader(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.headers.Get(name)
}

// loginA2Admin logs in the bootstrap admin and returns cookies. The
// adminSub is unique per test run (set by setupA2); the email is
// derived from it so UpsertUser finds the existing row instead of
// creating a new one.
func loginA2Admin(t *testing.T, srv *Server, adminSub string) []*http.Cookie {
	// adminSub = "password:a2-admin-<sfx>"; email = "a2-admin-<sfx>@example.test"
	email := strings.TrimPrefix(adminSub, "password:") + "@example.test"
	_, sc, csrf := login(t, srv, adminSub, email, "Admin")
	return []*http.Cookie{sc, csrf}
}

// newOrgWithCookies creates an org + returns orgID + session cookies
// for the org owner.
func newOrgWithCookies(t *testing.T, srv *Server, sub, email string) (orgID string, cookies []*http.Cookie) {
	_, sc, csrf := login(t, srv, sub, email, sub)
	cookies = []*http.Cookie{sc, csrf}
	rec := do(t, srv, http.MethodPost, "/api/orgs", `{"name":"`+sub+` org"}`, cookies...)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create org = %d: %s", rec.Code, rec.Body.String())
	}
	return extractString(t, rec.Body.String(), "id"), cookies
}

// orgIDFromMe extracts the first org id from /api/me.
func orgIDFromMe(t *testing.T, srv *Server, cookies []*http.Cookie) string {
	t.Helper()
	rec := do(t, srv, http.MethodGet, "/api/me", "", cookies...)
	if rec.Code != http.StatusOK {
		t.Fatalf("me = %d: %s", rec.Code, rec.Body.String())
	}
	var me struct {
		Orgs []struct {
			ID string `json:"id"`
		} `json:"orgs"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &me)
	if len(me.Orgs) == 0 {
		t.Fatal("no orgs in /api/me")
	}
	return me.Orgs[0].ID
}

// TestLLMConnectionCreateKeyNeverInResponse — the plaintext api_key
// must NEVER appear in any response body. The handler returns
// has_api_key + key_label only.
func TestLLMConnectionCreateKeyNeverInResponse(t *testing.T) {
	srv, _, adminSub := setupA2(t)
	cookies := loginA2Admin(t, srv, adminSub)
	body := `{"name":"platform-webhub","protocol":"openai","flavor":"webhub",` +
		`"base_url":"https://h11hp9dd1y90-d.space-z.ai/v1","default_model":"glm-4.6",` +
		`"models":[{"id":"glm-4.6","supports_tools":true}],"key_mode":"per_org",` +
		`"timeout_s":60,"max_tokens":4096,"enabled":true,"is_default":true,` +
		`"api_key":"wh_supersecret_12345"}`
	rec := do(t, srv, http.MethodPost, "/api/admin/llm/connections", body, cookies...)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	resp := rec.Body.String()
	if strings.Contains(resp, "wh_supersecret_12345") {
		t.Errorf("plaintext api_key leaked in response: %s", resp)
	}
	if !strings.Contains(resp, `"has_api_key":true`) {
		t.Errorf("has_api_key missing in response: %s", resp)
	}
	if !strings.Contains(resp, `"key_label":"2345"`) {
		t.Errorf("key_label missing or wrong (want 2345): %s", resp)
	}
}

// TestLLMConnectionListKeyNeverInResponse — listing connections must
// not expose the key. Only has_api_key + key_label.
func TestLLMConnectionListKeyNeverInResponse(t *testing.T) {
	srv, _, adminSub := setupA2(t)
	cookies := loginA2Admin(t, srv, adminSub)
	body := `{"name":"c","protocol":"openai","flavor":"generic",` +
		`"base_url":"https://api.openai.com/v1","default_model":"gpt-4o",` +
		`"models":[],"key_mode":"shared","timeout_s":60,"max_tokens":4096,` +
		`"enabled":true,"is_default":true,"api_key":"sk-list-test-12345"}`
	rec := do(t, srv, http.MethodPost, "/api/admin/llm/connections", body, cookies...)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	rec = do(t, srv, http.MethodGet, "/api/admin/llm/connections", "", cookies...)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "sk-list-test-12345") {
		t.Errorf("plaintext key leaked in list response: %s", rec.Body.String())
	}
}

// TestLLMConnectionOrgCrossOrgInvisible — org A cannot see org B's
// connections. The list endpoint returns only the caller's org rows
// plus platform rows.
func TestLLMConnectionOrgCrossOrgInvisible(t *testing.T) {
	srv, _, _ := setupA2(t)
	orgA, cookiesA := newOrgWithCookies(t, srv, "a2-org-a", "a2-org-a@example.test")
	_, cookiesB := newOrgWithCookies(t, srv, "a2-org-b", "a2-org-b@example.test")
	body := `{"name":"org-a-llm","protocol":"openai","flavor":"generic",` +
		`"base_url":"https://api.openai.com/v1","default_model":"gpt-4o",` +
		`"models":[],"key_mode":"shared","timeout_s":60,"max_tokens":4096,` +
		`"enabled":true,"is_default":true,"api_key":"sk-orga-key-12345"}`
	rec := do(t, srv, http.MethodPost, "/api/orgs/"+orgA+"/llm/connections", body, cookiesA...)
	if rec.Code != http.StatusCreated {
		t.Fatalf("org A create = %d: %s", rec.Code, rec.Body.String())
	}
	// Org B lists its own connections: must NOT see org A's.
	orgBID := orgIDFromMe(t, srv, cookiesB)
	rec = do(t, srv, http.MethodGet, "/api/orgs/"+orgBID+"/llm/connections", "", cookiesB...)
	if rec.Code != http.StatusOK {
		t.Fatalf("org B list = %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "org-a-llm") {
		t.Errorf("org B can see org A's connection: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "sk-orga-key-12345") {
		t.Errorf("org A's key leaked to org B: %s", rec.Body.String())
	}
}

// TestLLMConnectionSSRFBlock — an org connection whose base_url
// resolves to a private IP must be refused (400 ssrf_blocked).
// Platform connections (admin-created) may point internally.
func TestLLMConnectionSSRFBlock(t *testing.T) {
	srv, _, adminSub := setupA2(t)
	orgID, cookies := newOrgWithCookies(t, srv, "a2-ssrf", "a2-ssrf@example.test")
	body := `{"name":"internal","protocol":"openai","flavor":"generic",` +
		`"base_url":"http://10.0.0.1:4000/v1","default_model":"gpt-4o",` +
		`"models":[],"key_mode":"shared","timeout_s":60,"max_tokens":4096,` +
		`"enabled":true,"is_default":true,"api_key":"sk-test"}`
	rec := do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/llm/connections", body, cookies...)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("SSRF create = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ssrf_blocked") {
		t.Errorf("error code missing ssrf_blocked: %s", rec.Body.String())
	}
	// Platform admin CAN create an internal connection (LiteLLM in cluster).
	adminCookies := loginA2Admin(t, srv, adminSub)
	body = strings.Replace(body, "10.0.0.1", "127.0.0.1", 1)
	rec = do(t, srv, http.MethodPost, "/api/admin/llm/connections", body, adminCookies...)
	if rec.Code != http.StatusCreated {
		t.Errorf("platform internal create = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
}

// TestLLMConnectionMemberCannotEdit — a member-role user can list
// connections but cannot create/update/delete.
func TestLLMConnectionMemberCannotEdit(t *testing.T) {
	srv, _, _ := setupA2(t)
	orgID, ownerCookies := newOrgWithCookies(t, srv, "a2-member-org", "a2-member-org@example.test")
	// Invite a member.
	rec := do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/invites",
		`{"email":"a2-member@example.test","role":"member"}`, ownerCookies...)
	if rec.Code != http.StatusCreated {
		t.Fatalf("invite = %d: %s", rec.Code, rec.Body.String())
	}
	inviteURL := extractString(t, rec.Body.String(), "url")
	_, memberSC, memberCSRF := login(t, srv, "portal:a2-member", "a2-member@example.test", "Member")
	memberCookies := []*http.Cookie{memberSC, memberCSRF}
	token := inviteURL[strings.Index(inviteURL, "token=")+6:]
	rec = do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/invites/accept",
		`{"token":"`+token+`"}`, memberCookies...)
	if rec.Code != http.StatusOK {
		t.Fatalf("accept = %d: %s", rec.Code, rec.Body.String())
	}
	// Member can list.
	rec = do(t, srv, http.MethodGet, "/api/orgs/"+orgID+"/llm/connections", "", memberCookies...)
	if rec.Code != http.StatusOK {
		t.Fatalf("member list = %d: %s", rec.Code, rec.Body.String())
	}
	// Member cannot create.
	body := `{"name":"x","protocol":"openai","flavor":"generic",` +
		`"base_url":"https://api.openai.com/v1","default_model":"gpt-4o",` +
		`"models":[],"key_mode":"shared","timeout_s":60,"max_tokens":4096,` +
		`"enabled":true,"is_default":true,"api_key":"sk-test"}`
	rec = do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/llm/connections", body, memberCookies...)
	if rec.Code != http.StatusForbidden {
		t.Errorf("member create = %d, want 403", rec.Code)
	}
}

// TestLLMConnectionTestCallsProviderWithKey — POST /test loads the
// connection, decrypts its key, and calls the provider. The fake LLM
// records the Authorization header; we assert it carries the key.
func TestLLMConnectionTestCallsProviderWithKey(t *testing.T) {
	srv, fake, adminSub := setupA2(t)
	adminCookies := loginA2Admin(t, srv, adminSub)
	body := `{"name":"test-conn","protocol":"openai","flavor":"generic",` +
		`"base_url":"` + fake.URL + `/v1","default_model":"glm-4.6",` +
		`"models":[],"key_mode":"per_org","timeout_s":60,"max_tokens":4096,` +
		`"enabled":true,"is_default":true,"api_key":"sk-test-key-12345"}`
	rec := do(t, srv, http.MethodPost, "/api/admin/llm/connections", body, adminCookies...)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	cid := extractString(t, rec.Body.String(), "id")
	rec = do(t, srv, http.MethodPost, "/api/admin/llm/connections/"+cid+"/test", "", adminCookies...)
	if rec.Code != http.StatusOK {
		t.Fatalf("test = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Errorf("test result not ok: %s", rec.Body.String())
	}
	auth := fake.lastHeader("Authorization")
	if auth != "Bearer sk-test-key-12345" {
		t.Errorf("provider received Authorization = %q, want 'Bearer sk-test-key-12345'", auth)
	}
}

// TestLLMConnectionListModels — GET .../{cid}/models calls the
// provider's /v1/models and returns the id list.
func TestLLMConnectionListModels(t *testing.T) {
	srv, fake, adminSub := setupA2(t)
	adminCookies := loginA2Admin(t, srv, adminSub)
	body := `{"name":"m","protocol":"openai","flavor":"generic",` +
		`"base_url":"` + fake.URL + `/v1","default_model":"glm-4.6",` +
		`"models":[],"key_mode":"per_org","timeout_s":60,"max_tokens":4096,` +
		`"enabled":true,"is_default":true,"api_key":"sk-test"}`
	rec := do(t, srv, http.MethodPost, "/api/admin/llm/connections", body, adminCookies...)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	cid := extractString(t, rec.Body.String(), "id")
	rec = do(t, srv, http.MethodGet, "/api/admin/llm/connections/"+cid+"/models", "", adminCookies...)
	if rec.Code != http.StatusOK {
		t.Fatalf("models = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "glm-4.6") {
		t.Errorf("models response missing glm-4.6: %s", rec.Body.String())
	}
}

// keep imports honest
var _ = db.WithOrg
