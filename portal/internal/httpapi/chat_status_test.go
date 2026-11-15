// chat_status_test.go — issue #82: GET /api/orgs/{orgID}/chat/status
// must report agent_available + agent_reason so the Chat page can
// disable the Agent composer before the user sends a prompt that would
// land on "sandbox_unavailable".
//
// Cases (all SET ROLE portal_app, real Postgres):
//   - sandboxd not configured (Server.Sandboxd == nil) → agent_available=false, agent_reason=sandboxd_not_configured
//   - fake sandboxd /healthz returns 200 → agent_available=true (when LLM is configured)
//   - fake sandboxd dead (server closed) → agent_available=false, agent_reason=sandboxd_unreachable
//   - cache: two requests back-to-back → exactly one /healthz call
//   - llm not configured → agent_available=false, agent_reason=llm_not_configured
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tastyeffectco/sandboxd/portal/internal/db"
	"github.com/tastyeffectco/sandboxd/portal/internal/llm"
	"github.com/tastyeffectco/sandboxd/portal/internal/sandboxd"
)

// chatStatusJSON is the subset of /chat/status we assert on.
type chatStatusJSON struct {
	Enabled        bool     `json:"enabled"`
	Model          string   `json:"model"`
	Models         []string `json:"models"`
	AgentAvailable bool     `json:"agent_available"`
	AgentReason    string   `json:"agent_reason"`
}

// fakeSandboxdHealth returns an httptest server that counts /healthz
// hits and responds with the given status code. The handler is plain
// so the test can drive both "200 ok" and "500" paths.
func fakeSandboxdHealth(t *testing.T, status int) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(status)
		_, _ = w.Write([]byte("ok\n"))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// chatStatusFor issues GET /api/orgs/{orgID}/chat/status with the
// owner's cookies and decodes the JSON body.
func chatStatusFor(t *testing.T, srv *Server, orgID string, cookies []*http.Cookie) chatStatusJSON {
	t.Helper()
	rec := do(t, srv, http.MethodGet, "/api/orgs/"+orgID+"/chat/status", "", cookies...)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat/status = %d: %s", rec.Code, rec.Body.String())
	}
	var st chatStatusJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode chat/status: %v (%s)", err, rec.Body.String())
	}
	return st
}

// rawBody returns the recorder body bytes (used by the cache test to
// assert via string-contains without decoding).
func rawBody(rec *httptest.ResponseRecorder) []byte { return rec.Body.Bytes() }

// TestChatStatusAgentNotConfigured: Server.Sandboxd == nil →
// agent_available=false, agent_reason=sandboxd_not_configured.
func TestChatStatusAgentNotConfigured(t *testing.T) {
	srv := setupBFF(t)
	// LLM is configured so the "llm_not_configured" reason does not win.
	srv.LLM = &llm.Client{BaseURL: "http://example.test/v1", APIKey: "k", Model: "glm-4.6"}
	// Sandboxd is nil (default) — matches "PORTAL_SANDBOXD_URL empty".
	orgID, cookies := newOrgWithCookies(t, srv, "cs-not-cfg", "cs-not-cfg@example.test")
	st := chatStatusFor(t, srv, orgID, cookies)
	if st.AgentAvailable {
		t.Errorf("agent_available = true, want false (sandboxd nil)")
	}
	if st.AgentReason != "sandboxd_not_configured" {
		t.Errorf("agent_reason = %q, want sandboxd_not_configured", st.AgentReason)
	}
}

// TestChatStatusAgentReachable: a fake sandboxd answering /healthz 200
// → agent_available=true (when LLM is also configured).
func TestChatStatusAgentReachable(t *testing.T) {
	srv := setupBFF(t)
	srv.LLM = &llm.Client{BaseURL: "http://example.test/v1", APIKey: "k", Model: "glm-4.6"}
	fake, hits := fakeSandboxdHealth(t, http.StatusOK)
	srv.Sandboxd = sandboxd.New(fake.URL, "", 5*time.Second)
	orgID, cookies := newOrgWithCookies(t, srv, "cs-reach", "cs-reach@example.test")
	st := chatStatusFor(t, srv, orgID, cookies)
	if !st.AgentAvailable {
		t.Errorf("agent_available = false, want true (sandboxd /healthz 200); reason=%q", st.AgentReason)
	}
	if st.AgentReason != "" {
		t.Errorf("agent_reason = %q, want empty", st.AgentReason)
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("sandboxd /healthz hits = %d, want 1 on first call", n)
	}
}

// TestChatStatusAgentUnreachable: fake sandboxd closed →
// agent_available=false, agent_reason=sandboxd_unreachable.
func TestChatStatusAgentUnreachable(t *testing.T) {
	srv := setupBFF(t)
	srv.LLM = &llm.Client{BaseURL: "http://example.test/v1", APIKey: "k", Model: "glm-4.6"}
	fake, _ := fakeSandboxdHealth(t, http.StatusOK)
	// Point the client at the server, then close it so the dial fails.
	fake.Close()
	srv.Sandboxd = sandboxd.New(fake.URL, "", 5*time.Second)
	orgID, cookies := newOrgWithCookies(t, srv, "cs-unreach", "cs-unreach@example.test")
	st := chatStatusFor(t, srv, orgID, cookies)
	if st.AgentAvailable {
		t.Errorf("agent_available = true, want false (sandboxd dead)")
	}
	if st.AgentReason != "sandboxd_unreachable" {
		t.Errorf("agent_reason = %q, want sandboxd_unreachable", st.AgentReason)
	}
}

