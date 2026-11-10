package kube

// Engine tests against the fake API server: the hardening the pod spec
// carries, the object mapping (Pod/Service/NetworkPolicy/Ingress), the
// docker-shaped lifecycle (Start recreates from the persisted spec,
// Stop waits for the pod to leave) and the exec/logs transports.

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/docker"
)

// volumeOf finds a pod volume by name.
func volumeOf(p *Pod, name string) *Volume {
	for i := range p.Spec.Volumes {
		if p.Spec.Volumes[i].Name == name {
			return &p.Spec.Volumes[i]
		}
	}
	return nil
}

func sampleSpec() docker.RunSpec {
	return docker.RunSpec{
		Name:     "s-01JTEST",
		Hostname: "s-01JTEST",
		ReadOnly: true,
		CapDrop:  []string{"ALL"},
		CPUs:     "0.5",
		Memory:   "512m",
		Env:      []string{"FOO=bar"},
		Volumes:  []string{"/var/lib/sandboxd/workspaces/x:/home/sandbox"},
		Tmpfs:    []string{"/tmp:size=512m", "/var/tmp:size=128m"},
		Labels:   []string{"traefik.enable=true"},
		User:     "1000:1000",
		Image:    "sandboxd-e2e:ci",
		Cmd:      []string{"sleep", "infinity"},
		Ports:    []int{3000, 8080},
	}
}

func TestPodForHardening(t *testing.T) {
	p := PodFor(sampleSpec(), "test")
	if p.Metadata.Name != "s-01jtest" {
		t.Fatalf("pod name %q, want lowercase s-01jtest", p.Metadata.Name)
	}
	if p.Spec.RestartPolicy != "Never" {
		t.Errorf("restartPolicy = %q", p.Spec.RestartPolicy)
	}
	if p.Spec.AutomountServiceAccountToken == nil || *p.Spec.AutomountServiceAccountToken {
		t.Errorf("service account token must NOT be mounted into a sandbox")
	}
	if p.Spec.SecurityContext.RunAsNonRoot == nil || !*p.Spec.SecurityContext.RunAsNonRoot {
		t.Errorf("runAsNonRoot not set")
	}
	sc := p.Spec.Containers[0].SecurityContext
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Errorf("root filesystem must be read-only")
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Errorf("privilege escalation must be off")
	}
	if len(sc.Capabilities.Drop) == 0 || sc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("capabilities must be dropped: %v", sc.Capabilities.Drop)
	}
	if len(sc.Capabilities.Add) != 0 {
		t.Errorf("no capability may be added back: %v", sc.Capabilities.Add)
	}
	if p.Spec.SecurityContext.RunAsUser == nil || *p.Spec.SecurityContext.RunAsUser != 1000 {
		t.Errorf("runAsUser = %v, want 1000", p.Spec.SecurityContext.RunAsUser)
	}
	// Workspace bind mount -> hostPath; tmpfs -> emptyDir.
	var wsMount, tmpMount *VolumeMount
	for i, m := range p.Spec.Containers[0].VolumeMounts {
		if m.MountPath == "/home/sandbox" {
			wsMount = &p.Spec.Containers[0].VolumeMounts[i]
		}
		if m.MountPath == "/tmp" {
			tmpMount = &p.Spec.Containers[0].VolumeMounts[i]
		}
	}
	if wsMount == nil {
		t.Fatalf("workspace not mounted at /home/sandbox: %+v", p.Spec.Containers[0].VolumeMounts)
	}
	if v := volumeOf(p, wsMount.Name); v == nil || v.HostPath == nil ||
		v.HostPath.Path != "/var/lib/sandboxd/workspaces/x" {
		t.Errorf("workspace must be a hostPath bind, got %+v", volumeOf(p, wsMount.Name))
	}
	if tmpMount == nil || volumeOf(p, tmpMount.Name) == nil || volumeOf(p, tmpMount.Name).EmptyDir == nil {
		t.Errorf("tmpfs must be an emptyDir")
	}
	// Resources.
	lim := p.Spec.Containers[0].Resources.Limits
	if lim["cpu"] != "500m" || lim["memory"] != "512Mi" {
		t.Errorf("limits = %v, want cpu=500m memory=512Mi", lim)
	}
	// Env + ports + no privileged.
	if len(p.Spec.Containers[0].Env) != 1 || p.Spec.Containers[0].Env[0].Value != "bar" {
		t.Errorf("env = %+v", p.Spec.Containers[0].Env)
	}
	if len(p.Spec.Containers[0].Ports) != 2 {
		t.Errorf("ports = %+v", p.Spec.Containers[0].Ports)
	}
	if p.Spec.Containers[0].SecurityContext.Privileged == nil || *p.Spec.Containers[0].SecurityContext.Privileged {
		t.Errorf("privileged must be false")
	}
}

