package policy

import (
	"fmt"
)

// Request is what Decide judges. Only the fields of the action are read.
type Request struct {
	Action string // see the Action* constants

	InstanceImage string
	Image         string // "" = the instance image
	Ports         []int
	CPUs          float64
	MemoryMB      int
	Egress        []string
	EgressMode    string   // "" = NetAllowlist
	EgressDeny    []string // block list; never needs policy
	KeepAlive     bool
	IdleTimeout   int // seconds; 0 = instance default
}

// Actions.
const (
	ActionCreate    = "sandbox.create"
	ActionPorts     = "sandbox.ports"
	ActionEgress    = "sandbox.egress"
	ActionLifecycle = "sandbox.lifecycle"
	ActionExec      = "sandbox.exec"
)

// Decision is the outcome. When Allow is true the Effective* fields carry
// the values to apply (resources and idle timeout clamped). Clamped lists
// what was lowered, for the response and the audit log. NeedsApproval names
// the approval-gated action the request triggers (M3c), if any.
type Decision struct {
	Allow  bool   `json:"allow"`
	Reason string `json:"reason,omitempty"`

	EffectiveCPUs        float64  `json:"effective_cpus,omitempty"`
	EffectiveMemoryMB    int      `json:"effective_memory_mb,omitempty"`
	EffectiveIdleTimeout int      `json:"effective_idle_timeout_secs,omitempty"`
	Egress               []string `json:"egress,omitempty"`
	EgressMode           string   `json:"egress_mode,omitempty"`
	EgressDeny           []string `json:"egress_deny,omitempty"`
	Clamped              []string `json:"clamped,omitempty"`
	NeedsApproval        string   `json:"needs_approval,omitempty"`
}

func deny(format string, a ...any) Decision {
	return Decision{Allow: false, Reason: fmt.Sprintf(format, a...)}
}

// Decide is the policy decision point. It never mutates p.
func Decide(p Policy, r Request) Decision {
	switch r.Action {
	case ActionCreate:
		return decideCreate(p, r)
	case ActionPorts:
		if d := checkPorts(p, r.Ports); !d.Allow {
			return d
		}
		return Decision{Allow: true}
	case ActionEgress:
		return decideEgress(p, r)
	case ActionLifecycle:
		return decideLifecycle(p, r)
	case ActionExec:
		return Decision{Allow: true}
	}
	return deny("unknown action %q", r.Action)
}

func decideCreate(p Policy, r Request) Decision {
	if r.Image != "" && r.Image != r.InstanceImage {
		ok := false
		for _, a := range p.Images.Allowed {
			if a == r.Image {
				ok = true
				break
			}
		}
		if !ok {
			return deny("image %q is not allowed by policy (allowed: the instance image%s)", r.Image, listSuffix(p.Images.Allowed))
		}
	}
	if d := checkPorts(p, r.Ports); !d.Allow {
		return d
	}
	if r.CPUs < 0 || r.MemoryMB < 0 {
		return deny("resources must not be negative")
	}
	out := Decision{Allow: true, EffectiveCPUs: r.CPUs, EffectiveMemoryMB: r.MemoryMB}
	if p.Resources.MaxCPUs > 0 && out.EffectiveCPUs > p.Resources.MaxCPUs {
		out.EffectiveCPUs = p.Resources.MaxCPUs
		out.Clamped = append(out.Clamped, "cpus")
	}
	if out.EffectiveMemoryMB > 0 && out.EffectiveMemoryMB < 64 {
		out.EffectiveMemoryMB = 64 // below this docker refuses to start most images
		out.Clamped = append(out.Clamped, "memory_mb")
	}
	if p.Resources.MaxMemoryMB > 0 && out.EffectiveMemoryMB > p.Resources.MaxMemoryMB {
		out.EffectiveMemoryMB = p.Resources.MaxMemoryMB
		out.Clamped = append(out.Clamped, "memory_mb")
	}
	eg := decideEgress(p, r)
	if !eg.Allow {
		if eg.NeedsApproval != "" {
			// Approvals bind to one request; create the sandbox inside the
			// policy and widen it with PUT /network/egress (which can be
			// approved) instead of parking the whole create.
			return deny("%s; create the sandbox without it, then request it with PUT /v1/sandboxes/{id}/network/egress", eg.Reason)
		}
		return eg
	}
	out.Egress = eg.Egress
	out.EgressMode = eg.EgressMode
	out.EgressDeny = eg.EgressDeny
	lc := decideLifecycle(p, r)
	if !lc.Allow {
		return lc
	}
	out.EffectiveIdleTimeout = lc.EffectiveIdleTimeout
	out.Clamped = append(out.Clamped, lc.Clamped...)
	return out
}

