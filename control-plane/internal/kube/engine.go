package kube

// Engine — the docker.Engine implementation on top of the Kubernetes
// REST API (M8). Object mapping:
//
//      docker container  ->  Pod (name s-<ulid>, lowercased)
//      preview ports     ->  Service (ClusterIP, one port each)
//      egress control    ->  NetworkPolicy (deny-by-default + DNS + proxy)
//      preview routing   ->  Ingress (one rule per port, optional)
//
// Lifecycle mapping (docker semantics preserved for callers):
//
//      Run     = create Pod + Service + NetworkPolicy (+ Ingress), and
//                persist the RunSpec for a later Start.
//      Stop    = delete the Pod (grace = timeout) and wait for it to go.
//      Start   = recreate the Pod from the persisted RunSpec (a stopped
//                sandbox's pod is gone, unlike a stopped container).
//      Remove  = delete Pod + Service + NetworkPolicy + Ingress + spec.
//      Logs    = the pod log endpoint (plain stream; follow streams).
//      Exec    = the exec subresource over WebSocket (v4.channel.k8s.io).
//      Stats   = ErrUnsupported (no metrics backend wired in M8).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/docker"
)

// jsonDecodeBody decodes a small JSON body, draining what it reads.
func jsonDecodeBody(r io.Reader, out any) error {
	defer io.Copy(io.Discard, io.LimitReader(r, 64<<10))
	return json.NewDecoder(r).Decode(out)
}

// Engine runs sandboxes as Kubernetes objects.
type Engine struct {
	cli      *Client
	ns       string
	stateDir string // persisted RunSpecs (Start after Stop / restart)
	log      *slog.Logger

	// IngressDomain enables per-sandbox Ingress objects ("<host>=preview
	// host"); empty disables Ingress entirely. PreviewHost computes the
	// host for one port (wired from previewhost in main.go).
	IngressDomain string
	IngressClass  string
	PreviewHost   func(id string, port int) string
	// ProxySelector, when set ("k=v,k=v"), lets sandbox egress reach the
	// pods it selects (the egress proxy) in addition to cluster DNS.
	ProxySelector map[string]string
}

// New builds the engine from an (already resolved) Config.
func New(cfg Config, stateDir string, log *slog.Logger) (*Engine, error) {
	cfg, err := LoadConfig(cfg)
	if err != nil {
		return nil, err
	}
	cli, err := NewClient(cfg)
	if err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Engine{
		cli:      cli,
		ns:       cfg.Namespace,
		stateDir: stateDir,
		log:      log,
	}, nil
}

// Namespace reports where sandbox objects are created.
func (e *Engine) Namespace() string { return e.ns }

// Backend identifies the engine in logs and readyz.
func (e *Engine) Backend() string { return "kubernetes namespace " + e.ns }

// --- Run -----------------------------------------------------------------

func (e *Engine) Run(ctx context.Context, spec docker.RunSpec) (string, error) {
	if spec.Image == "" {
		return "", errors.New("kube.Run: spec.Image is required")
	}
	pod := PodFor(spec, e.ns)
	if err := e.cli.do(ctx, http.MethodPost, podPath(e.ns), pod, nil); err != nil {
		if IsNotFound(err) {
			return "", fmt.Errorf("kube.Run: namespace %s missing or forbidden: %w", e.ns, err)
		}
		return "", fmt.Errorf("kube.Run: create pod %s: %w", pod.Metadata.Name, err)
	}
	if err := e.persistSpec(pod.Metadata.Name, spec); err != nil {
		// The pod is running; a missing spec file only breaks a future
		// start/stop cycle. Log it, don't fail the create.
		e.log.Warn("kube.Run: persist spec failed", "pod", pod.Metadata.Name, "err", err.Error())
	}
	for _, obj := range e.sidecars(ctx, spec) {
		if err := e.cli.do(ctx, http.MethodPost, obj.path, obj.obj, nil); err != nil {
			if !IsNotFound(err) {
				e.log.Warn("kube.Run: sidecar object failed (pod keeps running)",
					"kind", obj.kind, "pod", pod.Metadata.Name, "err", err.Error())
			}
		}
	}
	return pod.Metadata.Name, nil
}

// sidecar is one auxiliary object created alongside the pod.
type sidecar struct {
	kind string
	path string
	obj  any
}

