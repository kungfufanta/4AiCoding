package kube

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/docker"
)

// Labels the engine stamps on every object it owns. The sandbox-id
// label is the selector for the Service and the NetworkPolicy; the
// managed label drives ListByNamePrefix.
const (
	labelManaged = "sandboxd.io/managed"
	labelSandbox = "sandboxd.io/sandbox-id"
)

// dns1123 makes a k8s object name out of a docker container name:
// lowercase, ≤63 chars, [a-z0-9.-]. Sandbox names are "s-<ULID>" so
// lowercasing is the only transformation in practice; the truncation
// guard is for hostile input.
func dns1123(name string) string {
	n := strings.ToLower(name)
	n = regexp.MustCompile(`[^a-z0-9.-]+`).ReplaceAllString(n, "-")
	n = strings.Trim(n, "-.")
	if len(n) > 63 {
		n = n[:63]
	}
	if n == "" {
		n = "sandbox"
	}
	return n
}

// PodFor builds the sandbox Pod from a RunSpec (PLAN §8 baseline mapped
// onto Kubernetes): read-only rootfs, non-root uid:gid, ALL capabilities
// dropped, no privilege escalation, RuntimeDefault seccomp, workspace
// bind mount via hostPath, tmpfs mounts via emptyDir, resource limits
// from the spec, and NO service-account token (the sandbox must never
// reach the Kubernetes API).
func PodFor(spec docker.RunSpec, ns string) *Pod {
	name := dns1123(spec.Name)
	labels := map[string]string{labelManaged: "true", labelSandbox: name}
	for _, l := range spec.Labels {
		k, v, ok := strings.Cut(l, "=")
		if !ok || k == "" {
			continue
		}
		// Label keys/values are stricter than docker labels; keep what
		// fits, drop the rest (traefik labels are informational here —
		// routing on k8s goes through the Ingress).
		if labelKeyOK(k) && labelValueOK(v) {
			labels[k] = v
		}
	}

	c := Container{
		Name:            "sandbox",
		Image:           spec.Image,
		Command:         append([]string(nil), spec.Cmd...),
		SecurityContext: containerSecCtx(spec),
	}

	for _, e := range spec.Env {
		if k, v, ok := strings.Cut(e, "="); ok && k != "" {
			c.Env = append(c.Env, EnvVar{Name: k, Value: v})
		}
	}
	if spec.CPUs != "" {
		if q := cpuQuantity(spec.CPUs); q != "" {
			c.Resources = ensureRes(c.Resources)
			c.Resources.Limits["cpu"] = q
			c.Resources.Requests["cpu"] = q
		}
	}
	if q := memQuantity(spec.Memory); q != "" {
		c.Resources = ensureRes(c.Resources)
		c.Resources.Limits["memory"] = q
		c.Resources.Requests["memory"] = q
	}
	for _, p := range spec.Ports {
		if p > 0 && p < 65536 {
			c.Ports = append(c.Ports, ContainerPort{Name: fmt.Sprintf("p%d", p), ContainerPort: p, Protocol: "TCP"})
		}
	}

	pod := &Pod{
		APIVersion: "v1",
		Kind:       "Pod",
		Metadata:   ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Spec: PodSpec{
			Containers:                   []Container{c},
			RestartPolicy:                "Never",
			AutomountServiceAccountToken: boolPtr(false),
			EnableServiceLinks:           boolPtr(false),
			SecurityContext: &PodSecurityCtx{
				RunAsNonRoot:   boolPtr(true),
				SeccompProfile: &SeccompProfile{Type: "RuntimeDefault"},
			},
		},
	}
	if u := spec.User; u != "" {
		uid, gid, ok := splitUIDGID(u)
		if ok {
			pod.Spec.SecurityContext.RunAsUser = int64Ptr(uid)
			pod.Spec.SecurityContext.RunAsGroup = int64Ptr(gid)
			pod.Spec.SecurityContext.FSGroup = int64Ptr(gid)
		}
	}
	if h := dns1123(spec.Hostname); len(h) <= 63 {
		pod.Spec.Hostname = h
	}

	// Volumes: every "-v src:dst[:opts]" bind becomes a hostPath (the
	// executor owns the workspace tree on this node, exactly like the
	// docker engine's bind mount); every "--tmpfs dst:size=…" becomes an
	// emptyDir with that size.
	for i, v := range spec.Volumes {
		src, dst, opts := splitVolume(v)
		if src == "" || dst == "" {
			continue
		}
		vn := fmt.Sprintf("v%d", i)
		pod.Spec.Volumes = append(pod.Spec.Volumes, Volume{
			Name:     vn,
			HostPath: &HostPathVol{Path: src, Type: hostPathType(dst)},
		})
		pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts,
			VolumeMount{Name: vn, MountPath: dst, ReadOnly: strings.Contains(opts, "ro")})
	}
	for i, t := range spec.Tmpfs {
		dst, size := splitTmpfs(t)
		if dst == "" {
			continue
		}
		vn := fmt.Sprintf("tmp%d", i)
		vol := Volume{Name: vn, EmptyDir: &EmptyDirVol{}}
		if size != "" {
			vol.EmptyDir.SizeLimit = size
		}
		pod.Spec.Volumes = append(pod.Spec.Volumes, vol)
		pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts,
			VolumeMount{Name: vn, MountPath: dst})
	}
	return pod
}

