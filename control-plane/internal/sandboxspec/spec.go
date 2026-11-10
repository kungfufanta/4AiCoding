// Package sandboxspec builds the docker RunSpec for a sandbox from its stored
// row, so a container can be (re)created from the database alone.
//
// Why this exists: when the instance image changes, a container created from
// the OLD image keeps running it until it is recreated, so long-lived sandboxes
// would never pick up the new image. Recreating is safe because ALL durable
// state lives in the workspace bind mount, never in the container's writable
// layer (the rootfs is read-only).
package sandboxspec

import (
	"sort"
	"strconv"
	"strings"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/docker"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/previewhost"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/traefik"
)

// DefaultUser is the uid:gid every sandbox process runs as, whatever the
// image's USER says (PLAN §8 #3: never root). The workspace is owned by it.
const DefaultUser = "1000:1000"

// DefaultMemoryMB is the hard memory limit when neither the spec nor the
// instance sets one.
const DefaultMemoryMB = 10240

// Env carries the instance-level settings a sandbox container needs. These
// mirror the values the create path passes; keeping them in one struct lets
// the recreate path produce an identical container.
type Env struct {
	Image             string // current base image (the whole point: may be newer)
	Network           string
	Userns            string
	Runtime           string // "" = docker default, "runsc" = gVisor
	DNSResolvConf     string // gVisor: host resolv.conf to bind-mount
	PreviewDomain     string
	PreviewHostStyle  string // previewhost.StyleNested (default) or StyleFlat
	PreviewHostTag    string // optional instance tag, flat style only
	PreviewEntrypoint string
	PreviewTLS        bool
	User              string // "" = DefaultUser
	MemoryMB          int    // instance default hard limit; 0 = DefaultMemoryMB
	// EgressProxy is the forward proxy URL every sandbox must use for
	// outbound HTTP(S) (PLAN §8 #5); "" = not injected.
	EgressProxy string
}

// proxyEnv is what Build injects when an egress proxy is configured. Both
// spellings, since tools disagree on which they read.
func proxyEnv(url string) []string {
	if url == "" {
		return nil
	}
	return []string{
		"HTTP_PROXY=" + url, "HTTPS_PROXY=" + url,
		"http_proxy=" + url, "https_proxy=" + url,
		"NO_PROXY=localhost,127.0.0.1,::1", "no_proxy=localhost,127.0.0.1,::1",
	}
}

// Build returns the RunSpec for `sb`. Both the create path and the
// recreate (wake) path use it, so a sandbox always gets the same container:
// read-only rootfs, all capabilities dropped, no-new-privileges, a fixed
// non-root user, tmpfs /tmp, ONLY the workspace bind mount (never the docker
// socket or another host path), ONLY the env declared in its spec (never the
// control plane's own env), and the Traefik preview labels.
func Build(sb *store.Sandbox, e Env) docker.RunSpec {
	name := "s-" + sb.ID

	env := make([]string, 0, len(sb.Spec.Env)+6)
	for k, v := range sb.Spec.Env {
		if e.EgressProxy != "" && isProxyVar(k) {
			continue // the instance proxy cannot be overridden by the spec
		}
		env = append(env, k+"="+v)
	}
	env = append(env, proxyEnv(e.EgressProxy)...)
	sort.Strings(env) // deterministic flags (tests, diffing)

	volumes := []string{sb.WorkspaceMnt + ":/home/sandbox"}
	if e.DNSResolvConf != "" {
		volumes = append(volumes, e.DNSResolvConf+":/etc/resolv.conf:ro")
	}

	// Preview routers: the stored ports, ensuring the resolved web port is
	// present (mirrors ensurePort in the create path). A port-less sandbox
	// stays preview-less.
	ports := append([]int(nil), sb.Ports...)
	if len(ports) > 0 {
		web := 3000
		if sb.WebPort.Valid && sb.WebPort.Int64 > 0 {
			web = int(sb.WebPort.Int64)
		}
		found := false
		for _, p := range ports {
			if p == web {
				found = true
				break
			}
		}
		if !found {
			ports = append(ports, web)
		}
	}
	visibility := sb.Visibility
	if visibility == "" {
		visibility = "private"
	}
	labels := traefik.LabelsFor(e.Hosts(), sb.ID, ports, visibility, e.PreviewEntrypoint, e.PreviewTLS)

	user := e.User
	if user == "" {
		user = DefaultUser
	}
	mem := sb.Spec.MemoryMB
	if mem <= 0 {
		mem = e.MemoryMB
	}
	if mem <= 0 {
		mem = DefaultMemoryMB
	}
	memStr := strconv.Itoa(mem) + "m"
	var cpus string
	if sb.Spec.CPUs > 0 {
		cpus = strconv.FormatFloat(sb.Spec.CPUs, 'f', -1, 64)
	}

	return docker.RunSpec{
		Name:        name,
		Hostname:    name,
		Network:     e.Network,
		Userns:      e.Userns,
		Runtime:     e.Runtime,
		ReadOnly:    true,
		CapDrop:     []string{"ALL"},
		SecurityOpt: []string{"no-new-privileges"},
		CPUShares:   100,
		CPUs:        cpus,
		Memory:      memStr,
		MemorySwap:  memStr,
		PidsLimit:   1024,
		Ulimits:     []string{"nofile=65536:65536"},
		Tmpfs:       []string{"/tmp:size=512m", "/var/tmp:size=128m"},
		Env:         env,
		Volumes:     volumes,
		Labels:      labels,
		User:        user,
		Image:       DesiredImage(sb, e.Image),
		Cmd:         append([]string(nil), sb.Spec.Command...),
	}
}

// DesiredImage is the image a sandbox should run: its own image when the
// spec pins one, otherwise the instance image (which it then follows across
// image upgrades).
func DesiredImage(sb *store.Sandbox, instanceImage string) string {
	if sb.Spec.Image != "" {
		return sb.Spec.Image
	}
	return instanceImage
}

// NeedsRecreate reports whether a container should be replaced before start:
// it was built from a different image than the one the instance now runs.
// After an image change this is what moves old sandboxes onto the new image.
func NeedsRecreate(containerImage, currentImage string) bool {
	return containerImage != "" && currentImage != "" && containerImage != currentImage
}

// Hosts returns the preview hostname scheme for this environment.
func (e Env) Hosts() previewhost.Scheme {
	return previewhost.Scheme{Domain: e.PreviewDomain, Style: e.PreviewHostStyle, Tag: e.PreviewHostTag}
}

// PreviewLabelsStale reports whether a container's Traefik preview routers
// were generated for a different PREVIEW_DOMAIN (or host style / tag) than
// the instance uses now.
// Labels are baked in at create time; without this check, changing the
// domain (new domain, new IP, moved behind a proxy) leaves every existing
// sandbox answering only to the OLD host, and its new preview URL falls
// through to the wake page forever. Containers with no preview routers at
// all (e.g. a worker) are never stale.
func PreviewLabelsStale(labels map[string]string, hs previewhost.Scheme) bool {
	if hs.Domain == "" {
		return false
	}
	want := hs.Suffix()
	sawRouter := false
	for k, v := range labels {
		if !strings.HasPrefix(k, "traefik.http.routers.") || !strings.HasSuffix(k, ".rule") {
			continue
		}
		sawRouter = true
		if strings.Contains(v, want+"`") || strings.Contains(v, want+":") {
			return false // at least one router already targets the current domain
		}
	}
	return sawRouter
}

func isProxyVar(k string) bool {
	switch strings.ToUpper(k) {
	case "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY":
		return true
	}
	return false
}
