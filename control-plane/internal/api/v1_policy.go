// v1_policy.go — the admin policy (GET/PUT /v1/policy and its sections) and
// the per-sandbox lifecycle, keep-alive and egress routes that the policy
// decision point (internal/policy) gates.
package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/audit"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/policy"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

// writePolicyErr maps a buildSpec/checkPorts error to the v1 envelope.
func writePolicyErr(w http.ResponseWriter, err error) {
	var pe *policyError
	if errors.As(err, &pe) {
		code := "policy_denied"
		if pe.d.NeedsApproval != "" {
			code = "approval_required"
		}
		writeJSON(w, http.StatusForbidden, map[string]any{"error": map[string]any{
			"code": code, "message": pe.d.Reason, "retryable": false, "needs_approval": pe.d.NeedsApproval,
		}})
		return
	}
	writeV1Err(w, http.StatusBadRequest, "invalid_request", err.Error())
}

// --- GET /v1/policy -------------------------------------------------

func (s *Server) v1GetPolicy(w http.ResponseWriter, r *http.Request) {
	p, err := s.policy(r.Context())
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// --- PUT /v1/policy -------------------------------------------------

func (s *Server) v1PutPolicy(w http.ResponseWriter, r *http.Request) {
	var next policy.Policy
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&next); err != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "invalid json: "+err.Error())
		return
	}
	s.applyPolicy(w, r, next)
}

// policySections maps /v1/policy/{section} to the field it reads/writes.
var policySections = map[string]func(p *policy.Policy) any{
	"images":    func(p *policy.Policy) any { return &p.Images },
	"ports":     func(p *policy.Policy) any { return &p.Ports },
	"resources": func(p *policy.Policy) any { return &p.Resources },
	"egress":    func(p *policy.Policy) any { return &p.Egress },
	"lifecycle": func(p *policy.Policy) any { return &p.Lifecycle },
	"approvals": func(p *policy.Policy) any { return &p.Approvals },
}

// --- GET /v1/policy/{section} ---------------------------------------

func (s *Server) v1GetPolicySection(w http.ResponseWriter, r *http.Request) {
	field, ok := policySections[r.PathValue("section")]
	if !ok {
		writeV1Err(w, http.StatusNotFound, "not_found", "no such policy section")
		return
	}
	p, err := s.policy(r.Context())
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, field(&p))
}

// --- PUT /v1/policy/{section} ---------------------------------------

func (s *Server) v1PutPolicySection(w http.ResponseWriter, r *http.Request) {
	field, ok := policySections[r.PathValue("section")]
	if !ok {
		writeV1Err(w, http.StatusNotFound, "not_found", "no such policy section")
		return
	}
	cur, err := s.policy(r.Context())
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	next := cur
	next.Images.Allowed = append([]string(nil), cur.Images.Allowed...)
	next.Ports.Ranges = append([]policy.PortRange(nil), cur.Ports.Ranges...)
	next.Egress.Allow = append([]string(nil), cur.Egress.Allow...)
	next.Approvals = append([]string(nil), cur.Approvals...)
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(field(&next)); err != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "invalid json: "+err.Error())
		return
	}
	s.applyPolicy(w, r, next)
}

// applyPolicy validates and stores a new policy (or queues it for approval
// when the current policy gates policy changes, M3c).
func (s *Server) applyPolicy(w http.ResponseWriter, r *http.Request, next policy.Policy) {
	if err := next.Validate(); err != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	cur, err := s.policy(r.Context())
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if cur.RequiresApproval(policy.ApprovePolicyUpdate) {
		if s.requestApproval(w, r, policy.ApprovePolicyUpdate, "policy", next) {
			return
		}
	}
	saved, err := s.savePolicy(r.Context(), next)
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.auditAction(r, audit.Entry{Action: "policy.update", Target: "policy",
		Detail: map[string]any{"from": cur, "to": saved}})
	writeJSON(w, http.StatusOK, saved)
}

// --- GET/PATCH /v1/sandboxes/{id}/lifecycle -------------------------

func (s *Server) v1GetLifecycle(w http.ResponseWriter, r *http.Request) {
	sb, ok := s.v1Lookup(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.sandboxLifecycle(sb))
}

func (s *Server) sandboxLifecycle(sb *store.Sandbox) map[string]any {
	return map[string]any{
		"idle_timeout_secs": sb.Spec.IdleTimeoutSecs,
		"keep_alive":        sb.IdlePolicy == "always_on",
	}
}