func containerSecCtx(spec docker.RunSpec) *ContainerSecCtx {
	sc := &ContainerSecCtx{
		ReadOnlyRootFilesystem:   boolPtr(spec.ReadOnly),
		AllowPrivilegeEscalation: boolPtr(false),
		Privileged:               boolPtr(false),
		SeccompProfile:           &SeccompProfile{Type: "RuntimeDefault"},
		Capabilities:             &Capabilities{Drop: []string{"ALL"}},
	}
	if len(spec.CapDrop) > 0 {
		sc.Capabilities.Drop = append([]string(nil), spec.CapDrop...)
	}
	// Sandbox containers never add capabilities back; the executor pod
	// itself (a different deployment) may, but that is not this path.
	return sc
}

func ensureRes(r *Resources) *Resources {
	if r == nil {
		return &Resources{Limits: map[string]string{}, Requests: map[string]string{}}
	}
	if r.Limits == nil {
		r.Limits = map[string]string{}
	}
	if r.Requests == nil {
		r.Requests = map[string]string{}
	}
	return r
}

// cpuQuantity converts a docker --cpus value ("0.5", "1", "2.25") to a
// Kubernetes CPU quantity ("500m", "1", "2250m").
func cpuQuantity(s string) string {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || f <= 0 {
		return ""
	}
	if f == float64(int(f)) {
		return strconv.Itoa(int(f))
	}
	return fmt.Sprintf("%dm", int(f*1000))
}

var memRe = regexp.MustCompile(`^(\d+(?:\.\d+)?)\s*([bkmgt]?)$`)

// memQuantity converts a docker --memory value ("512m", "10g", "262144b")
// to a Kubernetes quantity. Docker's units are binary (1m = 1 MiB), so
// the mapping is b→Ki-ish? No: b→B with the exact byte count, k→Ki,
// m→Mi, g→Gi, t→Ti.
func memQuantity(s string) string {
	if s == "" {
		return ""
	}
	m := memRe.FindStringSubmatch(strings.TrimSpace(strings.ToLower(s)))
	if m == nil {
		return ""
	}
	f, err := strconv.ParseFloat(m[1], 64)
	if err != nil || f <= 0 {
		return ""
	}
	switch m[2] {
	case "", "b":
		return fmt.Sprintf("%d", int64(f))
	case "k":
		return fmt.Sprintf("%dKi", int64(f))
	case "m":
		return fmt.Sprintf("%dMi", int64(f))
	case "g":
		return fmt.Sprintf("%dGi", int64(f))
	case "t":
		return fmt.Sprintf("%dTi", int64(f))
	}
	return ""
}

// splitUIDGID parses "uid:gid" (or a bare "uid").
func splitUIDGID(u string) (uid, gid int64, ok bool) {
	parts := strings.SplitN(u, ":", 2)
	uid, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || uid < 0 {
		return 0, 0, false
	}
	if len(parts) == 1 {
		return uid, uid, true
	}
	gid, err = strconv.ParseInt(parts[1], 10, 64)
	if err != nil || gid < 0 {
		return 0, 0, false
	}
	return uid, gid, true
}

// splitVolume parses "-v" entries: "src:dst[:opts]". A mount with more
// than 3 colon-separated parts (none in sandboxd specs) is rejected by
// returning empties.
func splitVolume(v string) (src, dst, opts string) {
	parts := strings.Split(v, ":")
	if len(parts) == 2 {
		return parts[0], parts[1], ""
	}
	if len(parts) == 3 {
		return parts[0], parts[1], parts[2]
	}
	return "", "", ""
}

// splitTmpfs parses "--tmpfs dst:size=N" (dst alone = unbounded).
func splitTmpfs(t string) (dst, size string) {
	dst, size, _ = strings.Cut(t, ":")
	if size, ok := strings.CutPrefix(size, "size="); ok {
		return strings.TrimSpace(dst), memQuantityToK8s(size)
	}
	return strings.TrimSpace(dst), ""
}

// memQuantityToK8s maps docker tmpfs sizes (binary units) to k8s
// quantities; unknown formats pass through untouched.
func memQuantityToK8s(s string) string {
	m := memRe.FindStringSubmatch(strings.ToLower(strings.TrimSpace(s)))
	if m == nil {
		return s
	}
	f, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return s
	}
	switch m[2] {
	case "k":
		return fmt.Sprintf("%dKi", int64(f))
	case "m":
		return fmt.Sprintf("%dMi", int64(f))
	case "g":
		return fmt.Sprintf("%dGi", int64(f))
	case "t":
		return fmt.Sprintf("%dTi", int64(f))
	case "b", "":
		return fmt.Sprintf("%d", int64(f))
	}
	return s
}

