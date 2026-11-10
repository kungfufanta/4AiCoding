package auth

import "strings"

// Roles, from most to least privileged.
const (
	RoleAdmin    = "admin"
	RoleOperator = "operator"
	RoleViewer   = "viewer"
)

var roleRank = map[string]int{RoleViewer: 1, RoleOperator: 2, RoleAdmin: 3}

// ValidRole reports whether name is a defined role.
func ValidRole(name string) bool { _, ok := roleRank[name]; return ok }

// RoleAtLeast reports whether role grants at least min.
func RoleAtLeast(role, min string) bool { return roleRank[role] >= roleRank[min] && roleRank[min] > 0 }

// adminRoutes may only be called by admins: instance policy, users, keys,
// domain, approvals decisions, audit, settings changes, node drain.
var adminRoutes = []string{
	"PUT /v1/policy", "PUT /v1/policy/*",
	"* /v1/users", "* /v1/users/*",
	"* /v1/api-keys", "* /v1/api-keys/*",
	"PUT /v1/domain", "POST /v1/domain/*",
	"POST /v1/approvals/*/approve", "POST /v1/approvals/*/deny",
	"GET /v1/audit", "GET /v1/audit/verify",
	"PATCH /v1/settings",
	"POST /v1/nodes/*/drain", "POST /v1/nodes/*/undrain", "DELETE /v1/nodes/*",
	"DELETE /v1/auth/password",
	"POST /sandbox/*/purge", "POST /external-users/*/purge", "POST /external-projects/*/purge",
}

// viewerAllowed are the non-GET routes a viewer may still call (its own
// session housekeeping).
var viewerAllowed = []string{"POST /v1/auth/logout", "POST /v1/auth/password"}

// RoleAllows reports whether role may call method+path.
//   - admin: everything.
//   - operator: everything except adminRoutes.
//   - viewer: reads (GET/HEAD) except adminRoutes, plus its own logout and
//     password change.
func RoleAllows(role, method, path string) bool {
	switch role {
	case RoleAdmin:
		return true
	case RoleOperator:
		return !matchAny(adminRoutes, method, path)
	case RoleViewer:
		if matchAny(adminRoutes, method, path) {
			return false
		}
		return method == "GET" || method == "HEAD" || matchAny(viewerAllowed, method, path)
	}
	return false
}

// matchAny reports whether "METHOD /path" matches one of the patterns. A
// pattern is "METHOD /seg/seg" where METHOD may be "*" and a "*" segment
// matches exactly one path segment. A trailing "/**" matches the rest.
func matchAny(patterns []string, method, path string) bool {
	for _, p := range patterns {
		if matchRoute(p, method, path) {
			return true
		}
	}
	return false
}

func matchRoute(pattern, method, path string) bool {
	pm, pp, ok := strings.Cut(pattern, " ")
	if !ok || (pm != "*" && pm != method) {
		return false
	}
	ps := strings.Split(strings.TrimPrefix(pp, "/"), "/")
	rs := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, seg := range ps {
		if seg == "**" {
			return true
		}
		if i >= len(rs) {
			return false
		}
		if seg != "*" && seg != rs[i] {
			return false
		}
	}
	return len(ps) == len(rs)
}

// SandboxBindingAllows reports whether a key bound to sandboxID may call
// path: only that sandbox's own /v1 routes.
func SandboxBindingAllows(sandboxID, path string) bool {
	if sandboxID == "" {
		return true
	}
	base := "/v1/sandboxes/" + sandboxID
	return path == base || strings.HasPrefix(path, base+"/")
}