func checkPorts(p Policy, ports []int) Decision {
	if p.Resources.MaxPortsPerSandbox > 0 && len(ports) > p.Resources.MaxPortsPerSandbox {
		return deny("at most %d ports per sandbox", p.Resources.MaxPortsPerSandbox)
	}
	seen := map[int]bool{}
	for _, port := range ports {
		if port < 1 || port > 65535 {
			return deny("port out of range: %d", port)
		}
		if seen[port] {
			return deny("duplicate port: %d", port)
		}
		seen[port] = true
		in := false
		for _, rg := range p.Ports.Ranges {
			if port >= rg.Min && port <= rg.Max {
				in = true
				break
			}
		}
		if !in {
			return deny("port %d is outside the allowed ranges %s", port, rangesString(p.Ports.Ranges))
		}
	}
	return Decision{Allow: true}
}

// decideEgress validates a sandbox's egress rules (mode, allow list, deny
// list). In strict mode every allow entry must be covered by the policy
// allowlist and "open" is refused; an entry outside it (or "open") is denied,
// or (when the policy gates it) sent for approval. The deny list only ever
// narrows access, so it is never checked against the policy.
func decideEgress(p Policy, r Request) Decision {
	mode, err := NormalizeNetMode(r.EgressMode)
	if err != nil {
		return deny("egress: %v", err)
	}
	norm, err := normalizeList(r.Egress)
	if err != nil {
		return deny("egress: %v", err)
	}
	denyList, err := normalizeList(r.EgressDeny)
	if err != nil {
		return deny("egress deny: %v", err)
	}
	ok := Decision{Allow: true, Egress: norm, EgressMode: mode, EgressDeny: denyList}
	if p.Egress.Mode == EgressPermissive {
		return ok
	}
	var outside []string
	for _, h := range norm {
		if !HostAllowed(p.Egress.Allow, h) {
			outside = append(outside, h)
		}
	}
	if mode == NetOpen || len(outside) > 0 {
		what := fmt.Sprintf("egress to %v is outside the policy allowlist", outside)
		if mode == NetOpen {
			what = "open egress (network mode \"open\") is outside the strict policy"
		}
		if p.RequiresApproval(ApproveEgressOutside) {
			ap := ok
			ap.Allow = false
			ap.NeedsApproval = ApproveEgressOutside
			ap.Reason = what + " and needs approval"
			return ap
		}
		if mode == NetOpen {
			return deny("%s; set policy egress.mode to %q to allow it", what, EgressPermissive)
		}
		return deny("egress to %v is not allowed by policy (strict mode; allowlist: %v)", outside, p.Egress.Allow)
	}
	return ok
}

func decideLifecycle(p Policy, r Request) Decision {
	if r.KeepAlive && !p.Lifecycle.AllowKeepAlive {
		return deny("keep_alive is disabled by policy")
	}
	if r.IdleTimeout < 0 {
		return deny("idle_timeout_secs must not be negative")
	}
	out := Decision{Allow: true, EffectiveIdleTimeout: r.IdleTimeout}
	if p.Lifecycle.MaxIdleTimeoutSecs > 0 && out.EffectiveIdleTimeout > p.Lifecycle.MaxIdleTimeoutSecs {
		out.EffectiveIdleTimeout = p.Lifecycle.MaxIdleTimeoutSecs
		out.Clamped = append(out.Clamped, "idle_timeout_secs")
	}
	if out.EffectiveIdleTimeout > 0 && out.EffectiveIdleTimeout < 60 {
		out.EffectiveIdleTimeout = 60
		out.Clamped = append(out.Clamped, "idle_timeout_secs")
	}
	return out
}

func listSuffix(list []string) string {
	if len(list) == 0 {
		return ""
	}
	s := ""
	for _, x := range list {
		s += ", " + x
	}
	return s
}

func rangesString(rs []PortRange) string {
	s := ""
	for i, r := range rs {
		if i > 0 {
			s += ","
		}
		s += fmt.Sprintf("%d-%d", r.Min, r.Max)
	}
	return s
}
