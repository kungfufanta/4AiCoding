// Review tests for P5b (#53), P6 (#55) and A2 (#57). Each one fails on
// the code as z.ai opened it.
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tastyeffectco/sandboxd/portal/internal/db"
	"github.com/tastyeffectco/sandboxd/portal/internal/llmconfig"
)

// A create that omits ttl_seconds and egress.mode must still reach
// sandboxd with the plan's TTL and egress mode, otherwise sandboxd's
// own defaults (which the plan never sees) apply.
func TestPlanDefaultsForwardedToSandboxd(t *testing.T) {
	srv, fake := setupP5(t)
	_, _, key, _ := newOrgWithKey(t, srv, "plan-defaults", "plan-defaults@example.test")
	rec := doV1(t, srv, http.MethodPost, "/v1/sandboxes", `{"image":"ubuntu:24.04"}`, key)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	life, _ := fake.lastCreate["lifecycle"].(map[string]any)
	if life == nil || life["idle_timeout_secs"] != float64(1800) {
		t.Errorf("lifecycle = %v, want idle_timeout_secs 1800 (free plan max_ttl_seconds)", fake.lastCreate["lifecycle"])
	}
	netw, _ := fake.lastCreate["network"].(map[string]any)
	if netw == nil || netw["egress_mode"] != "allowlist" {
		t.Errorf("network = %v, want egress_mode allowlist (free plan)", fake.lastCreate["network"])
	}
}

// createPlatformConnection makes one platform connection as the
// bootstrap admin and returns its id.
func createPlatformConnection(t *testing.T, srv *Server, adminSub string) string {
	t.Helper()
	cookies := loginA2Admin(t, srv, adminSub)
	body := `{"name":"platform-shared","protocol":"openai","flavor":"generic",` +
		`"base_url":"https://api.openai.com/v1","default_model":"gpt-4o",` +
		`"models":[],"key_mode":"shared","timeout_s":60,"max_tokens":4096,` +
		`"enabled":true,"is_default":false,"api_key":"sk-platform-secret-9999"}`
	rec := do(t, srv, http.MethodPost, "/api/admin/llm/connections", body, cookies...)
	if rec.Code != http.StatusCreated {
		t.Fatalf("platform create = %d: %s", rec.Code, rec.Body.String())
	}
	var c struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &c)
	return c.ID
}

func platformBaseURL(t *testing.T, srv *Server, cid string) (string, bool) {
	t.Helper()
	var u string
	err := db.WithTx(context.Background(), srv.Pool, func(tx db.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT base_url FROM model_connections WHERE id = $1::uuid`, cid).Scan(&u)
	})
	return u, err == nil
}

// Every user owns a personal org. An org owner must not be able to
// repoint or delete the platform connection through the org routes:
// repointing base_url would send the shared platform key to the
// owner's server on the next Test or chat call.
func TestOrgAdminCannotWritePlatformConnection(t *testing.T) {
	srv, _, adminSub := setupA2(t)
	cid := createPlatformConnection(t, srv, adminSub)
	orgID, cookies := newOrgWithCookies(t, srv, "a2-hijack", "a2-hijack@example.test")

	patch := `{"name":"hijacked","protocol":"openai","flavor":"generic",` +
		`"base_url":"https://203.0.113.10/v1","default_model":"gpt-4o",` +
		`"models":[],"key_mode":"shared","timeout_s":60,"max_tokens":4096,` +
		`"enabled":true,"is_default":false}`
	rec := do(t, srv, http.MethodPatch, "/api/orgs/"+orgID+"/llm/connections/"+cid, patch, cookies...)
	if rec.Code != http.StatusNotFound {
		t.Errorf("org PATCH of platform connection = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	if u, _ := platformBaseURL(t, srv, cid); u != "https://api.openai.com/v1" {
		t.Errorf("platform base_url changed to %q", u)
	}

	rec = do(t, srv, http.MethodDelete, "/api/orgs/"+orgID+"/llm/connections/"+cid, "", cookies...)
	if rec.Code != http.StatusNotFound {
		t.Errorf("org DELETE of platform connection = %d, want 404", rec.Code)
	}
	if _, ok := platformBaseURL(t, srv, cid); !ok {
		t.Errorf("platform connection was deleted by an org owner")
	}
}

// The database must refuse the same write even if a handler forgets
// the scope filter: in an org context, platform rows are read-only.
func TestRLSPlatformConnectionReadOnlyForOrgs(t *testing.T) {
	srv, _, adminSub := setupA2(t)
	cid := createPlatformConnection(t, srv, adminSub)
	orgID, _ := newOrgWithCookies(t, srv, "a2-rls", "a2-rls@example.test")
	var updated, deleted int64
	err := db.WithOrg(context.Background(), srv.Pool, orgID, func(tx db.Tx) error {
		tag, err := tx.Exec(context.Background(),
			`UPDATE model_connections SET base_url = 'https://x.example/v1' WHERE id = $1::uuid`, cid)
		if err != nil {
			return err
		}
		updated = tag.RowsAffected()
		tag, err = tx.Exec(context.Background(), `DELETE FROM model_connections WHERE id = $1::uuid`, cid)
		deleted = tag.RowsAffected()
		return err
	})
	if err != nil {
		t.Fatalf("org tx: %v", err)
	}
	if updated != 0 || deleted != 0 {
		t.Errorf("org context touched platform row: updated=%d deleted=%d", updated, deleted)
	}
}

// Org connections are called through a dialer that refuses private
// addresses at connect time, so a hostname that later resolves (or
// redirects) to the cluster network is still blocked.
func TestGuardedHTTPClientRefusesLoopback(t *testing.T) {
	fake := newFakeLLMProbe()
	defer fake.Close()
	c := llmconfig.GuardedHTTPClient(2 * time.Second)
	_, err := c.Get(fake.URL + "/v1/models")
	if err == nil || !strings.Contains(err.Error(), "private") {
		t.Fatalf("guarded GET to %s err = %v, want private-address refusal", fake.URL, err)
	}
}
