// Package hosttelemetry samples the machine a sandboxd process runs on:
// CPU, memory, disk and network, read straight from procfs and statfs.
//
// Every fleet member (the control plane's own local node, `sandboxd
// worker`, an executor) sends a Snapshot with its heartbeat, so node
// metrics work the same on a plain VPS, under Docker or in a k3s pod. No
// Kubernetes Metrics API, Prometheus or node exporter is needed.
//
// What a container sees:
//   - /proc/stat, /proc/loadavg, /proc/meminfo and /proc/uptime are not
//     namespaced (unless lxcfs is installed), so CPU and memory are the
//     HOST's numbers even inside a container or pod.
//   - Disk is statfs of DiskPath. Inside a container "/" is the overlay
//     on the host's container storage; mount a host path and point
//     SANDBOXD_TELEMETRY_DISK_PATH at it for another filesystem.
//   - Network counters come from <proc>/net/dev, which is the network
//     namespace of the reader. A container on a bridge network reports
//     its own traffic. For the host's traffic run with --network host, or
//     mount the host's /proc read-only (e.g. at /host/proc) and set
//     SANDBOXD_HOST_PROC=/host/proc: the collector then reads
//     /host/proc/1/net/dev, the host init's namespace.
package hosttelemetry

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Snapshot is one sample of a host. Zero values mean "not reported".
// Rates (CPU percent, network bytes/s) need two samples, so the first
// Snapshot of a Collector reports them as 0.
type Snapshot struct {
	CollectedAt time.Time `json:"collected_at"`
	Hostname    string    `json:"hostname,omitempty"`
	OS          string    `json:"os,omitempty"`
	Arch        string    `json:"arch,omitempty"`
	Kernel      string    `json:"kernel,omitempty"`
	UptimeSec   int64     `json:"uptime_sec,omitempty"`
	// InContainer is true when the process runs in a container (Docker,
	// containerd/k8s); the numbers above still describe the host, see
	// the package comment for the exceptions.
	InContainer bool    `json:"in_container,omitempty"`
	CPU         CPU     `json:"cpu"`
	Memory      Memory  `json:"memory"`
	Disk        Disk    `json:"disk"`
	Network     Network `json:"network"`
}

type CPU struct {
	Cores        int     `json:"cores"`
	UsagePercent float64 `json:"usage_percent"` // all cores, 0-100
	Load1        float64 `json:"load1"`
	Load5        float64 `json:"load5"`
	Load15       float64 `json:"load15"`
}

type Memory struct {
	TotalMB     int64 `json:"total_mb"`
	UsedMB      int64 `json:"used_mb"` // total - available
	AvailableMB int64 `json:"available_mb"`
	SwapTotalMB int64 `json:"swap_total_mb,omitempty"`
	SwapUsedMB  int64 `json:"swap_used_mb,omitempty"`
}

type Disk struct {
	Path    string `json:"path,omitempty"`
	TotalMB int64  `json:"total_mb"`
	UsedMB  int64  `json:"used_mb"`
	FreeMB  int64  `json:"free_mb"` // available to unprivileged users
}

type Network struct {
	// Interfaces are the counted interfaces (loopback and container/
	// bridge plumbing like veth*, docker0, cni0 are skipped so traffic
	// is not counted twice).
	Interfaces []string `json:"interfaces,omitempty"`
	RxBytes    uint64   `json:"rx_bytes"`
	TxBytes    uint64   `json:"tx_bytes"`
	RxBps      float64  `json:"rx_bps"` // bytes per second since the last sample
	TxBps      float64  `json:"tx_bps"`
	// HostNamespace is false when the counters come from a container's
	// own network namespace (bridge network, no host /proc).
	HostNamespace bool `json:"host_namespace"`
}

// Collector samples one host. It keeps the previous sample to turn
// counters into rates; Collect is safe for concurrent use.
type Collector struct {
	// ProcRoot is the procfs mount ("/proc" when empty).
	ProcRoot string
	// DiskPath is the filesystem reported under Disk ("/" when empty).
	DiskPath string
	// Now is the clock (time.Now when nil; tests pin it).
	Now func() time.Time

	mu       sync.Mutex
	prevAt   time.Time
	prevCPU  cpuTimes
	prevRx   uint64
	prevTx   uint64
	havePrev bool
}

