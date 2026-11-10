package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tastyeffectco/sandboxd/portal/internal/db"
	"github.com/tastyeffectco/sandboxd/portal/internal/session"
	"github.com/tastyeffectco/sandboxd/portal/internal/tenants"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// bff_test.go — S1 end-to-end through the real BFF with a real
// Postgres (PORTAL_TEST_DATABASE_URL; skipped when unset): sessions,
// org creation, project creation, invites, CSRF enforcement and the
// 404-foreign-org rule.
func setupBFF(t *testing.T) *Server {
	t.Helper()
	url := os.Getenv("PORTAL_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("PORTAL_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	// Migrations run as the owner login; queries then run as
	// portal_app (RLS enforced).
	admin, err := db.Open(ctx, discardLogger(), url, "")
	if err != nil {
		t.Fatalf("Open admin: %v", err)
	}
	if err := db.Migrate(ctx, admin, discardLogger()); err != nil {
		admin.Close()
		t.Fatalf("Migrate: %v", err)
	}
	admin.Close()

	pool, err := db.Open(ctx, discardLogger(), url, "portal_app")
	if err != nil {
		t.Fatalf("Open app: %v", err)
	}
	t.Cleanup(pool.Close)

	return &Server{
		Log:           discardLogger(),
		Pool:          pool,
		Sessions:      session.New(pool, time.Hour),
		PublicURL:     "https://portal.test",
		SecureCookies: true,
		InviteTTL:     time.Hour,
	}
}

// login creates a user directly in the store (SSO is external) and
// mints a real session, returning the cookies to send.
func login(t *testing.T, srv *Server, sub, email, name string) (userID string, sessionCookie, csrfCookie *http.Cookie) {
	t.Helper()
	ctx := context.Background()
	var id string
	if err := db.WithTx(ctx, srv.Pool, func(tx db.Tx) error {
		u, _, err := tenants.UpsertUser(ctx, tx, sub, email, name)
		id = u.ID
		return err
	}); err != nil {
		t.Fatalf("upsert user %s: %v", email, err)
	}
	token, csrf, err := srv.Sessions.Create(ctx, id)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return id,
		&http.Cookie{Name: session.SessionCookie, Value: token},
		&http.Cookie{Name: session.CSRFCookie, Value: csrf}
}

func do(t *testing.T, srv *Server, method, target, body string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, rdr)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	// Mirror the browser: the SPA echoes the CSRF cookie back as a
	// header on every state-changing request.
	if method != http.MethodGet {
		for _, c := range cookies {
			if c.Name == session.CSRFCookie {
				req.Header.Set("X-CSRF-Token", c.Value)
			}
		}
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func extractString(t *testing.T, body, key string) string {
	t.Helper()
	marker := `"` + key + `":"`
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("key %q not found in %s", key, body)
	}
	rest := body[i+len(marker):]
	j := strings.Index(rest, `"`)
	if j < 0 {
		t.Fatalf("unterminated value for %q in %s", key, body)
	}
	return rest[:j]
}

// TestBFFDashboardFlow walks the S1 surface: me → create org →
// create project → invite → accept, all through HTTP with real
// Postgres, and asserts a foreign org answers 404.
func TestBFFDashboardFlow(t *testing.T) {
	srv := setupBFF(t)

	_, alice, aliceCSRF := login(t, srv, "bff-alice-sub", "bff-alice@example.test", "Alice")
	_, bob, bobCSRF := login(t, srv, "bff-bob-sub", "bff-bob@example.test", "Bob")

	// /api/me returns the user.
	rec := do(t, srv, http.MethodGet, "/api/me", "", alice, aliceCSRF)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/me = %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "bff-alice@example.test") {
		t.Errorf("/api/me body missing email: %s", rec.Body.String())
	}

	// Alice creates an org.
	rec = do(t, srv, http.MethodPost, "/api/orgs", `{"name":"Acme Corp"}`, alice, aliceCSRF)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /api/orgs = %d: %s", rec.Code, rec.Body.String())
	}
	orgID := extractString(t, rec.Body.String(), "id")

	// Alice creates a project in her org.
	rec = do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/projects", `{"name":"Website"}`, alice, aliceCSRF)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST project = %d: %s", rec.Code, rec.Body.String())
	}

	// Bob must see 404 (not 403) on Alice's org — no existence leak.
	rec = do(t, srv, http.MethodGet, "/api/orgs/"+orgID+"/projects", "", bob, bobCSRF)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET foreign org projects = %d, want 404", rec.Code)
	}
	rec = do(t, srv, http.MethodGet, "/api/orgs/"+orgID+"/members", "", bob, bobCSRF)
	if rec.Code != http.StatusNotFound {
		t.Errorf("GET foreign org members = %d, want 404", rec.Code)
	}

	// Alice invites Bob; the token is returned once.
	rec = do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/invites", `{"email":"bff-bob@example.test","role":"member"}`, alice, aliceCSRF)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST invite = %d: %s", rec.Code, rec.Body.String())
	}
	inviteURL := extractString(t, rec.Body.String(), "url")
	token := inviteURL[strings.Index(inviteURL, "token=")+len("token="):]
	if token == "" {
		t.Fatalf("invite url missing token: %s", inviteURL)
	}

	// Bob accepts with the token from the link.
	rec = do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/invites/accept", `{"token":"`+token+`"}`, bob, bobCSRF)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST invite accept = %d: %s", rec.Code, rec.Body.String())
	}

	// Now Bob can read the org's members...
	rec = do(t, srv, http.MethodGet, "/api/orgs/"+orgID+"/members", "", bob, bobCSRF)
	if rec.Code != http.StatusOK {
		t.Errorf("GET members after accept = %d: %s", rec.Code, rec.Body.String())
	}
	// ...but not change roles (member, not admin → hidden as 404).
	rec = do(t, srv, http.MethodPatch, `/api/orgs/`+orgID+`/members/`+extractString(t, rec.Body.String(), "user_id"), `{"role":"admin"}`, bob, bobCSRF)
	if rec.Code != http.StatusNotFound {
		t.Errorf("PATCH role as member = %d, want 404", rec.Code)
	}

	// Audit trail records the actions.
	rec = do(t, srv, http.MethodGet, "/api/orgs/"+orgID+"/audit", "", alice, aliceCSRF)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET audit = %d: %s", rec.Code, rec.Body.String())
	}
	for _, action := range []string{"org.create", "project.create", "invite.create", "invite.accept"} {
		if !strings.Contains(rec.Body.String(), action) {
			t.Errorf("audit missing %q: %s", action, rec.Body.String())
		}
	}
}

// TestBFFCSRFEnforced: without the double-submit pair even a valid
// session cannot mutate anything.
func TestBFFCSRFEnforced(t *testing.T) {
	srv := setupBFF(t)
	_, alice, aliceCSRF := login(t, srv, "bff-csrf-sub", "bff-csrf@example.test", "Alice")

	mutate := func(header string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/orgs", strings.NewReader(`{"name":"X"}`))
		req.AddCookie(alice)
		req.AddCookie(aliceCSRF)
		if header != "" {
			req.Header.Set("X-CSRF-Token", header)
		}
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	if rec := mutate(""); rec.Code != http.StatusForbidden {
		t.Errorf("POST without header = %d, want 403", rec.Code)
	}
	if rec := mutate("evil"); rec.Code != http.StatusForbidden {
		t.Errorf("POST with evil header = %d, want 403", rec.Code)
	}
	if rec := mutate(aliceCSRF.Value); rec.Code != http.StatusCreated {
		t.Errorf("POST with valid pair = %d: %s", rec.Code, rec.Body.String())
	}
}
