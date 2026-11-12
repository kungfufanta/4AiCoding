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
	"github.com/tastyeffectco/sandboxd/portal/internal/ratelimit"
	"github.com/tastyeffectco/sandboxd/portal/internal/sandboxd"
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
	// makes sending a message answer 503 chat_not_configured. A2
	// replaces this with per-org model connections resolved at chat
	// time; the env-based client is kept as a fallback for dev stacks
	// that have no DB connection configured.
	LLM *llm.Client
	// LLMHTTP is the HTTP client used for LLM test/models probes. nil
	// = default with a 10s timeout. Tests override it to point at a
	// fake httptest.Server.
	LLMHTTP *http.Client
	// PlatformAdminEnabled gates the /api/admin/llm/* routes. When
	// false, those routes answer 403. The bootstrap admin (password
	// login) is the only platform admin until a platform_admins table
	// is added.
	PlatformAdminEnabled bool
	// BootstrapAdminID is the user id of the bootstrap admin (from
	// passwordauth). requirePlatformAdmin checks the session user id
	// against this.
	BootstrapAdminID string
	// Sandboxd is the gateway client for the S3 surface; nil disables
	// /v1/sandboxes* (503) and the BFF Sandboxes page reports
	// "not configured". WebhookSecret enables the lifecycle webhook
	// receiver; without it the route is not registered at all.
	Sandboxd      *sandboxd.Client
	WebhookSecret string

	PublicURL     string // e.g. https://app.example.com (no trailing slash)
	SecureCookies bool   // derived in main from PublicURL's scheme
	InviteTTL     time.Duration
	// SessionTTL sets the cookie lifetime; it should match the
	// session store's TTL (PORTAL_SESSION_TTL). Zero means 12h.
	SessionTTL time.Duration

	// QuotaEnabled gates the P5 concurrent-sandbox ceiling check at
	// sandbox create time (false = behave exactly like pre-P5; the
	// Usage page still reads whatever usage_events exist). Tests use
	// false to isolate other behaviour from the plan catalogue.
	QuotaEnabled bool
	// Limiter applies the per-key token bucket from the org's plan
	// (P5). nil disables rate limiting entirely (the existing P4
	// behaviour, kept for tests that pre-date P5).
	Limiter *ratelimit.Limiter
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
	// A2: the status route is org-scoped so ResolveEffective can
	// see DB-configured org/platform connections (the legacy
	// /api/chat/status path left orgID empty and ResolveEffective
	// returned "empty org id", which made the chat textarea appear
	// disabled even after the admin added a platform connection).
	mux.Handle("GET /api/orgs/{orgID}/chat/status", s.withSession(http.HandlerFunc(s.handleChatStatus)))
	mux.Handle("GET /api/orgs/{orgID}/chats", s.withSession(http.HandlerFunc(s.handleListChats)))
	mux.Handle("POST /api/orgs/{orgID}/chats", s.withSession(http.HandlerFunc(s.handleCreateChat)))
	mux.Handle("DELETE /api/orgs/{orgID}/chats/{chatID}", s.withSession(http.HandlerFunc(s.handleDeleteChat)))
	mux.Handle("GET /api/orgs/{orgID}/chats/{chatID}/messages", s.withSession(http.HandlerFunc(s.handleListChatMessages)))
	mux.Handle("POST /api/orgs/{orgID}/chats/{chatID}/messages", s.withSession(http.HandlerFunc(s.handleSendChatMessage)))
	// Sandboxes BFF (PORTAL_PLAN_V2 P4 UI): session + CSRF, member role.
	mux.Handle("GET /api/orgs/{orgID}/sandboxes", s.withSession(http.HandlerFunc(s.handleBFFListSandboxes)))
	mux.Handle("POST /api/orgs/{orgID}/sandboxes", s.withSession(http.HandlerFunc(s.handleBFFCreateSandbox)))
	mux.Handle("GET /api/orgs/{orgID}/sandboxes/{id}", s.withSession(http.HandlerFunc(s.handleBFFGetSandbox)))
	mux.Handle("POST /api/orgs/{orgID}/sandboxes/{id}/exec", s.withSession(http.HandlerFunc(s.handleBFFExecSandbox)))
	mux.Handle("GET /api/orgs/{orgID}/sandboxes/{id}/logs", s.withSession(http.HandlerFunc(s.handleBFFSandboxLogs)))
	mux.Handle("GET /api/orgs/{orgID}/sandboxes/{id}/metrics/stream", s.withSession(http.HandlerFunc(s.handleBFFSandboxMetricsStream)))
	mux.Handle("POST /api/orgs/{orgID}/sandboxes/{id}/start", s.withSession(http.HandlerFunc(s.handleBFFSandboxAction)))
	mux.Handle("POST /api/orgs/{orgID}/sandboxes/{id}/stop", s.withSession(http.HandlerFunc(s.handleBFFSandboxAction)))
	mux.Handle("DELETE /api/orgs/{orgID}/sandboxes/{id}", s.withSession(http.HandlerFunc(s.handleBFFSandboxAction)))
	// Public API surface (S2: token exchange; S3 grows here).
	mux.Handle("POST /v1/tokens", http.HandlerFunc(s.handleAgentToken))
	// S3 sandbox gateway (PORTAL_PLAN 5): Bearer API keys, one org per
	// key, ownership 404s. Exec/logs/metrics proxy live streams.
	mux.Handle("POST /v1/sandboxes", http.HandlerFunc(s.handleV1CreateSandbox))
	mux.Handle("GET /v1/sandboxes", http.HandlerFunc(s.handleV1ListSandboxes))
	mux.Handle("GET /v1/sandboxes/{id}", http.HandlerFunc(s.handleV1GetSandbox))
	mux.Handle("POST /v1/sandboxes/{id}/exec", http.HandlerFunc(s.handleV1ExecSandbox))
	mux.Handle("GET /v1/sandboxes/{id}/logs", http.HandlerFunc(s.handleV1SandboxLogs))
	mux.Handle("POST /v1/sandboxes/{id}/start", http.HandlerFunc(s.handleV1SandboxAction))
	mux.Handle("POST /v1/sandboxes/{id}/stop", http.HandlerFunc(s.handleV1SandboxAction))
	mux.Handle("DELETE /v1/sandboxes/{id}", http.HandlerFunc(s.handleV1SandboxAction))
	// P5: org usage rollup for the public API (scope usage:read).
	mux.Handle("GET /v1/usage", http.HandlerFunc(s.handleV1Usage))
	// P5: Usage page BFF (dashboard cookie + CSRF, member role).
	mux.Handle("GET /api/orgs/{orgID}/usage", s.withSession(http.HandlerFunc(s.handleBFFUsage)))
	// A2: LLM model connections. Platform admin manages the platform
	// defaults; org owner/admin manages org connections + BYO key.
	// Members read; the Chat page resolves the effective connection.
	mux.Handle("GET /api/admin/llm/connections", s.withSession(http.HandlerFunc(s.handleListLLMConnections)))
	mux.Handle("POST /api/admin/llm/connections", s.withSession(http.HandlerFunc(s.handleCreateLLMConnection)))
	mux.Handle("PATCH /api/admin/llm/connections/{cid}", s.withSession(http.HandlerFunc(s.handleUpdateLLMConnection)))
	mux.Handle("DELETE /api/admin/llm/connections/{cid}", s.withSession(http.HandlerFunc(s.handleDeleteLLMConnection)))
	mux.Handle("POST /api/admin/llm/connections/{cid}/test", s.withSession(http.HandlerFunc(s.handleTestLLMConnection)))
	mux.Handle("GET /api/admin/llm/connections/{cid}/models", s.withSession(http.HandlerFunc(s.handleListLLMModels)))
	mux.Handle("GET /api/orgs/{orgID}/llm/connections", s.withSession(http.HandlerFunc(s.handleListLLMConnections)))
	mux.Handle("POST /api/orgs/{orgID}/llm/connections", s.withSession(http.HandlerFunc(s.handleCreateLLMConnection)))
	mux.Handle("PATCH /api/orgs/{orgID}/llm/connections/{cid}", s.withSession(http.HandlerFunc(s.handleUpdateLLMConnection)))
	mux.Handle("DELETE /api/orgs/{orgID}/llm/connections/{cid}", s.withSession(http.HandlerFunc(s.handleDeleteLLMConnection)))
	mux.Handle("POST /api/orgs/{orgID}/llm/connections/{cid}/test", s.withSession(http.HandlerFunc(s.handleTestLLMConnection)))
	mux.Handle("GET /api/orgs/{orgID}/llm/connections/{cid}/models", s.withSession(http.HandlerFunc(s.handleListLLMModels)))
	// A1: workspace files gateway (PORTAL_PLAN 5; AGENT_PLAN A0/A1).
	// Each route checks keyOwnsSandbox (org + project for pinned keys)
	// and the Portal scope `sandboxes:files`, then proxies to sandboxd
	// byte-for-byte. Path safety is enforced at sandboxd (A0 review #47).
	mux.Handle("GET /v1/sandboxes/{id}/files", http.HandlerFunc(s.handleV1ListFiles))
	mux.Handle("GET /v1/sandboxes/{id}/files/content", http.HandlerFunc(s.handleV1FileContent))
	mux.Handle("PUT /v1/sandboxes/{id}/files/content", http.HandlerFunc(s.handleV1PutFile))
	mux.Handle("DELETE /v1/sandboxes/{id}/files", http.HandlerFunc(s.handleV1DeleteFile))
	mux.Handle("POST /v1/sandboxes/{id}/files/move", http.HandlerFunc(s.handleV1MoveFile))
	// Lifecycle webhook (PORTAL_PLAN 4.4): signature-authed, so it does
	// NOT sit behind withSession. Registered only with a secret.
	if s.WebhookSecret != "" {
		mux.Handle("POST /api/webhooks/sandboxd", http.HandlerFunc(s.handleSandboxdWebhook))
	}
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
