// Package httpapi serves the Portal HTTP surface: the health probes
// (/healthz, /readyz), the dashboard BFF (/auth/*, /api/*) and — from
// S3 — the public /v1 API. The BFF shares cookies with the embedded
// SPA on one origin (PORTAL_PLAN 3.1).
package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tastyeffectco/sandboxd/portal/internal/agenttoken"
	"github.com/tastyeffectco/sandboxd/portal/internal/apikeys"
	"github.com/tastyeffectco/sandboxd/portal/internal/llm"
	"github.com/tastyeffectco/sandboxd/portal/internal/oidcauth"
	"github.com/tastyeffectco/sandboxd/portal/internal/session"
)

// SessionStore is the part of session.Manager the BFF needs; an
// interface so tests can fake it.
type SessionStore interface {
	Create(ctx context.Context, userID string) (token, csrf string, err error)
	Get(ctx context.Context, token string) (session.Session, error)
	Delete(ctx context.Context, token string) error
}

// Server wires the dependencies for the HTTP handlers.
type Server struct {
	Log  *slog.Logger
	Pool *pgxpool.Pool

	Sessions SessionStore
	// OIDC is nil until SSO is configured; /auth/login then answers
	// 503 sso_not_configured.
	OIDC *oidcauth.Provider
	// PasswordLogin enables the local username/password sign-in
	// (POST /auth/login) and surfaces it on GET /auth/methods. The
	// bootstrap admin is created at boot by passwordauth when set
	// (cmd/portal main).
	PasswordLogin bool

	// Keys verifies Bearer API keys (POST /v1/tokens) and caches
	// results for 60 s (PORTAL_PLAN 3.2). Tokens mints the agent
	// tokens. Both are wired in main; nil disables /v1/tokens.
	Keys   *apikeys.Verifier
	Tokens *agenttoken.Signer

	// LLM answers the Chat page; nil (no PORTAL_DEFAULT_LLM_API_KEY)
	// makes sending a message answer 503 chat_not_configured.
	LLM *llm.Client

	PublicURL     string // e.g. https://app.example.com (no trailing slash)
	SecureCookies bool   // derived in main from PublicURL's scheme
	InviteTTL     time.Duration
	// SessionTTL sets the cookie lifetime; it should match the
	// session store's TTL (PORTAL_SESSION_TTL). Zero means 12h.
	SessionTTL time.Duration
}

func (s *Server) sessionTTL() time.Duration {
	if s.SessionTTL > 0 {
		return s.SessionTTL
	}
	return 12 * time.Hour
}

// Handler returns the root mux with every route registered. Callers
// may mount more handlers (the SPA in main) on the returned mux.
func (s *Server) Handler() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)

	// Auth flow (browser redirects, no session yet). GET /auth/login
	// is the SSO redirect; POST /auth/login is the local
	// username/password sign-in (both exist side by side).
	mux.Handle("GET /auth/login", http.HandlerFunc(s.handleLogin))
	mux.Handle("POST /auth/login", http.HandlerFunc(s.handlePasswordLogin))
	mux.Handle("GET /auth/methods", http.HandlerFunc(s.handleAuthMethods))
	mux.Handle("GET /auth/callback", http.HandlerFunc(s.handleCallback))
	mux.Handle("POST /auth/logout", s.withSession(s.handleLogout))

	// Dashboard BFF. Every route sits behind the session+CSRF
	// middleware; unknown /api paths fall through to the JSON 404.
	mux.Handle("GET /api/me", s.withSession(http.HandlerFunc(s.handleMe)))
	mux.Handle("POST /api/orgs", s.withSession(http.HandlerFunc(s.handleCreateOrg)))
	mux.Handle("GET /api/orgs/{orgID}/projects", s.withSession(http.HandlerFunc(s.handleListProjects)))
	mux.Handle("POST /api/orgs/{orgID}/projects", s.withSession(http.HandlerFunc(s.handleCreateProject)))
	mux.Handle("GET /api/orgs/{orgID}/members", s.withSession(http.HandlerFunc(s.handleListMembers)))
	mux.Handle("PATCH /api/orgs/{orgID}/members/{userID}", s.withSession(http.HandlerFunc(s.handleSetMemberRole)))
	mux.Handle("POST /api/orgs/{orgID}/invites", s.withSession(http.HandlerFunc(s.handleCreateInvite)))
	mux.Handle("POST /api/orgs/{orgID}/invites/accept", s.withSession(http.HandlerFunc(s.handleAcceptInvite)))
	mux.Handle("GET /api/orgs/{orgID}/audit", s.withSession(http.HandlerFunc(s.handleListAudit)))
	// API keys (PORTAL_PLAN 6/S2): dashboard management.
	mux.Handle("GET /api/orgs/{orgID}/api-keys", s.withSession(http.HandlerFunc(s.handleListApiKeys)))
	mux.Handle("POST /api/orgs/{orgID}/api-keys", s.withSession(http.HandlerFunc(s.handleCreateApiKey)))
	mux.Handle("DELETE /api/orgs/{orgID}/api-keys/{keyID}", s.withSession(http.HandlerFunc(s.handleRevokeApiKey)))
	// Chat MVP: conversations with the platform LLM (SSE replies).
	mux.Handle("GET /api/chat/status", s.withSession(http.HandlerFunc(s.handleChatStatus)))
	mux.Handle("GET /api/orgs/{orgID}/chats", s.withSession(http.HandlerFunc(s.handleListChats)))
	mux.Handle("POST /api/orgs/{orgID}/chats", s.withSession(http.HandlerFunc(s.handleCreateChat)))
	mux.Handle("DELETE /api/orgs/{orgID}/chats/{chatID}", s.withSession(http.HandlerFunc(s.handleDeleteChat)))
	mux.Handle("GET /api/orgs/{orgID}/chats/{chatID}/messages", s.withSession(http.HandlerFunc(s.handleListChatMessages)))
	mux.Handle("POST /api/orgs/{orgID}/chats/{chatID}/messages", s.withSession(http.HandlerFunc(s.handleSendChatMessage)))
	// Public API surface (S2: token exchange; S3 grows here).
	mux.Handle("POST /v1/tokens", http.HandlerFunc(s.handleAgentToken))
	mux.HandleFunc("/api/", NotFound)
	mux.HandleFunc("/auth/", NotFound)
	mux.HandleFunc("/v1/", NotFound)
	return mux
}

// writeJSON renders body with status.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeErr renders the shared error envelope
// {"error":{"code":"...","message":"..."}} (PORTAL_PLAN 5).
func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"code": code, "message": msg},
	})
}
