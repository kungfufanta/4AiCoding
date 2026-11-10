// sandboxd — control plane for the sandboxd.
//
// A single Go binary. It listens on the internal network (default
// 0.0.0.0:9000, see defaultListenAddr), with auth enforced by the
// internal/auth middleware, and shells out to the `docker` CLI via
// os/exec.
//
// Phase 4 added: SQLite migrations + open, boot-time reconciler,
// HTTP API on 127.0.0.1, signal handling, graceful shutdown.
//
// Phase 5 adds: access-log tailer goroutine, optional Traefik
// open-connection poller goroutine (fallback if the metric can't be
// verified), the 30-second idle reaper, the 10-second host-memory
// pressure reaper, and the /wake/{id} handler (both Traefik
// catch-all and programmatic shapes).
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/activity"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/api"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/audit"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/auth"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/docker"
	domainpkg "github.com/tastyeffectco/sandboxd/control-plane/internal/domain"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/egress"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/egressproxy"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/events"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/fleet"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/idlock"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/instancecfg"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/kube"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/logging"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/loopback"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/metrics"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/netguard"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/netlog"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/policy"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/previewhost"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/reaper"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/reconcile"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/sandboxspec"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/secrets"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/snapshot"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/wake"
)

const (
	// OSS default: sandboxd runs in its own container and Traefik
	// reaches it over the compose network by service name, so it binds
	// all interfaces (it is NOT published to the host — only reachable
	// on the internal sandboxd_net). Override with SANDBOXD_ADDR.
	defaultListenAddr = "0.0.0.0:9000"
	defaultImage      = "sandboxd-base:0.3.0"
	migrationsDir     = "/usr/local/share/sandboxd/migrations"

	// Default data root for the portable build. The compose file
	// bind-mounts this path host:container symmetric, so it is a valid
	// host path for the sibling `docker run -v`. Override with
	// SANDBOXD_DATA_DIR / SANDBOXD_LOG_DIR (paths derived in main()).
	defaultDataDir = "/var/lib/sandboxd"
	defaultLogDir  = "/var/log/sandboxd"

	// The project used to be called "sandboxed"; installs from before the
	// rename keep their data where it is. Never move data silently.
	legacyDataDir = "/var/lib/sandboxed"
	legacyLogDir  = "/var/log/sandboxed"
	legacyEtcDir  = "/etc/sandboxed"

	// Idle / pressure / wake defaults. Each overridable via env; see
	// .env.example and README "Configuration".
	defaultIdleThresholdSec    = 2100 // 35 min idle → docker stop
	defaultIdleReapIntervalSec = 30
	defaultPressureIntervalSec = 10
	defaultMemHeadroomPct      = 15
	defaultMemRefusePct        = 10
	defaultMemEmergencyPct     = 5
	defaultWakeCostMB          = 800
	defaultWakeTCPReadySec     = 8
	defaultWakeGraceSec        = 60
	defaultKeepaliveMaxSec     = 86400

	// Snapshot subsystem defaults (auto-snapshotter is disabled in the
	// OSS build — see main(); these remain for the manual API surface).
	defaultSnapshotIntervalSec   = 3600
	defaultSnapshotRetentionDays = 7
	defaultSnapshotIdleHours     = 24
)

