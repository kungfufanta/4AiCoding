package api

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/sandboxspec"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

// Limits bounds a sandbox spec. Zero values mean "no extra bound".
type Limits struct {
	// ImageAllowlist lists the images a sandbox may name besides the instance
	// image. Empty = only the instance image (PLAN §4 policy rule 1).
	ImageAllowlist []string
	MaxCPUs        float64 // clamp target for resources.cpus; 0 = no clamp
	MaxMemoryMB    int     // clamp target for resources.memory_mb; 0 = no clamp
	MaxPorts       int     // max preview ports per sandbox; 0 = no limit
}

// imageRefRE is a conservative docker image reference: registry/path:tag or
// @digest, no leading '-' (it must never parse as a docker flag).
var imageRefRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]*(:[A-Za-z0-9_][A-Za-z0-9._-]{0,127})?(@sha256:[a-f0-9]{64})?$`)

// envKeyRE is a POSIX-ish env var name.
var envKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

const (
	maxCommandArgs = 64
	maxArgLen      = 4096
	maxEnvVars     = 128
)

// specEnv returns the container environment for this server.
func (s *Server) specEnv() sandboxspec.Env {
	e := s.SpecEnv
	if e.Image == "" {
		e.Image = s.Image
		e.Network = s.Network
		e.Userns = s.Userns
		e.Runtime = s.Runtime
		e.DNSResolvConf = s.DNSResolvConf
		e.PreviewDomain = s.PreviewDomain
		e.PreviewHostStyle = s.PreviewHostStyle
		e.PreviewHostTag = s.PreviewHostTag
		e.PreviewEntrypoint = s.PreviewEntrypoint
		e.PreviewTLS = s.PreviewTLS
	}
	return e
}

// specInput is the user-supplied part of a sandbox spec.
type specInput struct {
	Image    string
	Command  []string
	Env      map[string]string
	CPUs     float64
	MemoryMB int
}

// buildSpec validates a spec request and returns the stored spec, with
// resources clamped (never rejected) to the instance limits.
func (s *Server) buildSpec(in specInput) (store.Spec, error) {
	var sp store.Spec
	if in.Image != "" && in.Image != s.Image {
		if !imageRefRE.MatchString(in.Image) {
			return sp, fmt.Errorf("invalid image reference %q", in.Image)
		}
		if !contains(s.Limits.ImageAllowlist, in.Image) {
			return sp, fmt.Errorf("image %q is not allowed on this instance (allowed: the instance image%s)",
				in.Image, allowListSuffix(s.Limits.ImageAllowlist))
		}
		sp.Image = in.Image
	}
	if len(in.Command) > maxCommandArgs {
		return sp, fmt.Errorf("command has more than %d arguments", maxCommandArgs)
	}
	for _, a := range in.Command {
		if len(a) > maxArgLen || strings.ContainsRune(a, 0) {
			return sp, fmt.Errorf("invalid command argument")
		}
	}
	sp.Command = in.Command
	if len(in.Env) > maxEnvVars {
		return sp, fmt.Errorf("more than %d env vars", maxEnvVars)
	}
	for k, v := range in.Env {
		if !envKeyRE.MatchString(k) || strings.ContainsAny(v, "\x00\n\r") {
			return sp, fmt.Errorf("invalid env var name/value: %s", k)
		}
	}
	if len(in.Env) > 0 {
		sp.Env = in.Env
	}
	if in.CPUs < 0 || in.MemoryMB < 0 {
		return sp, fmt.Errorf("resources must not be negative")
	}
	sp.CPUs = in.CPUs
	if s.Limits.MaxCPUs > 0 && sp.CPUs > s.Limits.MaxCPUs {
		sp.CPUs = s.Limits.MaxCPUs
	}
	sp.MemoryMB = in.MemoryMB
	if sp.MemoryMB > 0 && sp.MemoryMB < 64 {
		sp.MemoryMB = 64 // below this docker refuses to start most images
	}
	if s.Limits.MaxMemoryMB > 0 && sp.MemoryMB > s.Limits.MaxMemoryMB {
		sp.MemoryMB = s.Limits.MaxMemoryMB
	}
	return sp, nil
}

// checkPorts validates a preview port list.
func (s *Server) checkPorts(ports []int) error {
	seen := map[int]bool{}
	for _, p := range ports {
		if p < 1 || p > 65535 {
			return fmt.Errorf("port out of range: %d", p)
		}
		if seen[p] {
			return fmt.Errorf("duplicate port: %d", p)
		}
		seen[p] = true
	}
	if s.Limits.MaxPorts > 0 && len(ports) > s.Limits.MaxPorts {
		return fmt.Errorf("at most %d ports per sandbox", s.Limits.MaxPorts)
	}
	return nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func allowListSuffix(list []string) string {
	if len(list) == 0 {
		return ""
	}
	return ", " + strings.Join(list, ", ")
}