func (e *Engine) sidecars(ctx context.Context, spec docker.RunSpec) []sidecar {
	name := dns1123(spec.Name)
	out := []sidecar{}
	if svc := ServiceFor(spec, e.ns); len(svc.Spec.Ports) > 0 {
		out = append(out, sidecar{"Service", servicePath(e.ns), svc})
	}
	out = append(out, sidecar{"NetworkPolicy", netpolPath(e.ns), NetworkPolicyFor(spec, e.ns, e.ProxySelector)})
	if e.IngressDomain != "" && e.PreviewHost != nil {
		id := sandboxIDOf(name)
		hostFor := func(port int) string {
			h := e.PreviewHost(id, port)
			if h == "" {
				return ""
			}
			return strings.ToLower(strings.SplitN(h, ":", 2)[0])
		}
		if ig := IngressFor(spec, e.ns, e.IngressClass, hostFor); ig != nil {
			out = append(out, sidecar{"Ingress", ingressPath(e.ns), ig})
		}
	}
	return out
}

// sandboxIDOf recovers the sandbox id (as the API layer knows it: the
// original case) from a pod name. Pod names are lowercased, so this is
// exact only for the lowercased form — which is what every caller of
// the engine uses as the container name anyway.
func sandboxIDOf(podName string) string {
	return strings.TrimPrefix(podName, "s-")
}

// --- Inspect -------------------------------------------------------------

func (e *Engine) Inspect(ctx context.Context, name string) (*docker.ContainerJSON, error) {
	var p Pod
	if err := e.cli.do(ctx, http.MethodGet, podPath(e.ns)+"/"+dns1123(name), nil, &p); err != nil {
		if IsNotFound(err) {
			return nil, docker.ErrNotFound
		}
		return nil, fmt.Errorf("kube.Inspect %s: %w", name, err)
	}
	cj := &docker.ContainerJSON{}
	cj.ID = p.Metadata.Name
	switch p.Status.Phase {
	case "Running":
		cj.State.Status = "running"
		cj.State.Running = true
	case "Succeeded", "Failed":
		cj.State.Status = "exited"
	case "Pending":
		cj.State.Status = "created"
	default:
		cj.State.Status = "created"
	}
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Name != "sandbox" {
			continue
		}
		switch {
		case cs.State.Running != nil:
			cj.State.Status = "running"
			cj.State.Running = true
		case cs.State.Terminated != nil:
			cj.State.Status = "exited"
		}
	}
	cj.NetworkSettings.IPAddress = p.Status.PodIP
	if p.Status.PodIP != "" {
		cj.NetworkSettings.Networks = map[string]*docker.NetworkEndpointInfo{
			"pod": {IPAddress: p.Status.PodIP},
		}
	}
	if len(p.Spec.Containers) > 0 {
		c := p.Spec.Containers[0]
		cj.Config.Image = c.Image
		cj.Config.Cmd = c.Command
		cj.Config.Labels = p.Metadata.Labels
		uid, gid := int64(0), int64(0)
		if p.Spec.SecurityContext != nil && p.Spec.SecurityContext.RunAsUser != nil {
			uid = *p.Spec.SecurityContext.RunAsUser
		}
		if p.Spec.SecurityContext != nil && p.Spec.SecurityContext.RunAsGroup != nil {
			gid = *p.Spec.SecurityContext.RunAsGroup
		}
		cj.Config.User = fmt.Sprintf("%d:%d", uid, gid)
		// Mounts mirror the pod's volumes so GET .../volumes keeps working.
		for _, v := range p.Spec.Volumes {
			for _, m := range c.VolumeMounts {
				if m.Name != v.Name {
					continue
				}
				mnt := docker.Mount{Destination: m.MountPath, RW: !m.ReadOnly}
				if v.HostPath != nil {
					mnt.Type = "bind"
					mnt.Source = v.HostPath.Path
				} else {
					mnt.Type = "volume"
					mnt.Source = v.Name
				}
				cj.Mounts = append(cj.Mounts, mnt)
			}
		}
	}
	return cj, nil
}

// --- Remove ----------------------------------------------------------------

