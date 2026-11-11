package httpapi

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tastyeffectco/sandboxd/portal/internal/agenttoken"
	"github.com/tastyeffectco/sandboxd/portal/internal/apikeys"
)

// apikeys_test.go — S2 flow through the real BFF with real Postgres
// (PORTAL_TEST_DATABASE_URL; skipped when unset): create → list →
// exchange for an agent token → revoke → token refused, plus the
// org-isolation and scope rules.
func setupKeysServer(t *testing.T) *Server {
	t.Helper()
	srv := setupBFF(t)
	srv.Keys = &apikeys.Verifier{
		Pool: srv.Pool,
		Log:  discardLogger(),
		TTL:  150 * time.Millisecond, // short so the revoke test is quick
	}
	signer, err := agenttoken.NewSigner()
	if err != nil {
		t.Fatalf("agenttoken.NewSigner: %v", err)
	}
	srv.Tokens = signer
	return srv
}

func createOrgFor(t *testing.T, srv *Server, cookies []*http.Cookie, name string) string {
	t.Helper()
	rec := do(t, srv, http.MethodPost, "/api/orgs", `{"name":"`+name+`"}`, cookies...)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/orgs = %d: %s", rec.Code, rec.Body.String())
	}
	return extractString(t, rec.Body.String(), "id")
}

