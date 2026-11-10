package httpapi

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/tastyeffectco/sandboxd/portal/internal/db"
	"github.com/tastyeffectco/sandboxd/portal/internal/oidcauth"
	"github.com/tastyeffectco/sandboxd/portal/internal/session"
	"github.com/tastyeffectco/sandboxd/portal/internal/tenants"
)

// Cookie builders (PORTAL_PLAN 3.1): HttpOnly + SameSite=Lax, Secure
// whenever the public URL is https. The CSRF cookie is intentionally
// readable by the SPA (double submit); the session cookie is not.
func (s *Server) cookie(name, value string, maxAge int, httpOnly bool) *http.Cookie {
	return &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: httpOnly,
		Secure:   s.SecureCookies,
		SameSite: http.SameSiteLaxMode,
	}
}

func (s *Server) sessionCookie(value string, maxAge int) *http.Cookie {
	return s.cookie(session.SessionCookie, value, maxAge, true)
}

func (s *Server) csrfCookie(value string, maxAge int) *http.Cookie {
	return s.cookie(session.CSRFCookie, value, maxAge, false)
}

// oidcStateCookie carries the login flow's state+PKCE pair to the
// callback (10 minutes, HttpOnly).
const oidcStateCookie = "portal_oidc"

func (s *Server) oidcCookie(value string, maxAge int) *http.Cookie {
	return s.cookie(oidcStateCookie, value, maxAge, true)
}

// withSession resolves the session cookie, validates the CSRF pair
// on state-changing methods and pins app.user_id into the request
// context. Everything under /api and /auth/logout flows through here.
func (s *Server) withSession(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := cookieValue(r, session.SessionCookie)
		if token == "" {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "sign in required")
			return
		}
		sess, err := s.Sessions.Get(r.Context(), token)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "unauthorized", "session expired or unknown")
			return
		}
		ctx := db.WithUserID(r.Context(), sess.UserID)

		if mutating(r.Method) {
			header := r.Header.Get("X-CSRF-Token")
			browser := cookieValue(r, session.CSRFCookie)
			if header == "" || browser == "" ||
				subtle.ConstantTimeCompare([]byte(header), []byte(browser)) != 1 ||
				session.CSRFHash(header) != sess.CSRFHash {
				writeErr(w, http.StatusForbidden, "csrf", "missing or invalid X-CSRF-Token")
				return
			}
		}
		next(w, r.WithContext(ctx))
	})
}

func mutating(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

func cookieValue(r *http.Request, name string) string {
	c, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return c.Value
}

// handleLogin starts the OIDC redirect (Authorization Code + PKCE,
// plan 3.1). Without an IdP configured it answers 503 so the SPA can
// show a clear message.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if s.OIDC == nil {
		writeErr(w, http.StatusServiceUnavailable, "sso_not_configured",
			"SSO is not configured (PORTAL_OIDC_ISSUER / _CLIENT_ID / _CLIENT_SECRET)")
		return
	}
	state := oidcauth.RandomToken()
	verifier, challenge := oidcauth.PKCEPair()
	nonce := oidcauth.RandomToken()

	blob, err := json.Marshal(map[string]string{
		"state": state, "verifier": verifier, "nonce": nonce,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "marshal login state")
		return
	}
	http.SetCookie(w, s.oidcCookie(base64.RawURLEncoding.EncodeToString(blob), 600))
	http.Redirect(w, r, s.OIDC.AuthCodeURL(state, challenge, nonce), http.StatusFound)
}