// FromEnv builds a collector from SANDBOXD_HOST_PROC and
// SANDBOXD_TELEMETRY_DISK_PATH, with defaultDisk as the disk fallback.
// Returns nil when SANDBOXD_TELEMETRY is "off" / "false" / "0".
func FromEnv(defaultDisk string) *Collector {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("SANDBOXD_TELEMETRY"))) {
	case "off", "false", "0", "no":
		return nil
	}
	disk := strings.TrimSpace(os.Getenv("SANDBOXD_TELEMETRY_DISK_PATH"))
	if disk == "" {
		disk = defaultDisk
	}
	return &Collector{ProcRoot: strings.TrimSpace(os.Getenv("SANDBOXD_HOST_PROC")), DiskPath: disk}
}

func (c *Collector) proc() string {
	if c.ProcRoot == "" {
		return "/proc"
	}
	return c.ProcRoot
}

func (c *Collector) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Collect takes one sample. Each part is best-effort: a source that
// cannot be read leaves its fields at zero instead of failing the whole
// snapshot (a heartbeat must never fail because of telemetry).
func (c *Collector) Collect() *Snapshot {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	s := &Snapshot{CollectedAt: now.UTC(), OS: runtime.GOOS, Arch: runtime.GOARCH}
	s.Hostname = c.hostname()
	s.Kernel = strings.TrimSpace(readString(filepath.Join(c.proc(), "sys/kernel/osrelease")))
	s.UptimeSec = readUptime(filepath.Join(c.proc(), "uptime"))
	s.InContainer = inContainer()

	cpu, cores, cpuErr := readCPU(filepath.Join(c.proc(), "stat"))
	s.CPU.Cores = cores
	if s.CPU.Cores == 0 {
		s.CPU.Cores = runtime.NumCPU()
	}
	s.CPU.Load1, s.CPU.Load5, s.CPU.Load15 = readLoad(filepath.Join(c.proc(), "loadavg"))
	s.Memory = readMem(filepath.Join(c.proc(), "meminfo"))
	s.Disk = statDisk(c.diskPath())

	netPath, hostNS := c.netDevPath()
	ifaces, rx, tx, netErr := readNetDev(netPath)
	s.Network = Network{Interfaces: ifaces, RxBytes: rx, TxBytes: tx, HostNamespace: hostNS}

	if c.havePrev {
		if dt := now.Sub(c.prevAt).Seconds(); dt > 0 {
			if cpuErr == nil {
				s.CPU.UsagePercent = cpuPercent(c.prevCPU, cpu)
			}
			if netErr == nil && rx >= c.prevRx && tx >= c.prevTx {
				s.Network.RxBps = round1(float64(rx-c.prevRx) / dt)
				s.Network.TxBps = round1(float64(tx-c.prevTx) / dt)
			}
		}
	}
	c.prevAt, c.havePrev = now, true
	if cpuErr == nil {
		c.prevCPU = cpu
	}
	if netErr == nil {
		c.prevRx, c.prevTx = rx, tx
	}
	return s
}

func (c *Collector) diskPath() string {
	if c.DiskPath == "" {
		return "/"
	}
	return c.DiskPath
}

// netDevPath picks the net/dev file. With a host procfs mounted somewhere
// other than /proc, pid 1's view is the host's network namespace.
func (c *Collector) netDevPath() (string, bool) {
	if c.ProcRoot != "" && c.ProcRoot != "/proc" {
		p := filepath.Join(c.ProcRoot, "1/net/dev")
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	return filepath.Join(c.proc(), "net/dev"), !inContainer()
}

func (c *Collector) hostname() string {
	// With a host procfs mounted, prefer the host's name over the
	// container's (which is usually the container id).
	if c.ProcRoot != "" && c.ProcRoot != "/proc" {
		if h := strings.TrimSpace(readString(filepath.Join(c.ProcRoot, "sys/kernel/hostname"))); h != "" {
			return h
		}
	}
	h, _ := os.Hostname()
	return h
}

// --- procfs parsers (pure on file contents so tests feed fixtures) ---------

type cpuTimes struct{ idle, total uint64 }

func readCPU(path string) (cpuTimes, int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return cpuTimes{}, 0, err
	}
	return parseCPU(b)
}

// parseCPU reads the aggregate "cpu" line of /proc/stat and counts the
// per-core "cpuN" lines.
func parseCPU(b []byte) (cpuTimes, int, error) {
	var agg cpuTimes
	found := false
	cores := 0
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 || !strings.HasPrefix(f[0], "cpu") {
			continue
		}
		if f[0] != "cpu" {
			cores++
			continue
		}
		// user nice system idle iowait irq softirq steal [guest guest_nice]
		// guest time is already included in user/nice, so stop at steal.
		var vals []uint64
		for _, x := range f[1:] {
			v, err := strconv.ParseUint(x, 10, 64)
			if err != nil {
				break
			}
			vals = append(vals, v)
		}
		if len(vals) < 4 {
			return cpuTimes{}, 0, fmt.Errorf("short cpu line")
		}
		n := min(len(vals), 8)
		for _, v := range vals[:n] {
			agg.total += v
		}
		agg.idle = vals[3]
		if len(vals) > 4 {
			agg.idle += vals[4] // iowait is idle time
		}
		found = true
	}
	if !found {
		return cpuTimes{}, cores, fmt.Errorf("no cpu line")
	}
	return agg, cores, nil
}

