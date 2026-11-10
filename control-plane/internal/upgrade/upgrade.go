// Package upgrade runs an in-place sandboxd upgrade from the API.
//
// The control plane cannot upgrade itself from inside its own container —
// upgrade.sh rebuilds and recreates that container — so the work is handed to
// a DETACHED upgrader container (image sandboxd-upgrader:<version>, docker
// CLI + compose + git) with the docker socket and the source checkout mounted
// at the same host path. It runs the existing ./upgrade.sh <tag>, which backs
// up, rebuilds, restarts, health-checks and rolls back on failure. Progress is
// a file under the data dir so it survives the control plane restart.
package upgrade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/docker"
)

const (
	ContainerName = "sandboxd-upgrader"
	stateFile     = "upgrade.json"
	logFile       = "upgrade.log"
	minFreeBytes  = 2 << 30 // a source build needs headroom
)

var (
	ErrRunning   = errors.New("an upgrade is already running")
	ErrBadTarget = errors.New("target is not a published release")
	ErrNoSrcDir  = errors.New("SANDBOXD_SRC_DIR is not set — upgrade from the CLI: ./upgrade.sh")
	ErrLowDisk   = errors.New("less than 2 GB free in the data dir")
	tagRe        = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)
)

// Runner is the docker subset we use (satisfied by *docker.Client; faked in tests).
type Runner interface {
	Run(ctx context.Context, spec docker.RunSpec) (string, error)
	Inspect(ctx context.Context, name string) (*docker.ContainerJSON, error)
	Remove(ctx context.Context, name string) error
}

// State is what GET /v1/upgrade returns and what upgrade.sh's wrapper writes.
type State struct {
	Phase     string `json:"phase"` // idle | running | succeeded | failed | rolled_back
	Target    string `json:"target,omitempty"`
	From      string `json:"from,omitempty"`
	StartedAt string `json:"started_at,omitempty"`
	EndedAt   string `json:"ended_at,omitempty"`
	Message   string `json:"message,omitempty"`
	LogTail   string `json:"log_tail,omitempty"`
	// Reported is set once the terminal result has been passed to telemetry,
	// so a finished upgrade is counted exactly once across restarts.
	Reported bool `json:"reported,omitempty"`
}

type Manager struct {
	Docker        Runner
	DataDir       string // host path, also mounted in the control plane at the same path
	SrcDir        string // host path of the checkout ("" = unsupported install)
	UpgraderImage string // e.g. sandboxd-upgrader:v0.3.10
	Version       string // running version (State.From)
	// ReleaseExists reports whether tag is a published release. Injected so
	// tests never hit GitHub.
	ReleaseExists func(ctx context.Context, tag string) bool
	// FreeBytes reports free space at path. Injected for tests.
	FreeBytes func(path string) uint64
	// Now for deterministic tests.
	Now func() time.Time

	mu sync.Mutex
}

