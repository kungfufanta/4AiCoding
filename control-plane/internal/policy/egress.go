package policy

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

// Per-sandbox network modes. They decide what a sandbox may reach through
// the egress proxy; the deny list applies on top of every mode.
const (
	// NetNone: nothing leaves the sandbox.
	NetNone = "none"
	// NetAllowlist: only hosts on the sandbox's allow list (deny-by-default).
	// An empty mode is read as this one, which is how sandboxes created
	// before modes existed behave.
	NetAllowlist = "allowlist"
	// NetOpen: every host except those on the deny list. Needs a
	// permissive policy (or an approval when the policy gates it).
	NetOpen = "open"
)

// NetModes lists the valid modes.
func NetModes() []string { return []string{NetNone, NetAllowlist, NetOpen} }

// NormalizeNetMode maps "" to NetAllowlist and rejects unknown modes.
func NormalizeNetMode(m string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(m)) {
	case "", NetAllowlist:
		return NetAllowlist, nil
	case NetNone:
		return NetNone, nil
	case NetOpen:
		return NetOpen, nil
	}
	return "", fmt.Errorf("network mode must be one of %v", NetModes())
}

// EgressRules are the effective egress rules of one sandbox.
type EgressRules struct {
	Mode  string   `json:"mode"`
	Allow []string `json:"allow"`
	Deny  []string `json:"deny"`
}

// Check reports whether a request to host may go out, and why not.
// host is a hostname or an IP literal, already lower-cased.
func (r EgressRules) Check(host string) (bool, string) {
	if HostAllowed(r.Deny, host) {
		return false, "host is on this sandbox's block list"
	}
	switch r.Mode {
	case NetNone:
		return false, "this sandbox has no network access (mode none)"
	case NetOpen:
		return true, ""
	}
	if HostAllowed(r.Allow, host) {
		return true, ""
	}
	return false, "host is not on this sandbox's allow list"
}

// AddrDenied reports whether a resolved address is on the deny list (an IP
// or CIDR entry), so a permitted name cannot reach a blocked address.
func (r EgressRules) AddrDenied(a netip.Addr) bool {
	return addrListed(r.Deny, a)
}

// AddrAllowedExplicitly reports whether the allow list names the address
// (an IP or CIDR entry); private addresses are reachable only then.
func (r EgressRules) AddrAllowedExplicitly(a netip.Addr) bool {
	return addrListed(r.Allow, a)
}

func addrListed(list []string, a netip.Addr) bool {
	a = a.Unmap()
	for _, x := range list {
		if strings.Contains(x, "/") {
			if p, err := netip.ParsePrefix(x); err == nil && p.Contains(a) {
				return true
			}
			continue
		}
		if b, err := netip.ParseAddr(x); err == nil && b.Unmap() == a {
			return true
		}
	}
	return false
}

// normalizeList normalises, de-duplicates and sorts a host list.
func normalizeList(hosts []string) ([]string, error) {
	out := make([]string, 0, len(hosts))
	seen := map[string]bool{}
	for _, h := range hosts {
		n, err := NormalizeHost(h)
		if err != nil {
			return nil, err
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, nil
}
