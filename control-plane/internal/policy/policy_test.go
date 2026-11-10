package policy

import (
	"strings"
	"testing"
)

func TestDefaultIsValidAndDenyByDefault(t *testing.T) {
	p := Default()
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	if d := Decide(p, Request{Action: ActionEgress, Egress: []string{"github.com"}}); d.Allow {
		t.Fatal("default policy must deny egress")
	}
	if d := Decide(p, Request{Action: ActionEgress}); !d.Allow {
		t.Fatal("no egress is always allowed")
	}
	if d := Decide(p, Request{Action: ActionLifecycle, KeepAlive: true}); d.Allow {
		t.Fatal("keep-alive is off by default")
	}
}

func TestDecideCreate(t *testing.T) {
	p := Default()
	p.Images.Allowed = []string{"alpine:3.20"}
	p.Resources.MaxCPUs, p.Resources.MaxMemoryMB = 2, 1024
	p.Egress.Allow = []string{"github.com"}
	cases := []struct {
		name string
		req  Request
		ok   bool
		want string
	}{
		{"instance image", Request{Image: "base", InstanceImage: "base"}, true, ""},
		{"allowlisted image", Request{Image: "alpine:3.20", InstanceImage: "base"}, true, ""},
		{"other image", Request{Image: "evil", InstanceImage: "base"}, false, "not allowed"},
		{"privileged port", Request{Ports: []int{80}}, false, "outside the allowed ranges"},
		{"duplicate port", Request{Ports: []int{3000, 3000}}, false, "duplicate"},
		{"egress subdomain", Request{Egress: []string{"api.github.com"}}, true, ""},
		{"egress outside", Request{Egress: []string{"evil.com"}}, false, "not allowed by policy"},
		{"bad host", Request{Egress: []string{"a b"}}, false, "invalid host"},
		{"keep alive off", Request{KeepAlive: true}, false, "keep_alive"},
	}
	for _, c := range cases {
		c.req.Action = ActionCreate
		d := Decide(p, c.req)
		if d.Allow != c.ok || (c.want != "" && !strings.Contains(d.Reason, c.want)) {
			t.Errorf("%s: %+v", c.name, d)
		}
	}
	d := Decide(p, Request{Action: ActionCreate, CPUs: 8, MemoryMB: 99999, IdleTimeout: 10})
	if !d.Allow || d.EffectiveCPUs != 2 || d.EffectiveMemoryMB != 1024 || d.EffectiveIdleTimeout != 60 || len(d.Clamped) != 3 {
		t.Fatalf("clamp: %+v", d)
	}
}

func TestEgressApprovalAndPermissive(t *testing.T) {
	p := Default()
	p.Approvals = []string{ApproveEgressOutside}
	d := Decide(p, Request{Action: ActionEgress, Egress: []string{"Example.COM."}})
	if d.Allow || d.NeedsApproval != ApproveEgressOutside || d.Egress[0] != "example.com" {
		t.Fatalf("approval-gated egress: %+v", d)
	}
	p.Egress.Mode = EgressPermissive
	if d := Decide(p, Request{Action: ActionEgress, Egress: []string{"anything.io"}}); !d.Allow {
		t.Fatalf("permissive: %+v", d)
	}
}

func TestValidate(t *testing.T) {
	for _, mutate := range []func(*Policy){
		func(p *Policy) { p.Ports.Ranges = nil },
		func(p *Policy) { p.Ports.Ranges = []PortRange{{Min: 10, Max: 5}} },
		func(p *Policy) { p.Egress.Mode = "open" },
		func(p *Policy) { p.Egress.Allow = []string{"bad host"} },
		func(p *Policy) { p.Approvals = []string{"nope"} },
		func(p *Policy) { p.Images.Allowed = []string{"--privileged"} },
		func(p *Policy) { p.Resources.MaxCPUs = -1 },
	} {
		p := Default()
		mutate(&p)
		if err := p.Validate(); err == nil {
			t.Errorf("invalid policy accepted: %+v", p)
		}
	}
}

func TestHostAllowed(t *testing.T) {
	allow := []string{"github.com", "*.npmjs.org", "10.0.0.5"}
	for h, want := range map[string]bool{
		"github.com": true, "api.github.com": true, "evilgithub.com": false,
		"npmjs.org": false, "registry.npmjs.org": true, "10.0.0.5": true, "10.0.0.6": false,
	} {
		if got := HostAllowed(allow, h); got != want {
			t.Errorf("HostAllowed(%q) = %v", h, got)
		}
	}
}
