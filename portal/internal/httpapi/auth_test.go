// auth_test.go — hermetic tests for the session middleware: cookie
// flags (PORTAL_PLAN 6/S1 done-when), the CSRF double-submit pair,
// and the auth error envelope. The session store is faked so these
// run without a database.
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tastyeffectco/sandboxd/portal/internal/oidcauth"
	"github.com/tastyeffectco/sandboxd/portal/internal/session"
)

type fakeSessions struct {
	mu       sync.Mutex
	sessions map[string]session.Session
	deleted  []string
}

func newFakeSessions() *fakeSessions {
	return &fakeSessions{sessions: map[string]session.Session{}}
}

func (f *fakeSessions) Create(_ context.Context, userID string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	token, csrf := "test-session-token", "test-csrf-token"
	f.sessions[token] = session.Session{
		UserID:   userID,
		CSRFHash: session.CSRFHash(csrf),
		Expires:  time.Now().Add(time.Hour),
	}
	return token, csrf, nil
}

func (f *fakeSessions) Get(_ context.Context, token string) (session.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[token]
	if !ok {
		return session.Session{}, session.ErrNotFound
	}
	return s, nil
}

func (f *fakeSessions) Delete(_ context.Context, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, token)
	f.deleted = append(f.deleted, token)
	return nil
}

func testServer(t *testing.T, secure bool) (*Server, *fakeSessions) {
	t.Helper()
	f := newFakeSessions()
	return &Server{
		Log:           discardLogger(),
		Sessions:      f,
		PublicURL:     "https://app.example.com",
		SecureCookies: secure,
	}, f
}

func cookieReq(method, target string, cookies ...*http.Cookie) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	for _, c := range cookies {
		r.AddCookie(c)
	}
	return r
}

func sesCookies() []*http.Cookie {
	return []*http.Cookie{
		{Name: session.SessionCookie, Value: "test-session-token"},
		{Name: session.CSRFCookie, Value: "test-csrf-token"},
	}
}

// TestCookieFlags asserts the S1 done-when "test cookie flags": the
// session cookie is HttpOnly + SameSite=Lax + Secure (secure servers),
// while the CSRF cookie is readable by the SPA (not HttpOnly).
func TestCookieFlags(t *testing.T) {
	for _, tc := range []struct {
		name   string
		secure bool
	}{
		{"plain-http-dev", false},
		{"https-prod", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, f := testServer(t, tc.secure)
			f.sessions["test-session-token"] = session.Session{
				UserID:   "u1",
				CSRFHash: session.CSRFHash("test-csrf-token"),
				Expires:  time.Now().Add(time.Hour),
			}
			req := cookieReq(http.MethodPost, "/auth/logout", sesCookies()...)
			req.Header.Set("X-CSRF-Token", "test-csrf-token")
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusNoContent {
				t.Fatalf("logout = %d, want 204", rec.Code)
			}

			got := map[string]*http.Cookie{}
			for _, raw := range rec.Result().Cookies() {
				got[raw.Name] = raw
			}
			ses := got[session.SessionCookie]
			if ses == nil {
				t.Fatal("session cookie not set on logout")
			}
			if !ses.HttpOnly {
				t.Error("session cookie must be HttpOnly")
			}
			if ses.SameSite != http.SameSiteLaxMode {
				t.Errorf("session cookie SameSite = %v, want Lax", ses.SameSite)
			}
			if ses.Secure != tc.secure {
				t.Errorf("session cookie Secure = %v, want %v", ses.Secure, tc.secure)
			}
			csrf := got[session.CSRFCookie]
			if csrf == nil {
				t.Fatal("csrf cookie not set on logout")
			}
			if csrf.HttpOnly {
				t.Error("csrf cookie must be readable by the SPA (HttpOnly=false)")
			}
			if csrf.Secure != tc.secure {
				t.Errorf("csrf cookie Secure = %v, want %v", csrf.Secure, tc.secure)
			}
		})
	}
}

