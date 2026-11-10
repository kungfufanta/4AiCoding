package auth

import (
	"sort"
	"strings"
)

// Scopes narrow an API key below its role (PLAN §8 #9). A key with no scopes
// may do whatever its role allows; a scoped key may only call the routes of
// its scopes (and still within its role).
const (
	ScopeHealth          = "health"           // liveness / version only (monitors)
	ScopeSandboxesRead   = "sandboxes:read"   // list / get / metrics / volumes / ports
	ScopeSandboxesWrite  = "sandboxes:write"  // create / start / stop / restart / delete / ports / lifecycle
	ScopeSandboxesExec   = "sandboxes:exec"   // exec + logs
	ScopeSnapshots       = "snapshots"        // snapshot CRUD
	ScopeSandboxesEgress = "sandboxes:egress" // read / change a sandbox's egress rules, watch its network activity
)

// scopeRoutes is the single source of truth for what each scope permits, as
// route patterns (see matchRoute).
var scopeRoutes = map[string][]string{
	ScopeHealth: {"GET /healthz", "GET /readyz", "GET /version", "GET /v1/auth/status"},
	ScopeSandboxesRead: {
		"GET /v1/sandboxes", "GET /v1/sandboxes/*",
		"GET /v1/sandboxes/*/metrics", "GET /v1/sandboxes/*/metrics/stream",
		"GET /v1/sandboxes/*/volumes", "GET /v1/sandboxes/*/network/ports",
		"GET /v1/sandboxes/*/lifecycle",
	},
	ScopeSandboxesWrite: {
		"POST /v1/sandboxes", "DELETE /v1/sandboxes/*",
		"POST /v1/sandboxes/*/start", "POST /v1/sandboxes/*/stop", "POST /v1/sandboxes/*/restart",
		"PUT /v1/sandboxes/*/network/ports", "POST /v1/sandboxes/*/network/ports",
		"DELETE /v1/sandboxes/*/network/ports/*",
		"PATCH /v1/sandboxes/*/lifecycle", "POST /v1/sandboxes/*/keep-alive", "DELETE /v1/sandboxes/*/keep-alive",
	},
	ScopeSandboxesExec: {"POST /v1/sandboxes/*/exec", "GET /v1/sandboxes/*/logs"},
	ScopeSnapshots:     {"GET /v1/snapshots", "POST /v1/snapshots", "GET /v1/snapshots/*", "DELETE /v1/snapshots/*"},
	ScopeSandboxesEgress: {
		"GET /v1/sandboxes/*/network/egress", "PUT /v1/sandboxes/*/network/egress",
		"GET /v1/sandboxes/*/network/activity",
	},
}

// KnownScopes returns every defined scope name, sorted.
func KnownScopes() []string {
	out := make([]string, 0, len(scopeRoutes))
	for s := range scopeRoutes {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// ValidScope reports whether name is a defined scope.
func ValidScope(name string) bool {
	_, ok := scopeRoutes[name]
	return ok
}

// ScopesAllow reports whether a request is permitted by at least one of the
// scopes. An empty scope list means no scope restriction. A stored scope that
// no longer exists (e.g. the removed "upgrade") matches nothing.
func ScopesAllow(scopes []string, method, path string) bool {
	if len(scopes) == 0 {
		return true
	}
	for _, s := range scopes {
		if matchAny(scopeRoutes[s], method, path) {
			return true
		}
	}
	return false
}

// scopeDeniedMessage is the 403 body text for a scoped key hitting a route
// outside its scopes.
func scopeDeniedMessage(scopes []string) string {
	return "this API key is limited to: " + strings.Join(scopes, ", ")
}
