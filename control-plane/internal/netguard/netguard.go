// Package netguard keeps the control plane unreachable from sandboxes
// (PLAN §8 #13). Sandboxes share a docker network with the edge router; any
// request whose source address is inside that network is refused before auth
// runs, so a sandbox cannot even probe the API with a stolen token. The
// network's gateway is exempt: it is the host itself (published ports and
// docker-proxy traffic arrive from it), and a sandbox has no CAP_NET_RAW to
// forge it.
package netguard

import (
	"net"
	"net/http"
	"net/netip"
	"sync/atomic"
)

// Guard rejects requests from denied subnets. The zero value allows all.
type Guard struct {
	rules atomic.Pointer[rules]
}

type rules struct {
	deny  []netip.Prefix
	allow map[netip.Addr]bool
}

// Set replaces the denied subnets and the exempt gateway addresses. Invalid
// entries are returned so the caller can log them.
func (g *Guard) Set(subnets, gateways []string) (bad []string) {
	r := &rules{allow: map[netip.Addr]bool{}}
	for _, s := range subnets {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			bad = append(bad, s)
			continue
		}
		r.deny = append(r.deny, p.Masked())
	}
	for _, s := range gateways {
		a, err := netip.ParseAddr(s)
		if err != nil {
			bad = append(bad, s)
			continue
		}
		r.allow[a.Unmap()] = true
	}
	g.rules.Store(r)
	return bad
}

// Denied reports whether a request from remoteAddr ("ip:port") is refused.
func (g *Guard) Denied(remoteAddr string) bool {
	r := g.rules.Load()
	if r == nil || len(r.deny) == 0 {
		return false
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	a = a.Unmap()
	if r.allow[a] {
		return false
	}
	for _, p := range r.deny {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Wrap refuses denied sources with 403 before next runs.
func (g *Guard) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if g.Denied(r.RemoteAddr) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"the control plane is not reachable from the sandbox network","retryable":false}}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}
