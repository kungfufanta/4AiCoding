package auth

import "context"

// SessionCookie is the name of the console session cookie.
const SessionCookie = "sbx_session"

// CSRFHeader must accompany every state-changing request made with a session
// cookie. A cross-site form or image cannot set a custom header, and a
// cross-site fetch that sets one needs a CORS preflight the API never grants.
const CSRFHeader = "X-Sandboxd-CSRF"

// Principal is what a credential resolves to.
type Principal struct {
	Owner     string   // tenant (store.DefaultTenant in single-tenant installs)
	User      string   // username (sessions) or key name (API keys)
	Role      string   // admin | operator | viewer
	Scopes    []string // API keys only; nil = no scope restriction
	SandboxID string   // API keys only; "" = not bound to a sandbox
	// RequireExternal (API keys only; Portal S3a): sandbox creates must
	// carry external.workspace_id, else 400.
	RequireExternal bool
}

// CredentialResolver resolves the two DB-backed credential types. It is
// implemented in the api package over *store.Store; declaring it here keeps
// internal/auth free of a store dependency. A nil resolver means "only
// env-configured SANDBOXD_API_TOKENS are accepted".
type CredentialResolver interface {
	// ResolveSession maps a session cookie value to its user. ok=false when
	// the cookie is absent, unknown, expired, or its user is disabled.
	ResolveSession(ctx context.Context, cookieValue string) (Principal, bool)
	// ResolveAPIKey maps a presented bearer key to its principal. ok=false
	// when the key is absent, unknown or expired.
	ResolveAPIKey(ctx context.Context, presented string) (Principal, bool)
}