func createKey(t *testing.T, srv *Server, cookies []*http.Cookie, orgID, name, scopesJSON string) (fullSecret, keyID string) {
	t.Helper()
	rec := do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/api-keys",
		`{"name":"`+name+`","scopes":`+scopesJSON+`}`, cookies...)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST api-keys = %d: %s", rec.Code, rec.Body.String())
	}
	fullSecret = extractString(t, rec.Body.String(), "secret")
	// key_id rides in the nested key object; pull it from the body.
	var resp struct {
		Key struct {
			KeyID string `json:"key_id"`
		} `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	return fullSecret, resp.Key.KeyID
}

func TestApiKeyFlow(t *testing.T) {
	srv := setupKeysServer(t)
	_, alice, aliceCSRF := login(t, srv, "keys-alice-sub", "keys-alice@example.test", "Alice")
	orgID := createOrgFor(t, srv, []*http.Cookie{alice, aliceCSRF}, "Alice Keys Org")

	full, keyID := createKey(t, srv, []*http.Cookie{alice, aliceCSRF}, orgID,
		"CI key", `["sandbox:read","sandbox:exec"]`)
	if !strings.HasPrefix(full, apikeys.Prefix) {
		t.Fatalf("secret missing prefix: %q", full)
	}

	// List shows the key but never the secret or its hash.
	rec := do(t, srv, http.MethodGet, "/api/orgs/"+orgID+"/api-keys", "", alice, aliceCSRF)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET api-keys = %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), full) || strings.Contains(rec.Body.String(), "secret") {
		t.Errorf("list leaks secret: %s", rec.Body.String())
	}

	// Exchange the key for an agent token.
	reqToken := func() int {
		rec := doReqHdr(t, srv.Handler(), http.MethodPost, "/v1/tokens",
			map[string]any{"scopes": []string{"sandbox:read"}, "ttl_seconds": 900},
			map[string]string{"Authorization": "Bearer " + full})
		return rec.Code
	}
	if code := reqToken(); code != http.StatusOK {
		t.Fatalf("POST /v1/tokens = %d (before revoke)", code)
	}

	// Verify the minted token against the server's signer.
	rec = doReqHdr(t, srv.Handler(), http.MethodPost, "/v1/tokens",
		map[string]any{}, map[string]string{"Authorization": "Bearer " + full})
	var tok struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &tok); err != nil {
		t.Fatalf("decode token response: %v", err)
	}
	claims, err := srv.Tokens.Verify(tok.Token)
	if err != nil {
		t.Fatalf("server token does not verify: %v", err)
	}
	if claims.OrgID != orgID {
		t.Errorf("token org = %s, want %s", claims.OrgID, orgID)
	}
	if len(claims.Scopes) != 2 {
		t.Errorf("token scopes = %v, want the key's two scopes", claims.Scopes)
	}

	// Scope escalation is refused.
	rec = doReqHdr(t, srv.Handler(), http.MethodPost, "/v1/tokens",
		map[string]any{"scopes": []string{"sandbox:exec", "usage:read"}},
		map[string]string{"Authorization": "Bearer " + full})
	if rec.Code != http.StatusForbidden {
		t.Errorf("scope escalation = %d, want 403: %s", rec.Code, rec.Body.String())
	}

	// TTL above the cap is clamped, not an error.
	rec = doReqHdr(t, srv.Handler(), http.MethodPost, "/v1/tokens",
		map[string]any{"ttl_seconds": 99999},
		map[string]string{"Authorization": "Bearer " + full})
	if rec.Code != http.StatusOK {
		t.Errorf("over-cap ttl = %d, want 200", rec.Code)
	}

	// Revoke through the dashboard…
	rec = do(t, srv, http.MethodDelete, "/api/orgs/"+orgID+"/api-keys/"+url.PathEscape(keyID), "", alice, aliceCSRF)
	if rec.Code != http.StatusOK {
		t.Fatalf("DELETE api-key = %d: %s", rec.Code, rec.Body.String())
	}

	// …and the key dies as soon as the cache expires (TTL 150 ms).
	time.Sleep(250 * time.Millisecond)
	if code := reqToken(); code != http.StatusUnauthorized {
		t.Errorf("POST /v1/tokens after revoke = %d, want 401", code)
	}

	// The audit trail recorded create + revoke.
	rec = do(t, srv, http.MethodGet, "/api/orgs/"+orgID+"/audit", "", alice, aliceCSRF)
	if !strings.Contains(rec.Body.String(), "apikey.create") || !strings.Contains(rec.Body.String(), "apikey.revoke") {
		t.Errorf("audit trail missing apikey events: %s", rec.Body.String())
	}
}

func TestApiKeyOrgIsolation(t *testing.T) {
	srv := setupKeysServer(t)
	_, alice, aliceCSRF := login(t, srv, "keys-iso-alice", "keys-iso-alice@example.test", "Alice")
	_, bob, bobCSRF := login(t, srv, "keys-iso-bob", "keys-iso-bob@example.test", "Bob")
	aliceOrg := createOrgFor(t, srv, []*http.Cookie{alice, aliceCSRF}, "Alice Iso Org")
	bobOrg := createOrgFor(t, srv, []*http.Cookie{bob, bobCSRF}, "Bob Iso Org")

	full, keyID := createKey(t, srv, []*http.Cookie{alice, aliceCSRF}, aliceOrg, "Alice only", `["sandbox:read"]`)

	// Bob cannot see, revoke or use-scope Alice's org.
	rec := do(t, srv, http.MethodGet, "/api/orgs/"+aliceOrg+"/api-keys", "", bob, bobCSRF)
	if rec.Code != http.StatusNotFound {
		t.Errorf("foreign list = %d, want 404", rec.Code)
	}
	rec = do(t, srv, http.MethodDelete, "/api/orgs/"+aliceOrg+"/api-keys/"+url.PathEscape(keyID), "", bob, bobCSRF)
	if rec.Code != http.StatusNotFound {
		t.Errorf("foreign revoke = %d, want 404", rec.Code)
	}
	rec = doReqHdr(t, srv.Handler(), http.MethodPost, "/v1/tokens", nil,
		map[string]string{"Authorization": "Bearer " + full})
	if rec.Code != http.StatusOK {
		t.Fatalf("own key exchange = %d: %s", rec.Code, rec.Body.String())
	}

	// A member (non-admin) of the org can read keys but not create them.
	_, member, memberCSRF := login(t, srv, "keys-iso-member", "keys-iso-member@example.test", "Member")
	rec = do(t, srv, http.MethodPost, "/api/orgs/"+aliceOrg+"/invites",
		`{"email":"keys-iso-member@example.test","role":"member"}`, alice, aliceCSRF)
	if rec.Code != http.StatusCreated {
		t.Fatalf("invite member = %d: %s", rec.Code, rec.Body.String())
	}
	inviteURL := extractString(t, rec.Body.String(), "url")
	inviteToken := inviteURL[strings.Index(inviteURL, "token=")+len("token="):]
	rec = do(t, srv, http.MethodPost, "/api/orgs/"+aliceOrg+"/invites/accept",
		`{"token":"`+inviteToken+`"}`, member, memberCSRF)
	if rec.Code != http.StatusOK {
		t.Fatalf("accept invite = %d: %s", rec.Code, rec.Body.String())
	}
	rec = do(t, srv, http.MethodGet, "/api/orgs/"+aliceOrg+"/api-keys", "", member, memberCSRF)
	if rec.Code != http.StatusOK {
		t.Errorf("member list = %d, want 200", rec.Code)
	}
	rec = do(t, srv, http.MethodPost, "/api/orgs/"+aliceOrg+"/api-keys",
		`{"name":"nope","scopes":["sandbox:read"]}`, member, memberCSRF)
	if rec.Code != http.StatusNotFound {
		t.Errorf("member create key = %d, want 404 (admin gate)", rec.Code)
	}

	// Bob's org starts with an empty list, not an error.
	rec = do(t, srv, http.MethodGet, "/api/orgs/"+bobOrg+"/api-keys", "", bob, bobCSRF)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("bob's own list = %d %s, want 200 []", rec.Code, rec.Body.String())
	}
}

func TestAgentTokenHostileInput(t *testing.T) {
	srv := setupKeysServer(t)
	// Malformed / hostile bearer values must answer 401, never panic
	// or leak internals (plan S2: fuzz parse không panic).
	inputs := []string{
		"", "Bearer", "Bearer ", "Bearer garbage", "Bearer sbx_live_",
		"Bearer sbx_live_AAAAAAAAAAAA_short", "Basic dXNlcjpwYXNz",
		"Bearer sbx_live_AAAAAAAAAAAA_" + strings.Repeat("A", 5000),
		"Bearer " + strings.Repeat("sbx_live_", 100),
		"Bearer sbx_live_AAAAAAAAAAAA_../../etc/passwd",
		"Bearer null",
	}
	for _, in := range inputs {
		hdr := map[string]string{"Authorization": in}
		rec := doReqHdr(t, srv.Handler(), http.MethodPost, "/v1/tokens", nil, hdr)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("POST /v1/tokens with Authorization %q = %d, want 401", in, rec.Code)
		}
	}
	// A well-formed but unknown key is also 401 (hits the verifier).
	rec := doReqHdr(t, srv.Handler(), http.MethodPost, "/v1/tokens", nil,
		map[string]string{"Authorization": "Bearer sbx_live_AAAAAAAAAAAA_" + strings.Repeat("a", 43)})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("unknown key = %d, want 401", rec.Code)
	}
}

func TestApiKeyValidation(t *testing.T) {
	srv := setupKeysServer(t)
	_, alice, aliceCSRF := login(t, srv, "keys-val-alice", "keys-val-alice@example.test", "Alice")
	orgID := createOrgFor(t, srv, []*http.Cookie{alice, aliceCSRF}, "Alice Val Org")

	cases := []struct {
		name string
		body string
	}{
		{"empty name", `{"name":"","scopes":["sandbox:read"]}`},
		{"no scopes", `{"name":"k","scopes":[]}`},
		{"unknown scope", `{"name":"k","scopes":["sandbox:drop"]}`},
		{"duplicate scopes", `{"name":"k","scopes":["sandbox:read","sandbox:read"]}`},
		{"bad expiry", `{"name":"k","scopes":["sandbox:read"],"expires_in_days":900}`},
		{"foreign project", `{"name":"k","scopes":["sandbox:read"],"project_id":"11111111-1111-1111-1111-111111111111"}`},
	}
	for _, c := range cases {
		rec := do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/api-keys", c.body, alice, aliceCSRF)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: POST api-keys = %d, want 400: %s", c.name, rec.Code, rec.Body.String())
		}
	}

	// A project that does belong to the org is accepted, and the
	// expires window lands where it should.
	rec := do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/projects", `{"name":"p1"}`, alice, aliceCSRF)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create project = %d", rec.Code)
	}
	projectID := extractString(t, rec.Body.String(), "id")
	rec = do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/api-keys",
		`{"name":"scoped","scopes":["sandbox:read"],"project_id":"`+projectID+`","expires_in_days":30}`,
		alice, aliceCSRF)
	if rec.Code != http.StatusCreated {
		t.Errorf("scoped key = %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Key struct {
			ProjectID *string    `json:"project_id"`
			ExpiresAt *time.Time `json:"expires_at"`
		} `json:"key"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Key.ProjectID == nil || *resp.Key.ProjectID != projectID {
		t.Errorf("key project = %v, want %s", resp.Key.ProjectID, projectID)
	}
	if resp.Key.ExpiresAt == nil || time.Until(*resp.Key.ExpiresAt) > 31*24*time.Hour {
		t.Errorf("key expiry = %v, want ~30 days", resp.Key.ExpiresAt)
	}
}
