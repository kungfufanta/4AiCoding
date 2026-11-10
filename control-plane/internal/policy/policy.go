// Package policy is the single Policy Decision Point (PLAN §8 #10). Every
// create / port / egress / lifecycle / exec request is checked by Decide
// against the admin policy before anything touches docker. The sandbox
// itself never reaches this code: decisions are made in the control plane
// from the request and the stored policy only.
package policy

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

// Egress modes.
const (
	EgressStrict     = "strict"     // a sandbox may only allow hosts on Egress.Allow
	EgressPermissive = "permissive" // a sandbox may allow any host
)

// Policy is the admin policy. The zero value is not valid; use Default.
type Policy struct {
	Images    Images    `json:"images"`
	Ports     Ports     `json:"ports"`
	Resources Resources `json:"resources"`
	Egress    Egress    `json:"egress"`
	Lifecycle Lifecycle `json:"lifecycle"`
	// Approvals lists the actions that need a one-time human approval
	// (PLAN §8 #11), e.g. "sandbox.delete_keep_alive", "policy.update",
	// "sandbox.egress_outside_policy", "sandbox.public". See M3c.
	Approvals []string `json:"approvals"`
}

// Images: the instance image is always allowed; Allowed adds more.
type Images struct {
	Allowed []string `json:"allowed"`
}

// PortRange is an inclusive container-port range.
type PortRange struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

// Ports: every preview port must be inside one of the ranges.
type Ports struct {
	Ranges []PortRange `json:"ranges"`
}

// Resources are clamp targets (requests above them are lowered, not refused).
type Resources struct {
	MaxCPUs            float64 `json:"max_cpus"`      // 0 = no clamp
	MaxMemoryMB        int     `json:"max_memory_mb"` // 0 = no clamp
	MaxPortsPerSandbox int     `json:"max_ports_per_sandbox"`
}

// Egress: deny-by-default. Allow is the instance allowlist of hostnames
// ("example.com" also matches subdomains; "*.example.com" only subdomains).
type Egress struct {
	Mode  string   `json:"mode"`
	Allow []string `json:"allow"`
}

// Lifecycle bounds idle timeouts and keep-alive.
type Lifecycle struct {
	AllowKeepAlive     bool `json:"allow_keep_alive"`
	MaxIdleTimeoutSecs int  `json:"max_idle_timeout_secs"` // 0 = no clamp
}

// Default is the policy of a fresh install: unprivileged ports only (the
// sandbox runs as non-root without NET_BIND_SERVICE), at most 16 ports,
// strict egress with an empty allowlist (no egress), keep-alive off.
func Default() Policy {
	return Policy{
		Images:    Images{Allowed: []string{}},
		Ports:     Ports{Ranges: []PortRange{{Min: 1024, Max: 65535}}},
		Resources: Resources{MaxPortsPerSandbox: 16, MaxMemoryMB: 10240},
		Egress:    Egress{Mode: EgressStrict, Allow: []string{}},
		Lifecycle: Lifecycle{AllowKeepAlive: false, MaxIdleTimeoutSecs: 7 * 24 * 3600},
		Approvals: []string{},
	}
}

