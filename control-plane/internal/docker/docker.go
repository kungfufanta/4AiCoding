// Package docker is a thin, typed wrapper around the `docker` CLI.
//
// Shells out to the `docker` CLI via os/exec. Replace with the Docker
// SDK only when shell-out is a measured bottleneck.
//
// Design rule: NO MAGIC DEFAULTS inside this package. Callers pass
// the full RunSpec so a change to the locked flag set is obvious in
// code review. The package only encodes the CLI invocation, not the
// policy choices.
package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// ErrNotFound is returned when `docker inspect` reports the target
// container does not exist.
var ErrNotFound = errors.New("container not found")

// Client is the entry point for the wrapper.
type Client struct {
	// Bin is the docker binary path. Defaults to "docker" via NewClient.
	Bin string
}

func NewClient() *Client { return &Client{Bin: "docker"} }

// RunSpec carries every flag the Phase 2/3 sandbox-up script emitted.
// No defaults; callers fill all of these explicitly.
type RunSpec struct {
	Name        string   // --name
	Hostname    string   // --hostname
	Network     string   // --network (OSS: shared sandboxd_net so Traefik can route)
	Userns      string   // --userns (OSS: "host" for deterministic workspace ownership)
	Runtime     string   // --runtime ("runsc"=gVisor); "" = daemon default (runc)
	ReadOnly    bool     // --read-only
	CapDrop     []string // --cap-drop=ALL (passed once per entry)
	SecurityOpt []string // --security-opt=no-new-privileges
	CPUShares   int      // --cpu-shares=100
	CPUs        string   // --cpus (hard CPU limit); "" = none
	Memory      string   // --memory=10g
	MemorySwap  string   // --memory-swap=10g
	PidsLimit   int      // --pids-limit=1024
	Ulimits     []string // --ulimit nofile=65536:65536
	Tmpfs       []string // --tmpfs /tmp:size=512m   (one --tmpfs flag per entry)
	Env         []string // --env KEY=VAL   (one --env flag per entry)
	Volumes     []string // -v <host>:<container>[:opts]
	Labels      []string // --label key=value
	User        string   // --user uid:gid ("" = the image's user)
	ShmSize     string   // --shm-size (e.g. 1g for a browser); "" = docker default
	Restart     string   // --restart policy ("" = none)
	Image       string   // last positional
	Cmd         []string // optional CMD override
}

// Run starts the container detached and returns its 12-char short ID.
func (c *Client) Run(ctx context.Context, spec RunSpec) (string, error) {
	args := []string{"run", "-d"}
	if spec.Name != "" {
		args = append(args, "--name", spec.Name)
	}
	if spec.Hostname != "" {
		args = append(args, "--hostname", spec.Hostname)
	}
	if spec.Network != "" {
		args = append(args, "--network", spec.Network)
	}
	if spec.Userns != "" {
		args = append(args, "--userns", spec.Userns)
	}
	if spec.Runtime != "" {
		args = append(args, "--runtime", spec.Runtime)
	}
	if spec.ReadOnly {
		args = append(args, "--read-only")
	}
	for _, v := range spec.CapDrop {
		args = append(args, "--cap-drop="+v)
	}
	for _, v := range spec.SecurityOpt {
		args = append(args, "--security-opt="+v)
	}
	if spec.CPUShares > 0 {
		args = append(args, fmt.Sprintf("--cpu-shares=%d", spec.CPUShares))
	}
	if spec.CPUs != "" {
		args = append(args, "--cpus="+spec.CPUs)
	}
	if spec.Memory != "" {
		args = append(args, "--memory="+spec.Memory)
	}
	if spec.MemorySwap != "" {
		args = append(args, "--memory-swap="+spec.MemorySwap)
	}
	if spec.PidsLimit > 0 {
		args = append(args, fmt.Sprintf("--pids-limit=%d", spec.PidsLimit))
	}
	for _, v := range spec.Ulimits {
		args = append(args, "--ulimit", v)
	}
	for _, v := range spec.Tmpfs {
		args = append(args, "--tmpfs", v)
	}
	for _, v := range spec.Env {
		args = append(args, "--env", v)
	}
	for _, v := range spec.Volumes {
		args = append(args, "-v", v)
	}
	for _, v := range spec.Labels {
		args = append(args, "--label", v)
	}
	if spec.User != "" {
		args = append(args, "--user", spec.User)
	}
	if spec.ShmSize != "" {
		args = append(args, "--shm-size="+spec.ShmSize)
	}
	if spec.Restart != "" {
		args = append(args, "--restart="+spec.Restart)
	}
	if spec.Image == "" {
		return "", errors.New("docker.Run: spec.Image is required")
	}
	args = append(args, spec.Image)
	args = append(args, spec.Cmd...)
	out, err := c.run(ctx, args...)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			// Surface the daemon's reason (bad image, rlimit, …) instead of a
			// bare "exit status 125".
			return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", err
	}
	full := strings.TrimSpace(string(out))
	if len(full) >= 12 {
		return full[:12], nil
	}
	return full, nil
}

