package netlog

import (
	"testing"
	"time"
)

func TestRecordSnapshotCursor(t *testing.T) {
	r := New()
	r.Record("a", Event{Host: "x.com", Port: "443", IP: "1.2.3.4", Allowed: true})
	r.Record("b", Event{Host: "y.com", Port: "443", Allowed: false, Reason: "no"})
	s3 := r.Record("a", Event{Host: "x.com", Port: "80", IP: "1.2.3.5", Allowed: false, Reason: "blocked"})
	r.AddBytes("a", "x.com", 10, 20)

	ev, hosts, cur := r.Snapshot("a", 0, 0)
	if len(ev) != 2 || cur != s3 {
		t.Fatalf("events=%d cursor=%d", len(ev), cur)
	}
	if len(hosts) != 1 || hosts[0].Allowed != 1 || hosts[0].Denied != 1 || hosts[0].BytesDown != 20 ||
		len(hosts[0].IPs) != 2 || len(hosts[0].Ports) != 2 || hosts[0].LastAllowed {
		t.Fatalf("host = %+v", hosts)
	}
	ev, _, _ = r.Snapshot("a", ev[0].Seq, 0)
	if len(ev) != 1 || ev[0].Seq != s3 {
		t.Fatalf("after cursor: %+v", ev)
	}
	ev, hosts, _ = r.Snapshot("missing", 0, 0)
	if ev == nil || hosts == nil || len(ev) != 0 {
		t.Fatal("empty snapshot must be non-nil and empty")
	}
}

func TestRingAndLimits(t *testing.T) {
	r := New()
	r.MaxEvents, r.MaxHosts, r.MaxSandboxes = 3, 2, 2
	base := time.Unix(1000, 0)
	for i := 0; i < 5; i++ {
		r.Record("a", Event{Host: []string{"h1", "h2", "h3", "h4", "h5"}[i], Time: base.Add(time.Duration(i) * time.Second)})
	}
	ev, hosts, _ := r.Snapshot("a", 0, 0)
	if len(ev) != 3 || ev[0].Host != "h3" || ev[2].Host != "h5" {
		t.Fatalf("ring = %+v", ev)
	}
	if len(hosts) != 2 || hosts[0].Host != "h5" || hosts[1].Host != "h4" {
		t.Fatalf("hosts = %+v", hosts)
	}
	ev, _, _ = r.Snapshot("a", 0, 2)
	if len(ev) != 2 || ev[1].Host != "h5" {
		t.Fatalf("limit = %+v", ev)
	}
	r.Record("b", Event{Host: "x", Time: base.Add(time.Hour)})
	r.Record("c", Event{Host: "x", Time: base.Add(2 * time.Hour)})
	if ev, _, _ := r.Snapshot("a", 0, 0); len(ev) != 0 {
		t.Fatal("oldest sandbox should be evicted")
	}
	r.Forget("c")
	if ev, _, _ := r.Snapshot("c", 0, 0); len(ev) != 0 {
		t.Fatal("forget")
	}
}
