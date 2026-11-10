// v1_approvals.go — human-in-the-loop approvals (PLAN §8 #11).
//
// Flow: a gated request (see policy.Approvals) is answered 202 with an
// approval id and parked as "pending". An admin with a console session
// approves or denies it — an API key can never approve, so an agent cannot
// approve its own request. The caller then repeats the SAME request with
// header X-Sandboxd-Approval: <id>; it succeeds once, only if the payload
// hash matches, and only before the approval expires.
package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/audit"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/auth"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

// ApprovalHeader carries the approval id on the repeated request.
const ApprovalHeader = "X-Sandboxd-Approval"

// FleetGrantHeader (M7b) names the approval action the control plane already
// cleared before forwarding a request to the worker that runs the sandbox.
// It is honoured ONLY from the fleet actor (the control plane's own node
// token), so approvals stay decided in one place: the control plane.
const FleetGrantHeader = "X-Sandboxd-Fleet-Grant"

const (
	approvalPendingTTL = 24 * time.Hour   // time an admin has to decide
	approvalUseTTL     = 15 * time.Minute // time the caller has to use an approval
)

func approvalHash(action, target string, payload []byte) string {
	h := sha256.New()
	h.Write([]byte(action))
	h.Write([]byte{0})
	h.Write([]byte(target))
	h.Write([]byte{0})
	h.Write(payload)
	return hex.EncodeToString(h.Sum(nil))
}

// requestApproval gates a risky request. It returns false when the request
// carries a valid, approved, unused approval for exactly this action, target
// and payload (the approval is consumed and the caller proceeds). Otherwise
// it parks a new pending approval, writes 202, and returns true (stop).
func (s *Server) requestApproval(w http.ResponseWriter, r *http.Request, action, target string, payload any) bool {
	body, err := json.Marshal(payload)
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return true
	}
	if a := auth.ActorFrom(r.Context()); a.Kind == auth.ActorFleet && r.Header.Get(FleetGrantHeader) == action {
		return false // cleared on the control plane (see forwardRemote)
	}
	hash := approvalHash(action, target, body)
	now := time.Now()
	if id := r.Header.Get(ApprovalHeader); id != "" {
		_ = s.Store.ExpireApprovals(r.Context(), now.Unix())
		a, err := s.Store.GetApproval(r.Context(), id)
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeV1Err(w, http.StatusForbidden, "approval_invalid", "no such approval")
			return true
		case err != nil:
			writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
			return true
		case a.Action != action || a.Target != target || a.PayloadHash != hash:
			writeV1Err(w, http.StatusForbidden, "approval_invalid", "the approval was granted for a different request")
			return true
		case a.Status != "approved":
			writeV1Err(w, http.StatusForbidden, "approval_invalid", "approval is "+a.Status)
			return true
		}
		// Single use: compare-and-set approved -> used.
		if err := s.Store.TransitionApproval(r.Context(), a.ID, "approved", "used", "", "", 0); err != nil {
			writeV1Err(w, http.StatusForbidden, "approval_invalid", "approval already used")
			return true
		}
		s.auditAction(r, audit.Entry{Action: "approval.used", Target: a.ID,
			Detail: map[string]any{"action": action, "target": target}})
		return false
	}
	actor := auth.ActorFrom(r.Context())
	a := &store.Approval{
		ID: newULID(), Action: action, Target: target, Payload: string(body), PayloadHash: hash,
		Status: "pending", RequestedBy: actor.Kind + ":" + actor.User,
		CreatedAt: now.Unix(), ExpiresAt: now.Add(approvalPendingTTL).Unix(),
	}
	if err := s.Store.CreateApproval(r.Context(), a); err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return true
	}
	s.auditAction(r, audit.Entry{Action: "approval.requested", Target: a.ID,
		Detail: map[string]any{"action": action, "target": target}})
	writeJSON(w, http.StatusAccepted, map[string]any{
		"approval_id": a.ID, "status": "pending", "action": action, "target": target,
		"message": "this request needs an admin's approval; repeat it with header " + ApprovalHeader + " once approved",
	})
	return true
}

// GET /v1/approvals?status=
func (s *Server) v1ListApprovals(w http.ResponseWriter, r *http.Request) {
	_ = s.Store.ExpireApprovals(r.Context(), time.Now().Unix())
	status := r.URL.Query().Get("status")
	switch status {
	case "", "pending", "approved", "denied", "used", "expired":
	default:
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "unknown status")
		return
	}
	list, err := s.Store.ListApprovals(r.Context(), status, 200)
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if list == nil {
		list = []*store.Approval{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"approvals": list})
}

// GET /v1/approvals/{id}
func (s *Server) v1GetApproval(w http.ResponseWriter, r *http.Request) {
	_ = s.Store.ExpireApprovals(r.Context(), time.Now().Unix())
	a, err := s.Store.GetApproval(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeV1Err(w, http.StatusNotFound, "not_found", "no such approval")
		return
	}
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// POST /v1/approvals/{id}/approve and /deny {reason?}
func (s *Server) v1ApproveApproval(w http.ResponseWriter, r *http.Request) {
	s.decide(w, r, "approved")
}
func (s *Server) v1DenyApproval(w http.ResponseWriter, r *http.Request) { s.decide(w, r, "denied") }

func (s *Server) decide(w http.ResponseWriter, r *http.Request, to string) {
	actor := auth.ActorFrom(r.Context())
	if actor.Kind != "user" {
		// The whole point: an API key (e.g. an agent) cannot approve.
		writeV1Err(w, http.StatusForbidden, "forbidden", "approvals are decided by a person signed in to the console")
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if r.ContentLength > 0 {
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4*1024))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			writeV1Err(w, http.StatusBadRequest, "invalid_request", "invalid json: "+err.Error())
			return
		}
	}
	id := r.PathValue("id")
	_ = s.Store.ExpireApprovals(r.Context(), time.Now().Unix())
	var exp int64
	if to == "approved" {
		exp = time.Now().Add(approvalUseTTL).Unix()
	}
	err := s.Store.TransitionApproval(r.Context(), id, "pending", to, actor.User, body.Reason, exp)
	if errors.Is(err, store.ErrConflict) {
		if _, gerr := s.Store.GetApproval(r.Context(), id); errors.Is(gerr, store.ErrNotFound) {
			writeV1Err(w, http.StatusNotFound, "not_found", "no such approval")
			return
		}
		writeV1Err(w, http.StatusConflict, "conflict", "approval is no longer pending")
		return
	}
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.auditAction(r, audit.Entry{Action: "approval." + to, Target: id, Detail: map[string]any{"reason": body.Reason}})
	a, _ := s.Store.GetApproval(r.Context(), id)
	writeJSON(w, http.StatusOK, a)
}