// ContainerJSON is the slice of `docker inspect` output we care
// about. Fields can be added as needed; unknown fields are ignored
// by encoding/json.
type ContainerJSON struct {
	ID    string `json:"Id"`
	State struct {
		Status  string `json:"Status"`
		Running bool   `json:"Running"`
		Pid     int    `json:"Pid"`
	} `json:"State"`
	Config struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
		User   string            `json:"User"`
		Cmd    []string          `json:"Cmd"`
	} `json:"Config"`
	// Mounts lists what is mounted into the container (bind / volume /
	// tmpfs). Surfaced by GET /v1/sandboxes/{id}/volumes.
	Mounts []Mount `json:"Mounts"`
	// NetworkSettings is the subset Phase 5's wake-readiness probe
	// needs: the container's bridge IP on the default network. We
	// deliberately surface only what we use; adding fields later is
	// non-breaking because callers reach in by name.
	NetworkSettings struct {
		IPAddress string                          `json:"IPAddress"`
		Networks  map[string]*NetworkEndpointInfo `json:"Networks"`
	} `json:"NetworkSettings"`
}

// NetworkEndpointInfo carries the per-network endpoint info; the
// wake-readiness probe walks Networks["bridge"].IPAddress when the
// top-level IPAddress is empty (which it can be when a non-default
// network is the only attachment).
type NetworkEndpointInfo struct {
	IPAddress string `json:"IPAddress"`
}

// BridgeIP returns the container's bridge IP, preferring the
// top-level IPAddress and falling back to the explicit "bridge"
// entry under Networks. Empty string when the container has no
// bridge attachment yet (race against docker start).
func (cj *ContainerJSON) BridgeIP() string {
	if cj.NetworkSettings.IPAddress != "" {
		return cj.NetworkSettings.IPAddress
	}
	// Prefer the legacy default bridge if present, then fall back to the
	// first network that has an IP. In the OSS build sandboxes join a
	// user-defined network (sandboxd_net), so the "bridge" key is absent
	// and we must scan whatever network the container actually got.
	if n, ok := cj.NetworkSettings.Networks["bridge"]; ok && n != nil && n.IPAddress != "" {
		return n.IPAddress
	}
	for _, n := range cj.NetworkSettings.Networks {
		if n != nil && n.IPAddress != "" {
			return n.IPAddress
		}
	}
	return ""
}

// Inspect returns the parsed JSON for a container, or ErrNotFound.
// The "not found" detection uses a CASE-INSENSITIVE substring match
// because docker's exact wording varies by version and locale:
//
//	"Error response from daemon: No such object: s-..."   (older / some)
//	"Error: no such object: s-..."                         (current)
//	"Error response from daemon: no such container: s-..." (legacy)
//
// The original Phase 4 draft used case-sensitive matches and missed
// the lowercase 'no such object' variant Docker emitted on this host,
// causing the reconciler's V6 path to mis-handle missing containers.
// Issue #7 in ops/implementation/phase-4-report.md.
func (c *Client) Inspect(ctx context.Context, name string) (*ContainerJSON, error) {
	out, err := c.run(ctx, "inspect", "--format", "{{json .}}", name)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if isNotFoundStderr(exitErr.Stderr) {
				return nil, ErrNotFound
			}
		}
		return nil, err
	}
	var cj ContainerJSON
	if err := json.Unmarshal(bytes.TrimSpace(out), &cj); err != nil {
		return nil, fmt.Errorf("inspect: parse json: %w", err)
	}
	return &cj, nil
}

