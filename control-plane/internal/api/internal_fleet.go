// internal_fleet.go — the /internal fleet surface (M6). These routes are
// the worker-to-control-plane protocol: POST /internal/nodes/register and
// POST /internal/nodes/heartbeat, both gated by the worker token in the
// auth middleware (an /internal path without a worker credential never
// even reaches this file — it is answered 404 by the middleware).
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/audit"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

// internalRegisterReq is POST /internal/nodes/register. Re-registering an
// existing node refreshes address/version/capacity and keeps its drain
// state (a worker restarting does not un-drain itself).
type internalRegisterReq struct {
	NodeID   string         `json:"node_id"`
	Address  string         `json:"address,omitempty"`
	Version  string         `json:"version,omitempty"`
	Capacity store.Capacity `json:"capacity"`
	APIToken string         `json:"api_token,omitempty"` // M7b executor credential
}

type internalRegisterResp struct {
	NodeID     string `json:"node_id"`
	DrainState string `json:"drain_state"`
	Now        string `json:"now"`
}

func (s *Server) internalNodeRegister(w http.ResponseWriter, r *http.Request) {
	var req internalRegisterReq
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "invalid json: "+err.Error())
		return
	}
	req.NodeID = strings.TrimSpace(req.NodeID)
	if !store.ValidNodeID(req.NodeID) {
		writeV1Err(w, http.StatusBadRequest, "invalid_request",
			"node_id must match [a-z0-9][a-z0-9.-]{0,62}")
		return
	}
	if len(req.Address) > 512 {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "address too long")
		return
	}
	if req.Capacity.CPUs < 0 || req.Capacity.MemoryMB < 0 || req.Capacity.DiskMB < 0 {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "capacity must be non-negative")
		return
	}
	if len(req.APIToken) > 256 {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "api_token too long")
		return
	}
	// M7b: a node that registered an API token is bound to it. Another
	// holder of the (shared) worker token cannot take over its id and
	// redirect the control plane's traffic; an operator resets a lost
	// worker with DELETE /v1/nodes/{id}.
	if ok := s.checkNodeToken(w, r, req.NodeID, req.APIToken); !ok {
		return
	}
	n := &store.Node{
		NodeID:   req.NodeID,
		Role:     store.RoleWorker,
		Address:  strings.TrimSpace(req.Address),
		Version:  strings.TrimSpace(req.Version),
		Capacity: req.Capacity,
	}
	if err := s.Store.UpsertNode(r.Context(), n); err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", "register node: "+err.Error())
		return
	}
	if req.APIToken != "" {
		if err := s.storeNodeToken(r.Context(), req.NodeID, req.APIToken); err != nil {
			writeV1Err(w, http.StatusInternalServerError, "internal", "store node token: "+err.Error())
			return
		}
	}
	s.auditAction(r, audit.Entry{Action: "node.register", Target: req.NodeID,
		Detail: map[string]any{"address": n.Address, "cpus": n.Capacity.CPUs,
			"memory_mb": n.Capacity.MemoryMB, "disk_mb": n.Capacity.DiskMB}})
	writeJSON(w, http.StatusOK, internalRegisterResp{
		NodeID: req.NodeID, DrainState: store.DrainActive, Now: time.Now().UTC().Format(time.RFC3339),
	})
}

// internalHeartbeatReq is POST /internal/nodes/heartbeat. Capacity/used
// are optional updates; the response returns the node's desired state —
// above all drain_state, so a drained worker learns to stop taking work.
type internalHeartbeatReq struct {
	NodeID   string          `json:"node_id"`
	Capacity *store.Capacity `json:"capacity,omitempty"`
	Used     *store.Capacity `json:"used,omitempty"`
}

type internalHeartbeatResp struct {
	NodeID     string          `json:"node_id"`
	DrainState string          `json:"drain_state"`
	Now        string          `json:"now"`
	Policy     json.RawMessage `json:"policy,omitempty"`
}

func (s *Server) internalNodeHeartbeat(w http.ResponseWriter, r *http.Request) {
	var req internalHeartbeatReq
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "invalid json: "+err.Error())
		return
	}
	req.NodeID = strings.TrimSpace(req.NodeID)
	if !store.ValidNodeID(req.NodeID) {
		writeV1Err(w, http.StatusBadRequest, "invalid_request",
			"node_id must match [a-z0-9][a-z0-9.-]{0,62}")
		return
	}
	n, err := s.Store.NodeHeartbeat(r.Context(), req.NodeID, req.Capacity, req.Used)
	if errors.Is(err, store.ErrNotFound) {
		// The control plane does not know this node (fresh DB, or the
		// row was removed). The worker must register again.
		writeV1Err(w, http.StatusNotFound, "not_found", "unknown node: register again")
		return
	}
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", "heartbeat: "+err.Error())
		return
	}
	resp := internalHeartbeatResp{
		NodeID: n.NodeID, DrainState: n.DrainState, Now: time.Now().UTC().Format(time.RFC3339),
	}
	if p, err := s.policy(r.Context()); err == nil {
		resp.Policy, _ = json.Marshal(p)
	}
	writeJSON(w, http.StatusOK, resp)
}
