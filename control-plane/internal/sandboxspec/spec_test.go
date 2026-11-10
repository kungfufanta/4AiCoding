package sandboxspec

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/previewhost"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

func row() *store.Sandbox {
	return &store.Sandbox{
		ID:           "01TEST",
		Status:       "stopped",
		Image:        "sandboxd-base:0.3.0", // what it was CREATED from
		WorkspaceMnt: "/var/lib/sandboxd/workspaces/01TEST",
		Ports:        []int{3000},
		WebPort:      sql.NullInt64{Int64: 3000, Valid: true},
		Visibility:   "private",
	}
}

func TestBuildReproducesTheHardenedCreateFlags(t *testing.T) {
	s := Build(row(), Env{Image: "sandboxd-base:0.4.0", Network: "sandboxd_net", PreviewDomain: "example.test", PreviewEntrypoint: "web"})

	if s.Name != "s-01TEST" || s.Hostname != "s-01TEST" {
		t.Errorf("name/hostname = %q/%q", s.Name, s.Hostname)
	}
	// The point of recreation: the CURRENT image, not the row's old one.
	if s.Image != "sandboxd-base:0.4.0" {
		t.Errorf("image = %q, want the instance's current image", s.Image)
	}
	if !s.ReadOnly {
		t.Error("rootfs must stay read-only")
	}
	if len(s.CapDrop) != 1 || s.CapDrop[0] != "ALL" {
		t.Errorf("CapDrop = %v, want [ALL]", s.CapDrop)
	}
	if len(s.SecurityOpt) != 1 || s.SecurityOpt[0] != "no-new-privileges" {
		t.Errorf("SecurityOpt = %v", s.SecurityOpt)
	}
	if s.PidsLimit != 1024 {
		t.Errorf("PidsLimit = %d", s.PidsLimit)
	}
	// The workspace bind mount is what makes recreation safe.
	if len(s.Volumes) == 0 || !strings.HasSuffix(s.Volumes[0], ":/home/sandbox") {
		t.Errorf("volumes = %v, want the workspace mounted at /home/sandbox", s.Volumes)
	}
}

func TestBuildEmitsPreviewLabelsForStoredPorts(t *testing.T) {
	s := Build(row(), Env{Image: "img", PreviewDomain: "example.test", PreviewEntrypoint: "web"})
	joined := strings.Join(s.Labels, "\n")
	for _, want := range []string{"traefik.enable=true", "s-01TEST-3000", "example.test"} {
		if !strings.Contains(joined, want) {
			t.Errorf("labels missing %q\ngot: %s", want, joined)
		}
	}
}

func TestBuildAddsTheWebPortWhenMissingFromPorts(t *testing.T) {
	r := row()
	r.Ports = []int{8080}
	r.WebPort = sql.NullInt64{Int64: 3000, Valid: true}
	s := Build(r, Env{Image: "img", PreviewDomain: "d", PreviewEntrypoint: "web"})
	joined := strings.Join(s.Labels, "\n")
	if !strings.Contains(joined, "s-01TEST-3000") || !strings.Contains(joined, "s-01TEST-8080") {
		t.Errorf("both the requested and the web port need routers, got:\n%s", joined)
	}
}

func TestBuildPortlessSandboxGetsNoPreviewRouters(t *testing.T) {
	r := row()
	r.Ports = nil
	s := Build(r, Env{Image: "img", PreviewDomain: "d", PreviewEntrypoint: "web"})
	if strings.Contains(strings.Join(s.Labels, "\n"), "routers") {
		t.Errorf("a port-less sandbox must stay preview-less, got %v", s.Labels)
	}
}

func TestBuildPassesRuntimeAndDNSForGvisor(t *testing.T) {
	s := Build(row(), Env{Image: "img", Runtime: "runsc", DNSResolvConf: "/var/lib/sandboxd/gvisor-resolv.conf"})
	if s.Runtime != "runsc" {
		t.Errorf("Runtime = %q", s.Runtime)
	}
	if !strings.Contains(strings.Join(s.Volumes, "\n"), "/etc/resolv.conf:ro") {
		t.Errorf("gVisor needs the resolv.conf mount, got %v", s.Volumes)
	}
}

func TestNeedsRecreate(t *testing.T) {
	cases := []struct {
		name, container, current string
		want                     bool
	}{
		{"stale image after upgrade", "sandboxd-base:0.3.0", "sandboxd-base:0.4.0", true},
		{"already current", "sandboxd-base:0.4.0", "sandboxd-base:0.4.0", false},
		{"unknown container image", "", "sandboxd-base:0.4.0", false},
		{"unknown current image", "sandboxd-base:0.3.0", "", false},
	}
	for _, tc := range cases {
		if got := NeedsRecreate(tc.container, tc.current); got != tc.want {
			t.Errorf("%s: NeedsRecreate(%q,%q) = %v, want %v", tc.name, tc.container, tc.current, got, tc.want)
		}
	}
}

