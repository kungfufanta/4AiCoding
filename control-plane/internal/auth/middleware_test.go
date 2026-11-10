package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubResolver answers from fixed maps.
type stubResolver struct {
	sessions map[string]string // cookie value -> owner
	keys     map[string]string // presented key -> owner
	scopes   map[string][]string
	roles    map[string]string // cookie or key -> role (default admin)
	bound    map[string]string // key -> sandbox id
}

func (s stubResolver) role(v string) string {
	if r, ok := s.roles[v]; ok {
		return r
	}
	return RoleAdmin
}

func (s stubResolver) ResolveSession(_ context.Context, v string) (Principal, bool) {
	o, ok := s.sessions[v]
	return Principal{Owner: o, User: "u-" + v, Role: s.role(v)}, ok
}
func (s stubResolver) ResolveAPIKey(_ context.Context, v string) (Principal, bool) {
	o, ok := s.keys[v]
	return Principal{Owner: o, User: "k-" + v, Role: s.role(v), Scopes: s.scopes[v], SandboxID: s.bound[v]}, ok
}

func newMW(disabled bool, res CredentialResolver) *Middleware {
	return NewMiddleware(&Config{Disabled: disabled}, res, nil, nil)
}

// echo handler records the actor it saw.
func echoActor(seen *Actor) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = ActorFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	})
}

func TestMiddlewareUniformEnforcement(t *testing.T) {
	res := stubResolver{
		sessions: map[string]string{"good-cookie": "default"},
		keys:     map[string]string{"good-key": "default"},
	}
	mw := newMW(false, res)

	cases := []struct {
		name       string
		path       string
		cookie     string
		bearer     string
		remoteAddr string
		wantCode   int
		wantKind   string
	}{
		{"no credential non-exempt", "/v1/apps", "", "", "203.0.113.9:5000", 401, ""},
		{"loopback no credential still 401", "/v1/apps", "", "", "127.0.0.1:5000", 401, ""},
		{"valid session cookie", "/v1/apps", "good-cookie", "", "203.0.113.9:5000", 200, "user"},
		{"valid bearer key", "/v1/apps", "", "good-key", "203.0.113.9:5000", 200, "service"},
		{"bad session + bad key", "/v1/apps", "nope", "nope", "203.0.113.9:5000", 401, ""},
		{"exempt path no credential", "/v1/auth/status", "", "", "203.0.113.9:5000", 200, "unknown"},
		{"version exempt no credential", "/version", "", "", "203.0.113.9:5000", 200, "unknown"},
		{"exempt path with session", "/v1/auth/status", "good-cookie", "", "203.0.113.9:5000", 200, "user"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seen Actor
			h := mw.Wrap(echoActor(&seen))
			r := httptest.NewRequest("GET", tc.path, nil)
			r.RemoteAddr = tc.remoteAddr
			if tc.cookie != "" {
				r.AddCookie(&http.Cookie{Name: SessionCookie, Value: tc.cookie})
			}
			if tc.bearer != "" {
				r.Header.Set("Authorization", "Bearer "+tc.bearer)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.wantCode {
				t.Fatalf("code = %d, want %d", w.Code, tc.wantCode)
			}
			if tc.wantCode == 200 && tc.wantKind != "" && seen.Kind != tc.wantKind {
				t.Fatalf("actor kind = %q, want %q", seen.Kind, tc.wantKind)
			}
		})
	}
}

func TestMiddlewareDisabledRollback(t *testing.T) {
	mw := newMW(true, nil)
	var seen Actor
	h := mw.Wrap(echoActor(&seen))
	r := httptest.NewRequest("GET", "/v1/apps", nil)
	r.RemoteAddr = "203.0.113.9:5000"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("disabled: code = %d, want 200", w.Code)
	}
	if seen.Name != "default" {
		t.Fatalf("disabled actor name = %q, want default", seen.Name)
	}
}

func TestScopeTable(t *testing.T) {
	allowed := []string{"GET /healthz", "GET /readyz", "GET /version", "GET /v1/auth/status"}
	for _, route := range allowed {
		m, p, _ := strings.Cut(route, " ")
		if !ScopesAllow([]string{ScopeHealth}, m, p) {
			t.Errorf("health scope should allow %s", route)
		}
	}
	denied := []string{"GET /v1/sandboxes", "GET /v1/api-keys", "DELETE /v1/api-keys/x", "GET /v1/upgrade", "POST /v1/auth/status", "GET /healthz/"}
	for _, route := range denied {
		m, p, _ := strings.Cut(route, " ")
		if ScopesAllow([]string{ScopeHealth}, m, p) {
			t.Errorf("health scope should deny %s", route)
		}
	}
	if !ScopesAllow(nil, "DELETE", "/v1/api-keys/x") {
		t.Error("empty scopes must be unrestricted")
	}
	if ScopesAllow([]string{"bogus"}, "GET", "/healthz") {
		t.Error("unknown scope must allow nothing")
	}
	if !ValidScope(ScopeHealth) || ValidScope("bogus") {
		t.Error("ValidScope")
	}
	if got := KnownScopes(); len(got) != 6 {
		t.Errorf("KnownScopes = %v", got)
	}
	if !ScopesAllow([]string{ScopeSandboxesExec}, "POST", "/v1/sandboxes/01ABC/exec") ||
		ScopesAllow([]string{ScopeSandboxesExec}, "DELETE", "/v1/sandboxes/01ABC") ||
		!ScopesAllow([]string{ScopeSandboxesRead}, "GET", "/v1/sandboxes/01ABC") ||
		ScopesAllow([]string{ScopeSandboxesRead}, "GET", "/v1/sandboxes/01ABC/logs") {
		t.Error("sandbox scope patterns")
	}
}

