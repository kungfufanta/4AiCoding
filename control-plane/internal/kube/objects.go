package kube

// Minimal Kubernetes object shapes (only the fields we marshal). See
// the package comment: unknown fields round-trip as nothing — we always
// PUT/POST what we built ourselves.

// ObjectMeta is the metadata subset.
type ObjectMeta struct {
	Name        string            `json:"name,omitempty"`
	Namespace   string            `json:"namespace,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// --- core/v1 Pod ---------------------------------------------------------

type Pod struct {
	APIVersion string     `json:"apiVersion,omitempty"`
	Kind       string     `json:"kind,omitempty"`
	Metadata   ObjectMeta `json:"metadata"`
	Spec       PodSpec    `json:"spec"`
	Status     PodStatus  `json:"status,omitempty"`
}

type PodSpec struct {
	Containers                   []Container       `json:"containers"`
	RestartPolicy                string            `json:"restartPolicy,omitempty"`
	AutomountServiceAccountToken *bool             `json:"automountServiceAccountToken,omitempty"`
	EnableServiceLinks           *bool             `json:"enableServiceLinks,omitempty"`
	SecurityContext              *PodSecurityCtx   `json:"securityContext,omitempty"`
	Volumes                      []Volume          `json:"volumes,omitempty"`
	Hostname                     string            `json:"hostname,omitempty"`
	NodeSelector                 map[string]string `json:"nodeSelector,omitempty"`
}

type PodSecurityCtx struct {
	RunAsNonRoot   *bool           `json:"runAsNonRoot,omitempty"`
	RunAsUser      *int64          `json:"runAsUser,omitempty"`
	RunAsGroup     *int64          `json:"runAsGroup,omitempty"`
	FSGroup        *int64          `json:"fsGroup,omitempty"`
	SeccompProfile *SeccompProfile `json:"seccompProfile,omitempty"`
}

type SeccompProfile struct {
	Type string `json:"type"`
}

type Container struct {
	Name            string           `json:"name"`
	Image           string           `json:"image"`
	Command         []string         `json:"command,omitempty"`
	Env             []EnvVar         `json:"env,omitempty"`
	Ports           []ContainerPort  `json:"ports,omitempty"`
	SecurityContext *ContainerSecCtx `json:"securityContext,omitempty"`
	Resources       *Resources       `json:"resources,omitempty"`
	VolumeMounts    []VolumeMount    `json:"volumeMounts,omitempty"`
	WorkingDir      string           `json:"workingDir,omitempty"`
}

type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value,omitempty"`
}

type ContainerPort struct {
	Name          string `json:"name,omitempty"`
	ContainerPort int    `json:"containerPort"`
	Protocol      string `json:"protocol,omitempty"`
}

type ContainerSecCtx struct {
	RunAsNonRoot             *bool           `json:"runAsNonRoot,omitempty"`
	RunAsUser                *int64          `json:"runAsUser,omitempty"`
	RunAsGroup               *int64          `json:"runAsGroup,omitempty"`
	ReadOnlyRootFilesystem   *bool           `json:"readOnlyRootFilesystem,omitempty"`
	AllowPrivilegeEscalation *bool           `json:"allowPrivilegeEscalation,omitempty"`
	Privileged               *bool           `json:"privileged,omitempty"`
	Capabilities             *Capabilities   `json:"capabilities,omitempty"`
	SeccompProfile           *SeccompProfile `json:"seccompProfile,omitempty"`
}

type Capabilities struct {
	Drop []string `json:"drop,omitempty"`
	Add  []string `json:"add,omitempty"`
}

type Resources struct {
	Limits   map[string]string `json:"limits,omitempty"`
	Requests map[string]string `json:"requests,omitempty"`
}

type Volume struct {
	Name     string       `json:"name"`
	HostPath *HostPathVol `json:"hostPath,omitempty"`
	EmptyDir *EmptyDirVol `json:"emptyDir,omitempty"`
}

type HostPathVol struct {
	Path string `json:"path"`
	Type string `json:"type,omitempty"`
}

type EmptyDirVol struct {
	SizeLimit string `json:"sizeLimit,omitempty"`
}

type VolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

type PodStatus struct {
	Phase             string            `json:"phase,omitempty"`
	PodIP             string            `json:"podIP,omitempty"`
	HostIP            string            `json:"hostIP,omitempty"`
	ContainerStatuses []ContainerStatus `json:"containerStatuses,omitempty"`
}

type ContainerStatus struct {
	Name  string         `json:"name,omitempty"`
	State ContainerState `json:"state,omitempty"`
}

