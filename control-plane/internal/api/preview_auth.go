package api

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/audit"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/auth"
)

// previewCookieName carries the preview session JWT.
const previewCookieName = "sandbox_preview"

// flatAuthLandingPath is the path on a preview host where, in the flat
// host style, /forward-auth turns a fresh preview token into a host-only
// cookie (see handlePreviewAuth).
const flatAuthLandingPath = "/_sandboxd/preview-auth"

// handlePreviewAuth is the landing endpoint for a freshly minted
// upstream preview token. It validates the HS256 JWS, establishes the
// `sandbox_preview` cookie, and 302s to the allowlisted `return` URL.
// Reachable externally without the service token (the auth middleware
// exempts it) — it validates its own JWT.
//
// Cookie scope depends on the host style. Nested: one cookie on
// `.preview.<domain>`, a parent only this instance serves, so a single
// login covers every preview. Flat: the only shared parent is
// `.<domain>`, which the console, the API and any other instance using
// the same domain (other tags) also sit under — a cookie there would be
// sent to all of them. So in flat mode no cookie is set here at all;
// the browser is bounced to flatAuthLandingPath on the preview host
// itself, where /forward-auth sets a host-only cookie. Each preview
// host therefore authorises separately in flat mode.
func (s *Server) handlePreviewAuth(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tokenStr := q.Get("token")
	returnURL := q.Get("return")
	cfg := s.authCfg()
	hs := s.hosts()

	claims, err := auth.VerifyPreviewToken(tokenStr, cfg.PreviewSecrets, time.Now())
	if err != nil {
		// Do not surface the validation reason to the browser — 302
		// the user to the upstream auth URL to obtain a fresh token.
		sbID := parseReturnSandboxID(returnURL, hs)
		s.auditAction(r, audit.Entry{
			Action: "preview.access_denied",
			Target: sbID,
			Detail: map[string]any{"reason": "preview_auth_token_invalid"},
		})
		s.redirectToUpstreamAuth(w, r, cfg, sbID, returnURL)
		return
	}
	if !validateReturnURL(returnURL, claims.SandboxID, hs) {
		// An attacker-supplied `return` cannot point anywhere outside
		// this sandbox's own preview host or the API host.
		writeErr(w, http.StatusBadRequest, "return url not allowed for this sandbox")
		return
	}

	if hs.Flat() {
		if u, perr := url.Parse(returnURL); perr == nil && parseSandboxIDFromHost(u.Host, hs) != "" {
			landing := url.URL{Scheme: "https", Host: u.Host, Path: flatAuthLandingPath,
				RawQuery: url.Values{"token": {tokenStr}, "return": {returnURL}}.Encode()}
			http.Redirect(w, r, landing.String(), http.StatusFound)
			return
		}
		// A return to the API host needs no preview cookie.
		http.Redirect(w, r, returnURL, http.StatusFound)
		return
	}

	http.SetCookie(w, previewCookie(tokenStr, claims.Exp, hs.CookieDomain()))
	s.auditAction(r, audit.Entry{
		Action:         "preview.session_issued",
		Target:         claims.SandboxID,
		ExternalUserID: claims.Sub,
	})
	http.Redirect(w, r, returnURL, http.StatusFound)
}

// previewCookie builds the session cookie; an empty domain yields a
// host-only cookie (sent back to exactly the host that set it).
func previewCookie(token string, exp int64, domain string) *http.Cookie {
	maxAge := int(exp - time.Now().Unix())
	if maxAge < 1 {
		maxAge = 1
	}
	return &http.Cookie{
		Name:     previewCookieName,
		Value:    token,
		Path:     "/",
		Domain:   domain,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	}
}

// handleFlatAuthLanding is the flat-style counterpart of the cookie
// step in handlePreviewAuth. Traefik calls /forward-auth for
// `https://<preview host>/_sandboxd/preview-auth?token=..&return=..`;
// the response below is a non-2xx, which Traefik relays to the browser
// verbatim (headers included), so the Set-Cookie lands on the preview
// host's own origin with no Domain attribute. The return URL must be on
// that same host or the cookie would be useless — and it is never
// widened beyond it.
func (s *Server) handleFlatAuthLanding(w http.ResponseWriter, r *http.Request, id, fwdHost string, q url.Values) {
	cfg := s.authCfg()
	tokenStr := q.Get("token")
	returnURL := q.Get("return")
	claims, err := auth.VerifyPreviewToken(tokenStr, cfg.PreviewSecrets, time.Now())
	if err != nil || !strings.EqualFold(claims.SandboxID, id) {
		s.auditAction(r, audit.Entry{
			Action: "preview.access_denied",
			Target: id,
			Detail: map[string]any{"reason": "preview_auth_token_invalid"},
		})
		s.forwardAuthDeny(w, r, id, fwdHost, "/")
		return
	}
	u, perr := url.Parse(returnURL)
	if perr != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, fwdHost) {
		writeForwardAuthError(w, http.StatusBadRequest, id, "return url not allowed for this sandbox")
		return
	}
	http.SetCookie(w, previewCookie(tokenStr, claims.Exp, ""))
	s.auditAction(r, audit.Entry{
		Action:         "preview.session_issued",
		Target:         claims.SandboxID,
		ExternalUserID: claims.Sub,
	})
	s.forwardAuthRedirect(w, r, returnURL)
}

// redirectToUpstreamAuth 302s the browser to SANDBOXD_AUTH_REDIRECT_URL
// with {sandbox_id} and {return} substituted. When no redirect URL is
// configured it falls back to a plain 401.
func (s *Server) redirectToUpstreamAuth(w http.ResponseWriter, r *http.Request, cfg *auth.Config, sandboxID, returnURL string) {
	if cfg.AuthRedirectURL == "" {
		writeErr(w, http.StatusUnauthorized, "preview token invalid and no auth redirect configured")
		return
	}
	http.Redirect(w, r,
		auth.BuildRedirectURL(cfg.AuthRedirectURL, sandboxID, returnURL),
		http.StatusFound)
}
