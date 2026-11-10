package upgrade

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/docker"
)

type fakeDocker struct {
	runs    []docker.RunSpec
	running bool // Inspect finds the container
	runErr  error
	removed int
}

func (f *fakeDocker) Run(ctx context.Context, spec docker.RunSpec) (string, error) {
	f.runs = append(f.runs, spec)
	if f.runErr != nil {
		return "", f.runErr
	}
	f.running = true
	return "abc123def456", nil
}
func (f *fakeDocker) Inspect(ctx context.Context, name string) (*docker.ContainerJSON, error) {
	if !f.running {
		return nil, docker.ErrNotFound
	}
	return &docker.ContainerJSON{}, nil
}
func (f *fakeDocker) Remove(ctx context.Context, name string) error { f.removed++; return nil }

func manager(t *testing.T, fd *fakeDocker) *Manager {
	t.Helper()
	return &Manager{
		Docker: fd, DataDir: t.TempDir(), SrcDir: "/opt/sandboxd",
		UpgraderImage: "sandboxd-upgrader:v0.3.10", Version: "v0.3.10",
		ReleaseExists: func(ctx context.Context, tag string) bool { return tag == "v0.3.11" },
		FreeBytes:     func(string) uint64 { return 10 << 30 },
		Now:           func() time.Time { return time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC) },
	}
}

func TestStart_LaunchesDetachedUpgraderWithSamePathMounts(t *testing.T) {
	fd := &fakeDocker{}
	m := manager(t, fd)
	st, err := m.Start(context.Background(), "v0.3.11")
	if err != nil || st.Phase != "running" || st.Target != "v0.3.11" || st.From != "v0.3.10" {
		t.Fatalf("start: %+v err=%v", st, err)
	}
	if len(fd.runs) != 1 {
		t.Fatalf("runs %d", len(fd.runs))
	}
	spec := fd.runs[0]
	if spec.Network != "host" {
		t.Fatalf("upgrader must use host networking so the health check reaches the host, got %q", spec.Network)
	}
	if spec.Name != ContainerName || spec.Image != "sandboxd-upgrader:v0.3.10" {
		t.Fatalf("spec %+v", spec)
	}
	joined := strings.Join(spec.Volumes, " ")
	for _, want := range []string{"/var/run/docker.sock:/var/run/docker.sock", "/opt/sandboxd:/opt/sandboxd", m.DataDir + ":" + m.DataDir} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing mount %q in %q", want, joined)
		}
	}
	if !strings.Contains(spec.Cmd[1], "./upgrade.sh \"v0.3.11\"") {
		t.Fatalf("must run upgrade.sh with the target: %s", spec.Cmd[1])
	}
	// state persisted
	if got := m.Status(context.Background()); got.Phase != "running" {
		t.Fatalf("status %+v", got)
	}
}

func TestStart_Guards(t *testing.T) {
	ctx := context.Background()
	m := manager(t, &fakeDocker{})
	if _, err := m.Start(ctx, "main"); !errors.Is(err, ErrBadTarget) {
		t.Fatal("branch names are not allowed")
	}
	if _, err := m.Start(ctx, "v9.9.9"); !errors.Is(err, ErrBadTarget) {
		t.Fatal("unpublished tag must be refused")
	}
	m.FreeBytes = func(string) uint64 { return 1 << 30 }
	if _, err := m.Start(ctx, "v0.3.11"); !errors.Is(err, ErrLowDisk) {
		t.Fatal("low disk must be refused")
	}
	m.FreeBytes = func(string) uint64 { return 10 << 30 }
	m.SrcDir = ""
	if _, err := m.Start(ctx, "v0.3.11"); !errors.Is(err, ErrNoSrcDir) {
		t.Fatal("no src dir must point at the CLI")
	}
}

func TestStart_RefusesConcurrent(t *testing.T) {
	fd := &fakeDocker{}
	m := manager(t, fd)
	if _, err := m.Start(context.Background(), "v0.3.11"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Start(context.Background(), "v0.3.11"); !errors.Is(err, ErrRunning) {
		t.Fatalf("second start must be ErrRunning, got %v", err)
	}
	if len(fd.runs) != 1 {
		t.Fatal("must not launch twice")
	}
}

func TestStatus_DeadUpgraderBecomesFailed(t *testing.T) {
	fd := &fakeDocker{}
	m := manager(t, fd)
	m.Start(context.Background(), "v0.3.11")
	fd.running = false // container vanished without writing a result
	st := m.Status(context.Background())
	if st.Phase != "failed" || !strings.Contains(st.Message, "disappeared") {
		t.Fatalf("status %+v", st)
	}
	// and a new upgrade may start again
	fd.running = false
	if _, err := m.Start(context.Background(), "v0.3.11"); err != nil {
		t.Fatalf("restart after failure: %v", err)
	}
}

func TestStatus_ReadsResultWrittenByWrapper(t *testing.T) {
	m := manager(t, &fakeDocker{})
	os.MkdirAll(m.stateDir(), 0o755)
	os.WriteFile(m.statePath(), []byte(`{"phase":"rolled_back","target":"v0.3.11","from":"v0.3.10","message":"build failed; rolled back"}`), 0o644)
	os.WriteFile(m.logPath(), []byte("line1\nline2\n"), 0o644)
	st := m.Status(context.Background())
	if st.Phase != "rolled_back" || st.LogTail != "line1\nline2" {
		t.Fatalf("status %+v", st)
	}
}

func TestStart_RunFailureIsRecorded(t *testing.T) {
	fd := &fakeDocker{runErr: errors.New("no such image")}
	m := manager(t, fd)
	st, err := m.Start(context.Background(), "v0.3.11")
	if err == nil || st.Phase != "failed" {
		t.Fatalf("expected failed state, got %+v err=%v", st, err)
	}
	if _, err := os.Stat(filepath.Join(m.stateDir(), stateFile)); err != nil {
		t.Fatal("state file must exist")
	}
}
