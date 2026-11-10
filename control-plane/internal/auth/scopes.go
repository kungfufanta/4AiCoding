package auth

import (
	"sort"
	"strings"
)

// ScopeHealth lets a key read liveness/readiness/version and nothing else —
// the credential an external monitor should hold. Finer-grained,
// resource-scoped keys arrive with the policy engine (PLAN.md M3b).
// A key stored with a scope that no longer exists (e.g. the removed
// "upgrade" scope) matches nothing, so it is denied everywhere.
const ScopeHealth = "health"

// scopeRoutes is the single source of truth for what each scope permits, as
// "METHOD /path" entries matched exactly against the request.
var scopeRoutes = map[string]map[string]bool{
	ScopeHealth: {
		"GET /healthz":        true,
		"GET /readyz":         true,
		"GET /version":        true,
		"GET /v1/auth/status": true,
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
// scopes. An empty scope list means an unrestricted credential.
func ScopesAllow(scopes []string, method, path string) bool {
	if len(scopes) == 0 {
		return true
	}
	route := method + " " + path
	for _, s := range scopes {
		if scopeRoutes[s][route] {
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