// hostPathType: file-looking destinations get FileOrCreate, everything
// else DirectoryOrCreate (the executor provisions the workspace tree
// before Run; both types tolerate a pre-created path).
func hostPathType(dst string) string {
	last := dst
	if i := strings.LastIndexByte(dst, '/'); i >= 0 {
		last = dst[i+1:]
	}
	if strings.Contains(last, ".") {
		return "FileOrCreate"
	}
	return "DirectoryOrCreate"
}

// labelKeyOK guards a docker label key as a k8s label key: a name
// (optionally prefix/name) of alphanumerics, '-', '_', '.'.
var labelKeyRe = regexp.MustCompile(`^(?:[a-z0-9](?:[-a-z0-9.]{0,61}[a-z0-9])?/)?[a-z0-9A-Z](?:[-a-z0-9A-Z_.]{0,61}[a-z0-9A-Z])?$`)

func labelKeyOK(k string) bool { return len(k) <= 316 && labelKeyRe.MatchString(k) }

var labelValueRe = regexp.MustCompile(`^[a-z0-9A-Z](?:[-a-z0-9A-Z_.]{0,61}[a-z0-9A-Z])?$`)

func labelValueOK(v string) bool { return len(v) <= 63 && labelValueRe.MatchString(v) }

// ServiceFor exposes the spec's preview ports on a stable ClusterIP
// name (the pod name); preview routing hangs off it (Ingress) instead
// of Traefik-over-docker-DNS.
func ServiceFor(spec docker.RunSpec, ns string) *Service {
	name := dns1123(spec.Name)
	svc := &Service{
		APIVersion: "v1",
		Kind:       "Service",
		Metadata: ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{labelManaged: "true", labelSandbox: name},
		},
		Spec: ServiceSpec{
			Selector: map[string]string{labelSandbox: name},
			Type:     "ClusterIP",
		},
	}
	for _, p := range spec.Ports {
		if p > 0 && p < 65536 {
			svc.Spec.Ports = append(svc.Spec.Ports, ServicePort{
				Name: fmt.Sprintf("p%d", p), Port: p, TargetPort: p, Protocol: "TCP",
			})
		}
	}
	return svc
}

// NetworkPolicyFor is the k8s translation of egress deny-by-default
// (PLAN §8 #5): the sandbox's egress is denied except cluster DNS and
// any extra peers the operator configured (today: the egress-proxy
// pods, selected by cfg.ProxySelector — the HTTP(S)_PROXY env the spec
// already carries points sandboxes there, exactly as on docker).
func NetworkPolicyFor(spec docker.RunSpec, ns string, proxySelector map[string]string) *NetworkPolicy {
	name := dns1123(spec.Name)
	np := &NetworkPolicy{
		APIVersion: "networking.k8s.io/v1",
		Kind:       "NetworkPolicy",
		Metadata: ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{labelManaged: "true", labelSandbox: name},
		},
		Spec: NetworkPolicySpec{
			PodSelector: LabelSelector{MatchLabels: map[string]string{labelSandbox: name}},
			PolicyTypes: []string{"Egress"},
		},
	}
	// Cluster DNS (kube-dns in kube-system) — a sandbox that cannot
	// resolve anything is broken, not contained. Name resolution is not
	// network access: the NetworkPolicy still denies every destination
	// except the peers below.
	dnsPort := 53
	np.Spec.Egress = append(np.Spec.Egress, NetworkPolicyEgressRule{
		Ports: []NetworkPolicyPort{
			{Protocol: strPtr("UDP"), Port: intPtr(dnsPort)},
			{Protocol: strPtr("TCP"), Port: intPtr(dnsPort)},
		},
		To: []NetworkPolicyPeer{{
			NamespaceSelector: &LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}},
			PodSelector:       &LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}},
		}},
	})
	if len(proxySelector) > 0 {
		np.Spec.Egress = append(np.Spec.Egress, NetworkPolicyEgressRule{
			To: []NetworkPolicyPeer{{PodSelector: &LabelSelector{MatchLabels: proxySelector}}},
		})
	}
	return np
}

// IngressFor routes preview hosts to the sandbox's Service. rule hosts
// come from the engine's previewHost function (the same hosts Traefik
// would serve on docker), one rule per port.
func IngressFor(spec docker.RunSpec, ns, ingressClass string, hostFor func(port int) string) *Ingress {
	if hostFor == nil || len(spec.Ports) == 0 {
		return nil
	}
	name := dns1123(spec.Name)
	ig := &Ingress{
		APIVersion: "networking.k8s.io/v1",
		Kind:       "Ingress",
		Metadata: ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{labelManaged: "true", labelSandbox: name},
		},
		Spec: IngressSpec{IngressClassName: ingressClass},
	}
	for _, p := range spec.Ports {
		if p <= 0 || p >= 65536 {
			continue
		}
		host := hostFor(p)
		if host == "" {
			continue
		}
		ig.Spec.Rules = append(ig.Spec.Rules, IngressRule{
			Host: host,
			HTTP: &IngressHTTP{Paths: []IngressPath{{
				Path:     "/",
				PathType: "Prefix",
				Backend: IngressBackend{Service: &IngressServiceBackend{
					Name: name, Port: IngressServicePort{Number: p},
				}},
			}}},
		})
	}
	if len(ig.Spec.Rules) == 0 {
		return nil
	}
	return ig
}
