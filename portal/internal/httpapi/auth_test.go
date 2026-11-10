// auth_test.go — hermetic tests for the session middleware: cookie
// flags (PORTAL_PLAN 6/S1 done-when), the CSRF double-submit pair,
// and the auth error envelope. The session store is faked so these
// run without a database.
package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

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