func TestPreviewLabelsStale(t *testing.T) {
	rule := func(host string) map[string]string {
		return map[string]string{
			"sandboxd.managed":                            "true",
			"traefik.http.routers.s-01A-3000.rule":        "Host(`s-01A-3000.preview." + host + "`)",
			"traefik.http.routers.s-01A-3000.entrypoints": "web",
		}
	}
	cases := []struct {
		name   string
		labels map[string]string
		domain string
		want   bool
	}{
		{"same domain", rule("old.example"), "old.example", false},
		{"domain changed", rule("old.example"), "new.example", true},
		{"changed to org subdomain", rule("1.2.3.4.sslip.io"), "san.sandboxd.io", true},
		{"no routers (worker)", map[string]string{"sandboxd.managed": "true"}, "new.example", false},
		{"no domain configured", rule("old.example"), "", false},
		{"nil labels", nil, "x.example", false},
	}
	for _, c := range cases {
		if got := PreviewLabelsStale(c.labels, previewhost.Scheme{Domain: c.domain}); got != c.want {
			t.Errorf("%s: PreviewLabelsStale = %v; want %v", c.name, got, c.want)
		}
	}
	// Switching host style or tag re-labels too: the old nested router does
	// not serve the flat host, and a flat router of another tag is stale.
	flat := previewhost.Scheme{Domain: "old.example", Style: previewhost.StyleFlat, Tag: "eu1"}
	if !PreviewLabelsStale(rule("old.example"), flat) {
		t.Error("nested labels under a flat scheme: want stale")
	}
	flatRule := map[string]string{"traefik.http.routers.s-01A-3000.rule": "Host(`s-01A-3000--eu1.old.example`)"}
	if PreviewLabelsStale(flatRule, flat) {
		t.Error("flat labels matching the scheme: want fresh")
	}
	if !PreviewLabelsStale(flatRule, previewhost.Scheme{Domain: "old.example", Style: previewhost.StyleFlat, Tag: "eu2"}) {
		t.Error("flat labels of another tag: want stale")
	}
}

// PLAN §8 #1: only the workspace (and, for gVisor, a read-only resolv.conf)
// is ever mounted. Never the docker socket, never another host path.
func TestBuildMountsOnlyTheWorkspace(t *testing.T) {
	sb := row()
	sb.Spec.Env = map[string]string{"A": "1"}
	for _, e := range []Env{{Image: "img"}, {Image: "img", DNSResolvConf: "/var/lib/sandboxd/resolv.conf"}} {
		s := Build(sb, e)
		for _, v := range s.Volumes {
			if strings.Contains(v, "docker.sock") {
				t.Fatalf("docker socket mounted: %v", s.Volumes)
			}
			dst := strings.Split(v, ":")[1]
			if dst != "/home/sandbox" && v != e.DNSResolvConf+":/etc/resolv.conf:ro" {
				t.Fatalf("unexpected mount %q", v)
			}
		}
	}
}

// PLAN §8 #3: the sandbox never runs as root, whatever the image's USER.
func TestBuildForcesNonRootUser(t *testing.T) {
	if s := Build(row(), Env{Image: "img"}); s.User != DefaultUser {
		t.Fatalf("user = %q, want %q", s.User, DefaultUser)
	}
	if s := Build(row(), Env{Image: "img", User: "2000:2000"}); s.User != "2000:2000" {
		t.Fatalf("user override = %q", s.User)
	}
}

// PLAN §8 #7: the container env is exactly the spec env, nothing inherited.
func TestBuildEnvIsOnlyTheSpecEnv(t *testing.T) {
	t.Setenv("SANDBOXD_API_TOKENS", "default=secret")
	sb := row()
	if s := Build(sb, Env{Image: "img"}); len(s.Env) != 0 {
		t.Fatalf("env without a spec = %v, want empty", s.Env)
	}
	sb.Spec.Env = map[string]string{"B": "2", "A": "1"}
	s := Build(sb, Env{Image: "img"})
	if strings.Join(s.Env, ",") != "A=1,B=2" {
		t.Fatalf("env = %v, want [A=1 B=2]", s.Env)
	}
}

func TestBuildAppliesSpecImageCommandAndResources(t *testing.T) {
	sb := row()
	sb.Spec = store.Spec{Image: "alpine:3.20", Command: []string{"sleep", "infinity"}, CPUs: 1.5, MemoryMB: 512}
	s := Build(sb, Env{Image: "sandboxd-base:0.4.0"})
	if s.Image != "alpine:3.20" {
		t.Errorf("image = %q, want the pinned spec image", s.Image)
	}
	if strings.Join(s.Cmd, " ") != "sleep infinity" {
		t.Errorf("cmd = %v", s.Cmd)
	}
	if s.CPUs != "1.5" || s.Memory != "512m" || s.MemorySwap != "512m" {
		t.Errorf("resources cpus=%q mem=%q swap=%q", s.CPUs, s.Memory, s.MemorySwap)
	}
	d := Build(row(), Env{Image: "img", MemoryMB: 2048})
	if d.CPUs != "" || d.Memory != "2048m" || len(d.Cmd) != 0 {
		t.Errorf("defaults cpus=%q mem=%q cmd=%v", d.CPUs, d.Memory, d.Cmd)
	}
}