// Remove forces removal. Idempotent — returns nil on "no such container".
func (c *Client) Remove(ctx context.Context, name string) error {
	_, err := c.run(ctx, "rm", "-f", name)
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if isNotFoundStderr(exitErr.Stderr) {
			return nil
		}
	}
	return err
}

// isNotFoundStderr matches docker's varying "not found" wordings
// case-insensitively. See Inspect doc-comment for the catalogue.
func isNotFoundStderr(stderr []byte) bool {
	low := strings.ToLower(string(stderr))
	return strings.Contains(low, "no such object") ||
		strings.Contains(low, "no such container")
}

// Start runs `docker start <name>`. Idempotent — a no-op when the
// container is already running. Required by Phase 5's wake path.
func (c *Client) Start(ctx context.Context, name string) error {
	_, err := c.run(ctx, "start", name)
	return err
}

// Stop runs `docker stop --time=<sec> <name>`. The kernel still kills
// the container after the grace period, so a runaway process can't
// pin the call open indefinitely. Required by Phase 5's idle and
// pressure reapers.
//
// Idempotent: docker stop on an already-stopped container is a no-op
// (exit 0). On a missing container, returns ErrNotFound so callers
// can distinguish from a transient daemon error.
func (c *Client) Stop(ctx context.Context, name string, timeoutSec int) error {
	if timeoutSec < 0 {
		timeoutSec = 0
	}
	_, err := c.run(ctx, "stop", "--time", fmt.Sprintf("%d", timeoutSec), name)
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if isNotFoundStderr(exitErr.Stderr) {
			return ErrNotFound
		}
	}
	return err
}

// ExecResult carries the stdout/stderr/exit-code of a non-interactive
// `docker exec` call. See API exec semantics in §7 of the roadmap.
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Exec runs a non-interactive command inside the named container.
// There is deliberately no PTY / TTY / stdin support.
func (c *Client) Exec(ctx context.Context, name string, cmd []string) (ExecResult, error) {
	args := append([]string{"exec", name}, cmd...)
	cmdEx := exec.CommandContext(ctx, c.Bin, args...)
	var stdout, stderr bytes.Buffer
	cmdEx.Stdout = &stdout
	cmdEx.Stderr = &stderr
	err := cmdEx.Run()
	res := ExecResult{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: 0}
	if err == nil {
		return res, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		res.ExitCode = exitErr.ExitCode()
		return res, nil
	}
	return res, err
}

// --- exec v2 (AGENT_PLAN A0) -----------------------------------------

// ExecV2Request is the extended exec call: working directory, extra env,
// stdin, a hard timeout (enforced INSIDE the container by the `timeout`
// coreutils wrapper so the whole process group dies), and a bounded
// output buffer (head + tail kept, middle dropped — the control plane
// never buffers unbounded output in RAM).
type ExecV2Request struct {
	Cmd []string
	// WorkDir is passed as docker exec -w. "" = container default.
	WorkDir string
	// Env entries passed as docker exec -e K=V. The API layer validates
	// them (no PATH/HOME/LD_* overrides); this layer only encodes.
	Env []string
	// Stdin (optional) is fed to `docker exec -i`. nil = no stdin.
	Stdin []byte
	// TimeoutS > 0 wraps the command in `timeout --signal=KILL <n>` so
	// expiry kills the child's whole process group inside the container.
	TimeoutS int
	// MaxOutputBytes > 0 bounds stdout+stderr independently: over the
	// limit the head and tail of the stream are kept (75%/25%) and
	// Truncated reports it. 0 = unbounded (legacy behaviour).
	MaxOutputBytes int64
}

// ExecV2Result adds timeout/truncation/duration to ExecResult.
type ExecV2Result struct {
	Stdout     string
	Stderr     string
	ExitCode   int
	TimedOut   bool // the in-container timeout wrapper fired (exit 124)
	Truncated  bool // output exceeded MaxOutputBytes (head+tail kept)
	DurationMs int64
}

// execTimeoutExit is GNU timeout's "command timed out" exit code.
const execTimeoutExit = 124

