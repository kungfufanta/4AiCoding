package docker

import (
	"context"
	"io"
)

// Engine is the container-executor seam (PLAN M8, k3s B2). The sandbox
// lifecycle (create / inspect / stop / start / remove / exec / logs /
// list / stats) is expressed once, in terms of the docker.RunSpec shape,
// and may be served by two backends:
//
//   - *Client: the `docker` CLI (the OSS all-in-one and the M7b executor
//     on a Docker host — sandboxes are containers on sandboxd_net).
//   - internal/kube.Engine: the Kubernetes REST API (M8 executor in a
//     k3s cluster — a sandbox is a Pod, with a Service, a NetworkPolicy
//     and (optionally) an Ingress built around it).
//
// The interface is intentionally the union of what the control paths
// actually call — no speculative surface. Backends must keep the
// docker-flavoured semantics: ErrNotFound for missing objects,
// idempotent Remove, Stop honouring the grace period, Start on a
// stopped object reviving it, Logs streaming until ctx is done.
type Engine interface {
	// Run creates the object for spec and returns its short id (the
	// docker CLI's 12-char id; the pod name on the k8s engine).
	Run(ctx context.Context, spec RunSpec) (string, error)
	// Inspect returns the parsed state of the named object, or ErrNotFound.
	Inspect(ctx context.Context, name string) (*ContainerJSON, error)
	// Remove force-removes the named object. Idempotent: removing an
	// already-gone object is nil.
	Remove(ctx context.Context, name string) error
	// Start revives a stopped object. On the k8s engine the stopped object
	// was deleted, so Start recreates it from the spec persisted at Run
	// time. Idempotent when the object is already running.
	Start(ctx context.Context, name string) error
	// Stop halts the named object, waiting at most timeoutSec before the
	// kernel (or kubelet) kills it. ErrNotFound when the object is gone.
	Stop(ctx context.Context, name string, timeoutSec int) error
	// Exec runs a non-interactive command inside the named object.
	Exec(ctx context.Context, name string, cmd []string) (ExecResult, error)
	// Logs streams the object's merged stdout+stderr. tail <= 0 = all
	// lines; follow keeps the reader open until ctx is done. The caller
	// MUST Close the returned reader.
	Logs(ctx context.Context, name string, tail int, follow bool) (io.ReadCloser, error)
	// ListByNamePrefix returns the names of all objects (any state) whose
	// name starts with prefix.
	ListByNamePrefix(ctx context.Context, prefix string) ([]string, error)
	// Stats takes one resource sample of a running object. The k8s engine
	// returns ErrUnsupported until a metrics backend is wired.
	Stats(ctx context.Context, name string) (Stats, error)
	// Info returns a one-line backend identity for the readyz probe.
	Info(ctx context.Context) (string, error)
	// ServerVersion returns the backend version string.
	ServerVersion(ctx context.Context) (string, error)
}

// ErrUnsupported marks a capability the active engine does not provide
// (today: Stats on the kubernetes engine). Callers translate it into a
// clean API answer instead of a 500.
var ErrUnsupported = errUnsupported{}

type errUnsupported struct{}

func (errUnsupported) Error() string {
	return "unsupported by this engine"
}

// The docker CLI client is the reference implementation.
var _ Engine = (*Client)(nil)