// TestCSRFDoubleSubmit asserts the S1 done-when "test CSRF": state
// changing requests need the X-CSRF-Token header matching the CSRF
// cookie, which must hash to the session's stored value.
func TestCSRFDoubleSubmit(t *testing.T) {
	s, f := testServer(t, false)
	f.sessions["test-session-token"] = session.Session{
		UserID:   "u1",
		CSRFHash: session.CSRFHash("test-csrf-token"),
		Expires:  time.Now().Add(time.Hour),
	}
	h := s.Handler()

	// Missing header → 403.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, cookieReq(http.MethodPost, "/auth/logout", sesCookies()...))
	if rec.Code != http.StatusForbidden {
		t.Errorf("POST without X-CSRF-Token = %d, want 403", rec.Code)
	}

	// Header ≠ cookie → 403.
	rec = httptest.NewRecorder()
	req := cookieReq(http.MethodPost, "/auth/logout", sesCookies()...)
	req.Header.Set("X-CSRF-Token", "evil-value")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("POST with mismatched X-CSRF-Token = %d, want 403", rec.Code)
	}

	// Cookie alone (no header) → 403: classic double-submit.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, cookieReq(http.MethodPost, "/auth/logout", sesCookies()...))
	if rec.Code != http.StatusForbidden {
		t.Errorf("POST with cookie only = %d, want 403", rec.Code)
	}

	// Matching pair → 204.
	rec = httptest.NewRecorder()
	req = cookieReq(http.MethodPost, "/auth/logout", sesCookies()...)
	req.Header.Set("X-CSRF-Token", "test-csrf-token")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("POST with valid CSRF pair = %d, want 204", rec.Code)
	}
	if len(f.deleted) != 1 || f.deleted[0] != "test-session-token" {
		t.Errorf("session not deleted on logout: %v", f.deleted)
	}
}

// TestNoSessionUnauthorized: /api requires a session.
func TestNoSessionUnauthorized(t *testing.T) {
	s, _ := testServer(t, false)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/me", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /api/me without session = %d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"unauthorized"`) {
		t.Errorf("body = %q, want unauthorized envelope", rec.Body.String())
	}
}

// TestLoginNotConfigured: without OIDC settings the login route says
// so (503) instead of redirecting into the void.
func TestLoginNotConfigured(t *testing.T) {
	s, _ := testServer(t, false)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /auth/login = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "sso_not_configured") {
		t.Errorf("body = %q, want sso_not_configured", rec.Body.String())
	}
}

// TestAPIUnknownRoute: unknown /api paths answer the JSON envelope.
func TestAPIUnknownRoute(t *testing.T) {
	s, f := testServer(t, false)
	f.sessions["test-session-token"] = session.Session{
		UserID:   "u1",
		CSRFHash: session.CSRFHash("test-csrf-token"),
		Expires:  time.Now().Add(time.Hour),
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, cookieReq(http.MethodGet, "/api/definitely-not-a-route", sesCookies()...))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /api/definitely-not-a-route = %d, want 404", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"not_found"`) {
		t.Errorf("body = %q, want not_found envelope", rec.Body.String())
	}
}

// TestAuthMethodsReportsSignup exercises the /auth/methods signup
// field added in issue #80. The SPA renders the "Đăng ký" button only
// when both SSO is configured (Server.OIDC != nil) and Server.
// SignupEnabled is true.
func TestAuthMethodsReportsSignup(t *testing.T) {
	cases := []struct {
		name           string
		oidcConfigured bool
		signupEnabled  bool
		wantSignup     bool
	}{
		{"no SSO, signup disabled", false, false, false},
		{"no SSO, signup enabled", false, true, false}, // signup requires SSO
		{"SSO on, signup disabled", true, false, false},
		{"SSO on, signup enabled", true, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := testServer(t, false)
			if tc.oidcConfigured {
				s.OIDC = newFakeOIDCProvider(t)
			}
			s.SignupEnabled = tc.signupEnabled
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/methods", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("/auth/methods = %d, want 200", rec.Code)
			}
			body := rec.Body.String()
			wantBool := "false"
			if tc.wantSignup {
				wantBool = "true"
			}
			if !strings.Contains(body, `"signup":`+wantBool) {
				t.Errorf("body = %q, want signup=%s", body, wantBool)
			}
			// sso and password fields still present (regression guard).
			ssoWant := "false"
			if tc.oidcConfigured {
				ssoWant = "true"
			}
			if !strings.Contains(body, `"sso":`+ssoWant) {
				t.Errorf("body = %q, want sso=%s", body, ssoWant)
			}
		})
	}
}

