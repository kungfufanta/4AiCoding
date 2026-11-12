// password_test.go — the local username/password login (bootstrap
// admin): DB-backed like bff_test.go, driving the real mux end to end
// (auth methods, sign-in, cookie flags, the CSRF pair usability,
// rotation via EnsureBootstrap, disabled mode).
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/tastyeffectco/sandboxd/portal/internal/db"
	"github.com/tastyeffectco/sandboxd/portal/internal/passwordauth"
	"github.com/tastyeffectco/sandboxd/portal/internal/session"
)

const (
	adminUsername = "admin"
	adminPassword = "test-admin-password"
	adminEmail    = "admin@portal.test"
)

// setupPasswordBFF returns a Server with the password login enabled
// and the bootstrap admin ensured, exactly like main.go does at boot:
// migrations first (setupBFF), then EnsureBootstrap as the owner
// login, then the portal_app serving pool.
func setupPasswordBFF(t *testing.T) *Server {
	t.Helper()
	url := os.Getenv("PORTAL_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("PORTAL_TEST_DATABASE_URL not set")
	}
	srv := setupBFF(t)
	srv.PasswordLogin = true
	admin, err := db.Open(context.Background(), discardLogger(), url, "")
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer admin.Close()
	if _, err := passwordauth.EnsureBootstrap(context.Background(), admin,
		discardLogger(), adminUsername, adminPassword, adminEmail); err != nil {
		t.Fatalf("EnsureBootstrap: %v", err)
	}
	return srv
}

func doReq(t *testing.T, h http.Handler, method, target string, body any, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	return doReqHdr(t, h, method, target, body, nil, cookies...)
}

func doReqHdr(t *testing.T, h http.Handler, method, target string, body any, headers map[string]string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != nil {
		var buf bytes.Buffer
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
		rd = &buf
	}
	r := httptest.NewRequest(method, target, rd)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func bodyOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	return rec.Body.String()
}

func cookieNamed(t *testing.T, rec *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("cookie %s not set; got %v", name, rec.Result().Cookies())
	return nil
}

