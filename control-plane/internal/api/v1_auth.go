// v1_auth.go — the console's login authority. First-run admin setup (or the
// SANDBOXD_BOOTSTRAP_ADMIN env), username/password login → HttpOnly session
// cookie, logout, change-password, and the API-key-only console reset.
// Passwords are bcrypt-hashed; sessions are opaque random tokens stored as
// their sha256. All resolve to the single shared tenant (store.DefaultTenant).
package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/audit"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/auth"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/console"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

const sessionTTL = 30 * 24 * time.Hour

// minPasswordLen is the floor for a console password (defence-in-depth; the
// primary gate is that the API is unreachable without a credential at all).
const minPasswordLen = 8

// usersExist reports whether any user has been created.
func (s *Server) usersExist(r *http.Request) bool {
	if s.Store == nil {
		return false
	}
	n, err := s.Store.CountUsers(r.Context())
	return err == nil && n > 0
}

// GET /v1/auth/status — pre-login probe (exempt from the middleware).
// password_set is kept for older clients; it means "an admin exists".
func (s *Server) v1AuthStatus(w http.ResponseWriter, r *http.Request) {
	enabled := true
	if s.Auth != nil {
		enabled = !s.Auth.Snapshot().Disabled
	}
	actor := auth.ActorFrom(r.Context())
	authed := actor.Kind == "user" || actor.Kind == "service"
	exists := s.usersExist(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":       enabled,
		"authenticated": authed,
		"setup_needed":  !exists,
		"password_set":  exists,
	})
}

// GET /v1/auth/me — who the caller is.
func (s *Server) v1AuthMe(w http.ResponseWriter, r *http.Request) {
	a := auth.ActorFrom(r.Context())
	out := map[string]any{"kind": a.Kind, "user": a.User, "role": a.Role}
	if a.SandboxID != "" {
		out["sandbox_id"] = a.SandboxID
	}
	if len(a.Scopes) > 0 {
		out["scopes"] = a.Scopes
	}
	writeJSON(w, http.StatusOK, out)
}

type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func decodeCredentials(w http.ResponseWriter, r *http.Request) (credentials, bool) {
	var c credentials
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8*1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "invalid request body")
		return c, false
	}
	c.Username = strings.TrimSpace(c.Username)
	if c.Username == "" {
		c.Username = "admin" // single-admin installs and pre-0029 clients
	}
	return c, true
}

// POST /v1/auth/setup {username?, password} — first-run: create the first
// admin. 409 once any user exists.
func (s *Server) v1AuthSetup(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		writeV1Err(w, http.StatusServiceUnavailable, "unavailable", "auth store not configured")
		return
	}
	if s.usersExist(r) {
		writeV1Err(w, http.StatusConflict, "conflict", "an admin already exists; sign in instead")
		return
	}
	c, ok := decodeCredentials(w, r)
	if !ok {
		return
	}
	u, err := s.createUser(r, c.Username, c.Password, auth.RoleAdmin)
	if err != nil {
		writeUserErr(w, err)
		return
	}
	s.auditAction(r, audit.Entry{Action: "user.bootstrap", Target: u.ID, Detail: map[string]any{"username": u.Username}})
	if !s.issueSession(w, r, u) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// POST /v1/auth/login {username, password} — verify + set a session cookie.
func (s *Server) v1AuthLogin(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		writeV1Err(w, http.StatusServiceUnavailable, "unavailable", "auth store not configured")
		return
	}
	c, ok := decodeCredentials(w, r)
	if !ok {
		return
	}
	u, err := s.Store.GetUserByName(r.Context(), c.Username)
	if err != nil || u.Disabled || !console.CheckPassword(u.PasswordHash, c.Password) {
		s.auditAction(r, audit.Entry{Action: "auth.login_failed", Detail: map[string]any{"username": c.Username}})
		writeV1Err(w, http.StatusUnauthorized, "unauthorized", "invalid username or password")
		return
	}
	if !s.issueSession(w, r, u) {
		return
	}
	s.auditAction(r, audit.Entry{Action: "auth.login", Target: u.ID, Detail: map[string]any{"username": u.Username}})
	w.WriteHeader(http.StatusNoContent)
}