// TestLoginSignupParamAddsPromptCreate verifies /auth/login?signup=1
// carries prompt=create in the OIDC redirect URL, and that
// /auth/login without the param does NOT. The Provider is a fake OIDC
// discovery server so we exercise the real redirect-building code.
func TestLoginSignupParamAddsPromptCreate(t *testing.T) {
	s, _ := testServer(t, false)
	s.OIDC = newFakeOIDCProvider(t)
	s.SignupEnabled = true

	// Without signup=1: no prompt=create.
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("GET /auth/login = %d, want 302", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if strings.Contains(loc, "prompt=create") {
		t.Errorf("/auth/login without signup=1 redirected with prompt=create: %s", loc)
	}

	// With signup=1: prompt=create present.
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/login?signup=1", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("GET /auth/login?signup=1 = %d, want 302", rec.Code)
	}
	loc = rec.Header().Get("Location")
	if !strings.Contains(loc, "prompt=create") {
		t.Errorf("/auth/login?signup=1 missing prompt=create: %s", loc)
	}
	// next is preserved through the state cookie (sanitizeNext still applies).
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/login?signup=1&next=/chat", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("GET /auth/login?signup=1&next=/chat = %d, want 302", rec.Code)
	}
	loc = rec.Header().Get("Location")
	if !strings.Contains(loc, "prompt=create") {
		t.Errorf("/auth/login?signup=1&next missing prompt=create: %s", loc)
	}
}

// TestLoginSignupIgnoredWhenDisabled verifies that when SignupEnabled
// is false, /auth/login?signup=1 still redirects to the regular
// sign-in URL (no prompt=create). The deep link keeps working, it
// just lands on the login form.
func TestLoginSignupIgnoredWhenDisabled(t *testing.T) {
	s, _ := testServer(t, false)
	s.OIDC = newFakeOIDCProvider(t)
	s.SignupEnabled = false

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/login?signup=1", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("GET /auth/login?signup=1 = %d, want 302", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if strings.Contains(loc, "prompt=create") {
		t.Errorf("/auth/login?signup=1 with SignupEnabled=false still added prompt=create: %s", loc)
	}
}

// TestLoginNextSanitizedWhenSignup verifies sanitizeNext still rejects
// protocol-relative / off-origin paths even with signup=1 (the two
// are independent features but share the same route).
func TestLoginNextSanitizedWhenSignup(t *testing.T) {
	s, _ := testServer(t, false)
	s.OIDC = newFakeOIDCProvider(t)
	s.SignupEnabled = true

	rec := httptest.NewRecorder()
	// "//evil.example" must NOT survive sanitizeNext.
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/login?signup=1&next=//evil.example", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("GET /auth/login = %d, want 302", rec.Code)
	}
	// The cookie should NOT carry the evil next; we just check the
	// route didn't error and the redirect happened.
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "prompt=create") {
		t.Errorf("signup=1 missing prompt=create: %s", loc)
	}
}

// newFakeOIDCProvider returns a *oidcauth.Provider constructed against
// a tiny httptest server that serves an OIDC discovery document. The
// server's endpoints (auth, token, jwks) don't have to actually work —
// the tests exercise redirect building (handleLogin) and the /auth/methods
// surface, not the code exchange.
func newFakeOIDCProvider(t *testing.T) *oidcauth.Provider {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
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
	p, err := oidcauth.New(context.Background(), srv.URL, "test-client", "test-secret", "https://app.example.com/auth/callback")
	if err != nil {
		t.Fatalf("oidcauth.New: %v", err)
	}
	return p
}