func (e *Engine) Remove(ctx context.Context, name string) error {
	pod := dns1123(name)
	err := e.cli.do(ctx, http.MethodDelete, podPath(e.ns)+"/"+pod, nil, nil)
	if err != nil && !IsNotFound(err) {
		return fmt.Errorf("kube.Remove %s: %w", name, err)
	}
	for _, p := range []struct{ path, kind string }{
		{servicePath(e.ns) + "/" + pod, "Service"},
		{netpolPath(e.ns) + "/" + pod, "NetworkPolicy"},
		{ingressPath(e.ns) + "/" + pod, "Ingress"},
	} {
		if err := e.cli.do(ctx, http.MethodDelete, p.path, nil, nil); err != nil && !IsNotFound(err) {
			e.log.Warn("kube.Remove: sidecar delete failed", "kind", p.kind, "pod", pod, "err", err.Error())
		}
	}
	_ = os.Remove(e.specPath(pod))
	return nil
}

// --- Start / Stop -----------------------------------------------------------

// Start revives a stopped sandbox: on Kubernetes a "stopped" pod is a
// deleted pod, so Start recreates it from the RunSpec persisted at Run
// time. Idempotent when the pod is already running.
func (e *Engine) Start(ctx context.Context, name string) error {
	pod := dns1123(name)
	if _, err := e.Inspect(ctx, name); err == nil {
		return nil // already running
	} else if !errors.Is(err, docker.ErrNotFound) {
		return err
	}
	spec, ok, err := e.loadSpec(pod)
	if err != nil {
		return err
	}
	if !ok {
		return docker.ErrNotFound
	}
	// A terminated pod with restartPolicy=Never cannot be restarted;
	// delete the husk first so the create below succeeds.
	if err := e.cli.do(ctx, http.MethodDelete, podPath(e.ns)+"/"+pod, nil, nil); err != nil && !IsNotFound(err) {
		return fmt.Errorf("kube.Start %s: clear terminated pod: %w", name, err)
	}
	_, err = e.Run(ctx, spec)
	return err
}