// ExecV2 runs a non-interactive command with the v2 semantics above. The
// argv without any v2 fields is identical to Exec's, so a plain
// {cmd} request behaves byte-for-byte like the legacy path.
func (c *Client) ExecV2(ctx context.Context, name string, req ExecV2Request) (ExecV2Result, error) {
	args := []string{"exec"}
	if req.Stdin != nil {
		args = append(args, "-i")
	}
	if req.WorkDir != "" {
		args = append(args, "-w", req.WorkDir)
	}
	for _, e := range req.Env {
		args = append(args, "-e", e)
	}
	args = append(args, name)
	inner := req.Cmd
	if req.TimeoutS > 0 {
		args = append(args, "timeout", "--signal=KILL", strconv.Itoa(req.TimeoutS), "--")
	}
	args = append(args, inner...)
	started := time.Now()
	cmdEx := exec.CommandContext(ctx, c.Bin, args...)
	cmdEx.Stdin = stdinOrEmpty(req.Stdin)
	var stdout, stderr bytes.Buffer
	var outTrunc, errTrunc bool
	var outW, errW *headTailWriter
	if req.MaxOutputBytes > 0 {
		outW = newHeadTailWriter(&stdout, req.MaxOutputBytes, &outTrunc)
		errW = newHeadTailWriter(&stderr, req.MaxOutputBytes, &errTrunc)
		cmdEx.Stdout = outW
		cmdEx.Stderr = errW
	} else {
		cmdEx.Stdout = &stdout
		cmdEx.Stderr = &stderr
	}
	err := cmdEx.Run()
	if outW != nil {
		outW.flushTail()
	}
	if errW != nil {
		errW.flushTail()
	}
	res := ExecV2Result{
		Stdout:     stdout.String(),
		Stderr:     stderr.String(),
		ExitCode:   0,
		Truncated:  outTrunc || errTrunc,
		DurationMs: time.Since(started).Milliseconds(),
	}
	if err == nil {
		return res, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		res.ExitCode = exitErr.ExitCode()
		res.TimedOut = req.TimeoutS > 0 && res.ExitCode == execTimeoutExit
		return res, nil
	}
	return res, err
}

// stdinOrEmpty returns a reader for the exec stdin. With -i, docker always
// reads stdin; an empty non-nil reader closes it immediately.
func stdinOrEmpty(in []byte) io.Reader {
	if in == nil {
		return bytes.NewReader(nil)
	}
	return bytes.NewReader(in)
}

// ExecV2Stream is the streaming form of ExecV2 for `?stream=ndjson`:
// stdout/stderr arrive as live readers (no control-plane buffering) and
// wait() collects the exit status after both readers hit EOF. The timeout
// wrapper and env/workdir/stdin handling are identical to ExecV2.
// MaxOutputBytes is ignored here — streaming IS the unbounded-output
// escape hatch; the API layer frames the bytes as ndjson lines.
func (c *Client) ExecV2Stream(ctx context.Context, name string, req ExecV2Request) (
	stdout, stderr io.ReadCloser, wait func() (ExecV2Result, error), err error,
) {
	args := []string{"exec"}
	if req.Stdin != nil {
		args = append(args, "-i")
	}
	if req.WorkDir != "" {
		args = append(args, "-w", req.WorkDir)
	}
	for _, e := range req.Env {
		args = append(args, "-e", e)
	}
	args = append(args, name)
	if req.TimeoutS > 0 {
		args = append(args, "timeout", "--signal=KILL", strconv.Itoa(req.TimeoutS), "--")
	}
	args = append(args, req.Cmd...)
	cmdEx := exec.CommandContext(ctx, c.Bin, args...)
	cmdEx.Stdin = stdinOrEmpty(req.Stdin)
	stdout, err = cmdEx.StdoutPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	stderr, err = cmdEx.StderrPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	started := time.Now()
	if err := cmdEx.Start(); err != nil {
		return nil, nil, nil, err
	}
	wait = func() (ExecV2Result, error) {
		res := ExecV2Result{ExitCode: 0, DurationMs: time.Since(started).Milliseconds()}
		err := cmdEx.Wait()
		if err == nil {
			return res, nil
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
			res.TimedOut = req.TimeoutS > 0 && res.ExitCode == execTimeoutExit
			return res, nil
		}
		return res, err
	}
	return stdout, stderr, wait, nil
}