func TestMiddlewareScopedKey(t *testing.T) {
	res := stubResolver{
		keys:   map[string]string{"mon-key": "default", "full-key": "default"},
		scopes: map[string][]string{"mon-key": {ScopeHealth}},
	}
	mw := newMW(false, res)
	cases := []struct {
		method, path, bearer string
		want                 int
	}{
		{"GET", "/healthz", "mon-key", 200},
		{"GET", "/v1/sandboxes", "mon-key", 403},
		{"GET", "/v1/auth/status", "mon-key", 200},
		{"POST", "/v1/sandboxes", "mon-key", 403},
		{"GET", "/v1/api-keys", "mon-key", 403},
		{"GET", "/v1/apps", "full-key", 200},
	}
	for _, tc := range cases {
		var seen Actor
		h := mw.Wrap(echoActor(&seen))
		r := httptest.NewRequest(tc.method, tc.path, nil)
		r.RemoteAddr = "203.0.113.9:5000"
		r.Header.Set("Authorization", "Bearer "+tc.bearer)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("%s %s with %s: code = %d, want %d (%s)", tc.method, tc.path, tc.bearer, w.Code, tc.want, w.Body.String())
		}
		if tc.want == 403 && !strings.Contains(w.Body.String(), `"this API key is limited to: health"`) {
			t.Fatalf("403 body = %s", w.Body.String())
		}
		if tc.want == 200 && tc.bearer == "mon-key" && len(seen.Scopes) != 1 {
			t.Fatalf("actor scopes not propagated: %+v", seen)
		}
	}
}

func TestRoleTable(t *testing.T) {
	cases := []struct {
		role, method, path string
		want               bool
	}{
		{RoleAdmin, "PUT", "/v1/policy", true},
		{RoleOperator, "PUT", "/v1/policy", false},
		{RoleOperator, "GET", "/v1/policy", true},
		{RoleOperator, "POST", "/v1/sandboxes", true},
		{RoleOperator, "POST", "/v1/users", false},
		{RoleOperator, "DELETE", "/v1/users/x", false},
		{RoleOperator, "GET", "/v1/audit", false},
		{RoleOperator, "POST", "/v1/approvals/a1/approve", false},
		{RoleViewer, "GET", "/v1/sandboxes", true},
		{RoleViewer, "POST", "/v1/sandboxes", false},
		{RoleViewer, "POST", "/v1/sandboxes/x/exec", false},
		{RoleViewer, "POST", "/v1/auth/logout", true},
		{RoleViewer, "GET", "/v1/api-keys", false},
		{"", "GET", "/v1/sandboxes", false},
	}
	for _, c := range cases {
		if got := RoleAllows(c.role, c.method, c.path); got != c.want {
			t.Errorf("RoleAllows(%q, %s %s) = %v, want %v", c.role, c.method, c.path, got, c.want)
		}
	}
	if !SandboxBindingAllows("", "/v1/users") || !SandboxBindingAllows("S1", "/v1/sandboxes/S1/exec") ||
		SandboxBindingAllows("S1", "/v1/sandboxes/S12") || SandboxBindingAllows("S1", "/v1/sandboxes") {
		t.Error("sandbox binding")
	}
}

func TestMiddlewareRolesBindingAndCSRF(t *testing.T) {
	res := stubResolver{
		sessions: map[string]string{"admin-ck": "default", "viewer-ck": "default"},
		keys:     map[string]string{"bound-key": "default", "op-key": "default"},
		roles:    map[string]string{"viewer-ck": RoleViewer, "op-key": RoleOperator},
		bound:    map[string]string{"bound-key": "S1"},
	}
	mw := newMW(false, res)
	cases := []struct {
		name, method, path, cookie, bearer string
		csrf                               bool
		want                               int
	}{
		{"viewer can read", "GET", "/v1/sandboxes", "viewer-ck", "", false, 200},
		{"viewer cannot create", "POST", "/v1/sandboxes", "viewer-ck", "", true, 403},
		{"cookie POST without csrf header", "POST", "/v1/sandboxes", "admin-ck", "", false, 403},
		{"cookie POST with csrf header", "POST", "/v1/sandboxes", "admin-ck", "", true, 200},
		{"bearer POST needs no csrf header", "POST", "/v1/sandboxes", "", "op-key", false, 200},
		{"operator cannot change policy", "PUT", "/v1/policy", "", "op-key", false, 403},
		{"bound key on its sandbox", "POST", "/v1/sandboxes/S1/exec", "", "bound-key", false, 200},
		{"bound key on another sandbox", "POST", "/v1/sandboxes/S2/exec", "", "bound-key", false, 403},
		{"bound key cannot list", "GET", "/v1/sandboxes", "", "bound-key", false, 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seen Actor
			h := mw.Wrap(echoActor(&seen))
			r := httptest.NewRequest(tc.method, tc.path, nil)
			r.RemoteAddr = "203.0.113.9:5000"
			if tc.cookie != "" {
				r.AddCookie(&http.Cookie{Name: SessionCookie, Value: tc.cookie})
			}
			if tc.bearer != "" {
				r.Header.Set("Authorization", "Bearer "+tc.bearer)
			}
			if tc.csrf {
				r.Header.Set(CSRFHeader, "1")
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("code = %d, want %d (%s)", w.Code, tc.want, w.Body)
			}
		})
	}
}
