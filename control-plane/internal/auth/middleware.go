package auth

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
)

// Actor identifies the authenticated caller of a request. The auth
// middleware attaches it to the request context; handlers read it via
// ActorFrom to populate the audit log.
type Actor struct {
	Kind string // service | user | worker | system | unknown
	Name string // token name, or "loopback" for the operator path
	IP   string
	// Scopes restricts a service actor to the routes in scopes.go; nil/empty =
	// unrestricted (console sessions, env-configured tokens, full-access keys).
	Scopes []string
	// User is the username (sessions) or API key name.
	User string
	// Role is admin | operator | viewer (see roles.go).
	Role string
	// SandboxID binds an API key to one sandbox ("" = any).
	SandboxID string
}

type actorCtxKey struct{}

// ActorWorker is the credential class of fleet members (M6): a worker
// token holder. Its surface is exactly /internal (enforced in Wrap), and
// its role ("worker") is intentionally unknown to RoleAllows so a leaked
// worker credential cannot act on /v1 even if the path guard were bypassed.
const ActorWorker = "worker"

// ActorFleet is the control plane calling a worker's API (M7b). On a worker
// that executes sandboxes, the control plane authenticates with the node's
// own API token (see Middleware.SetFleetToken) and acts as an admin. Its
// name is fixed so handlers can recognise fleet-forwarded requests.
const (
	ActorFleet     = "fleet"
	FleetActorName = "control-plane"
)

// WithActor stores an Actor in ctx.
func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorCtxKey{}, a)
}

// ActorFrom returns the Actor stored in ctx, or {Kind:"unknown"}.
func ActorFrom(ctx context.Context) Actor {
	if a, ok := ctx.Value(actorCtxKey{}).(Actor); ok {
		return a
	}
	return Actor{Kind: "unknown"}
}

// AuditWriter is the slice of the audit logger the middleware needs.
// Declared here so internal/auth does not import internal/audit
// (internal/audit imports internal/store; keeping the dependency one-
// directional avoids a cycle).
type AuditWriter interface {
	TokenInvalid(ctx context.Context, ip string)
}

// Middleware is the uniform auth gate. Every request (regardless of origin —
// on-host, console-proxied, or Traefik-routed) must carry a valid credential: a
// console session cookie, or a bearer API key (DB-stored or env-configured).
// There is no locality bypass — "if auth is required, it is required."
type Middleware struct {
	cfg      atomic.Pointer[Config]
	resolver CredentialResolver
	audit    AuditWriter
	log      *slog.Logger
	// fleetToken (string) is the control plane's credential on a worker (M7b).
	fleetToken atomic.Value
}

// NewMiddleware constructs the middleware around an initial config. resolver may
// be nil (env-token-only mode).
func NewMiddleware(initial *Config, resolver CredentialResolver, audit AuditWriter, log *slog.Logger) *Middleware {
	m := &Middleware{resolver: resolver, audit: audit, log: log}
	m.cfg.Store(initial)
	return m
}

// Reload atomically swaps the config — the SIGHUP token-rotation path.
func (m *Middleware) Reload(c *Config) { m.cfg.Store(c) }

// SetFleetToken sets the credential the control plane uses to call this
// node's API (M7b, worker executor mode). Empty disables it.
func (m *Middleware) SetFleetToken(tok string) { m.fleetToken.Store(tok) }

// Snapshot returns the current config; callers treat it as read-only.
func (m *Middleware) Snapshot() *Config { return m.cfg.Load() }

// exemptPaths are reachable on the external path without a bearer
// token. /preview-auth and /forward-auth validate their
// own JWTs; /healthz, /readyz and /version carry nothing sensitive.
var exemptPaths = map[string]bool{
	"/healthz":        true,
	"/readyz":         true,
	"/version":        true,
	"/preview-auth":   true,
	"/forward-auth":   true,
	"/llm.txt":        true, // public API contract for integrators (no token)
	"/v1/auth/status": true, // console asks "is auth on / am I logged in / is a password set" pre-login
	"/v1/auth/login":  true, // you cannot be authenticated in order to authenticate
	"/v1/auth/setup":  true, // first-run "create password" (self-guards: 409 once set)
}