// headTailWriter keeps at most `max` bytes: the FIRST max bytes of the
// stream (the head) and, once the limit is exceeded, the trailing max/4
// bytes (the tail, flushed after a marker when the process exits). RAM
// per stream is bounded by max + max/4 regardless of output size.
type headTailWriter struct {
	dst       *bytes.Buffer
	max       int64
	headLen   int64 // bytes currently kept in dst (the head)
	written   int64 // total bytes seen
	truncated *bool
	tail      []byte
}

func newHeadTailWriter(dst *bytes.Buffer, max int64, truncated *bool) *headTailWriter {
	return &headTailWriter{dst: dst, max: max, truncated: truncated}
}

func (w *headTailWriter) Write(p []byte) (int, error) {
	n := len(p)
	w.written += int64(n)
	if room := w.max - w.headLen; room > 0 {
		take := int64(len(p))
		if take > room {
			take = room
		}
		w.dst.Write(p[:take])
		w.headLen += take
		p = p[take:]
	}
	if len(p) > 0 {
		*w.truncated = true
		keep := w.max / 4
		if keep <= 0 {
			keep = 1
		}
		w.tail = append(w.tail, p...)
		if int64(len(w.tail)) > keep {
			w.tail = w.tail[int64(len(w.tail))-keep:]
		}
	}
	return n, nil
}

// flushTail appends the kept tail after a marker. Call exactly once after
// the process exits.
func (w *headTailWriter) flushTail() {
	if !*w.truncated {
		return
	}
	w.dst.WriteString("\n-- output truncated --\n")
	w.dst.Write(w.tail)
	w.tail = nil
}

// Logs streams the container's stdout+stderr (`docker logs`), merged in the
// order docker emits them. tail <= 0 means all lines. With follow the reader
// stays open until ctx is cancelled or the container stops. The caller MUST
// Close the returned reader; Close also reaps the docker CLI process.
func (c *Client) Logs(ctx context.Context, name string, tail int, follow bool) (io.ReadCloser, error) {
	args := []string{"logs"}
	if tail > 0 {
		args = append(args, "--tail", strconv.Itoa(tail))
	}
	if follow {
		args = append(args, "--follow")
	}
	args = append(args, name)
	cmd := exec.CommandContext(ctx, c.Bin, args...)
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() { pw.CloseWithError(cmd.Wait()) }()
	return &logsReader{PipeReader: pr, cmd: cmd}, nil
}

type logsReader struct {
	*io.PipeReader
	cmd *exec.Cmd
}

func (l *logsReader) Close() error {
	if l.cmd.Process != nil {
		_ = l.cmd.Process.Kill()
	}
	return l.PipeReader.Close()
}

// ListByNamePrefix returns the names of all containers (any state)
// whose name starts with the given prefix. Used by the reconciler to
// find orphans.
func (c *Client) ListByNamePrefix(ctx context.Context, prefix string) ([]string, error) {
	out, err := c.run(ctx, "ps", "-a", "--filter", "name=^"+prefix, "--format", "{{.Names}}")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			names = append(names, line)
		}
	}
	return names, nil
}