func (m *Manager) stateDir() string  { return filepath.Join(m.DataDir, "state") }
func (m *Manager) statePath() string { return filepath.Join(m.stateDir(), stateFile) }
func (m *Manager) logPath() string   { return filepath.Join(m.stateDir(), logFile) }

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) freeBytes(p string) uint64 {
	if m.FreeBytes != nil {
		return m.FreeBytes(p)
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(p, &st); err != nil {
		return ^uint64(0) // unknown: do not block the upgrade on a stat failure
	}
	return st.Bavail * uint64(st.Bsize)
}

// Status returns the persisted state, reconciled with the container: a state
// that says "running" while no upgrader container exists means the upgrader
// died before writing a result.
func (m *Manager) Status(ctx context.Context) State {
	st := m.read()
	if st.Phase == "running" && m.Docker != nil {
		if _, err := m.Docker.Inspect(ctx, ContainerName); errors.Is(err, docker.ErrNotFound) {
			st.Phase = "failed"
			st.Message = "upgrader container disappeared before reporting a result"
			st.EndedAt = m.now().UTC().Format(time.RFC3339)
			m.write(st)
		}
	}
	st.LogTail = m.tail(4000)
	return st
}

// Start validates and launches the detached upgrader.
func (m *Manager) Start(ctx context.Context, target string) (State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.SrcDir == "" {
		return State{}, ErrNoSrcDir
	}
	if !tagRe.MatchString(target) {
		return State{}, ErrBadTarget
	}
	if m.ReleaseExists != nil && !m.ReleaseExists(ctx, target) {
		return State{}, ErrBadTarget
	}
	if cur := m.Status(ctx); cur.Phase == "running" {
		return cur, ErrRunning
	}
	if m.freeBytes(m.DataDir) < minFreeBytes {
		return State{}, ErrLowDisk
	}
	// A previous upgrader that finished is a stopped container with our name;
	// clear it so the name is free (ignore not-found).
	if m.Docker != nil {
		_ = m.Docker.Remove(ctx, ContainerName)
	}

	if err := os.MkdirAll(m.stateDir(), 0o755); err != nil {
		return State{}, err
	}
	_ = os.WriteFile(m.logPath(), nil, 0o644)
	st := State{Phase: "running", Target: target, From: m.Version,
		StartedAt: m.now().UTC().Format(time.RFC3339)}
	if err := m.write(st); err != nil {
		return State{}, err
	}

	// The wrapper records the outcome into the state file. upgrade.sh exits 0
	// on success and non-zero after a rollback (or on failure).
	script := fmt.Sprintf(`set -o pipefail
cd %q || { echo "src dir missing" >> %q; exit 97; }
if ./upgrade.sh %q >> %q 2>&1; then
  printf '{"phase":"succeeded","target":%q,"from":%q,"started_at":%q,"ended_at":"%%s","message":"upgraded to %s"}\n' "$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)" > %q
else
  rc=$?
  if grep -q 'rolled back' %q 2>/dev/null; then ph=rolled_back; msg="build or health check failed; rolled back to %s"; else ph=failed; msg="upgrade failed (exit $rc); see log"; fi
  printf '{"phase":"%%s","target":%q,"from":%q,"started_at":%q,"ended_at":"%%s","message":"%%s"}\n' "$ph" "$(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)" "$msg" > %q
  exit $rc
fi`,
		m.SrcDir, m.logPath(), target, m.logPath(),
		target, m.Version, st.StartedAt, target, m.statePath(),
		m.logPath(), m.Version,
		target, m.Version, st.StartedAt, m.statePath())

	if m.Docker != nil {
		_, err := m.Docker.Run(ctx, docker.RunSpec{
			Name: ContainerName,
			// Host networking: upgrade.sh health-checks http://$SANDBOXD_API_BIND/healthz
			// (127.0.0.1 by default), which must resolve to the host, not this container.
			Network: "host",
			Env:     []string{"SANDBOXD_DATA_DIR=" + m.DataDir},
			Volumes: []string{
				"/var/run/docker.sock:/var/run/docker.sock",
				m.SrcDir + ":" + m.SrcDir,   // same path: compose resolves ./traefik etc.
				m.DataDir + ":" + m.DataDir, // state + log + backups
			},
			Labels: []string{"sandboxd.managed=true", "sandboxd.role=upgrader"},
			Image:  m.UpgraderImage,
			Cmd:    []string{"-c", script},
		})
		if err != nil {
			st.Phase, st.Message = "failed", "could not start the upgrader: "+err.Error()
			st.EndedAt = m.now().UTC().Format(time.RFC3339)
			m.write(st)
			return st, err
		}
	}
	return st, nil
}

// MarkReported flags the current terminal state as reported to telemetry.
func (m *Manager) MarkReported() error {
	st := m.read()
	if st.Reported {
		return nil
	}
	st.Reported = true
	return m.write(st)
}

func (m *Manager) read() State {
	b, err := os.ReadFile(m.statePath())
	if err != nil {
		return State{Phase: "idle"}
	}
	var st State
	if json.Unmarshal(b, &st) != nil || st.Phase == "" {
		return State{Phase: "idle"}
	}
	return st
}

func (m *Manager) write(st State) error {
	b, _ := json.Marshal(st)
	tmp := m.statePath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, m.statePath())
}

func (m *Manager) tail(n int) string {
	b, err := os.ReadFile(m.logPath())
	if err != nil {
		return ""
	}
	if len(b) > n {
		b = b[len(b)-n:]
	}
	return strings.TrimSpace(string(b))
}
