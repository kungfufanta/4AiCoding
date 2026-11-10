// v1_policy.go — the admin policy (GET/PUT /v1/policy and its sections) and
// the per-sandbox lifecycle, keep-alive and egress routes that the policy
// decision point (internal/policy) gates.
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/audit"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/netlog"
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

// EgressRulesOf returns the effective egress rules stored in a spec.
func EgressRulesOf(sp store.Spec) policy.EgressRules {
	mode, err := policy.NormalizeNetMode(sp.EgressMode)
	if err != nil {
		mode = policy.NetNone // an unreadable mode fails closed
	}
	allow, deny := sp.Egress, sp.EgressDeny
	if allow == nil {
		allow = []string{}
	}
	if deny == nil {
		deny = []string{}
	}
	return policy.EgressRules{Mode: mode, Allow: allow, Deny: deny}
}

// v1EgressReq is the body of PUT /network/egress. Omitted fields keep their
// current value, so {"allow":[...]} (the pre-mode shape) still only edits
// the allow list.
type v1EgressReq struct {
	Mode  *string   `json:"mode,omitempty"`
	Allow *[]string `json:"allow,omitempty"`
	Deny  *[]string `json:"deny,omitempty"`
}

// merge applies the request onto the current rules.
func (req v1EgressReq) merge(cur policy.EgressRules) policy.EgressRules {
	next := cur
	if req.Mode != nil {
		next.Mode = *req.Mode
	}
	if req.Allow != nil {
		next.Allow = *req.Allow
	}
	if req.Deny != nil {
		next.Deny = *req.Deny
	}
	return next
}

func decodeEgressReq(w http.ResponseWriter, r *http.Request) (v1EgressReq, bool) {
	var body v1EgressReq
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "invalid json: "+err.Error())
		return body, false
	}
	return body, true
}

func egressDecide(p policy.Policy, rules policy.EgressRules) policy.Decision {
	return policy.Decide(p, policy.Request{Action: policy.ActionEgress,
		EgressMode: rules.Mode, Egress: rules.Allow, EgressDeny: rules.Deny})
}

// egressApprovalPayload is what an egress approval binds to.
func egressApprovalPayload(d policy.Decision) map[string]any {
	return map[string]any{"mode": d.EgressMode, "allow": d.Egress, "deny": d.EgressDeny}
}

func (s *Server) v1GetEgress(w http.ResponseWriter, r *http.Request) {
	sb, ok := s.v1Lookup(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	s.writeEgress(w, EgressRulesOf(sb.Spec))
}

func (s *Server) writeEgress(w http.ResponseWriter, rules policy.EgressRules) {
	writeJSON(w, http.StatusOK, map[string]any{
		"mode": rules.Mode, "allow": rules.Allow, "deny": rules.Deny, "proxy": s.EgressProxyURL,
	})
}

// v1PutEgress {mode?, allow?, deny?} changes the sandbox's egress rules. It
// takes effect on the next proxied connection; no restart.
func (s *Server) v1PutEgress(w http.ResponseWriter, r *http.Request) {
	body, ok := decodeEgressReq(w, r)
	if !ok {
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
	d := egressDecide(p, body.merge(EgressRulesOf(sb.Spec)))
	if !d.Allow {
		if d.NeedsApproval == "" {
			writePolicyErr(w, &policyError{d: d})
			return
		}
		if s.requestApproval(w, r, d.NeedsApproval, "sandbox:"+id+":egress", egressApprovalPayload(d)) {
			return
		}
		// Approved for exactly these (normalized) rules.
	}
	s.setEgress(w, r, sb, d)
}

func (s *Server) setEgress(w http.ResponseWriter, r *http.Request, sb *store.Sandbox, d policy.Decision) {
	from := EgressRulesOf(sb.Spec)
	sp := sb.Spec
	sp.Egress = d.Egress
	sp.EgressDeny = d.EgressDeny
	sp.EgressMode = d.EgressMode
	if err := s.Store.UpdateSpec(r.Context(), sb.ID, sp); err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	to := EgressRulesOf(sp)
	s.auditAction(r, audit.Entry{Action: "sandbox.egress", Target: sb.ID,
		Detail: map[string]any{"from": from, "to": to}})
	s.writeEgress(w, to)
}

// --- GET /v1/sandboxes/{id}/network/activity -------------------------

// v1NetworkActivity returns the live network view of a sandbox: its current
// egress rules, the proxied requests after ?after=<cursor> (at most ?limit,
// default 200) and a per-host summary. Poll it with the returned cursor.
func (s *Server) v1NetworkActivity(w http.ResponseWriter, r *http.Request) {
	sb, ok := s.v1Lookup(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	var after uint64
	if v := r.URL.Query().Get("after"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			writeV1Err(w, http.StatusBadRequest, "invalid_request", "after must be a number")
			return
		}
		after = n
	}
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1000 {
			writeV1Err(w, http.StatusBadRequest, "invalid_request", "limit must be 1..1000")
			return
		}
		limit = n
	}
	events, hosts, cursor := []netlog.Event{}, []netlog.Host{}, uint64(0)
	if s.NetLog != nil {
		events, hosts, cursor = s.NetLog.Snapshot(sb.ID, after, limit)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sandbox_id": sb.ID,
		"recording":  s.NetLog != nil && s.EgressProxyURL != "",
		"rules":      EgressRulesOf(sb.Spec),
		"cursor":     strconv.FormatUint(cursor, 10),
		"events":     events,
		"hosts":      hosts,
	})
}