// ServerVersion returns the daemon version via `docker version`, which keeps
// working on daemons where `docker info` fails to render (seen on 28.5:
// "netip.ParsePrefix: no '/'"). Used by telemetry; readyz keeps using Info.
func (c *Client) ServerVersion(ctx context.Context) (string, error) {
	out, err := c.run(ctx, "version", "--format", "{{.Server.Version}}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Info runs `docker info -f {{.ServerVersion}}` for the readyz probe.
func (c *Client) Info(ctx context.Context) (string, error) {
	out, err := c.run(ctx, "info", "--format", "{{.ServerVersion}}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// run is the single chokepoint for invoking the docker CLI.
func (c *Client) run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.Bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitErr.Stderr = stderr.Bytes()
			return stdout.Bytes(), exitErr
		}
		return stdout.Bytes(), fmt.Errorf("docker %s: %w (%s)", args[0], err, stderr.String())
	}
	return stdout.Bytes(), nil
}

// Mount is one entry of `docker inspect .Mounts`.
type Mount struct {
	Type        string `json:"Type"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	Mode        string `json:"Mode"`
	RW          bool   `json:"RW"`
}

// Stats is one `docker stats --no-stream` sample, parsed into numbers.
type Stats struct {
	CPUPercent    float64 `json:"cpu_percent"`
	MemUsageBytes int64   `json:"mem_usage_bytes"`
	MemLimitBytes int64   `json:"mem_limit_bytes"`
	MemPercent    float64 `json:"mem_percent"`
	NetRxBytes    int64   `json:"net_rx_bytes"`
	NetTxBytes    int64   `json:"net_tx_bytes"`
	BlockRead     int64   `json:"block_read_bytes"`
	BlockWrite    int64   `json:"block_write_bytes"`
	PIDs          int     `json:"pids"`
}

// Stats takes one resource sample of a running container.
func (c *Client) Stats(ctx context.Context, name string) (Stats, error) {
	out, err := c.run(ctx, "stats", "--no-stream", "--format", "{{json .}}", name)
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && isNotFoundStderr(exitErr.Stderr) {
			return Stats{}, ErrNotFound
		}
		return Stats{}, err
	}
	return ParseStats(out)
}

// ParseStats parses the `docker stats --format '{{json .}}'` line, whose
// numbers are human strings ("12.5%", "1.2MiB / 10GiB", "3kB / 1kB").
func ParseStats(line []byte) (Stats, error) {
	var raw struct {
		CPUPerc  string `json:"CPUPerc"`
		MemUsage string `json:"MemUsage"`
		MemPerc  string `json:"MemPerc"`
		NetIO    string `json:"NetIO"`
		BlockIO  string `json:"BlockIO"`
		PIDs     string `json:"PIDs"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(line), &raw); err != nil {
		return Stats{}, fmt.Errorf("parse docker stats: %w", err)
	}
	var st Stats
	st.CPUPercent = parsePercent(raw.CPUPerc)
	st.MemPercent = parsePercent(raw.MemPerc)
	st.MemUsageBytes, st.MemLimitBytes = parsePair(raw.MemUsage)
	st.NetRxBytes, st.NetTxBytes = parsePair(raw.NetIO)
	st.BlockRead, st.BlockWrite = parsePair(raw.BlockIO)
	st.PIDs, _ = strconv.Atoi(strings.TrimSpace(raw.PIDs))
	return st, nil
}

func parsePercent(s string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(s), "%"), 64)
	return f
}

func parsePair(s string) (int64, int64) {
	a, b, ok := strings.Cut(s, "/")
	if !ok {
		return ParseSize(a), 0
	}
	return ParseSize(a), ParseSize(b)
}

// ParseSize converts a docker human size ("1.5MiB", "3kB", "0B") to bytes.
func ParseSize(s string) int64 {
	s = strings.TrimSpace(s)
	units := []struct {
		suffix string
		mult   float64
	}{
		{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"TiB", 1 << 40},
		{"kB", 1e3}, {"KB", 1e3}, {"MB", 1e6}, {"GB", 1e9}, {"TB", 1e12}, {"B", 1},
	}
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			f, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(s, u.suffix)), 64)
			if err != nil {
				return 0
			}
			return int64(math.Round(f * u.mult))
		}
	}
	f, _ := strconv.ParseFloat(s, 64)
	return int64(f)
}

// NetworkSubnets returns the IPAM subnets of a docker network and their
// gateways (used to deny API requests that originate from the sandbox
// network while still allowing the host, which appears as the gateway).
func (c *Client) NetworkSubnets(ctx context.Context, network string) (subnets, gateways []string, err error) {
	out, err := c.run(ctx, "network", "inspect", "--format", "{{range .IPAM.Config}}{{.Subnet}}|{{.Gateway}} {{end}}", network)
	if err != nil {
		return nil, nil, err
	}
	for _, f := range strings.Fields(string(out)) {
		sub, gw, _ := strings.Cut(f, "|")
		if sub != "" {
			subnets = append(subnets, sub)
		}
		if gw != "" {
			gateways = append(gateways, gw)
		}
	}
	return subnets, gateways, nil
}
