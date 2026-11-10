// Package api wires the HTTP handlers. Auth is enforced by the
// internal/auth middleware, applied around this mux in main.go.
package api

import (
	"bufio"
	"errors"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/domain"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/activity"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/audit"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/auth"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/docker"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/egress"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/events"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/idlock"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/instancecfg"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/loopback"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/metrics"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/netlog"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/sandboxspec"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/secrets"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/snapshot"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/wake"
)

// Server bundles the collaborators the handlers need.
type Server struct {
	// SpecEnv is the instance-level container environment shared by the
	// create path and the wake recreate path (see sandboxspec.Build). When
	// its Image is empty (unit tests) it is derived from the fields below.
	SpecEnv sandboxspec.Env
	// Limits bounds what a create request may ask for (images, resources,
	// ports). It seeds the admin policy when none is stored yet.
	Limits Limits
	// APIKeyMaxTTL caps (and, when > 0, imposes) API key lifetimes.
	APIKeyMaxTTL time.Duration
	// policyCache caches the admin policy (see spec.go).
	policyCache policyCache
	// EgressProxyURL is the forward proxy sandboxes must use for outbound
	// traffic (http://host:port as seen from a sandbox). Empty = egress
	// enforcement not wired (sandboxes keep the network's default reach).
	EgressProxyURL string
	// NetLog is the live record of egress-proxy requests (network
	// activity view). nil = not recorded.
	NetLog *netlog.Recorder
	// Domain manager (v1_domain.go): DomainTLSMode is the running TLS mode
	// (custom / letsencrypt) when PreviewTLS is on; Certs stores custom
	// certificates for Traefik; DomainResolver is for verify (nil = system).
	DomainTLSMode  string
	Certs          *domain.CertStore
	DomainResolver domain.Resolver

	Store         *store.Store
	Docker        *docker.Client
	Loopback      *loopback.Manager
	Log           *slog.Logger
	PreviewDomain string
	Image         string

	// OSS docker-native knobs (see cmd/sandboxd/main.go for env wiring):
	//   Network           — shared docker network sandboxes join so Traefik
	//                        can route to them (e.g. sandboxd_net).
	//   PreviewEntrypoint — Traefik entrypoint on preview routers ("web"
	//                        plain HTTP by default, "websecure" for TLS).
	//   PreviewTLS        — emit tls=true on preview routers (needs a cert in
	//                        Traefik's default store; off by default).
	//   SetMemoryHigh     — write cgroup v2 memory.high after start. Needs
	//                        host cgroup access; off by default in the
	//                        portable build (the --memory ceiling still applies).
	Network           string
	Userns            string
	Runtime           string
	DNSResolvConf     string
	PreviewEntrypoint string
	PreviewTLS        bool
	// PreviewURLScheme forces the scheme of generated preview URLs ("http" or
	// "https") independently of PreviewTLS — for sandboxd behind something
	// else that terminates TLS (Cloudflare Tunnel, Caddy, a reverse tunnel):
	// Traefik keeps serving plain HTTP, but the console must hand out https://
	// URLs or it cannot iframe previews (mixed content). Empty = derive from
	// PreviewTLS, exactly the previous behaviour.
	PreviewURLScheme string
	// PublicHTTPPort is the HOST-facing port that preview/console URLs are
	// reached on (the host side of Traefik's published port). previewURL
	// appends it unless it's the default for the scheme (80 for http, 443
	// for https), so on a shared host with HTTP_PORT=18080 the API returns
	// a reachable ":18080" URL instead of a bare :80 one. Empty = default.
	PublicHTTPPort string
	SetMemoryHigh  bool
	// PreviewHostStyle / PreviewHostTag lay out preview, console and API
	// hostnames under PreviewDomain (PREVIEW_HOST_STYLE / PREVIEW_HOST_TAG,
	// see package previewhost). Empty style = nested, today's layout.
	PreviewHostStyle string
	PreviewHostTag   string

	// TemplatesDir is the host directory holding prebuilt golden
	// template .img files for the fast-cold-start path. Empty
	// disables the optional `template` field on POST /sandbox.
	TemplatesDir string

	// LibraryRoot is the host directory holding user-created snapshot
	// images —
	// /var/lib/sandboxd/library/<snapshot_id>.img. Independent of
	// _snapshots/ (the Phase 7 auto-snapshot tree) so the retention
	// pruner and per-sandbox purge never touch templates. Empty disables
	// the /v1/snapshots endpoints (503).
	LibraryRoot string

	// inner is the router behind the auth middleware.
	inner http.Handler

	// LLMTxtPath is the host file served at the public, tokenless
	// GET /llm.txt (the API contract for integrators). Empty → 404.
	LLMTxtPath string

	// Secrets encrypts sensitive app_config values at rest (Slice 1 of
	// app config/secrets). nil-safe: sensitive config writes return 503
	// when unset, but main.go always configures it.
	Secrets *secrets.Cipher

	// Phase 5 additions — nil-safe so existing tests that build a
	// Server without these still work.
	Inflight     *activity.InflightExec
	Wake         *wake.Handler
	Admit        wake.AdmitConfig
	KeepaliveMax time.Duration

	// Phase 6 — egress policy hook. nil-safe.
	Egress *egress.Manager

	// Phase 7 — snapshot/restore subsystem + the shared per-id lock.
	// nil-safe: the snapshot endpoints return 503 if Snapshot is nil.
	Snapshot *snapshot.Manager
	Locks    *idlock.Registry

	// Phase 8 — service-token auth, audit log, preview-token gate.
	// All nil-safe: a Server built without these behaves like the
	// Phase 7 server (the auth middleware is applied separately in
	// main.go around this mux).
	Auth                *auth.Middleware
	Audit               *audit.Logger
	Events              *events.Recorder // Phase 5 — durable app_events timeline
	SnapshotsRoot       string           // per-sandbox purge of _snapshots/<id>/
	ForwardAuthDenyMode string           // "redirect" (default) | "meta-refresh"

	// Phase 8A — static, safe instance metadata for GET /v1/settings.
	// Populated in main; contains no secrets.
	Instance InstanceInfo

	// Phase 8B — live, runtime-editable lifecycle tunables (idle/keepalive),
	// shared with the reaper. nil-safe: PATCH /v1/settings returns 503 when
	// unset, and the static KeepaliveMax is used as a fallback.
	Live *instancecfg.Live

	// M6 — fleet. NodeLease is how long after a worker's last heartbeat
	// it is still reported online (fleet.DefaultLease when 0).
	NodeLease time.Duration
}