// TestChatStatusHealthCache: two back-to-back /chat/status requests
// must hit sandboxd /healthz exactly once (30 s cache).
func TestChatStatusHealthCache(t *testing.T) {
	srv := setupBFF(t)
	srv.LLM = &llm.Client{BaseURL: "http://example.test/v1", APIKey: "k", Model: "glm-4.6"}
	fake, hits := fakeSandboxdHealth(t, http.StatusOK)
	srv.Sandboxd = sandboxd.New(fake.URL, "", 5*time.Second)
	orgID, cookies := newOrgWithCookies(t, srv, "cs-cache", "cs-cache@example.test")

	// First poll: 1 health call.
	_ = chatStatusFor(t, srv, orgID, cookies)
	if n := hits.Load(); n != 1 {
		t.Fatalf("after 1st poll: /healthz hits = %d, want 1", n)
	}
	// Second poll immediately: cache hit, no new health call.
	_ = chatStatusFor(t, srv, orgID, cookies)
	if n := hits.Load(); n != 1 {
		t.Errorf("after 2nd poll: /healthz hits = %d, want 1 (30s cache)", n)
	}
	// Third poll immediately: still 1.
	_ = chatStatusFor(t, srv, orgID, cookies)
	if n := hits.Load(); n != 1 {
		t.Errorf("after 3rd poll: /healthz hits = %d, want 1 (30s cache)", n)
	}
}

// TestChatStatusLLMNotConfigured: no LLM (Server.LLM == nil and no DB
// connection) → agent_available=false, agent_reason=llm_not_configured.
// The LLM reason wins over sandboxd even when sandboxd is also nil,
// because it is the more actionable one for an operator.
//
// The test shares the database with other chat-status tests, some of
// which create a default platform connection (TestChatStatusAndSend
// ViaPlatformConnection). A default platform connection is visible to
// every org through the org_id IS NULL RLS arm, so this test removes
// any lingering default platform rows first to force enabled=false.
func TestChatStatusLLMNotConfigured(t *testing.T) {
	srv := setupBFF(t)
	// LLM is nil. Clear any default platform connections left by other
	// tests so ResolveEffective returns ErrNotFound for this org.
	if err := clearDefaultPlatformConnections(t, srv); err != nil {
		t.Fatal(err)
	}
	// Wire a fake sandboxd to prove the LLM reason wins over sandboxd.
	fake, _ := fakeSandboxdHealth(t, http.StatusOK)
	srv.Sandboxd = sandboxd.New(fake.URL, "", 5*time.Second)
	orgID, cookies := newOrgWithCookies(t, srv, "cs-no-llm", "cs-no-llm@example.test")
	st := chatStatusFor(t, srv, orgID, cookies)
	if st.AgentAvailable {
		t.Errorf("agent_available = true, want false (LLM not configured)")
	}
	if st.AgentReason != "llm_not_configured" {
		t.Errorf("agent_reason = %q, want llm_not_configured", st.AgentReason)
	}
}

// clearDefaultPlatformConnections removes platform model connections
// (org_id IS NULL) that earlier tests in the shared database may have
// left behind. The test suite shares one database, and a default
// platform connection is visible to every org; tests that need
// enabled=false must clear them first. Runs as the table owner (no
// RLS) via a fresh admin pool.
func clearDefaultPlatformConnections(t *testing.T, srv *Server) error {
	t.Helper()
	// Open as owner (no SET ROLE) so RLS does not block the DELETE of
	// platform rows. The shared test database is the one the pool
	// points at; re-use the same DSN by parsing it from the config the
	// suite set up. We reach for PORTAL_TEST_DATABASE_URL since that
	// is what setupBFF reads.
	url := os.Getenv("PORTAL_TEST_DATABASE_URL")
	if url == "" {
		return nil // tests skip without it anyway
	}
	adm, err := db.Open(context.Background(), discardLogger(), url, "")
	if err != nil {
		return err
	}
	defer adm.Close()
	_, err = adm.Exec(context.Background(),
		`DELETE FROM model_connections WHERE org_id IS NULL`)
	return err
}

// TestChatStatusBackwardCompat: the existing fields (enabled, model,
// models) are still present and correct. Regression guard for the
// issue #82 additions.
func TestChatStatusBackwardCompat(t *testing.T) {
	srv := setupBFF(t)
	srv.LLM = &llm.Client{BaseURL: "http://example.test/v1", APIKey: "k", Model: "glm-4.6"}
	orgID, cookies := newOrgWithCookies(t, srv, "cs-compat", "cs-compat@example.test")
	// Use the raw body to assert the JSON keys are present (decoding
	// alone would not catch a missing key when the value is the zero
	// value).
	rec := do(t, srv, http.MethodGet, "/api/orgs/"+orgID+"/chat/status", "", cookies...)
	body := string(rawBody(rec))
	for _, want := range []string{
		`"enabled":true`,
		`"model":"glm-4.6"`,
		`"agent_available":false`, // sandboxd nil → false
		`"agent_reason":"sandboxd_not_configured"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("chat/status body missing %q: %s", want, body)
		}
	}
}