func main() {
	// Phase 8 — one-shot subcommands run and exit before the daemon
	// startup path. `sandboxd backfill-legacy ...` is the only one.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "backfill-legacy":
			os.Exit(runBackfillLegacy(os.Args[2:]))
		case "worker":
			os.Exit(runWorker(os.Args[2:]))
		case "version", "--version", "-v":
			v, c := buildIdent()
			fmt.Printf("sandboxd %s (%s)\n", v, c)
			os.Exit(0)
		default:
			fmt.Fprintf(os.Stderr, "sandboxd: unknown subcommand %q\n", os.Args[1])
			os.Exit(2)
		}
	}

	log := logging.NewLogger()

	addr := envDefault("SANDBOXD_ADDR", defaultListenAddr)
	image := envDefault("SANDBOXD_IMAGE", defaultImage)
	// OSS default preview domain is "localhost": browsers resolve any
	// *.localhost name to 127.0.0.1, so preview URLs work out of the box
	// with no DNS and no certificates. Set PREVIEW_DOMAIN to a real
	// wildcard domain (plus PREVIEW_ENTRYPOINT=websecure, PREVIEW_TLS=true)
	// for a public deployment.
	domain := envDefault("PREVIEW_DOMAIN", "localhost")

	// Data + log roots. All per-sandbox workspace paths derive from
	// dataDir; the compose file bind-mounts dataDir host:container
	// symmetric so the path sandboxd writes is also a valid host path
	// for the sibling `docker run -v <workspace>:/home/sandbox`.
	dataDir := envDefault("SANDBOXD_DATA_DIR", legacyAware(defaultDataDir, legacyDataDir))
	logDir := envDefault("SANDBOXD_LOG_DIR", legacyAware(defaultLogDir, legacyLogDir))
	stateDir := filepath.Join(dataDir, "state")
	dbPath := filepath.Join(stateDir, "sandboxd.db")
	workspacesRoot := filepath.Join(dataDir, "workspaces")
	snapshotsRoot := filepath.Join(dataDir, "_snapshots")
	templatesRoot := filepath.Join(dataDir, "templates")
	libraryRoot := filepath.Join(dataDir, "library")
	accessLogPath := filepath.Join(logDir, "traefik-access.log")
	tailerOffsetFs := filepath.Join(stateDir, "traefik-tail.offset")

	// OSS docker-native toggles (default to the portable behaviour).
	network := os.Getenv("SANDBOXD_NETWORK")        // shared docker network for Traefik routing
	userns := envDefault("SANDBOXD_USERNS", "host") // sandbox + seed --userns; "host" is deterministic on any daemon
	previewEntrypoint := envDefault("PREVIEW_ENTRYPOINT", "web")
	previewTLS := boolFromEnv("PREVIEW_TLS", false)
	previewURLScheme := strings.ToLower(strings.TrimSpace(os.Getenv("PREVIEW_URL_SCHEME")))
	if previewURLScheme != "" && previewURLScheme != "http" && previewURLScheme != "https" {
		fmt.Fprintf(os.Stderr, "PREVIEW_URL_SCHEME must be http, https, or empty (got %q)\n", previewURLScheme)
		os.Exit(2)
	}
	// Hostname layout under PREVIEW_DOMAIN: nested (s-<id>-<port>.preview.<D>,
	// the default) or flat (s-<id>-<port>[--tag].<D>, so one *.<D> wildcard
	// certificate covers every host and several instances can share a
	// domain by tag).
	previewHostStyle, err := previewhost.ParseStyle(os.Getenv("PREVIEW_HOST_STYLE"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(2)
	}
	previewHostTag := strings.TrimSpace(os.Getenv("PREVIEW_HOST_TAG"))
	if !previewhost.ValidTag(previewHostTag) {
		fmt.Fprintf(os.Stderr, "PREVIEW_HOST_TAG must match [a-z0-9][a-z0-9-]{0,30} (got %q)\n", previewHostTag)
		os.Exit(2)
	}
	hosts, _ := previewhost.New(domain, previewHostStyle, previewHostTag)
	// Host-facing port the preview/console URLs are reached on (compose passes
	// the published HTTP_PORT here). Default "80": bare URLs on a dedicated host.
	publicHTTPPort := envDefault("SANDBOXD_PUBLIC_HTTP_PORT", "80")
	setMemoryHigh := boolFromEnv("SANDBOXD_SET_MEMORY_HIGH", false)

	migrations := envDefault("SANDBOXD_MIGRATIONS", migrationsDir)
	if _, err := os.Stat(migrations); err != nil {
		if exe, e := os.Executable(); e == nil {
			alt := filepath.Join(filepath.Dir(exe), "..", "..", "migrations")
			if _, e2 := os.Stat(alt); e2 == nil {
				migrations = alt
			}
		}
	}

	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		log.Error("startup: mkdir state dir failed", "err", err.Error())
		os.Exit(1)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dsn := fmt.Sprintf("file:%s?_journal=WAL&_busy_timeout=5000&_fk=1", envDefault("SANDBOXD_DB", dbPath))
	st, err := store.Open(ctx, dsn, migrations)
	if err != nil {
		log.Error("startup: store open failed", "err", err.Error())
		os.Exit(1)
	}
	defer func() {
		if err := st.Close(); err != nil {
			log.Error("shutdown: store close failed", "err", err.Error())
		}
	}()

	// Domain manager (PUT /v1/domain): a stored domain / TLS mode overrides
	// PREVIEW_DOMAIN / PREVIEW_TLS. Applied here, at start, because every
	// preview route and URL derives from it.
	domainTLSMode := ""
	if dc, ok, derr := api.StoredDomain(st, ctx); derr != nil {
		log.Warn("startup: stored domain unreadable; using PREVIEW_DOMAIN", "err", derr.Error())
	} else if ok {
		domain, previewTLS = dc.Domain, dc.TLS()
		if previewTLS {
			domainTLSMode = dc.TLSMode
			if os.Getenv("PREVIEW_ENTRYPOINT") == "" {
				previewEntrypoint = "websecure"
			}
		}
		hosts, _ = previewhost.New(domain, previewHostStyle, previewHostTag)
		log.Info("startup: using the stored domain", "domain", domain, "tls_mode", dc.TLSMode)
	}

	// Phase 5 — Backfill last_active_at for legacy running rows where
	// the migration default (0) would otherwise make every existing
	// row an idle candidate the moment the daemon comes up.
	if n, err := st.BackfillRunningActivity(ctx); err != nil {
		log.Warn("startup: backfill last_active_at failed", "err", err.Error())
	} else if n > 0 {
		log.Info("startup: backfilled last_active_at for running rows", "rows", n)
	}

	// Phase 8 — audit logger + service-token auth middleware. The
	// initial config is read from the process environment (systemd has
	// already loaded the EnvironmentFile); SIGHUP re-reads the file.
	auditLog := audit.New(st, log.With("component", "audit"))
	eventRec := events.New(st, log.With("component", "events"))
	envFile := envDefault("SANDBOXD_ENV_FILE", legacyAware("/etc/sandboxd/sandboxd.env", legacyEtcDir+"/sandboxd.env"))
	// M7b — executor mode: this sandboxd runs sandboxes for a control plane.
	// Its SANDBOXD_WORKER_TOKEN is the credential it presents there, never
	// one it accepts, so it is taken out of the environment before the auth
	// config is read.
	var exec *executorConfig
	if cpURL := strings.TrimSpace(os.Getenv("SANDBOXD_CONTROL_PLANE_URL")); cpURL != "" {
		exec = &executorConfig{
			ControlPlaneURL: cpURL,
			WorkerToken:     strings.TrimSpace(os.Getenv("SANDBOXD_WORKER_TOKEN")),
			NodeID:          strings.TrimSpace(os.Getenv("SANDBOXD_NODE_ID")),
			Address:         strings.TrimSpace(os.Getenv("SANDBOXD_NODE_ADDRESS")),
			Interval:        durationFromEnvSec("SANDBOXD_HEARTBEAT_INTERVAL_SECONDS", int(defaultHeartbeatInterval.Seconds())),
		}
		_ = os.Unsetenv("SANDBOXD_WORKER_TOKEN")
		if exec.WorkerToken == "" || exec.NodeID == "" || exec.Address == "" {
			fmt.Fprintln(os.Stderr, "executor mode (SANDBOXD_CONTROL_PLANE_URL) needs SANDBOXD_WORKER_TOKEN, SANDBOXD_NODE_ID and SANDBOXD_NODE_ADDRESS")
			os.Exit(2)
		}
		tok, terr := loadNodeAPIToken(stateDir)
		if terr != nil {
			log.Error("startup: node API token", "err", terr.Error())
			os.Exit(1)
		}
		exec.APIToken = tok
	}
	authCfg := auth.ParseConfig(os.Getenv)
	// The credential resolver validates DB-backed console sessions and API keys;
	// env-configured SANDBOXD_API_TOKENS remain a fallback for the bootstrap key.
	authMw := auth.NewMiddleware(authCfg, api.NewStoreResolver(st), auditLog, log.With("component", "auth"))
	if exec != nil {
		authMw.SetFleetToken(exec.APIToken)
	}
	denyMode := envDefault("SANDBOXD_FORWARD_AUTH_DENY_MODE", "redirect")
	{
		ac := authMw.Snapshot()
		log.Info("startup: auth configured",
			"api_tokens", len(ac.APITokens),
			"preview_secrets", len(ac.PreviewSecrets),
			"auth_disabled", ac.Disabled,
			"forward_auth_deny_mode", denyMode,
		)
		if len(ac.APITokens) == 0 && !ac.Disabled {
			log.Warn("startup: SANDBOXD_API_TOKENS is empty — every external API call will 401 (loopback still works)")
		}
	}

	dockerClient := docker.NewClient()

	// M8 — the executor engine. "docker" (default) keeps the CLI
	// executor from M7b; "kubernetes" runs every sandbox as a Pod
	// (+Service/NetworkPolicy/Ingress) in the cluster this process
	// sits in (PLAN §2 k3s B2). The concrete dockerClient stays for
	// the two docker-only seams below (netguard subnets; loopback
	// seeding happens through the CLI either way).
	execEngine := docker.Engine(dockerClient)
	engineLabel := "docker"
	if strings.EqualFold(strings.TrimSpace(os.Getenv("SANDBOXD_EXECUTOR_ENGINE")), "kubernetes") {
		kcfg := kube.Config{
			APIServer: strings.TrimSpace(os.Getenv("SANDBOXD_K8S_API")),
			Token:     strings.TrimSpace(os.Getenv("SANDBOXD_K8S_TOKEN")),
			Namespace: strings.TrimSpace(os.Getenv("SANDBOXD_K8S_NAMESPACE")),
			Insecure:  boolFromEnv("SANDBOXD_K8S_INSECURE", false),
		}
		if ca := strings.TrimSpace(os.Getenv("SANDBOXD_K8S_CA")); ca != "" {
			kcfg.CAData = []byte(ca)
		} else if p := strings.TrimSpace(os.Getenv("SANDBOXD_K8S_CA_FILE")); p != "" {
			if b, err := os.ReadFile(p); err == nil {
				kcfg.CAData = b
			}
		}
		keng, kerr := kube.New(kcfg, filepath.Join(stateDir, "kube-engine"), log.With("component", "kube-engine"))
		if kerr != nil {
			log.Error("startup: kubernetes engine init failed", "err", kerr.Error())
			os.Exit(1)
		}
		// Preview hosts (Ingress rules) mirror the docker path: the
		// same domain, style and tag feed previewhost.
		keng.IngressDomain = envDefault("SANDBOXD_K8S_INGRESS_DOMAIN", domain)
		keng.IngressClass = strings.TrimSpace(os.Getenv("SANDBOXD_K8S_INGRESS_CLASS"))
		keng.PreviewHost = func(id string, port int) string { return hosts.Preview(id, port) }
		if sel := strings.TrimSpace(os.Getenv("SANDBOXD_K8S_PROXY_SELECTOR")); sel != "" {
			ps := map[string]string{}
			for _, kv := range strings.Split(sel, ",") {
				if k, v, ok := strings.Cut(strings.TrimSpace(kv), "="); ok && k != "" {
					ps[k] = v
				}
			}
			keng.ProxySelector = ps
		}
		execEngine = keng
		engineLabel = "kubernetes"
		log.Info("startup: kubernetes executor engine",
			"namespace", keng.Namespace(), "backend", keng.Backend(),
			"ingress_domain", keng.IngressDomain, "ingress_class", keng.IngressClass)
	}

	loopMgr := loopback.New()
	loopMgr.Root = workspacesRoot
	loopMgr.SeedImage = image
	loopMgr.DockerBin = "docker"
	loopMgr.Userns = userns
	loopMgr.Log = log.With("component", "loopback")

	// Egress (nftables) is DISABLED in the portable OSS build: it
	// requires host nftables + journald + systemd-timer refresh jobs
	// that don't exist in a plain docker-compose deployment. Every
	// consumer (reconciler, reapers, wake, API) treats a nil egress
	// manager as "no egress policy" — exactly the default-allow
	// behaviour, minus the connection logging. A nil manager here is
	// the single switch that keeps all of that off.
	var egressMgr *egress.Manager = nil

	// Phase 7 — shared per-id lock + snapshot subsystem. The snapshot
	// Manager is handed to the reconciler so its boot pass can sweep
	// crash debris (stale .tmp snapshots, interrupted-restore .bak).
	idLocks := idlock.New()
	snapshotMgr := &snapshot.Manager{
		WorkspacesRoot: workspacesRoot,
		SnapshotsRoot:  snapshotsRoot,
		RetentionDays:  intFromEnv("SANDBOXD_SNAPSHOT_RETENTION_DAYS", defaultSnapshotRetentionDays),
		IdleThreshold:  time.Duration(intFromEnv("SANDBOXD_SNAPSHOT_IDLE_HOURS", defaultSnapshotIdleHours)) * time.Hour,
		Store:          st,
		Locks:          idLocks,
		Log:            log.With("component", "snapshot"),
	}

	version, gitCommit := buildIdent()
	metrics.BuildInfo.WithLabelValues(version, gitCommit).Set(1)
	if err := metrics.RefreshSandboxGauge(ctx, st); err != nil {
		log.Warn("startup: refresh sandbox gauge failed", "err", err.Error())
	}

	// Reconciler once, before the listener.
	rcDeps := reconcile.Deps{
		Store:         st,
		Docker:        execEngine,
		Loopback:      loopMgr,
		Egress:        egressMgr,
		Snapshot:      snapshotMgr,
		Log:           log.With("component", "reconcile"),
		SetMemoryHigh: setMemoryHigh,
	}
	res, err := reconcile.Once(ctx, rcDeps)
	metrics.ReconcilerRuns.Inc()
	metrics.ReconcilerLastDuration.Set(res.Duration.Seconds())
	for kind, n := range res.Orphans {
		metrics.ReconcilerOrphans.WithLabelValues(kind).Set(float64(n))
	}
	if err != nil {
		log.Error("startup: reconcile failed (continuing)", "err", err.Error())
	} else {
		log.Info("startup: reconcile complete",
			"rows", res.Rows,
			"reapplied", res.Reapplied,
			"stopped", res.Stopped,
			"errored", res.Errored,
			"egress_added", res.EgressAdded,
			"orphan_containers", res.Orphans["container"],
			"orphan_mounts", res.Orphans["mount"],
			"duration_ms", res.Duration.Milliseconds(),
		)
	}
	// Egress is disabled in the OSS build (egressMgr == nil); the
	// sources-active gauge is therefore always zero.
	metrics.EgressSourcesActive.Set(0)
	_ = metrics.RefreshSandboxGauge(ctx, st)

	// Phase 5 env knobs.
	idleThreshold := durationFromEnvSec("SANDBOXD_IDLE_THRESHOLD_SECONDS", defaultIdleThresholdSec)
	idleInterval := durationFromEnvSec("SANDBOXD_IDLE_REAP_INTERVAL_SECONDS", defaultIdleReapIntervalSec)
	pressureInterval := durationFromEnvSec("SANDBOXD_PRESSURE_INTERVAL_SECONDS", defaultPressureIntervalSec)
	headroomPct := floatFromEnv("SANDBOXD_MEM_HEADROOM_PCT", defaultMemHeadroomPct)
	refuseWakesPct := floatFromEnv("SANDBOXD_MEM_REFUSE_WAKES_PCT", defaultMemRefusePct)
	emergencyPct := floatFromEnv("SANDBOXD_MEM_EMERGENCY_PCT", defaultMemEmergencyPct)
	wakeCostMB := uintFromEnv("SANDBOXD_WAKE_COST_MB", defaultWakeCostMB)
	wakeTCPReady := durationFromEnvSec("SANDBOXD_WAKE_TCP_READY_TIMEOUT_SECONDS", defaultWakeTCPReadySec)
	wakeGrace := durationFromEnvSec("SANDBOXD_WAKE_GRACE_SECONDS", defaultWakeGraceSec)
	keepaliveMax := durationFromEnvSec("SANDBOXD_KEEPALIVE_MAX_SECONDS", defaultKeepaliveMaxSec)

	// Phase 8B — live, runtime-editable lifecycle tunables. Start from env
	// defaults, then overlay any persisted edits (PATCH /v1/settings) so they
	// survive restart. The reaper + keepalive path read this live.
	live := instancecfg.New(instancecfg.Snapshot{
		IdleEnabled:          idleInterval > 0,
		IdleThresholdSeconds: int(idleThreshold.Seconds()),
		KeepaliveMaxSeconds:  int(keepaliveMax.Seconds()),
	})
	if persisted, perr := st.GetInstanceSettings(ctx); perr == nil {
		live.Set(instancecfg.Snapshot{
			IdleEnabled:          persisted.IdleReapEnabled,
			IdleThresholdSeconds: persisted.IdleThresholdSeconds,
			KeepaliveMaxSeconds:  persisted.KeepaliveMaxSeconds,
		})
		log.Info("instance settings: loaded persisted lifecycle tunables")
	}

	inflight := activity.NewInflightExec()
	refused := &atomic.Bool{}

	// Pressure reaper — long-lived. Also used synchronously by the
	// wake admission code, so we instantiate it before the wake
	// handler.
	pressure := &reaper.Pressure{
		Cfg: reaper.PressureConfig{
			Interval:       pressureInterval,
			HeadroomPct:    headroomPct,
			RefuseWakesPct: refuseWakesPct,
			EmergencyPct:   emergencyPct,
		},
		Store:    st,
		Docker:   execEngine,
		Inflight: inflight,
		Egress:   egressMgr,
		Refused:  refused,
		Log:      log.With("component", "pressure-reaper"),
	}

	admitCfg := wake.AdmitConfig{
		WakeCostMB: wakeCostMB,
		FloorPct:   refuseWakesPct,
		Refused:    refused,
		Tick:       pressure.Tick,
	}

	wakeHandler, err := wake.New(
		st, execEngine, hosts,
		wake.Config{
			TCPReadyTimeout: wakeTCPReady,
			RefreshSeconds:  2,
		},
		admitCfg,
		egressMgr,
		idLocks,
		log.With("component", "wake"),
	)
	if err != nil {
		log.Error("startup: wake handler init failed", "err", err.Error())
		os.Exit(1)
	}
	// Phase 8 — gate stopped private-sandbox wakes through the same
	// preview-token check as /forward-auth.
	wakeHandler.Auth = authMw
	wakeHandler.Audit = auditLog
	wakeHandler.ForwardAuthDenyMode = denyMode
	wakeHandler.SetMemoryHigh = setMemoryHigh

	// app_config/secrets encryption: SANDBOXD_SECRETS_KEY (base64 32
	// bytes) or an auto-generated 0600 keyfile under the data dir.
	secretsCipher, err := secrets.Load(os.Getenv("SANDBOXD_SECRETS_KEY"), filepath.Join(dataDir, "secrets.key"))
	if err != nil {
		log.Error("init secrets encryption", "err", err.Error())
		os.Exit(1)
	}

	// gVisor (opt-in): SANDBOXD_RUNTIME=runsc runs sandboxes under runsc.
	// gVisor's netstack can't reach Docker's embedded DNS (127.0.0.11) on a
	// user-defined network, so we write a resolv.conf with real nameservers
	// (SANDBOXD_DNS, default public resolvers) and bind-mount it into each
	// sandbox. Requires runsc registered with `--host-uds=create` (docs/gvisor.md).
	sbxRuntime := envDefault("SANDBOXD_RUNTIME", "")
	var dnsResolvConf string
	if sbxRuntime == "runsc" {
		var ns []string
		for _, d := range strings.Split(os.Getenv("SANDBOXD_DNS"), ",") {
			if d = strings.TrimSpace(d); d != "" {
				ns = append(ns, d)
			}
		}
		if len(ns) == 0 {
			ns = []string{"1.1.1.1", "8.8.8.8"}
		}
		var b strings.Builder
		for _, n := range ns {
			fmt.Fprintf(&b, "nameserver %s\n", n)
		}
		dnsResolvConf = filepath.Join(dataDir, "gvisor-resolv.conf")
		if err := os.WriteFile(dnsResolvConf, []byte(b.String()), 0o644); err != nil {
			log.Error("gvisor: write resolv.conf failed; sandbox DNS may not resolve", "err", err.Error())
			dnsResolvConf = ""
		} else {
			log.Info("gvisor runtime enabled", "runtime", sbxRuntime, "dns", ns)
		}
	}

	// Self-healing start: rebuild a container that is missing, or that was
	// created from an older base image than we now run, so long-lived sandboxes
	// move onto the current image. Lossless: everything durable is in the
	// workspace bind mount, and the container rootfs is read-only by construction.
	// One spec environment for every container sandboxd builds (create and
	// recreate), so both paths produce the same hardened container.
	// PLAN §8 #5: the egress proxy. SANDBOXD_EGRESS_PROXY_ADDR is where
	// sandboxd listens for it (e.g. ":3128"; empty = off); the URL is what a
	// sandbox is told to use (an alias of sandboxd on the sandbox network).
	egressProxyAddr := os.Getenv("SANDBOXD_EGRESS_PROXY_ADDR")
	egressProxyURL := ""
	if egressProxyAddr != "" {
		_, port, perr := net.SplitHostPort(egressProxyAddr)
		if perr != nil || port == "" {
			log.Error("startup: bad SANDBOXD_EGRESS_PROXY_ADDR", "value", egressProxyAddr)
			os.Exit(1)
		}
		egressProxyURL = envDefault("SANDBOXD_EGRESS_PROXY_URL", "http://sandboxd-egress:"+port)
	}
	specEnv := sandboxspec.Env{
		EgressProxy:       egressProxyURL,
		Nofile:            intFromEnv("SANDBOXD_SANDBOX_NOFILE", sandboxspec.DefaultNofile),
		Image:             image,
		Network:           network,
		Userns:            userns,
		Runtime:           sbxRuntime,
		DNSResolvConf:     dnsResolvConf,
		PreviewDomain:     domain,
		PreviewHostStyle:  hosts.Style,
		PreviewHostTag:    hosts.Tag,
		PreviewEntrypoint: previewEntrypoint,
		PreviewTLS:        previewTLS,
		User:              envDefault("SANDBOXD_SANDBOX_USER", sandboxspec.DefaultUser),
		MemoryMB:          intFromEnv("SANDBOXD_DEFAULT_MEMORY_MB", sandboxspec.DefaultMemoryMB),
	}
	limits := api.Limits{
		ImageAllowlist: splitList(os.Getenv("SANDBOXD_IMAGE_ALLOWLIST")),
		MaxCPUs:        floatFromEnv("SANDBOXD_MAX_CPUS", 0),
		MaxMemoryMB:    intFromEnv("SANDBOXD_MAX_MEMORY_MB", sandboxspec.DefaultMemoryMB),
		MaxPorts:       intFromEnv("SANDBOXD_MAX_PORTS_PER_SANDBOX", 16),
	}
	wakeHandler.Image = image
	wakeHandler.User = specEnv.User
	wakeHandler.Recreate = func(ctx context.Context, sb *store.Sandbox) error {
		spec := sandboxspec.Build(sb, specEnv)
		// Remove any existing container first so the name is free; ignore a
		// not-found (the common case) and let Run surface a real failure.
		if err := execEngine.Remove(ctx, spec.Name); err != nil && err != docker.ErrNotFound {
			log.Warn("recreate: remove existing container failed (continuing)", "err", err.Error())
		}
		_, err := execEngine.Run(ctx, spec)
		return err
	}

	// Live record of egress-proxy requests (console network view).
	netLog := netlog.New()
	server := &api.Server{
		Store:               st,
		Secrets:             secretsCipher,
		Docker:              execEngine,
		Loopback:            loopMgr,
		Log:                 log.With("component", "api"),
		PreviewDomain:       domain,
		PreviewHostStyle:    hosts.Style,
		PreviewHostTag:      hosts.Tag,
		Image:               image,
		Network:             network,
		Userns:              userns,
		Runtime:             sbxRuntime,
		DNSResolvConf:       dnsResolvConf,
		PreviewEntrypoint:   previewEntrypoint,
		PreviewTLS:          previewTLS,
		PreviewURLScheme:    previewURLScheme,
		PublicHTTPPort:      publicHTTPPort,
		SetMemoryHigh:       setMemoryHigh,
		Inflight:            inflight,
		Wake:                wakeHandler,
		Admit:               admitCfg,
		KeepaliveMax:        keepaliveMax,
		Egress:              egressMgr,
		Snapshot:            snapshotMgr,
		Locks:               idLocks,
		Auth:                authMw,
		Audit:               auditLog,
		Events:              eventRec,
		SnapshotsRoot:       snapshotsRoot,
		ForwardAuthDenyMode: denyMode,
		TemplatesDir:        envDefault("SANDBOXD_TEMPLATES_DIR", templatesRoot),
		LibraryRoot:         envDefault("SANDBOXD_LIBRARY_DIR", libraryRoot),
		LLMTxtPath:          envDefault("SANDBOXD_LLM_TXT_PATH", legacyAware("/etc/sandboxd/llm.txt", legacyEtcDir+"/llm.txt")),
		Instance: api.InstanceInfo{
			Version:              version,
			GitCommit:            gitCommit,
			AuthEnabled:          !authCfg.Disabled,
			StorageMode:          "directory", // OSS bind-mounted workspaces (see internal/loopback)
			EgressMode:           egressModeLabel(egressMgr),
			IdleReapEnabled:      idleInterval > 0,
			IdleThresholdSeconds: int(idleThreshold.Seconds()),
		},
		Live:           live,
		SpecEnv:        specEnv,
		Limits:         limits,
		APIKeyMaxTTL:   durationFromEnvSec("SANDBOXD_API_KEY_MAX_TTL_SECONDS", 0),
		EgressProxyURL: egressProxyURL,
		NetLog:         netLog,
		DomainTLSMode:  domainTLSMode,
		Certs: &domainpkg.CertStore{
			Dir:        os.Getenv("SANDBOXD_TRAEFIK_DYNAMIC_DIR"),
			TraefikDir: envDefault("SANDBOXD_TRAEFIK_DYNAMIC_DIR_IN_TRAEFIK", "/etc/traefik/dynamic"),
		},
	}
	// First admin from the environment (headless installs). No-op once any
	// user exists, so the variable can stay set.
	if created, name, berr := api.BootstrapAdmin(ctx, st, os.Getenv("SANDBOXD_BOOTSTRAP_ADMIN")); berr != nil {
		log.Error("startup: bootstrap admin failed", "err", berr.Error())
	} else if created {
		log.Info("startup: bootstrap admin created", "username", name)
	}

	// M6 — fleet membership. The control plane is itself a node (role
	// 'local'): it is the executor in the all-in-one topology, it shows
	// up in GET /v1/nodes and it obeys drain like any worker. Workers
	// join through POST /internal/nodes/register (worker token).
	nodeLease := durationFromEnvSec("SANDBOXD_NODE_LEASE_SECONDS", int(fleet.DefaultLease.Seconds()))
	server.NodeLease = nodeLease
	localNodeID := ensureLocalNode(ctx, st, log, engineLabel)

	// Phase 5 — after reconcile, if MemAvailable is
	// already below the healthy floor, run one synchronous pressure
	// tick before opening the listener. Keeps the host from
	// accepting requests while it's already saturated.
	if mi, err := reaper.ReadMemInfo(); err == nil {
		metrics.MemAvailablePercent.Set(mi.AvailablePct())
		metrics.MemAvailableBytes.Set(float64(mi.AvailableBytes()))
		if mi.AvailablePct() < headroomPct {
			log.Warn("startup: MemAvailable below headroom — running pressure tick before listener opens",
				"avail_pct", mi.AvailablePct())
			pressure.Tick(ctx)
		}
	}

	// Mux + middleware: catch-all dispatch in front of the API mux so
	// the loopback-only listener handles both the loopback API and
	// the Traefik catch-all reverse-proxied traffic. The Host header
	// is the only discriminator.
	// Phase 8 — the service-token auth middleware wraps the API mux
	// only. The wake catch-all (dispatched first by hostDispatch) is
	// the browser preview path and stays unauthenticated; a private
	// sandbox's stopped-wake is gated inside the wake handler itself.
	apiMux := authMw.Wrap(server.Handler())
	root := hostDispatch(wakeHandler, apiMux, log)
	// PLAN §8 #13: refuse anything that comes from the sandbox network, for
	// the API and the wake catch-all alike. The edge router reaches us over
	// the control network (see docker-compose.yml), never over this one.
	var guard netguard.Guard
	if network != "" && os.Getenv("SANDBOXD_ALLOW_API_FROM_SANDBOX_NETWORK") != "true" {
		subnets, gateways, gerr := dockerClient.NetworkSubnets(ctx, network)
		switch {
		case gerr != nil:
			log.Error("startup: cannot read the sandbox network subnets; API source guard is OFF",
				"network", network, "err", gerr.Error())
		default:
			if bad := guard.Set(subnets, gateways); len(bad) > 0 {
				log.Warn("startup: ignored unparsable network entries", "entries", bad)
			}
			log.Info("startup: API refuses requests from the sandbox network",
				"network", network, "subnets", subnets, "exempt_gateways", gateways)
		}
	}
	root = guard.Wrap(root)
	root = logging.Middleware(log, root)

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           root,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Egress proxy listener (separate from the API: sandboxes may reach this
	// one, never the API).
	var egressSrv *http.Server
	if egressProxyAddr != "" {
		elog := log.With("component", "egress-proxy")
		px := &egressproxy.Proxy{
			Log: elog,
			Lookup: func(ctx context.Context, ip string) (string, policy.EgressRules, bool) {
				sb, err := st.RunningByIP(ctx, ip)
				if err != nil {
					return "", policy.EgressRules{}, false
				}
				return sb.ID, api.EgressRulesOf(sb.Spec), true
			},
			OnDecision: func(d egressproxy.Decision) {
				elog.Info("egress", "sandbox_id", d.SandboxID, "method", d.Method, "host", d.Host, "port", d.Port,
					"ip", d.IP, "allowed", d.Allowed, "reason", d.Reason)
				netLog.Record(d.SandboxID, netlog.Event{Method: d.Method, Host: d.Host, Port: d.Port,
					IP: d.IP, Allowed: d.Allowed, Reason: d.Reason})
			},
			OnBytes: netLog.AddBytes,
		}
		egressSrv = &http.Server{Addr: egressProxyAddr, Handler: px, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			log.Info("egress proxy listening", "addr", egressProxyAddr, "url", egressProxyURL)
			if err := egressSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("egress proxy stopped", "err", err.Error())
			}
		}()
	} else {
		log.Warn("startup: SANDBOXD_EGRESS_PROXY_ADDR unset — sandbox egress is NOT enforced by sandboxd")
	}

	// --- Phase 5 background goroutines ---------------------------------
	gctx, gcancel := context.WithCancel(context.Background())
	defer gcancel()

	// M6 — keep the local node's heartbeat fresh so the fleet view
	// reflects liveness even though role='local' is always online.
	go bumpLocalNodeLoop(gctx, st, log, localNodeID)
	if exec != nil {
		exec.Engine = engineLabel
		go runExecutorAgent(gctx, log.With("component", "fleet-executor"), st, server, *exec)
	}

	// Idle reaper.
	idle := &reaper.Idle{
		Cfg: reaper.IdleConfig{
			Threshold: idleThreshold,
			Interval:  idleInterval,
			WakeGrace: wakeGrace,
		},
		Store:    st,
		Docker:   execEngine,
		Inflight: inflight,
		Egress:   egressMgr,
		Log:      log.With("component", "idle-reaper"),
		// Phase 8B — hot-read the runtime-editable threshold + enable toggle.
		ThresholdFn: live.IdleThreshold,
		EnabledFn:   live.IdleEnabled,
	}
	go func() {
		if err := idle.Run(gctx); err != nil {
			log.Warn("idle reaper exited", "err", err.Error())
		}
	}()

	// Pressure reaper.
	go func() {
		if err := pressure.Run(gctx); err != nil {
			log.Warn("pressure reaper exited", "err", err.Error())
		}
	}()

	// Access-log tailer.
	tailer := &activity.Tailer{
		LogPath:        envDefault("SANDBOXD_ACCESS_LOG", accessLogPath),
		CheckpointPath: envDefault("SANDBOXD_TAILER_OFFSET", tailerOffsetFs),
		Hosts:          hosts,
		Store:          st,
		Log:            log.With("component", "access-log-tailer"),
	}
	go func() {
		if err := tailer.Run(gctx); err != nil {
			log.Warn("access-log tailer exited", "err", err.Error())
		}
	}()

	// Open-connection poller — only if the operator has supplied a
	// verified metric name via env. Without the env, the poller logs
	// "fallback mode" and returns; the access-log tailer alone
	// drives last_active_at and the wider WakeGrace window
	// compensates for long-lived WS quiet periods.
	pollerMetric := os.Getenv("SANDBOXD_POLLER_METRIC_RE")
	pollerLabel := envDefault("SANDBOXD_POLLER_SERVICE_LABEL", "service")
	pollerURL := envDefault("SANDBOXD_POLLER_URL", "http://127.0.0.1:8082/metrics")
	pollerInterval := durationFromEnvSec("SANDBOXD_POLLER_INTERVAL_SECONDS", 15)
	var pollerRE *regexp.Regexp
	if pollerMetric != "" {
		var perr error
		pollerRE, perr = regexp.Compile(pollerMetric)
		if perr != nil {
			log.Warn("startup: SANDBOXD_POLLER_METRIC_RE invalid — running poller in fallback",
				"err", perr.Error())
			pollerRE = nil
		}
	}
	poller := &activity.Poller{
		MetricsURL:    pollerURL,
		Interval:      pollerInterval,
		MetricNameRE:  pollerRE,
		ServiceLabel:  pollerLabel,
		PreviewDomain: domain,
		Store:         st,
		Log:           log.With("component", "connection-poller"),
	}
	go func() {
		if err := poller.Run(gctx); err != nil {
			log.Warn("connection poller exited", "err", err.Error())
		}
	}()

	// --- Egress goroutines: DISABLED in the OSS build -----------------
	// Earlier deployments ran a journald-tail egress collector, an
	// nftables drop-counter poller, and a systemd refresh-job watcher.
	// All three depend on host nftables / journald / systemd, which a
	// portable docker-compose deployment does not provide. With
	// egressMgr == nil there is nothing to collect, so these goroutines
	// are intentionally not started.
	_ = egressMgr // documents the deliberate nil

	// --- Auto-snapshotter: DISABLED in the OSS build ------------------
	// The hourly auto-snapshotter zstd-compresses each sandbox's
	// workspace image. The portable build stores workspaces as plain
	// directories (not loopback .img files), so the compress path does
	// not apply. The manual snapshot/template REST endpoints remain
	// wired but are EXPERIMENTAL on directory storage (see README).
	_ = snapshotMgr // constructed for the reconciler debris sweep + API

	// --- Phase 8 workspace_owner orphan check -------------------------
	// Every 6 h, log workspace_owner rows whose .img is
	// gone. Never deletes; disposition is the operator's.
	go func() {
		ownerLog := log.With("component", "owner-orphan-check")
		t := time.NewTicker(6 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-gctx.Done():
				return
			case <-t.C:
				if n := reconcile.CheckWorkspaceOwnerOrphans(gctx, st, loopMgr, ownerLog); n > 0 {
					ownerLog.Warn("workspace_owner rows with no .img on disk", "count", n)
				}
			}
		}
	}()

	// Listen + serve.
	errCh := make(chan error, 1)
	go func() {
		log.Info("startup: listening",
			"addr", addr,
			"preview_domain", domain,
			"preview_host_style", hosts.Style,
			"preview_host_tag", hosts.Tag,
			"preview_host_example", hosts.Preview("<id>", 3000),
			"image", image,
			"idle_threshold", idleThreshold.String(),
			"idle_interval", idleInterval.String(),
			"pressure_interval", pressureInterval.String(),
			"mem_headroom_pct", headroomPct,
			"mem_refuse_wakes_pct", refuseWakesPct,
			"mem_emergency_pct", emergencyPct,
			"wake_cost_mb", wakeCostMB,
			"wake_tcp_ready", wakeTCPReady.String(),
			"wake_grace", wakeGrace.String(),
			"poller_mode", pollerModeLabel(pollerRE),
		)
		err := httpSrv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	// Block until a terminating signal or server-error. SIGHUP is not
	// terminating — it re-reads the auth config (token rotation) and
	// the loop continues. SIGINT / SIGTERM break out to shutdown.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	for {
		select {
		case sig := <-sigCh:
			if sig == syscall.SIGHUP {
				log.Info("reload: SIGHUP — re-reading auth config", "env_file", envFile)
				if env, err := auth.LoadEnvFile(envFile); err != nil {
					log.Error("reload: read env file failed (keeping current config)",
						"err", err.Error())
				} else {
					nc := auth.ParseConfig(auth.MapGetter(env))
					authMw.Reload(nc)
					log.Info("reload: auth config reloaded",
						"api_tokens", len(nc.APITokens),
						"preview_secrets", len(nc.PreviewSecrets),
						"auth_disabled", nc.Disabled)
				}
				continue
			}
			log.Info("shutdown: signal received", "signal", sig.String())
		case err := <-errCh:
			log.Error("shutdown: server error", "err", err.Error())
		}
		break
	}

	gcancel()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	if egressSrv != nil {
		_ = egressSrv.Shutdown(shutCtx)
	}
	if err := httpSrv.Shutdown(shutCtx); err != nil {
		log.Error("shutdown: http server shutdown failed", "err", err.Error())
	}
}