func TestRunCreatesPodServiceNetpolIngress(t *testing.T) {
	f := newFakeAPI()
	eng, _ := newTestEngine(t, f, func(e *Engine) {
		e.IngressDomain = "preview.example"
		e.PreviewHost = func(id string, port int) string {
			return "s-" + id + "-" + strconv.Itoa(port) + ".preview.example"
		}
	})
	_, err := eng.Run(context.Background(), sampleSpec())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.counts("pods/s-01jtest") != 1 {
		t.Errorf("pod not created (created=%v)", f.created)
	}
	if f.counts("services/s-01jtest") != 1 {
		t.Errorf("service not created (created=%v)", f.created)
	}
	if f.counts("networkpolicies/s-01jtest") != 1 {
		t.Errorf("networkpolicy not created (created=%v)", f.created)
	}
	if f.counts("ingresses/s-01jtest") != 1 {
		t.Errorf("ingress not created (created=%v)", f.created)
	}
}

func TestRunWithoutIngressDomain(t *testing.T) {
	f := newFakeAPI()
	eng, _ := newTestEngine(t, f, nil)
	if _, err := eng.Run(context.Background(), sampleSpec()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.counts("ingresses/s-01jtest") != 0 {
		t.Errorf("no Ingress without a preview domain")
	}
	if f.counts("services/s-01jtest") != 1 || f.counts("networkpolicies/s-01jtest") != 1 {
		t.Errorf("service+netpol are unconditional (created=%v)", f.created)
	}
}

func TestInspectMapping(t *testing.T) {
	f := newFakeAPI()
	eng, _ := newTestEngine(t, f, nil)
	ctx := context.Background()
	if _, err := eng.Run(ctx, sampleSpec()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Pending -> created, no IP.
	cj, err := eng.Inspect(ctx, "s-01jtest")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if cj.State.Status != "created" || cj.State.Running {
		t.Errorf("pending pod = %+v", cj.State)
	}
	// Running -> running + pod IP in the docker-shaped fields.
	f.setPodPhase("s-01jtest", "Running", "10.42.0.7", true)
	cj, err = eng.Inspect(ctx, "s-01jtest")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if cj.State.Status != "running" || !cj.State.Running {
		t.Errorf("running pod = %+v", cj.State)
	}
	if cj.BridgeIP() != "10.42.0.7" {
		t.Errorf("BridgeIP = %q, want the pod IP", cj.BridgeIP())
	}
	if cj.Config.Image != "sandboxd-e2e:ci" {
		t.Errorf("image = %q", cj.Config.Image)
	}
	// Gone -> ErrNotFound.
	if err := eng.Remove(ctx, "s-01jtest"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := eng.Inspect(ctx, "s-01jtest"); !errors.Is(err, docker.ErrNotFound) {
		t.Errorf("inspect gone = %v, want ErrNotFound", err)
	}
	if err := eng.Remove(ctx, "s-01jtest"); err != nil {
		t.Errorf("Remove must be idempotent, got %v", err)
	}
}

func TestFailedPodMapsToExited(t *testing.T) {
	f := newFakeAPI()
	eng, _ := newTestEngine(t, f, nil)
	ctx := context.Background()
	if _, err := eng.Run(ctx, sampleSpec()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	f.mu.Lock()
	p := f.pods["s-01jtest"]
	p.Status.Phase = "Failed"
	p.Status.ContainerStatuses = []ContainerStatus{{Name: "sandbox",
		State: ContainerState{Terminated: &struct {
			ExitCode int    `json:"exitCode,omitempty"`
			Reason   string `json:"reason,omitempty"`
		}{ExitCode: 1, Reason: "Error"}}}}
	f.pods["s-01jtest"] = p
	f.mu.Unlock()
	cj, err := eng.Inspect(ctx, "s-01jtest")
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if cj.State.Status != "exited" || cj.State.Running {
		t.Errorf("failed pod = %+v, want exited", cj.State)
	}
}

func TestStopWaitsForPod(t *testing.T) {
	f := newFakeAPI()
	eng, _ := newTestEngine(t, f, nil)
	ctx := context.Background()
	if _, err := eng.Run(ctx, sampleSpec()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The "kubelet" honours the grace period: the pod lingers 300ms.
	f.setPodPhase("s-01jtest", "Running", "10.42.0.7", true)
	f.mu.Lock()
	f.deleteDelay = 300 * time.Millisecond
	f.mu.Unlock()
	start := time.Now()
	if err := eng.Stop(ctx, "s-01jtest", 5); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if time.Since(start) < 250*time.Millisecond {
		t.Errorf("Stop returned before the pod was gone")
	}
	if err := eng.Stop(ctx, "s-01jtest", 5); !errors.Is(err, docker.ErrNotFound) {
		t.Errorf("Stop on a gone pod = %v, want ErrNotFound", err)
	}
}

func TestStartRecreatesFromPersistedSpec(t *testing.T) {
	f := newFakeAPI()
	eng, _ := newTestEngine(t, f, nil)
	ctx := context.Background()
	name, err := eng.Run(ctx, sampleSpec())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// docker start on a running container is a no-op.
	if err := eng.Start(ctx, name); err != nil {
		t.Fatalf("Start on running: %v", err)
	}
	if err := eng.Stop(ctx, name, 5); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := eng.Start(ctx, name); err != nil {
		t.Fatalf("Start after stop: %v", err)
	}
	if f.counts("pods/s-01jtest") != 2 {
		t.Errorf("pod creates = %d, want 2 (the recreate)", f.counts("pods/s-01jtest"))
	}
	// The recreated pod carries the same hardening (it is the same spec).
	cj, err := eng.Inspect(ctx, name)
	if err != nil {
		t.Fatalf("Inspect after restart: %v", err)
	}
	if cj.Config.Image != "sandboxd-e2e:ci" {
		t.Errorf("recreated image = %q", cj.Config.Image)
	}
	// A daemon restart keeps the spec files: stop again, then a fresh
	// engine (same state dir) can Start the sandbox.
	if err := eng.Stop(ctx, name, 5); err != nil {
		t.Fatalf("Stop again: %v", err)
	}
	eng2, _ := newTestEngine(t, f, func(e *Engine) { e.stateDir = eng.stateDir })
	if err := eng2.Start(ctx, name); err != nil {
		t.Fatalf("Start from a fresh engine: %v", err)
	}
	if f.counts("pods/s-01jtest") != 3 {
		t.Errorf("pod creates = %d, want 3", f.counts("pods/s-01jtest"))
	}
}

func TestListByNamePrefix(t *testing.T) {
	f := newFakeAPI()
	eng, _ := newTestEngine(t, f, nil)
	ctx := context.Background()
	if _, err := eng.Run(ctx, sampleSpec()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The fake store is keyed by pod name — clone the row under a second
	// name to prove the list returns both.
	f.mu.Lock()
	p2 := f.pods["s-01jtest"]
	p2.Metadata.Name = "s-01jtest2"
	f.pods["s-01jtest2"] = p2
	f.mu.Unlock()
	names, err := eng.ListByNamePrefix(ctx, "s-")
	if err != nil {
		t.Fatalf("ListByNamePrefix: %v", err)
	}
	if len(names) != 2 || names[0] != "s-01jtest" || names[1] != "s-01jtest2" {
		t.Errorf("names = %v", names)
	}
}

func TestLogsStreamsBody(t *testing.T) {
	f := newFakeAPI()
	f.logBody = "line1\nline2\n"
	eng, _ := newTestEngine(t, f, nil)
	ctx := context.Background()
	if _, err := eng.Run(ctx, sampleSpec()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	rc, err := eng.Logs(ctx, "s-01jtest", 10, false)
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read logs: %v", err)
	}
	if string(b) != "line1\nline2\n" {
		t.Errorf("logs = %q", b)
	}
}

func TestStatsUnsupported(t *testing.T) {
	eng, _ := newTestEngine(t, newFakeAPI(), nil)
	if _, err := eng.Stats(context.Background(), "s-x"); !errors.Is(err, docker.ErrUnsupported) {
		t.Errorf("Stats = %v, want ErrUnsupported", err)
	}
}

func TestVersionEndpoints(t *testing.T) {
	eng, _ := newTestEngine(t, newFakeAPI(), nil)
	v, err := eng.ServerVersion(context.Background())
	if err != nil {
		t.Fatalf("ServerVersion: %v", err)
	}
	if v != "1.31.4+k3s1" {
		t.Errorf("ServerVersion = %q", v)
	}
	info, err := eng.Info(context.Background())
	if err != nil || !strings.Contains(info, "kubernetes") {
		t.Errorf("Info = %q (%v)", info, err)
	}
}