type ContainerState struct {
	Running *struct {
		StartedAt string `json:"startedAt,omitempty"`
	} `json:"running,omitempty"`
	Terminated *struct {
		ExitCode int    `json:"exitCode,omitempty"`
		Reason   string `json:"reason,omitempty"`
	} `json:"terminated,omitempty"`
	Waiting *struct {
		Reason string `json:"reason,omitempty"`
	} `json:"waiting,omitempty"`
}

// --- core/v1 Service -------------------------------------------------------

type Service struct {
	APIVersion string      `json:"apiVersion,omitempty"`
	Kind       string      `json:"kind,omitempty"`
	Metadata   ObjectMeta  `json:"metadata"`
	Spec       ServiceSpec `json:"spec"`
}

type ServiceSpec struct {
	Selector map[string]string `json:"selector"`
	Type     string            `json:"type,omitempty"`
	Ports    []ServicePort     `json:"ports"`
}

type ServicePort struct {
	Name       string `json:"name,omitempty"`
	Port       int    `json:"port"`
	TargetPort int    `json:"targetPort"`
	Protocol   string `json:"protocol,omitempty"`
}

// --- networking.k8s.io/v1 NetworkPolicy ------------------------------------

type NetworkPolicy struct {
	APIVersion string            `json:"apiVersion,omitempty"`
	Kind       string            `json:"kind,omitempty"`
	Metadata   ObjectMeta        `json:"metadata"`
	Spec       NetworkPolicySpec `json:"spec"`
}

type NetworkPolicySpec struct {
	PodSelector LabelSelector              `json:"podSelector"`
	PolicyTypes []string                   `json:"policyTypes,omitempty"`
	Ingress     []NetworkPolicyIngressRule `json:"ingress,omitempty"`
	Egress      []NetworkPolicyEgressRule  `json:"egress,omitempty"`
}

type LabelSelector struct {
	MatchLabels map[string]string `json:"matchLabels,omitempty"`
}

type NetworkPolicyIngressRule struct {
	From []NetworkPolicyPeer `json:"from,omitempty"`
}

type NetworkPolicyEgressRule struct {
	Ports []NetworkPolicyPort `json:"ports,omitempty"`
	To    []NetworkPolicyPeer `json:"to,omitempty"`
}

type NetworkPolicyPort struct {
	Protocol *string `json:"protocol,omitempty"`
	Port     *int    `json:"port,omitempty"`
}

type NetworkPolicyPeer struct {
	PodSelector       *LabelSelector `json:"podSelector,omitempty"`
	NamespaceSelector *LabelSelector `json:"namespaceSelector,omitempty"`
	IPBlock           *IPBlock       `json:"ipBlock,omitempty"`
}

type IPBlock struct {
	CIDR   string   `json:"cidr"`
	Except []string `json:"except,omitempty"`
}

// --- networking.k8s.io/v1 Ingress -------------------------------------------

type Ingress struct {
	APIVersion string      `json:"apiVersion,omitempty"`
	Kind       string      `json:"kind,omitempty"`
	Metadata   ObjectMeta  `json:"metadata"`
	Spec       IngressSpec `json:"spec"`
}

type IngressSpec struct {
	IngressClassName string        `json:"ingressClassName,omitempty"`
	Rules            []IngressRule `json:"rules,omitempty"`
}

type IngressRule struct {
	Host string       `json:"host,omitempty"`
	HTTP *IngressHTTP `json:"http,omitempty"`
}

type IngressHTTP struct {
	Paths []IngressPath `json:"paths"`
}

type IngressPath struct {
	Path     string         `json:"path"`
	PathType string         `json:"pathType"`
	Backend  IngressBackend `json:"backend"`
}

type IngressBackend struct {
	Service *IngressServiceBackend `json:"service,omitempty"`
}

type IngressServiceBackend struct {
	Name string             `json:"name"`
	Port IngressServicePort `json:"port"`
}

type IngressServicePort struct {
	Number int `json:"number,omitempty"`
}

// --- misc --------------------------------------------------------------------

// Version is GET /version.
type Version struct {
	Major      string `json:"major"`
	Minor      string `json:"minor"`
	GitVersion string `json:"gitVersion"`
}

// PodList is the envelope of a pod LIST.
type PodList struct {
	Items []Pod `json:"items"`
}

// ptr helpers.
func boolPtr(b bool) *bool    { return &b }
func int64Ptr(i int64) *int64 { return &i }
func intPtr(i int) *int       { return &i }
func strPtr(s string) *string { return &s }
