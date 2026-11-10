package telemetry

import "testing"

func TestPreviewKind(t *testing.T) {
	cases := map[string]string{
		"": "lan", "localhost": "lan", "app.localhost": "lan", "nas.local": "lan", "box.lan": "lan",
		"192.168.1.20": "lan", "10.0.0.5": "lan", "172.20.3.1": "lan", "100.90.1.1": "lan",
		"192.168.1.20.sslip.io": "lan", "10-0-0-5.nip.io": "lan", "127.0.0.1.sslip.io": "lan",
		"147.224.191.9": "ip", "147.224.191.9.sslip.io": "ip", "147-224-191-9.nip.io": "ip",
		"acme.sandboxd.io": "tunnel", "previews.example.com": "domain", "EXAMPLE.COM": "domain",
	}
	for in, want := range cases {
		if got := PreviewKind(in); got != want {
			t.Errorf("PreviewKind(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPropsNeverCarryRawValues(t *testing.T) {
	s := Snapshot{
		SandboxCount: 7, AppCount: 12, Tasks7d: 0, PreviewDomain: "secret-host.example.com", PreviewTLS: true,
		AgentDefault: "OpenCode", Runtime: "gvisor", EgressMode: "allowlist", InstallMethod: "bootstrap",
		DockerVersion: "27.3.1", CPUs: 4, MemBytes: 6 << 30,
	}
	p := Props("v0.3.14", "arm64", "linux", s)
	want := map[string]any{
		"sandbox_bucket": "4-10", "apps_bucket": "10+", "tasks_7d_bucket": "0", "preview_kind": "domain",
		"agent_default": "opencode", "runtime": "gvisor", "egress_mode": "allowlist", "storage_mode": "directory",
		"install_method": "bootstrap", "docker_major": "27", "cpu_bucket": "3-4", "mem_bucket": "4-8g", "$ip": "",
	}
	for k, v := range want {
		if p[k] != v {
			t.Errorf("%s = %v, want %v", k, p[k], v)
		}
	}
	for _, v := range p {
		if str, ok := v.(string); ok && str == "secret-host.example.com" {
			t.Fatalf("raw preview domain leaked into props")
		}
	}
}

func TestBuckets(t *testing.T) {
	if bucketCPUs(0) != "unknown" || bucketCPUs(2) != "1-2" || bucketCPUs(16) != "9+" {
		t.Fatal("cpu buckets")
	}
	if bucketMem(0) != "unknown" || bucketMem(2<<30) != "<4g" || bucketMem(32<<30) != "16g+" {
		t.Fatal("mem buckets")
	}
	if DockerMajor("") != "unknown" || DockerMajor("26.1.4") != "26" || DockerMajor("28") != "28" {
		t.Fatal("docker major")
	}
}

func TestUpgradeProps(t *testing.T) {
	p := UpgradeProps("v0.3.12", "v0.3.13", "rolled_back", "console")
	if p["from"] != "v0.3.12" || p["to"] != "v0.3.13" || p["result"] != "rolled_back" || p["source"] != "console" || p["$ip"] != "" {
		t.Fatalf("unexpected: %v", p)
	}
}
