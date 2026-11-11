package hosttelemetry

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeProc(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

const netDev1 = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 5000      10    0    0    0     0          0         0     5000      10    0    0    0     0       0          0
  eth0: 1000      10    0    0    0     0          0         0     2000      10    0    0    0     0       0          0
veth12: 9999      10    0    0    0     0          0         0     9999      10    0    0    0     0       0          0
docker0: 7777     10    0    0    0     0          0         0     7777      10    0    0    0     0       0          0
`

const netDev2 = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 6000      10    0    0    0     0          0         0     6000      10    0    0    0     0       0          0
  eth0: 11000     10    0    0    0     0          0         0     4000      10    0    0    0     0       0          0
`

func TestCollectorRatesAndParsing(t *testing.T) {
	root := t.TempDir()
	writeProc(t, root, map[string]string{
		"stat":                 "cpu  100 0 100 700 100 0 0 0 0 0\ncpu0 50 0 50 350 50 0 0 0 0 0\ncpu1 50 0 50 350 50 0 0 0 0 0\nintr 1\n",
		"loadavg":              "0.50 0.25 0.10 1/100 123\n",
		"meminfo":              "MemTotal:       4194304 kB\nMemFree:         100000 kB\nMemAvailable:    1048576 kB\nSwapTotal:       1048576 kB\nSwapFree:         524288 kB\n",
		"uptime":               "3600.55 100.00\n",
		"net/dev":              netDev1,
		"sys/kernel/osrelease": "6.1.0-test\n",
	})
	now := time.Unix(1000, 0)
	c := &Collector{ProcRoot: root, DiskPath: root, Now: func() time.Time { return now }}

	s1 := c.Collect()
	if s1.CPU.Cores != 2 {
		t.Fatalf("cores = %d, want 2", s1.CPU.Cores)
	}
	if s1.CPU.UsagePercent != 0 || s1.Network.RxBps != 0 {
		t.Fatalf("first sample must not report rates: %+v", s1)
	}
	if s1.Memory.TotalMB != 4096 || s1.Memory.AvailableMB != 1024 || s1.Memory.UsedMB != 3072 {
		t.Fatalf("memory = %+v", s1.Memory)
	}
	if s1.Memory.SwapTotalMB != 1024 || s1.Memory.SwapUsedMB != 512 {
		t.Fatalf("swap = %+v", s1.Memory)
	}
	if s1.CPU.Load1 != 0.5 || s1.CPU.Load15 != 0.1 {
		t.Fatalf("load = %+v", s1.CPU)
	}
	if s1.UptimeSec != 3600 || s1.Kernel != "6.1.0-test" {
		t.Fatalf("uptime/kernel = %d %q", s1.UptimeSec, s1.Kernel)
	}
	// /proc/1/net/dev does not exist in the fixture, so the plain net/dev
	// is used; only eth0 counts (lo, veth*, docker0 skipped).
	if s1.Network.RxBytes != 1000 || s1.Network.TxBytes != 2000 || len(s1.Network.Interfaces) != 1 {
		t.Fatalf("network = %+v", s1.Network)
	}
	if s1.Disk.TotalMB <= 0 {
		t.Fatalf("disk = %+v", s1.Disk)
	}

	// 10 s later: 1000 jiffies passed, 500 of them idle (idle 600->1000 +
	// iowait 100->200 = +500) → 50 %; eth0 rx +10000 B, tx +2000 B.
	writeProc(t, root, map[string]string{
		"stat":    "cpu  300 0 400 1100 200 0 0 0 0 0\ncpu0 1 0 1 1 1 0 0 0 0 0\ncpu1 1 0 1 1 1 0 0 0 0 0\n",
		"net/dev": netDev2,
	})
	now = now.Add(10 * time.Second)
	s2 := c.Collect()
	if s2.CPU.UsagePercent != 50 {
		t.Fatalf("cpu usage = %v, want 50", s2.CPU.UsagePercent)
	}
	if s2.Network.RxBps != 1000 || s2.Network.TxBps != 200 {
		t.Fatalf("net rates = %v / %v", s2.Network.RxBps, s2.Network.TxBps)
	}
}

func TestHostProcPrefersPID1NetNamespace(t *testing.T) {
	root := t.TempDir()
	writeProc(t, root, map[string]string{
		"net/dev":   netDev1, // the container's own namespace
		"1/net/dev": netDev2, // host init's namespace
	})
	c := &Collector{ProcRoot: root}
	s := c.Collect()
	if !s.Network.HostNamespace || s.Network.RxBytes != 11000 {
		t.Fatalf("want host namespace counters, got %+v", s.Network)
	}
}

func TestMissingSourcesDoNotFail(t *testing.T) {
	c := &Collector{ProcRoot: filepath.Join(t.TempDir(), "nope"), DiskPath: "/definitely/not/here"}
	s := c.Collect()
	if s == nil || s.CPU.Cores == 0 {
		t.Fatalf("expected a best-effort snapshot with NumCPU fallback, got %+v", s)
	}
	if s.Memory.TotalMB != 0 || s.Disk.TotalMB != 0 {
		t.Fatalf("unreadable sources must stay zero: %+v", s)
	}
	var nilC *Collector
	if nilC.Collect() != nil {
		t.Fatal("nil collector must return nil")
	}
}

func TestMemFallbackWithoutMemAvailable(t *testing.T) {
	m := parseMem([]byte("MemTotal: 2097152 kB\nMemFree: 524288 kB\nBuffers: 262144 kB\nCached: 262144 kB\n"))
	if m.AvailableMB != 1024 || m.UsedMB != 1024 {
		t.Fatalf("mem = %+v", m)
	}
}

func TestFromEnvOff(t *testing.T) {
	t.Setenv("SANDBOXD_TELEMETRY", "off")
	if FromEnv("/") != nil {
		t.Fatal("telemetry off must return nil")
	}
	t.Setenv("SANDBOXD_TELEMETRY", "")
	t.Setenv("SANDBOXD_TELEMETRY_DISK_PATH", "/data")
	if c := FromEnv("/"); c == nil || c.DiskPath != "/data" {
		t.Fatalf("got %+v", c)
	}
}