// Handler returns the http.Handler ready for ListenAndServe.
// Wraps every route in the metric-recording middleware.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /sandbox", s.observe("POST /sandbox", s.handleCreate))
	mux.HandleFunc("GET /sandboxes", s.observe("GET /sandboxes", s.handleList))
	mux.HandleFunc("GET /sandbox/{id}", s.observe("GET /sandbox/{id}", s.handleGet))
	mux.HandleFunc("DELETE /sandbox/{id}", s.observe("DELETE /sandbox/{id}", s.handleDelete))
	mux.HandleFunc("POST /sandbox/{id}/exec", s.observe("POST /sandbox/{id}/exec", s.handleExec))
	mux.HandleFunc("POST /sandbox/{id}/keepalive", s.observe("POST /sandbox/{id}/keepalive", s.handleKeepalive))
	mux.HandleFunc("POST /wake/{id}", s.observe("POST /wake/{id}", s.handleWakeJSON))
	mux.HandleFunc("POST /sandbox/{id}/snapshots", s.observe("POST /sandbox/{id}/snapshots", s.handleSnapshotTake))
	mux.HandleFunc("GET /sandbox/{id}/snapshots", s.observe("GET /sandbox/{id}/snapshots", s.handleSnapshotList))
	mux.HandleFunc("POST /sandbox/{id}/restore", s.observe("POST /sandbox/{id}/restore", s.handleSnapshotRestore))
	mux.HandleFunc("GET /llm.txt", s.observe("GET /llm.txt", s.handleLLMTxt))
	mux.HandleFunc("GET /healthz", s.observe("GET /healthz", s.handleHealthz))
	mux.HandleFunc("GET /version", s.observe("GET /version", s.handleVersion))
	mux.HandleFunc("GET /readyz", s.observe("GET /readyz", s.handleReadyz))
	mux.Handle("GET /metrics", promhttp.HandlerFor(metrics.Registry, promhttp.HandlerOpts{}))

	// Phase 8 — external identity, purge, and preview-auth endpoints.
	mux.HandleFunc("POST /sandbox/{id}/claim", s.observe("POST /sandbox/{id}/claim", s.handleClaim))
	mux.HandleFunc("POST /sandbox/{id}/purge", s.observe("POST /sandbox/{id}/purge", s.handlePurgeSandbox))
	mux.HandleFunc("POST /external-users/{external_user_id}/purge", s.observe("POST /external-users/{id}/purge", s.handlePurgeExternalUser))
	mux.HandleFunc("POST /external-projects/{external_project_id}/purge", s.observe("POST /external-projects/{id}/purge", s.handlePurgeExternalProject))
	mux.HandleFunc("GET /preview-auth", s.observe("GET /preview-auth", s.handlePreviewAuth))
	mux.HandleFunc("GET /forward-auth", s.observe("GET /forward-auth", s.handleForwardAuth))

	// Public v1 sandbox lifecycle API — a translation layer over the
	// internal handlers above.
	mux.HandleFunc("GET /v1/sandboxes", s.observe("GET /v1/sandboxes", s.v1ListSandboxes))
	mux.HandleFunc("POST /v1/sandboxes", s.observe("POST /v1/sandboxes", s.v1CreateSandbox))
	mux.HandleFunc("GET /v1/sandboxes/{id}", s.observe("GET /v1/sandboxes/{id}", s.remoteRoute(s.v1GetSandbox)))
	mux.HandleFunc("POST /v1/sandboxes/{id}/stop", s.observe("POST /v1/sandboxes/{id}/stop", s.remoteRoute(s.v1StopSandbox)))
	mux.HandleFunc("POST /v1/sandboxes/{id}/start", s.observe("POST /v1/sandboxes/{id}/start", s.remoteRoute(s.v1StartSandbox)))
	mux.HandleFunc("POST /v1/sandboxes/{id}/restart", s.observe("POST /v1/sandboxes/{id}/restart", s.remoteRoute(s.v1RestartSandbox)))
	mux.HandleFunc("POST /v1/sandboxes/{id}/exec", s.observe("POST /v1/sandboxes/{id}/exec", s.remoteRoute(s.v1ExecSandbox)))
	mux.HandleFunc("GET /v1/sandboxes/{id}/logs", s.observe("GET /v1/sandboxes/{id}/logs", s.remoteRoute(s.v1SandboxLogs)))
	mux.HandleFunc("GET /v1/sandboxes/{id}/network/ports", s.observe("GET /v1/sandboxes/{id}/network/ports", s.remoteRoute(s.v1GetPorts)))
	mux.HandleFunc("PUT /v1/sandboxes/{id}/network/ports", s.observe("PUT /v1/sandboxes/{id}/network/ports", s.remoteRoute(s.v1PutPorts)))
	mux.HandleFunc("POST /v1/sandboxes/{id}/network/ports", s.observe("POST /v1/sandboxes/{id}/network/ports", s.remoteRoute(s.v1AddPort)))
	mux.HandleFunc("DELETE /v1/sandboxes/{id}/network/ports/{port}", s.observe("DELETE /v1/sandboxes/{id}/network/ports/{port}", s.remoteRoute(s.v1DeletePort)))
	mux.HandleFunc("GET /v1/sandboxes/{id}/metrics", s.observe("GET /v1/sandboxes/{id}/metrics", s.remoteRoute(s.v1SandboxMetrics)))
	mux.HandleFunc("GET /v1/sandboxes/{id}/metrics/stream", s.observe("GET /v1/sandboxes/{id}/metrics/stream", s.remoteRoute(s.v1SandboxMetricsStream)))
	mux.HandleFunc("GET /v1/sandboxes/{id}/volumes", s.observe("GET /v1/sandboxes/{id}/volumes", s.remoteRoute(s.v1SandboxVolumes)))
	mux.HandleFunc("DELETE /v1/sandboxes/{id}", s.observe("DELETE /v1/sandboxes/{id}", s.remoteRoute(s.v1DeleteSandbox)))
	mux.HandleFunc("GET /v1/sandboxes/{id}/lifecycle", s.observe("GET /v1/sandboxes/{id}/lifecycle", s.remoteRoute(s.v1GetLifecycle)))
	mux.HandleFunc("PATCH /v1/sandboxes/{id}/lifecycle", s.observe("PATCH /v1/sandboxes/{id}/lifecycle", s.remoteRoute(s.v1PatchLifecycle)))
	mux.HandleFunc("POST /v1/sandboxes/{id}/keep-alive", s.observe("POST /v1/sandboxes/{id}/keep-alive", s.remoteRoute(s.v1KeepAliveOn)))
	mux.HandleFunc("DELETE /v1/sandboxes/{id}/keep-alive", s.observe("DELETE /v1/sandboxes/{id}/keep-alive", s.remoteRoute(s.v1KeepAliveOff)))
	mux.HandleFunc("GET /v1/sandboxes/{id}/network/egress", s.observe("GET /v1/sandboxes/{id}/network/egress", s.remoteRoute(s.v1GetEgress)))
	mux.HandleFunc("PUT /v1/sandboxes/{id}/network/egress", s.observe("PUT /v1/sandboxes/{id}/network/egress", s.remoteRoute(s.v1PutEgress)))
	mux.HandleFunc("GET /v1/sandboxes/{id}/network/activity", s.observe("GET /v1/sandboxes/{id}/network/activity", s.remoteRoute(s.v1NetworkActivity)))
	mux.HandleFunc("GET /v1/domain", s.observe("GET /v1/domain", s.v1GetDomain))
	mux.HandleFunc("PUT /v1/domain", s.observe("PUT /v1/domain", s.v1PutDomain))
	mux.HandleFunc("POST /v1/domain/verify", s.observe("POST /v1/domain/verify", s.v1VerifyDomain))
	mux.HandleFunc("GET /v1/domain/certificates", s.observe("GET /v1/domain/certificates", s.v1ListCertificates))
	mux.HandleFunc("POST /v1/domain/certificates", s.observe("POST /v1/domain/certificates", s.v1AddCertificate))
	mux.HandleFunc("GET /v1/policy", s.observe("GET /v1/policy", s.v1GetPolicy))
	mux.HandleFunc("PUT /v1/policy", s.observe("PUT /v1/policy", s.v1PutPolicy))
	mux.HandleFunc("GET /v1/policy/{section}", s.observe("GET /v1/policy/{section}", s.v1GetPolicySection))
	mux.HandleFunc("PUT /v1/policy/{section}", s.observe("PUT /v1/policy/{section}", s.v1PutPolicySection))
	mux.HandleFunc("GET /v1/approvals", s.observe("GET /v1/approvals", s.v1ListApprovals))
	mux.HandleFunc("GET /v1/approvals/{id}", s.observe("GET /v1/approvals/{id}", s.v1GetApproval))
	mux.HandleFunc("POST /v1/approvals/{id}/approve", s.observe("POST /v1/approvals/{id}/approve", s.v1ApproveApproval))
	mux.HandleFunc("POST /v1/approvals/{id}/deny", s.observe("POST /v1/approvals/{id}/deny", s.v1DenyApproval))

	// Node / cluster (M6). Drain/undrain are admin routes (roles.go);
	// reads are open to every role for dashboards.
	mux.HandleFunc("GET /v1/nodes", s.observe("GET /v1/nodes", s.v1ListNodes))
	mux.HandleFunc("GET /v1/nodes/{id}/capacity", s.observe("GET /v1/nodes/{id}/capacity", s.v1NodeCapacity))
	mux.HandleFunc("POST /v1/nodes/{id}/drain", s.observe("POST /v1/nodes/{id}/drain", s.v1DrainNode))
	mux.HandleFunc("DELETE /v1/nodes/{id}", s.observe("DELETE /v1/nodes/{id}", s.v1DeleteNode))
	mux.HandleFunc("POST /v1/nodes/{id}/undrain", s.observe("POST /v1/nodes/{id}/undrain", s.v1UndrainNode))
	mux.HandleFunc("GET /v1/capacity", s.observe("GET /v1/capacity", s.v1FleetCapacity))

	// M6 — internal worker fleet protocol. Reachable ONLY with a worker
	// token (auth middleware answers 404 for anything else).
	mux.HandleFunc("POST /internal/nodes/register", s.observe("POST /internal/nodes/register", s.internalNodeRegister))
	mux.HandleFunc("POST /internal/nodes/heartbeat", s.observe("POST /internal/nodes/heartbeat", s.internalNodeHeartbeat))

	// Instance settings.
	mux.HandleFunc("GET /v1/settings", s.observe("GET /v1/settings", s.v1GetSettings))
	mux.HandleFunc("PATCH /v1/settings", s.observe("PATCH /v1/settings", s.v1PatchSettings))

	// Console auth — the control plane is the login authority. status/login/setup
	// are exempt from the auth middleware (see internal/auth/middleware.go).
	mux.HandleFunc("GET /v1/auth/status", s.observe("GET /v1/auth/status", s.v1AuthStatus))
	mux.HandleFunc("POST /v1/auth/setup", s.observe("POST /v1/auth/setup", s.v1AuthSetup))
	mux.HandleFunc("POST /v1/auth/login", s.observe("POST /v1/auth/login", s.v1AuthLogin))
	mux.HandleFunc("POST /v1/auth/logout", s.observe("POST /v1/auth/logout", s.v1AuthLogout))
	mux.HandleFunc("POST /v1/auth/password", s.observe("POST /v1/auth/password", s.v1AuthPassword))
	mux.HandleFunc("DELETE /v1/auth/password", s.observe("DELETE /v1/auth/password", s.v1AuthPasswordReset))
	mux.HandleFunc("GET /v1/api-keys", s.observe("GET /v1/api-keys", s.v1ListAPIKeys))
	mux.HandleFunc("POST /v1/api-keys", s.observe("POST /v1/api-keys", s.v1CreateAPIKey))
	mux.HandleFunc("DELETE /v1/api-keys/{id}", s.observe("DELETE /v1/api-keys/{id}", s.v1DeleteAPIKey))
	mux.HandleFunc("GET /v1/auth/me", s.observe("GET /v1/auth/me", s.v1AuthMe))
	mux.HandleFunc("GET /v1/users", s.observe("GET /v1/users", s.v1ListUsers))
	mux.HandleFunc("POST /v1/users", s.observe("POST /v1/users", s.v1CreateUser))
	mux.HandleFunc("PATCH /v1/users/{id}", s.observe("PATCH /v1/users/{id}", s.v1UpdateUser))
	mux.HandleFunc("DELETE /v1/users/{id}", s.observe("DELETE /v1/users/{id}", s.v1DeleteUser))
	mux.HandleFunc("GET /v1/audit", s.observe("GET /v1/audit", s.v1ListAudit))
	mux.HandleFunc("GET /v1/audit/verify", s.observe("GET /v1/audit/verify", s.v1VerifyAudit))

	// Snapshots-as-templates.
	mux.HandleFunc("POST /v1/snapshots", s.observe("POST /v1/snapshots", s.v1CreateSnapshot))
	mux.HandleFunc("GET /v1/snapshots", s.observe("GET /v1/snapshots", s.v1ListSnapshots))
	mux.HandleFunc("GET /v1/snapshots/{id}", s.observe("GET /v1/snapshots/{id}", s.v1GetSnapshot))
	mux.HandleFunc("DELETE /v1/snapshots/{id}", s.observe("DELETE /v1/snapshots/{id}", s.v1DeleteSnapshot))

	s.inner = mux
	return mux
}

// observe records request count + duration per endpoint+method.
// (Logging middleware in the logging package wraps this from main.go.)
func (s *Server) observe(endpoint string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		h(sw, r)
		metrics.APIDuration.WithLabelValues(endpoint, r.Method).Observe(time.Since(start).Seconds())
		metrics.APIRequests.WithLabelValues(endpoint, r.Method, statusBucket(sw.status)).Inc()
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Flush forwards to the wrapped writer's flusher. Without it this
// wrapper hides http.Flusher from streaming handlers (SSE task
// events), so Go buffers the whole response and the client receives
// every event in one burst at the end. See internal/api/v1_tasks.go.
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack forwards to the wrapped writer so a WebSocket upgrade (the in-sandbox
// terminal) can take over the connection. Without it this wrapper hides
// http.Hijacker and the upgrade fails.
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := w.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, errors.New("ResponseWriter does not support hijacking")
}

func statusBucket(code int) string {
	switch {
	case code >= 500:
		return "5xx"
	case code >= 400:
		return "4xx"
	case code >= 300:
		return "3xx"
	case code >= 200:
		return "2xx"
	default:
		return "1xx"
	}
}