func cpuPercent(prev, cur cpuTimes) float64 {
	if cur.total <= prev.total || cur.idle < prev.idle {
		return 0
	}
	dt := float64(cur.total - prev.total)
	di := float64(cur.idle - prev.idle)
	p := (dt - di) / dt * 100
	if p < 0 {
		p = 0
	}
	if p > 100 {
		p = 100
	}
	return round1(p)
}

func readLoad(path string) (float64, float64, float64) {
	f := strings.Fields(readString(path))
	if len(f) < 3 {
		return 0, 0, 0
	}
	a, _ := strconv.ParseFloat(f[0], 64)
	b, _ := strconv.ParseFloat(f[1], 64)
	c, _ := strconv.ParseFloat(f[2], 64)
	return a, b, c
}

func readUptime(path string) int64 {
	f := strings.Fields(readString(path))
	if len(f) < 1 {
		return 0
	}
	v, _ := strconv.ParseFloat(f[0], 64)
	return int64(v)
}

func readMem(path string) Memory {
	b, err := os.ReadFile(path)
	if err != nil {
		return Memory{}
	}
	return parseMem(b)
}

func parseMem(b []byte) Memory {
	kv := map[string]int64{}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		k, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			continue
		}
		v, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil {
			continue
		}
		kv[k] = v // kB
	}
	m := Memory{TotalMB: kv["MemTotal"] / 1024, SwapTotalMB: kv["SwapTotal"] / 1024}
	avail, ok := kv["MemAvailable"]
	if !ok { // kernels before 3.14
		avail = kv["MemFree"] + kv["Buffers"] + kv["Cached"]
	}
	m.AvailableMB = avail / 1024
	m.UsedMB = max(m.TotalMB-m.AvailableMB, 0)
	m.SwapUsedMB = max((kv["SwapTotal"]-kv["SwapFree"])/1024, 0)
	return m
}

func statDisk(path string) Disk {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return Disk{Path: path}
	}
	bs := int64(st.Bsize)
	total := int64(st.Blocks) * bs
	free := int64(st.Bfree) * bs
	avail := int64(st.Bavail) * bs
	const mb = 1 << 20
	return Disk{Path: path, TotalMB: total / mb, UsedMB: (total - free) / mb, FreeMB: avail / mb}
}

// skipIface reports interfaces whose traffic is either local or already
// counted on a physical interface: container/bridge/overlay plumbing, and
// VPN tunnels (wg*, tailscale*) whose packets also cross the uplink.
func skipIface(name string) bool {
	if name == "lo" {
		return true
	}
	for _, p := range []string{"veth", "docker", "br-", "cni", "flannel", "cali", "tunl", "vxlan", "kube-", "virbr", "lxc", "weave", "cilium", "genev", "tap", "fwbr", "fwln", "fwpr", "vmbr", "ifb", "dummy", "wg", "tailscale", "zt", "nebula"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func readNetDev(path string) ([]string, uint64, uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, 0, err
	}
	return parseNetDev(b)
}

// parseNetDev sums rx/tx bytes over the counted interfaces of a
// /proc/net/dev file.
func parseNetDev(b []byte) ([]string, uint64, uint64, error) {
	var names []string
	var rx, tx uint64
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		name, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue // the two header lines
		}
		name = strings.TrimSpace(name)
		if skipIface(name) {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 16 {
			continue
		}
		r, err1 := strconv.ParseUint(f[0], 10, 64)
		t, err2 := strconv.ParseUint(f[8], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		names = append(names, name)
		rx += r
		tx += t
	}
	return names, rx, tx, nil
}

func readString(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

var (
	containerOnce sync.Once
	containerVal  bool
)

func inContainer() bool {
	containerOnce.Do(func() {
		if _, err := os.Stat("/.dockerenv"); err == nil {
			containerVal = true
			return
		}
		if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
			containerVal = true
			return
		}
		if _, err := os.Stat("/run/.containerenv"); err == nil { // podman
			containerVal = true
			return
		}
		cg := readString("/proc/1/cgroup")
		containerVal = strings.Contains(cg, "docker") || strings.Contains(cg, "kubepods") || strings.Contains(cg, "containerd")
	})
	return containerVal
}

func round1(v float64) float64 {
	return float64(int64(v*10+0.5)) / 10
}
