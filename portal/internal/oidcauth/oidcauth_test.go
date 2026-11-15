// oidcauth_test.go — tests for the AuthCodeURL prompt=create option
// (issue #80). The Provider is constructed from a fake OIDC discovery
// server so the URL building is exercised end-to-end without a real
// IdP.
package oidcauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// fakeDiscovery serves a minimal OIDC discovery document so oidc.NewProvider
// succeeds; the actual endpoint URLs in it don't have to work — we only
// exercise AuthCodeURL, which builds a redirect string.
func fakeDiscovery(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		// issuer must match the server's own base URL or the verifier
		// rejects tokens (we don't exchange tokens here).
		doc := map[string]any{
			"issuer":                                srv.URL,
			"authorization_endpoint":                srv.URL + "/auth",
			"token_endpoint":                        srv.URL + "/token",
			"jwks_uri":                              srv.URL + "/jwks",
			"userinfo_endpoint":                     srv.URL + "/userinfo",
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(doc)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAuthCodeURLNoPromptByDefault(t *testing.T) {
	ctx := context.Background()
	srv := fakeDiscovery(t)
	p, err := New(ctx, srv.URL, "client-id", "client-secret", "https://app.example.test/auth/callback")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := url.Parse(p.AuthCodeURL("state123", "challenge456", "nonce789", false))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Path != "/auth" {
		t.Errorf("path = %s, want /auth", got.Path)
	}
	if got.Query().Get("prompt") != "" {
		t.Errorf("prompt = %q, want absent on signup=false", got.Query().Get("prompt"))
	}
	if got.Query().Get("client_id") != "client-id" {
		t.Errorf("client_id = %q, want client-id", got.Query().Get("client_id"))
	}
	if got.Query().Get("state") != "state123" {
		t.Errorf("state = %q, want state123", got.Query().Get("state"))
	}
	if got.Query().Get("code_challenge") != "challenge456" {
		t.Errorf("code_challenge = %q", got.Query().Get("code_challenge"))
	}
	if got.Query().Get("code_challenge_method") != "S256" {
		t.Errorf("code_challenge_method = %q, want S256", got.Query().Get("code_challenge_method"))
	}
	if got.Query().Get("nonce") != "nonce789" {
		t.Errorf("nonce = %q, want nonce789", got.Query().Get("nonce"))
	}
}

func TestAuthCodeURLSignupAddsPromptCreate(t *testing.T) {
	ctx := context.Background()
	srv := fakeDiscovery(t)
	p, err := New(ctx, srv.URL, "client-id", "client-secret", "https://app.example.test/auth/callback")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := url.Parse(p.AuthCodeURL("state123", "challenge456", "nonce789", true))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	prompt := got.Query().Get("prompt")
	if prompt != "create" {
		t.Errorf("prompt = %q, want create", prompt)
	}
	// The standard params stay intact.
	if got.Query().Get("state") != "state123" {
		t.Errorf("state = %q, want state123", got.Query().Get("state"))
	}
	if got.Query().Get("code_challenge_method") != "S256" {
		t.Errorf("code_challenge_method = %q", got.Query().Get("code_challenge_method"))
	}
}

// The whole URL still carries everything a normal OIDC redirect does.
func TestAuthCodeURLContainsAllExpectedParams(t *testing.T) {
	ctx := context.Background()
	srv := fakeDiscovery(t)
	p, err := New(ctx, srv.URL, "cid", "secret", "https://app.example.test/auth/callback")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	raw := p.AuthCodeURL("s", "c", "n", true)
	for _, want := range []string{
		"client_id=cid",
		"response_type=code",
		"scope=openid",
		"state=s",
		"code_challenge=c",
		"code_challenge_method=S256",
		"nonce=n",
		"prompt=create",
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("AuthCodeURL(%q) missing %q in %q", want, want, raw)
		}
	}
}