func TestPasswordLoginMethods(t *testing.T) {
	srv := setupPasswordBFF(t)
	h := srv.Handler()

	rec := doReq(t, h, http.MethodGet, "/auth/methods", nil)
	if rec.Code != 200 {
		t.Fatalf("methods status = %d, body %s", rec.Code, bodyOf(t, rec))
	}
	var methods struct {
		SSO      bool `json:"sso"`
		Password bool `json:"password"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &methods); err != nil {
		t.Fatalf("methods body %q: %v", rec.Body.String(), err)
	}
	if methods.SSO || !methods.Password {
		t.Errorf("methods = %+v, want sso=false (no IdP in tests) password=true", methods)
	}
}

func TestPasswordLoginEndToEnd(t *testing.T) {
	srv := setupPasswordBFF(t)
	h := srv.Handler()

	// Sign in.
	rec := doReq(t, h, http.MethodPost, "/auth/login",
		map[string]string{"username": adminUsername, "password": adminPassword})
	if rec.Code != 200 {
		t.Fatalf("login status = %d, body %s", rec.Code, bodyOf(t, rec))
	}
	if !strings.Contains(bodyOf(t, rec), `"ok":true`) {
		t.Errorf("login body = %q, want ok:true", bodyOf(t, rec))
	}
	sess := cookieNamed(t, rec, session.SessionCookie)
	csrf := cookieNamed(t, rec, session.CSRFCookie)
	if !sess.HttpOnly {
		t.Error("session cookie must be HttpOnly")
	}
	if !sess.Secure {
		t.Error("session cookie must be Secure when PublicURL is https")
	}
	if sess.SameSite != http.SameSiteLaxMode {
		t.Errorf("session cookie SameSite = %v, want Lax", sess.SameSite)
	}
	if csrf.HttpOnly {
		t.Error("CSRF cookie must stay readable by the SPA (double submit)")
	}

	// The session reads /api/me and passes a CSRF-protected mutation.
	me := doReq(t, h, http.MethodGet, "/api/me", nil, sess)
	if me.Code != 200 {
		t.Fatalf("/api/me status = %d, body %s", me.Code, bodyOf(t, me))
	}
	if !strings.Contains(bodyOf(t, me), adminEmail) {
		t.Errorf("/api/me body = %q, want admin email", bodyOf(t, me))
	}
	org := doReqHdr(t, h, http.MethodPost, "/api/orgs",
		map[string]string{"name": "Password Login Smoke"},
		map[string]string{"X-CSRF-Token": csrf.Value}, sess, csrf)
	if org.Code != http.StatusCreated {
		t.Errorf("POST /api/orgs with the fresh session status = %d, body %s", org.Code, bodyOf(t, org))
	}
}

func TestPasswordLoginRejected(t *testing.T) {
	srv := setupPasswordBFF(t)
	h := srv.Handler()

	for _, tc := range []struct {
		name       string
		username   string
		password   string
		wantStatus int
		wantCode   string
	}{
		{"wrong password", adminUsername, "nope", 401, "invalid_credentials"},
		{"unknown user", "nobody", "whatever", 401, "invalid_credentials"},
		{"missing username", "", adminPassword, 400, "invalid_request"},
		{"missing password", adminUsername, "", 400, "invalid_request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := doReq(t, h, http.MethodPost, "/auth/login",
				map[string]string{"username": tc.username, "password": tc.password})
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, bodyOf(t, rec))
			}
			var got struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("body %q: %v", rec.Body.String(), err)
			}
			if got.Error.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", got.Error.Code, tc.wantCode)
			}
		})
	}

	// Unknown user and wrong password share the exact same error
	// envelope so the endpoint cannot be used to enumerate accounts.
	miss := doReq(t, h, http.MethodPost, "/auth/login",
		map[string]string{"username": "ghost", "password": "x"})
	wrong := doReq(t, h, http.MethodPost, "/auth/login",
		map[string]string{"username": adminUsername, "password": "x"})
	if bodyOf(t, miss) != bodyOf(t, wrong) {
		t.Errorf("unknown-user body %q != wrong-password body %q; accounts are enumerable", bodyOf(t, miss), bodyOf(t, wrong))
	}

	// CITEXT: the username comparison is case-insensitive.
	rec := doReq(t, h, http.MethodPost, "/auth/login",
		map[string]string{"username": "ADMIN", "password": adminPassword})
	if rec.Code != 200 {
		t.Errorf("uppercase username status = %d, body %s", rec.Code, bodyOf(t, rec))
	}

	// Malformed JSON is a request error, not a credential failure.
	bad := doReq(t, h, http.MethodPost, "/auth/login", nil)
	bad = doReq(t, h, http.MethodPost, "/auth/login", nil)
	if bad.Code != 400 {
		t.Errorf("missing body status = %d, want 400", bad.Code)
	}
}

func TestPasswordLoginRotation(t *testing.T) {
	srv := setupPasswordBFF(t)
	h := srv.Handler()

	// main.go runs EnsureBootstrap on every boot with the configured
	// password — rotation is "change the env, redeploy". Prove the
	// stored hash converges to the new value and back again.
	url := os.Getenv("PORTAL_TEST_DATABASE_URL")
	admin, err := db.Open(context.Background(), discardLogger(), url, "")
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer admin.Close()
	// A session opened with the old password must not survive the
	// rotation.
	before := doReq(t, h, http.MethodPost, "/auth/login",
		map[string]string{"username": adminUsername, "password": adminPassword})
	if before.Code != 200 {
		t.Fatalf("login before rotation status = %d, body %s", before.Code, bodyOf(t, before))
	}
	oldSess := cookieNamed(t, before, session.SessionCookie)
	if me := doReq(t, h, http.MethodGet, "/api/me", nil, oldSess); me.Code != 200 {
		t.Fatalf("/api/me before rotation status = %d", me.Code)
	}
	if _, err := passwordauth.EnsureBootstrap(context.Background(), admin,
		discardLogger(), adminUsername, "rotated-pass", adminEmail); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if me := doReq(t, h, http.MethodGet, "/api/me", nil, oldSess); me.Code != 401 {
		t.Errorf("/api/me with a pre-rotation session status = %d, want 401", me.Code)
	}

	old := doReq(t, h, http.MethodPost, "/auth/login",
		map[string]string{"username": adminUsername, "password": adminPassword})
	if old.Code != 401 {
		t.Errorf("old password after rotation status = %d, want 401", old.Code)
	}
	rotated := doReq(t, h, http.MethodPost, "/auth/login",
		map[string]string{"username": adminUsername, "password": "rotated-pass"})
	if rotated.Code != 200 {
		t.Errorf("rotated password status = %d, body %s", rotated.Code, bodyOf(t, rotated))
	}

	// Converging back is also just a boot.
	if _, err := passwordauth.EnsureBootstrap(context.Background(), admin,
		discardLogger(), adminUsername, adminPassword, adminEmail); err != nil {
		t.Fatalf("rotate back: %v", err)
	}
	again := doReq(t, h, http.MethodPost, "/auth/login",
		map[string]string{"username": adminUsername, "password": adminPassword})
	if again.Code != 200 {
		t.Errorf("original password after rotate-back status = %d, body %s", again.Code, bodyOf(t, again))
	}
}

func TestPasswordLoginDisabled(t *testing.T) {
	srv := setupBFF(t) // PasswordLogin stays false (zero value)
	h := srv.Handler()

	rec := doReq(t, h, http.MethodPost, "/auth/login",
		map[string]string{"username": adminUsername, "password": adminPassword})
	if rec.Code != 404 {
		t.Fatalf("disabled login status = %d, want 404 (body %s)", rec.Code, bodyOf(t, rec))
	}
	if !strings.Contains(bodyOf(t, rec), "not_found") {
		t.Errorf("disabled login body = %q, want not_found", bodyOf(t, rec))
	}
	methods := doReq(t, h, http.MethodGet, "/auth/methods", nil)
	if !strings.Contains(bodyOf(t, methods), `"password":false`) {
		t.Errorf("methods body = %q, want password:false", bodyOf(t, methods))
	}
}

// TestPasswordLoginDisableAll covers the boot path with
// PORTAL_ADMIN_PASSWORD unset: stored logins are removed and sessions
// opened with them end immediately.
func TestPasswordLoginDisableAll(t *testing.T) {
	srv := setupPasswordBFF(t)
	h := srv.Handler()
	rec := doReq(t, h, http.MethodPost, "/auth/login",
		map[string]string{"username": adminUsername, "password": adminPassword})
	if rec.Code != 200 {
		t.Fatalf("login status = %d, body %s", rec.Code, bodyOf(t, rec))
	}
	sess := cookieNamed(t, rec, session.SessionCookie)

	admin, err := db.Open(context.Background(), discardLogger(), os.Getenv("PORTAL_TEST_DATABASE_URL"), "")
	if err != nil {
		t.Fatalf("open admin pool: %v", err)
	}
	defer admin.Close()
	if err := passwordauth.DisableAll(context.Background(), admin, discardLogger()); err != nil {
		t.Fatalf("DisableAll: %v", err)
	}
	if me := doReq(t, h, http.MethodGet, "/api/me", nil, sess); me.Code != 401 {
		t.Errorf("/api/me after DisableAll status = %d, want 401", me.Code)
	}
	// Even with the endpoint still on, the old credentials are gone.
	again := doReq(t, h, http.MethodPost, "/auth/login",
		map[string]string{"username": adminUsername, "password": adminPassword})
	if again.Code != 401 {
		t.Errorf("login after DisableAll status = %d, want 401", again.Code)
	}
}
