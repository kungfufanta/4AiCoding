package httpapi

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tastyeffectco/sandboxd/portal/internal/apikeys"
)

// The verify cache is keyed by the public key id. These tests pin that
// a cached entry never authenticates a different secret, and that a
// wrong secret cannot lock the real key out.
func TestApiKeyCacheBindsSecret(t *testing.T) {
	srv := setupKeysServer(t)
	srv.Keys.TTL = time.Minute
	_, alice, aliceCSRF := login(t, srv, "cache-alice-sub", "cache-alice@example.test", "Alice")
	orgID := createOrgFor(t, srv, []*http.Cookie{alice, aliceCSRF}, "Cache Org")
	full, keyID := createKey(t, srv, []*http.Cookie{alice, aliceCSRF}, orgID, "k", `["sandbox:read"]`)

	token := func(bearer string) int {
		return doReqHdr(t, srv.Handler(), http.MethodPost, "/v1/tokens", map[string]any{},
			map[string]string{"Authorization": "Bearer " + bearer}).Code
	}
	forged := apikeys.Format(keyID, strings.Repeat("A", 43))

	if c := token(full); c != http.StatusOK {
		t.Fatalf("real key = %d", c)
	}
	if c := token(forged); c != http.StatusUnauthorized {
		t.Errorf("forged secret with a cached key id = %d, want 401", c)
	}
	if c := token(full); c != http.StatusOK {
		t.Errorf("real key after a forged attempt = %d, want 200", c)
	}
}

func TestApiKeyWrongSecretCannotLockOut(t *testing.T) {
	srv := setupKeysServer(t)
	srv.Keys.TTL = time.Minute
	_, alice, aliceCSRF := login(t, srv, "lock-alice-sub", "lock-alice@example.test", "Alice")
	orgID := createOrgFor(t, srv, []*http.Cookie{alice, aliceCSRF}, "Lock Org")
	full, keyID := createKey(t, srv, []*http.Cookie{alice, aliceCSRF}, orgID, "k", `["sandbox:read"]`)

	forged := apikeys.Format(keyID, strings.Repeat("A", 43))
	doReqHdr(t, srv.Handler(), http.MethodPost, "/v1/tokens", map[string]any{},
		map[string]string{"Authorization": "Bearer " + forged})
	rec := doReqHdr(t, srv.Handler(), http.MethodPost, "/v1/tokens", map[string]any{},
		map[string]string{"Authorization": "Bearer " + full})
	if rec.Code != http.StatusOK {
		t.Errorf("real key after a wrong-secret attempt = %d, want 200", rec.Code)
	}
}