// Stop deletes the pod (grace = timeoutSec) and waits for it to leave,
// matching docker stop's blocking semantics for the reapers.
func (e *Engine) Stop(ctx context.Context, name string, timeoutSec int) error {
	pod := dns1123(name)
	if timeoutSec < 0 {
		timeoutSec = 0
	}
	body := map[string]any{"apiVersion": "v1", "kind": "DeleteOptions", "gracePeriodSeconds": int64(timeoutSec)}
	err := e.cli.do(ctx, http.MethodDelete, podPath(e.ns)+"/"+pod, body, nil)
	if err != nil {
		if IsNotFound(err) {
			return docker.ErrNotFound
		}
		return fmt.Errorf("kube.Stop %s: %w", name, err)
	}
	deadline := time.Now().Add(time.Duration(timeoutSec)*time.Second + 10*time.Second)
	for {
		if _, err := e.Inspect(ctx, name); errors.Is(err, docker.ErrNotFound) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("kube.Stop %s: pod still present after %ds", name, timeoutSec)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// --- Exec --------------------------------------------------------------------

func (e *Engine) Exec(ctx context.Context, name string, cmd []string) (docker.ExecResult, error) {
	if len(cmd) == 0 {
		return docker.ExecResult{}, errors.New("kube.Exec: empty cmd")
	}
	pod := dns1123(name)
	q := url.Values{}
	q.Set("container", "sandbox")
	for _, c := range cmd {
		q.Add("command", c)
	}
	q.Set("stdout", "true")
	q.Set("stderr", "true")
	ws, err := e.cli.wsDial(ctx, execPath(e.ns, pod)+"?"+q.Encode())
	if err != nil {
		if IsNotFound(err) {
			return docker.ExecResult{}, docker.ErrNotFound
		}
		return docker.ExecResult{}, fmt.Errorf("kube.Exec %s: %w", name, err)
	}
	defer ws.close()

	res := docker.ExecResult{ExitCode: -1}
	type errStatus struct {
		Status  string `json:"status"`
		Message string `json:"message"`
		Reason  string `json:"reason"`
		Details struct {
			Causes []struct {
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"causes"`
		} `json:"details"`
	}
	var status errStatus
	for {
		f, err := ws.readFrame()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			return res, fmt.Errorf("kube.Exec %s: %w", name, err)
		}
		switch f.Channel {
		case v4Channels.Stdout:
			res.Stdout += string(f.Payload)
		case v4Channels.Stderr:
			res.Stderr += string(f.Payload)
		case v4Channels.Error:
			status = errStatus{}
			_ = json.Unmarshal(f.Payload, &status)
		}
	}
	if status.Status == "" || status.Status == "Success" {
		res.ExitCode = 0
		return res, nil
	}
	res.ExitCode = 1
	for _, c := range status.Details.Causes {
		if c.Reason == "ExitCode" {
			if n, err := strconv.Atoi(strings.TrimSpace(c.Message)); err == nil {
				res.ExitCode = n
			}
		}
	}
	return res, nil
}

// --- Logs ----------------------------------------------------------------------

func (e *Engine) Logs(ctx context.Context, name string, tail int, follow bool) (io.ReadCloser, error) {
	pod := dns1123(name)
	q := url.Values{}
	q.Set("container", "sandbox")
	if tail > 0 {
		q.Set("tailLines", strconv.Itoa(tail))
	}
	if follow {
		q.Set("follow", "true")
	}
	rc, err := e.cli.stream(ctx, podPath(e.ns)+"/"+pod+"/log?"+q.Encode())
	if err != nil {
		if IsNotFound(err) {
			return nil, docker.ErrNotFound
		}
		return nil, fmt.Errorf("kube.Logs %s: %w", name, err)
	}
	return rc, nil
}

// --- list / stats / identity ------------------------------------------------------

func (e *Engine) ListByNamePrefix(ctx context.Context, prefix string) ([]string, error) {
	var list PodList
	path := podPath(e.ns) + "?labelSelector=" + labelManaged + "%3Dtrue"
	if err := e.cli.do(ctx, http.MethodGet, path, nil, &list); err != nil {
		return nil, fmt.Errorf("kube.ListByNamePrefix: %w", err)
	}
	prefix = strings.ToLower(prefix)
	var names []string
	for _, p := range list.Items {
		if strings.HasPrefix(p.Metadata.Name, prefix) {
			names = append(names, p.Metadata.Name)
		}
	}
	sort.Strings(names) // deterministic for the reconciler and tests
	return names, nil
}

func (e *Engine) Stats(ctx context.Context, name string) (docker.Stats, error) {
	return docker.Stats{}, docker.ErrUnsupported
}

func (e *Engine) Info(ctx context.Context) (string, error) {
	v, err := e.version(ctx)
	if err != nil {
		return "", err
	}
	return "kubernetes " + v.GitVersion, nil
}

func (e *Engine) ServerVersion(ctx context.Context) (string, error) {
	v, err := e.version(ctx)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(v.GitVersion, "v"), nil
}

func (e *Engine) version(ctx context.Context) (*Version, error) {
	var v Version
	if err := e.cli.do(ctx, http.MethodGet, "/version", nil, &v); err != nil {
		return nil, fmt.Errorf("kube.version: %w", err)
	}
	return &v, nil
}

// --- persisted RunSpecs (Start after Stop / daemon restart) ----------------

func (e *Engine) specPath(pod string) string {
	return filepath.Join(e.stateDir, pod+".json")
}

func (e *Engine) persistSpec(pod string, spec docker.RunSpec) error {
	if e.stateDir == "" {
		return nil
	}
	if err := os.MkdirAll(e.stateDir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(spec)
	if err != nil {
		return err
	}
	return os.WriteFile(e.specPath(pod), b, 0o600)
}

func (e *Engine) loadSpec(pod string) (docker.RunSpec, bool, error) {
	var spec docker.RunSpec
	if e.stateDir == "" {
		return spec, false, nil
	}
	b, err := os.ReadFile(e.specPath(pod))
	if errors.Is(err, os.ErrNotExist) {
		return spec, false, nil
	}
	if err != nil {
		return spec, false, fmt.Errorf("kube.loadSpec %s: %w", pod, err)
	}
	if err := json.Unmarshal(b, &spec); err != nil {
		return spec, false, fmt.Errorf("kube.loadSpec %s: %w", pod, err)
	}
	return spec, true, nil
}

// --- API paths ---------------------------------------------------------------

func podPath(ns string) string     { return "/api/v1/namespaces/" + ns + "/pods" }
func servicePath(ns string) string { return "/api/v1/namespaces/" + ns + "/services" }
func netpolPath(ns string) string {
	return "/apis/networking.k8s.io/v1/namespaces/" + ns + "/networkpolicies"
}
func ingressPath(ns string) string {
	return "/apis/networking.k8s.io/v1/namespaces/" + ns + "/ingresses"
}
func execPath(ns, pod string) string { return podPath(ns) + "/" + pod + "/exec" }