// Validate normalises p and reports the first problem.
func (p *Policy) Validate() error {
	if p.Images.Allowed == nil {
		p.Images.Allowed = []string{}
	}
	for _, img := range p.Images.Allowed {
		if strings.TrimSpace(img) == "" || strings.HasPrefix(img, "-") || strings.ContainsAny(img, " \t\n") {
			return fmt.Errorf("images.allowed: invalid image %q", img)
		}
	}
	if len(p.Ports.Ranges) == 0 {
		return fmt.Errorf("ports.ranges: at least one range is required")
	}
	for _, r := range p.Ports.Ranges {
		if r.Min < 1 || r.Max > 65535 || r.Min > r.Max {
			return fmt.Errorf("ports.ranges: invalid range %d-%d", r.Min, r.Max)
		}
	}
	if p.Resources.MaxCPUs < 0 || p.Resources.MaxMemoryMB < 0 || p.Resources.MaxPortsPerSandbox < 0 {
		return fmt.Errorf("resources: limits must not be negative")
	}
	switch p.Egress.Mode {
	case "":
		p.Egress.Mode = EgressStrict
	case EgressStrict, EgressPermissive:
	default:
		return fmt.Errorf("egress.mode must be %q or %q", EgressStrict, EgressPermissive)
	}
	if p.Egress.Allow == nil {
		p.Egress.Allow = []string{}
	}
	for i, h := range p.Egress.Allow {
		n, err := NormalizeHost(h)
		if err != nil {
			return fmt.Errorf("egress.allow: %w", err)
		}
		p.Egress.Allow[i] = n
	}
	if p.Lifecycle.MaxIdleTimeoutSecs < 0 {
		return fmt.Errorf("lifecycle.max_idle_timeout_secs must not be negative")
	}
	if p.Approvals == nil {
		p.Approvals = []string{}
	}
	for _, a := range p.Approvals {
		if !knownApprovalActions[a] {
			return fmt.Errorf("approvals: unknown action %q (known: %s)", a, strings.Join(ApprovalActions(), ", "))
		}
	}
	return nil
}

// Approval-gated actions (M3c).
const (
	ApproveDeleteKeepAlive = "sandbox.delete_keep_alive"
	ApprovePolicyUpdate    = "policy.update"
	ApproveEgressOutside   = "sandbox.egress_outside_policy"
	ApprovePublic          = "sandbox.public"
)

var knownApprovalActions = map[string]bool{
	ApproveDeleteKeepAlive: true, ApprovePolicyUpdate: true, ApproveEgressOutside: true, ApprovePublic: true,
}

// ApprovalActions lists the actions that can be approval-gated.
func ApprovalActions() []string {
	out := make([]string, 0, len(knownApprovalActions))
	for a := range knownApprovalActions {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// RequiresApproval reports whether the policy gates action behind approval.
func (p Policy) RequiresApproval(action string) bool {
	for _, a := range p.Approvals {
		if a == action {
			return true
		}
	}
	return false
}

// NormalizeHost lower-cases a hostname allowlist entry and checks its shape.
// Entries are hostnames, "*.domain" wildcards, IP literals or CIDR ranges.
func NormalizeHost(h string) (string, error) {
	h = strings.ToLower(strings.TrimSpace(h))
	h = strings.TrimSuffix(h, ".")
	if h == "" || len(h) > 253 {
		return "", fmt.Errorf("invalid host %q", h)
	}
	if a, err := netip.ParseAddr(h); err == nil {
		return a.Unmap().String(), nil
	}
	if strings.Contains(h, "/") {
		p, err := netip.ParsePrefix(h)
		if err != nil {
			return "", fmt.Errorf("invalid CIDR %q", h)
		}
		return p.Masked().String(), nil
	}
	body := strings.TrimPrefix(h, "*.")
	for _, label := range strings.Split(body, ".") {
		if label == "" || len(label) > 63 {
			return "", fmt.Errorf("invalid host %q", h)
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return "", fmt.Errorf("invalid host %q", h)
			}
		}
	}
	return h, nil
}

// HostAllowed reports whether host matches an allowlist entry.
// "example.com" matches itself and its subdomains; "*.example.com" only
// subdomains; IP literals match exactly; a CIDR matches IP hosts inside it.
func HostAllowed(allow []string, host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	ip, ipErr := netip.ParseAddr(host)
	for _, a := range allow {
		switch {
		case strings.Contains(a, "/"):
			if ipErr == nil {
				if p, err := netip.ParsePrefix(a); err == nil && p.Contains(ip.Unmap()) {
					return true
				}
			}
		case strings.HasPrefix(a, "*."):
			if strings.HasSuffix(host, a[1:]) {
				return true
			}
		case host == a || strings.HasSuffix(host, "."+a):
			return true
		}
	}
	return false
}