// hostDispatch routes incoming requests to either the wake catch-all
// (when Host header matches the preview host shape) or the
// loopback API. Both share the same listener so the operator only
// has to wire one entry into Traefik's file provider.
func hostDispatch(w *wake.Handler, apiMux http.Handler, _ any) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if w != nil && w.HostMatchesPreview(r.Host) {
			w.ServeCatchAll(rw, r)
			return
		}
		apiMux.ServeHTTP(rw, r)
	})
}

func envDefault(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// envSplit returns the env var split by `sep`, trimming empty entries.
// Falls back to `def` (also split) if unset. Returns nil if the
// resulting list is empty (i.e. operator deliberately disabled).
func envSplit(k, def, sep string) []string {
	raw := os.Getenv(k)
	if raw == "" {
		raw = def
	}
	if raw == "" {
		return nil
	}
	parts := []string{}
	for _, p := range splitNonEmpty(raw, sep) {
		parts = append(parts, p)
	}
	return parts
}

func splitNonEmpty(s, sep string) []string {
	out := []string{}
	cur := ""
	for _, r := range s {
		if string(r) == sep {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func durationFromEnvSec(k string, dSec int) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		return time.Duration(dSec) * time.Second
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return time.Duration(dSec) * time.Second
	}
	if n < 0 {
		n = 0
	}
	return time.Duration(n) * time.Second
}

func floatFromEnv(k string, d float64) float64 {
	v := os.Getenv(k)
	if v == "" {
		return d
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return d
	}
	return f
}

func uintFromEnv(k string, d uint64) uint64 {
	v := os.Getenv(k)
	if v == "" {
		return d
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return d
	}
	return n
}

// splitList parses a comma/space separated env list, dropping blanks.
func splitList(v string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
		out = append(out, f)
	}
	return out
}

func intFromEnv(k string, d int) int {
	v := os.Getenv(k)
	if v == "" {
		return d
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return d
	}
	return n
}

// boolFromEnv parses a boolean env var. Accepts 1/true/yes/on (any
// case) as true and 0/false/no/off as false; anything else (including
// unset) returns the default.
func boolFromEnv(k string, d bool) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(k))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return d
	}
}

