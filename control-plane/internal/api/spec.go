package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/policy"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/sandboxspec"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

// Limits are the env-configured bounds (SANDBOXD_IMAGE_ALLOWLIST, ...).
// They seed the admin policy on an install that has none stored yet.
type Limits struct {
	ImageAllowlist []string
	MaxCPUs        float64
	MaxMemoryMB    int
	MaxPorts       int
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
	policyKey      = "policy"
)

// policyCache holds the decoded policy; invalidated on PUT /v1/policy.
type policyCache struct {
	mu  sync.Mutex
	cur *policy.Policy
}

// defaultPolicy is the policy derived from the env Limits.
func (s *Server) defaultPolicy() policy.Policy {
	p := policy.Default()
	if len(s.Limits.ImageAllowlist) > 0 {
		p.Images.Allowed = append([]string(nil), s.Limits.ImageAllowlist...)
	}
	if s.Limits.MaxCPUs > 0 {
		p.Resources.MaxCPUs = s.Limits.MaxCPUs
	}
	if s.Limits.MaxMemoryMB > 0 {
		p.Resources.MaxMemoryMB = s.Limits.MaxMemoryMB
	}
	if s.Limits.MaxPorts > 0 {
		p.Resources.MaxPortsPerSandbox = s.Limits.MaxPorts
	}
	return p
}

// policy returns the current admin policy: the stored one, else the default.
func (s *Server) policy(ctx context.Context) (policy.Policy, error) {
	s.policyCache.mu.Lock()
	defer s.policyCache.mu.Unlock()
	if s.policyCache.cur != nil {
		return *s.policyCache.cur, nil
	}
	p := s.defaultPolicy()
	if s.Store != nil {
		raw, err := s.Store.GetConfig(ctx, policyKey)
		switch {
		case err == nil:
			var stored policy.Policy
			if err := json.Unmarshal([]byte(raw), &stored); err != nil {
				return p, fmt.Errorf("stored policy is corrupt: %w", err)
			}
			if err := stored.Validate(); err != nil {
				return p, fmt.Errorf("stored policy is invalid: %w", err)
			}
			p = stored
		case errors.Is(err, store.ErrNotFound):
		default:
			return p, err
		}
	}
	s.policyCache.cur = &p
	return p, nil
}

// savePolicy validates, stores and caches p.
func (s *Server) savePolicy(ctx context.Context, p policy.Policy) (policy.Policy, error) {
	if err := p.Validate(); err != nil {
		return p, err
	}
	b, err := json.Marshal(p)
	if err != nil {
		return p, err
	}
	if err := s.Store.SetConfig(ctx, policyKey, string(b)); err != nil {
		return p, err
	}
	s.policyCache.mu.Lock()
	s.policyCache.cur = &p
	s.policyCache.mu.Unlock()
	return p, nil
}

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
	Image       string
	Command     []string
	Env         map[string]string
	CPUs        float64
	MemoryMB    int
	Ports       []int
	Egress      []string
	EgressMode  string
	EgressDeny  []string
	KeepAlive   bool
	IdleTimeout int
}

// policyError is a request the policy refuses (400), or one it sends for
// approval (202, M3c).
type policyError struct {
	d policy.Decision
}

func (e *policyError) Error() string { return e.d.Reason }

// buildSpec validates a spec request (syntax here, rules in the PDP) and
// returns the spec to store with resources and timeouts clamped.
func (s *Server) buildSpec(ctx context.Context, in specInput) (store.Spec, policy.Decision, error) {
	var sp store.Spec
	if in.Image == s.Image {
		in.Image = "" // naming the instance image = follow it
	}
	if in.Image != "" && !imageRefRE.MatchString(in.Image) {
		return sp, policy.Decision{}, fmt.Errorf("invalid image reference %q", in.Image)
	}
	if len(in.Command) > maxCommandArgs {
		return sp, policy.Decision{}, fmt.Errorf("command has more than %d arguments", maxCommandArgs)
	}
	for _, a := range in.Command {
		if len(a) > maxArgLen || strings.ContainsRune(a, 0) {
			return sp, policy.Decision{}, fmt.Errorf("invalid command argument")
		}
	}
	if len(in.Env) > maxEnvVars {
		return sp, policy.Decision{}, fmt.Errorf("more than %d env vars", maxEnvVars)
	}
	for k, v := range in.Env {
		if !envKeyRE.MatchString(k) || strings.ContainsAny(v, "\x00\n\r") {
			return sp, policy.Decision{}, fmt.Errorf("invalid env var name/value: %s", k)
		}
	}
	p, err := s.policy(ctx)
	if err != nil {
		return sp, policy.Decision{}, err
	}
	d := policy.Decide(p, policy.Request{
		Action: policy.ActionCreate, InstanceImage: s.Image, Image: in.Image, Ports: in.Ports,
		CPUs: in.CPUs, MemoryMB: in.MemoryMB, Egress: in.Egress, KeepAlive: in.KeepAlive, IdleTimeout: in.IdleTimeout,
		EgressMode: in.EgressMode, EgressDeny: in.EgressDeny,
	})
	if !d.Allow {
		return sp, d, &policyError{d: d}
	}
	sp.Image = in.Image
	sp.Command = in.Command
	if len(in.Env) > 0 {
		sp.Env = in.Env
	}
	sp.CPUs = d.EffectiveCPUs
	sp.MemoryMB = d.EffectiveMemoryMB
	sp.IdleTimeoutSecs = d.EffectiveIdleTimeout
	sp.Egress = d.Egress
	sp.EgressDeny = d.EgressDeny
	if in.EgressMode != "" {
		// Keep "" for a create that never chose a mode (= allowlist).
		sp.EgressMode = d.EgressMode
	}
	return sp, d, nil
}

// checkPorts validates a preview port list against the policy.
func (s *Server) checkPorts(ctx context.Context, ports []int) error {
	p, err := s.policy(ctx)
	if err != nil {
		return err
	}
	if d := policy.Decide(p, policy.Request{Action: policy.ActionPorts, Ports: ports}); !d.Allow {
		return &policyError{d: d}
	}
	return nil
}