// Wrap returns next gated by the uniform credential check.
func (m *Middleware) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := m.cfg.Load()
		ip := ClientIP(r)

		// /metrics is loopback-only — never exposed externally. (This is the
		// only remaining use of loopback detection: it does NOT bypass auth.)
		if r.URL.Path == "/metrics" && !isLoopbackReq(r) {
			http.NotFound(w, r)
			return
		}

		// Resolve a credential, in order: console session cookie, then a bearer
		// API key (DB-stored via the resolver, else env-configured tokens).
		actor, authed := m.resolve(r, cfg, ip)

		// M6 — the /internal surface is the worker fleet's only door. It is
		// closed to everyone else, always — even in the auth-disabled
		// rollback (a sandbox must never reach it, and the rollback must
		// not silently widen the fleet API). A request here with NO
		// worker token configured is a misconfiguration: 404, not 401,
		// so probing learns nothing either way.
		if strings.HasPrefix(r.URL.Path, "/internal/") {
			if authed && actor.Kind == ActorWorker {
				next.ServeHTTP(w, r.WithContext(WithActor(r.Context(), actor)))
				return
			}
			http.NotFound(w, r)
			return
		}
		// A worker credential is valid ONLY for /internal (the mirror of
		// the rule above); it must never reach /v1 or legacy routes.
		if authed && actor.Kind == ActorWorker {
			writeForbidden(w, "worker tokens may only call /internal endpoints")
			return
		}

		// Exempt paths serve regardless of whether a credential was present
		// (they carry nothing sensitive, or self-guard). Attach whatever actor
		// resolved so handlers like /v1/auth/status can report authenticated.
		if exemptPaths[r.URL.Path] {
			next.ServeHTTP(w, r.WithContext(WithActor(r.Context(), actor)))
			return
		}

		// SANDBOXD_API_AUTH_DISABLED rollback — every request runs as the shared
		// tenant, unauthenticated. Explicit opt-out; trips the warning banner.
		if cfg.Disabled {
			next.ServeHTTP(w, r.WithContext(WithActor(r.Context(),
				Actor{Kind: "service", Name: "default", IP: ip, Role: RoleAdmin})))
			return
		}

		if !authed {
			if m.audit != nil {
				m.audit.TokenInvalid(r.Context(), ip)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}` + "\n"))
			return
		}
		if !ScopesAllow(actor.Scopes, r.Method, r.URL.Path) {
			writeForbidden(w, scopeDeniedMessage(actor.Scopes))
			return
		}
		if !SandboxBindingAllows(actor.SandboxID, r.URL.Path) {
			writeForbidden(w, "this API key is bound to sandbox "+actor.SandboxID)
			return
		}
		if !RoleAllows(actor.Role, r.Method, r.URL.Path) {
			writeForbidden(w, "role "+actor.Role+" may not "+r.Method+" "+r.URL.Path)
			return
		}
		// CSRF: a cookie-authenticated, state-changing request must carry the
		// custom header (bearer clients are not exposed to CSRF).
		if actor.Kind == "user" && !safeMethod(r.Method) && r.Header.Get(CSRFHeader) == "" {
			writeForbidden(w, "missing "+CSRFHeader+" header")
			return
		}
		next.ServeHTTP(w, r.WithContext(WithActor(r.Context(), actor)))
	})
}

// resolve returns the authenticated actor (and true) for a request, or a
// zero-value actor (and false) when no valid credential is present.
func (m *Middleware) resolve(r *http.Request, cfg *Config, ip string) (Actor, bool) {
	// 1. console session cookie.
	if m.resolver != nil {
		if ck, err := r.Cookie(SessionCookie); err == nil && ck.Value != "" {
			if p, ok := m.resolver.ResolveSession(r.Context(), ck.Value); ok {
				return Actor{Kind: "user", Name: p.Owner, IP: ip, User: p.User, Role: p.Role}, true
			}
		}
	}
	// 2. bearer API key — DB-stored, then env-configured.
	if tok := bearerToken(r); tok != "" {
		// The control plane calling this worker (M7b). Held outside the
		// reloadable Config so a SIGHUP never drops it.
		if ft, _ := m.fleetToken.Load().(string); ft != "" && matchWorkerToken(tok, []string{ft}) {
			return Actor{Kind: ActorFleet, Name: FleetActorName, IP: ip, User: FleetActorName, Role: RoleAdmin}, true
		}
		// Worker fleet token (M6): identifies a fleet member, valid
		// only for the /internal surface (enforced in Wrap).
		if matchWorkerToken(tok, cfg.WorkerTokens) {
			return Actor{Kind: ActorWorker, Name: "worker", IP: ip, Role: "worker"}, true
		}
		if m.resolver != nil {
			if p, ok := m.resolver.ResolveAPIKey(r.Context(), tok); ok {
				return Actor{Kind: "service", Name: p.Owner, IP: ip, Scopes: p.Scopes,
					User: p.User, Role: p.Role, SandboxID: p.SandboxID}, true
			}
		}
		// Env-configured tokens are the operator's root credential: admin.
		if name, ok := MatchToken(tok, cfg.APITokens); ok {
			return Actor{Kind: "service", Name: name, IP: ip, User: name, Role: RoleAdmin}, true
		}
	}
	return Actor{Kind: "unknown", IP: ip}, false
}

// writeForbidden emits a 403 in the v1 error envelope.
func writeForbidden(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"code": "forbidden", "message": msg, "retryable": false,
	}})
}

// bearerToken extracts the token from an `Authorization: Bearer <t>`
// header, or "" when absent / malformed.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) > len(p) && strings.EqualFold(h[:len(p)], p) {
		return strings.TrimSpace(h[len(p):])
	}
	return ""
}

// ClientIP returns the best-effort caller IP: the first hop of
// X-Forwarded-For when present, else the RemoteAddr host.
func ClientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// isLoopbackReq reports whether the request arrived directly over the
// loopback socket with no X-Forwarded-For — i.e. an on-host operator
// call, not a Traefik-forwarded one.
func isLoopbackReq(r *http.Request) bool {
	if r.Header.Get("X-Forwarded-For") != "" {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func safeMethod(m string) bool { return m == "GET" || m == "HEAD" || m == "OPTIONS" }