func pollerModeLabel(re *regexp.Regexp) string {
	if re == nil {
		return "fallback"
	}
	return "active"
}

// egressModeLabel is the safe egress mode string for GET /v1/settings. A nil
// manager (the OSS default) means no egress policy is enforced.
func egressModeLabel(m *egress.Manager) string {
	if m == nil {
		return "disabled"
	}
	return "enabled"
}

// buildVersion and buildCommit are stamped at build time via
//
//	-ldflags "-X main.buildVersion=… -X main.buildCommit=…"
//
// (see control-plane/Dockerfile + docker-compose build args, fed by
// the operator from `git describe`). When unset — e.g. a bare
// `go build` — buildIdent falls back to the module's VCS build info, then to
// dev/unknown. This is what `sandboxd version`, /version and /v1/settings report.
var (
	buildVersion string
	buildCommit  string
)

func buildIdent() (version, gitCommit string) {
	version = buildVersion
	gitCommit = buildCommit
	if version == "" || gitCommit == "" {
		if info, ok := debug.ReadBuildInfo(); ok {
			for _, s := range info.Settings {
				if s.Key == "vcs.revision" && s.Value != "" && gitCommit == "" {
					gitCommit = s.Value
				}
			}
			if info.Main.Version != "" && info.Main.Version != "(devel)" && version == "" {
				version = info.Main.Version
			}
		}
	}
	if version == "" {
		version = "dev"
	}
	if gitCommit == "" {
		gitCommit = "unknown"
	}
	if len(gitCommit) > 12 {
		gitCommit = gitCommit[:12]
	}
	return version, gitCommit
}

// legacyAware returns preferred unless it does not exist and the pre-rename
// path does — the "sandboxed" → "sandboxd" rename must never strand an
// existing install's files.
func legacyAware(preferred, legacy string) string {
	if _, err := os.Stat(preferred); err == nil {
		return preferred
	}
	if _, err := os.Stat(legacy); err == nil {
		return legacy
	}
	return preferred
}