// v1PatchLifecycle {idle_timeout_secs?, keep_alive?}
func (s *Server) v1PatchLifecycle(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IdleTimeoutSecs *int  `json:"idle_timeout_secs"`
		KeepAlive       *bool `json:"keep_alive"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8*1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "invalid json: "+err.Error())
		return
	}
	s.updateLifecycle(w, r, body.IdleTimeoutSecs, body.KeepAlive)
}

// POST /v1/sandboxes/{id}/keep-alive and DELETE: toggle keep-alive.
func (s *Server) v1KeepAliveOn(w http.ResponseWriter, r *http.Request) {
	on := true
	s.updateLifecycle(w, r, nil, &on)
}

func (s *Server) v1KeepAliveOff(w http.ResponseWriter, r *http.Request) {
	off := false
	s.updateLifecycle(w, r, nil, &off)
}

func (s *Server) updateLifecycle(w http.ResponseWriter, r *http.Request, idle *int, keep *bool) {
	id := r.PathValue("id")
	sb, ok := s.v1Lookup(w, r, id)
	if !ok {
		return
	}
	req := policy.Request{Action: policy.ActionLifecycle,
		IdleTimeout: sb.Spec.IdleTimeoutSecs, KeepAlive: sb.IdlePolicy == "always_on"}
	if idle != nil {
		req.IdleTimeout = *idle
	}
	if keep != nil {
		req.KeepAlive = *keep
	}
	p, err := s.policy(r.Context())
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	// Turning keep-alive OFF is always allowed, even if policy now forbids it.
	check := req
	if !req.KeepAlive {
		check.KeepAlive = false
	}
	d := policy.Decide(p, check)
	if !d.Allow {
		writePolicyErr(w, &policyError{d: d})
		return
	}
	sp := sb.Spec
	sp.IdleTimeoutSecs = d.EffectiveIdleTimeout
	if err := s.Store.UpdateSpec(r.Context(), id, sp); err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	idlePolicy := "sleep"
	if req.KeepAlive {
		idlePolicy = "always_on"
	}
	if err := s.Store.SetIdlePolicy(r.Context(), id, idlePolicy); err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.auditAction(r, audit.Entry{Action: "sandbox.lifecycle", Target: id,
		Detail: map[string]any{"idle_timeout_secs": sp.IdleTimeoutSecs, "keep_alive": req.KeepAlive, "clamped": d.Clamped}})
	fresh, ok := s.v1Lookup(w, r, id)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.sandboxLifecycle(fresh))
}

// --- GET/PUT /v1/sandboxes/{id}/network/egress ----------------------

func (s *Server) v1GetEgress(w http.ResponseWriter, r *http.Request) {
	sb, ok := s.v1Lookup(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	s.writeEgress(w, sb.Spec.Egress)
}

func (s *Server) writeEgress(w http.ResponseWriter, allow []string) {
	if allow == nil {
		allow = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"allow": allow, "proxy": s.EgressProxyURL})
}

// v1PutEgress {allow: [...]} replaces the sandbox's egress allowlist. It takes
// effect on the next proxied connection; no restart.
func (s *Server) v1PutEgress(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Allow []string `json:"allow"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "invalid json: "+err.Error())
		return
	}
	id := r.PathValue("id")
	sb, ok := s.v1Lookup(w, r, id)
	if !ok {
		return
	}
	p, err := s.policy(r.Context())
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	d := policy.Decide(p, policy.Request{Action: policy.ActionEgress, Egress: body.Allow})
	if !d.Allow {
		if d.NeedsApproval == "" {
			writePolicyErr(w, &policyError{d: d})
			return
		}
		if s.requestApproval(w, r, d.NeedsApproval, "sandbox:"+id+":egress", d.Egress) {
			return
		}
		// Approved for exactly this (normalized) list.
	}
	s.setEgress(w, r, sb, d.Egress)
}

func (s *Server) setEgress(w http.ResponseWriter, r *http.Request, sb *store.Sandbox, allow []string) {
	sp := sb.Spec
	sp.Egress = allow
	if err := s.Store.UpdateSpec(r.Context(), sb.ID, sp); err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.auditAction(r, audit.Entry{Action: "sandbox.egress", Target: sb.ID,
		Detail: map[string]any{"from": sb.Spec.Egress, "to": allow}})
	s.writeEgress(w, allow)
}