// POST /v1/auth/logout — revoke the current session + clear the cookie.
// With ?all=true it revokes every session of the current user.
func (s *Server) v1AuthLogout(w http.ResponseWriter, r *http.Request) {
	if s.Store != nil {
		if r.URL.Query().Get("all") == "true" {
			if u := s.currentUser(r); u != nil {
				_ = s.Store.DeleteUserSessions(r.Context(), u.ID)
			}
		} else if ck, err := r.Cookie(auth.SessionCookie); err == nil && ck.Value != "" {
			_ = s.Store.DeleteSession(r.Context(), console.HashToken(ck.Value))
		}
	}
	http.SetCookie(w, s.clearSessionCookie(r))
	w.WriteHeader(http.StatusNoContent)
}

// currentUser returns the session user of the request, or nil.
func (s *Server) currentUser(r *http.Request) *store.User {
	a := auth.ActorFrom(r.Context())
	if a.Kind != "user" || s.Store == nil {
		return nil
	}
	u, err := s.Store.GetUserByName(r.Context(), a.User)
	if err != nil {
		return nil
	}
	return u
}

// POST /v1/auth/password {current_password, new_password} — change your own
// password (session only). Signs you out everywhere, then re-issues a session.
func (s *Server) v1AuthPassword(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		writeV1Err(w, http.StatusServiceUnavailable, "unavailable", "auth store not configured")
		return
	}
	u := s.currentUser(r)
	if u == nil {
		writeV1Err(w, http.StatusForbidden, "forbidden", "change password requires a console login")
		return
	}
	var body struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8*1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "invalid request body")
		return
	}
	if !console.CheckPassword(u.PasswordHash, body.CurrentPassword) {
		writeV1Err(w, http.StatusUnauthorized, "unauthorized", "current password is incorrect")
		return
	}
	if len(body.NewPassword) < minPasswordLen {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "new password must be at least 8 characters")
		return
	}
	newHash, err := console.HashPassword(body.NewPassword)
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", "could not hash password")
		return
	}
	u.PasswordHash = newHash
	if err := s.Store.UpdateUser(r.Context(), u); err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", "could not save password")
		return
	}
	_ = s.Store.DeleteUserSessions(r.Context(), u.ID)
	s.auditAction(r, audit.Entry{Action: "user.password_changed", Target: u.ID})
	if !s.issueSession(w, r, u) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DELETE /v1/auth/password — recovery: remove every user so the console goes
// back to first-run setup. Only an admin API key (bearer) may call it: the
// operator proves control of the server by holding its key.
func (s *Server) v1AuthPasswordReset(w http.ResponseWriter, r *http.Request) {
	if s.Store == nil {
		writeV1Err(w, http.StatusServiceUnavailable, "unavailable", "auth store not configured")
		return
	}
	if auth.ActorFrom(r.Context()).Kind != "service" {
		writeV1Err(w, http.StatusForbidden, "forbidden", "console reset requires an admin API key")
		return
	}
	if err := s.Store.DeleteAllUsers(r.Context()); err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", "could not reset users")
		return
	}
	s.auditAction(r, audit.Entry{Action: "auth.console_reset"})
	w.WriteHeader(http.StatusNoContent)
}

// ── helpers ──────────────────────────────────────────────────────────

// issueSession mints a session, stores it, and sets the cookie. Writes an error
// response and returns false on failure.
func (s *Server) issueSession(w http.ResponseWriter, r *http.Request, u *store.User) bool {
	value, hash, err := console.NewSessionValue()
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", "could not create session")
		return false
	}
	now := time.Now()
	exp := now.Add(sessionTTL)
	if err := s.Store.CreateSession(r.Context(), hash, store.DefaultTenant, u.ID, now.Unix(), now.Unix(), exp.Unix()); err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", "could not persist session")
		return false
	}
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookie,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPS(r), // only over HTTPS — never break plain-HTTP test deployments
		Expires:  exp,
		MaxAge:   int(sessionTTL / time.Second),
	})
	return true
}

func (s *Server) clearSessionCookie(r *http.Request) *http.Cookie {
	return &http.Cookie{
		Name:     auth.SessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   isHTTPS(r),
		MaxAge:   -1,
	}
}

// isHTTPS reports whether the original client request was over TLS, honouring
// the Traefik/nginx X-Forwarded-Proto header (the control plane itself speaks
// plain HTTP behind the proxy).
func isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return r.Header.Get("X-Forwarded-Proto") == "https"
}