// handleCallback completes the login: verify state, exchange the
// code, upsert the user, create the personal org on first login,
// then mint the session cookies.
func (s *Server) handleCallback(w http.ResponseWriter, r *http.Request) {
	if s.OIDC == nil {
		writeErr(w, http.StatusServiceUnavailable, "sso_not_configured", "SSO is not configured")
		return
	}
	raw := cookieValue(r, oidcStateCookie)
	var flow struct {
		State    string `json:"state"`
		Verifier string `json:"verifier"`
		Nonce    string `json:"nonce"`
	}
	if blob, err := base64.RawURLEncoding.DecodeString(raw); err != nil ||
		json.Unmarshal(blob, &flow) != nil || flow.State == "" || flow.Verifier == "" {
		writeErr(w, http.StatusBadRequest, "login_flow", "unknown login flow, start again at /auth/login")
		return
	}
	// The state cookie is single-use no matter what happens next.
	defer http.SetCookie(w, s.oidcCookie("", -1))

	if r.URL.Query().Get("state") != flow.State {
		writeErr(w, http.StatusBadRequest, "state_mismatch", "login state mismatch")
		return
	}
	claims, err := s.OIDC.Exchange(r.Context(), r.URL.Query().Get("code"), flow.Verifier, flow.Nonce)
	if err != nil {
		s.Log.Warn("oidc exchange failed", "error", err)
		writeErr(w, http.StatusUnauthorized, "login_failed", "OIDC sign-in failed")
		return
	}
	email := strings.TrimSpace(claims.Email)
	if email == "" {
		writeErr(w, http.StatusUnauthorized, "login_failed", "ID token carries no email claim")
		return
	}

	// Step 1 (no org context): upsert the user; on first login also
	// mint the personal org id (users has no RLS; the org row itself
	// is created in step 2 inside BootstrapTx pinned to that id).
	var userID, newOrgID string
	err = db.WithTx(r.Context(), s.Pool, func(tx db.Tx) error {
		u, created, err := tenants.UpsertUser(r.Context(), tx, claims.Sub, email, claims.Name)
		if err != nil {
			return err
		}
		userID = u.ID
		if created {
			newOrgID, err = db.NewOrgID()
		}
		return err
	})
	if err == nil && newOrgID != "" {
		// Step 2: personal org + owner membership + audit, all under
		// the new org's RLS context (WITH CHECK sees id = app.org_id).
		err = db.BootstrapTx(r.Context(), s.Pool, newOrgID, func(btx db.Tx) error {
			slug, err := tenants.UniqueOrgSlug(r.Context(), btx, tenants.Slugify(emailLocalPart(email)))
			if err != nil {
				return err
			}
			name := claims.Name
			if name == "" {
				name = emailLocalPart(email)
			}
			if _, err := tenants.CreateOrg(r.Context(), btx, newOrgID, slug, name+"'s org", userID); err != nil {
				return err
			}
			meta, _ := json.Marshal(map[string]string{"org_id": newOrgID, "slug": slug})
			return tenants.Audit(r.Context(), btx, "user", userID, "org.create", newOrgID, string(meta))
		})
	}
	if err != nil {
		s.Log.Error("login bootstrap failed", slog.Any("error", err))
		writeErr(w, http.StatusInternalServerError, "internal", "could not complete sign-in")
		return
	}

	token, csrf, err := s.Sessions.Create(r.Context(), userID)
	if err != nil {
		s.Log.Error("session create failed", slog.Any("error", err))
		writeErr(w, http.StatusInternalServerError, "internal", "could not create session")
		return
	}
	maxAge := int((12 * time.Hour).Seconds())
	http.SetCookie(w, s.sessionCookie(token, maxAge))
	http.SetCookie(w, s.csrfCookie(csrf, maxAge))
	target := "/"
	if s.PublicURL != "" {
		target = s.PublicURL + "/"
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// handleLogout deletes the session and clears both cookies.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if token := cookieValue(r, session.SessionCookie); token != "" {
		if err := s.Sessions.Delete(r.Context(), token); err != nil && !errors.Is(err, session.ErrNotFound) {
			s.Log.Warn("logout delete failed", slog.Any("error", err))
		}
	}
	http.SetCookie(w, s.sessionCookie("", -1))
	http.SetCookie(w, s.csrfCookie("", -1))
	w.WriteHeader(http.StatusNoContent)
}

func emailLocalPart(email string) string {
	if i := strings.IndexByte(email, '@'); i > 0 {
		return email[:i]
	}
	return email
}
