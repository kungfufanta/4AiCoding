// v1_fleet.go — the operator-facing fleet API (M6, PLAN §4 "Node /
// cluster"): GET /v1/nodes, GET /v1/nodes/{id}/capacity, GET /v1/capacity
// and the admin-only POST /v1/nodes/{id}/drain | /undrain. Reads are open
// to every role (dashboards); drain is an admin route per roles.go.
package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/audit"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/fleet"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

// v1NodeCapacity is the capacity/usage block of a node response. Fields
// mirror store.Capacity/store.Usage so the console renders them directly.
type v1NodeCapacity struct {
	CPUs     float64 `json:"cpus"`
	MemoryMB int     `json:"memory_mb"`
	DiskMB   int     `json:"disk_mb"`
}

type v1NodeUsage struct {
	Sandboxes int     `json:"sandboxes"`
	CPUs      float64 `json:"cpus"`
	MemoryMB  int     `json:"memory_mb"`
	DiskMB    int     `json:"disk_mb"`
}

type v1Node struct {
	NodeID       string         `json:"node_id"`
	Role         string         `json:"role"`
	Status       string         `json:"status"` // online | offline (derived from the heartbeat lease)
	DrainState   string         `json:"drain_state"`
	Address      string         `json:"address,omitempty"`
	Version      string         `json:"version,omitempty"`
	Capacity     v1NodeCapacity `json:"capacity"`
	Used         v1NodeUsage    `json:"used"`
	Sandboxes    int            `json:"sandboxes"`
	RegisteredAt string         `json:"registered_at"`
	HeartbeatAt  string         `json:"heartbeat_at"`
}

// v1NodeFromRow reshapes a node row (+ its placement-derived usage) for
// the /v1 responses. `now`/`lease` come from the server so tests can pin
// the clock.
func (s *Server) v1NodeFromRow(n *store.Node, derived store.Usage) v1Node {
	now := time.Now()
	lease := s.NodeLease
	if lease <= 0 {
		lease = fleet.DefaultLease
	}
	u := fleet.UsageOf(n, derived)
	out := v1Node{
		NodeID:       n.NodeID,
		Role:         n.Role,
		Status:       fleet.Status(n, now, lease),
		DrainState:   n.DrainState,
		Address:      n.Address,
		Version:      n.Version,
		Capacity:     v1NodeCapacity{CPUs: n.Capacity.CPUs, MemoryMB: n.Capacity.MemoryMB, DiskMB: n.Capacity.DiskMB},
		Used:         v1NodeUsage{Sandboxes: u.Sandboxes, CPUs: u.CPUs, MemoryMB: u.MemoryMB, DiskMB: u.DiskMB},
		Sandboxes:    u.Sandboxes,
		RegisteredAt: n.RegisteredAt.UTC().Format(time.RFC3339),
		HeartbeatAt:  n.HeartbeatAt.UTC().Format(time.RFC3339),
	}
	return out
}

// nodeLease returns the configured heartbeat lease.
func (s *Server) nodeLease() time.Duration {
	if s.NodeLease > 0 {
		return s.NodeLease
	}
	return fleet.DefaultLease
}

// fleetSnapshot loads nodes + placement-derived usage in one go.
func (s *Server) fleetSnapshot(r *http.Request) ([]*store.Node, map[string]store.Usage, error) {
	nodes, err := s.Store.ListNodes(r.Context())
	if err != nil {
		return nil, nil, err
	}
	usage, err := s.fleetUsage(r.Context())
	if err != nil {
		return nil, nil, err
	}
	return nodes, usage, nil
}

// --- GET /v1/nodes ---------------------------------------------------

func (s *Server) v1ListNodes(w http.ResponseWriter, r *http.Request) {
	nodes, usage, err := s.fleetSnapshot(r)
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	out := make([]v1Node, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, s.v1NodeFromRow(n, usage[n.NodeID]))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"nodes": out,
		"now":   time.Now().UTC().Format(time.RFC3339),
	})
}

// --- GET /v1/capacity --------------------------------------------------

// v1FleetCapacity reports fleet-wide totals plus the per-node breakdown,
// so a scheduler (or an operator eyeballing the console) sees both.
func (s *Server) v1FleetCapacity(w http.ResponseWriter, r *http.Request) {
	nodes, usage, err := s.fleetSnapshot(r)
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	totals := fleet.Sum(nodes, usage, time.Now(), s.nodeLease())
	perNode := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		u := fleet.UsageOf(n, usage[n.NodeID])
		perNode = append(perNode, map[string]any{
			"node_id":     n.NodeID,
			"role":        n.Role,
			"status":      fleet.Status(n, time.Now(), s.nodeLease()),
			"drain_state": n.DrainState,
			"capacity":    v1NodeCapacity{CPUs: n.Capacity.CPUs, MemoryMB: n.Capacity.MemoryMB, DiskMB: n.Capacity.DiskMB},
			"used":        v1NodeUsage{Sandboxes: u.Sandboxes, CPUs: u.CPUs, MemoryMB: u.MemoryMB, DiskMB: u.DiskMB},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"capacity": v1NodeCapacity{CPUs: totals.Capacity.CPUs, MemoryMB: totals.Capacity.MemoryMB, DiskMB: totals.Capacity.DiskMB},
		"used":     v1NodeUsage{Sandboxes: totals.Used.Sandboxes, CPUs: totals.Used.CPUs, MemoryMB: totals.Used.MemoryMB, DiskMB: totals.Used.DiskMB},
		"nodes":    totals.Nodes,
		"online":   totals.Online,
		"offline":  totals.Offline,
		"draining": totals.Draining,
		"per_node": perNode,
		"now":      time.Now().UTC().Format(time.RFC3339),
	})
}

// --- GET /v1/nodes/{id}/capacity ---------------------------------------

func (s *Server) v1NodeCapacity(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	n, err := s.Store.GetNode(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeV1Err(w, http.StatusNotFound, "not_found", "no such node")
		return
	}
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	usage, err := s.fleetUsage(r.Context())
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	derived := usage[n.NodeID]
	u := fleet.UsageOf(n, derived)
	writeJSON(w, http.StatusOK, map[string]any{
		"node_id":     n.NodeID,
		"role":        n.Role,
		"status":      fleet.Status(n, time.Now(), s.nodeLease()),
		"drain_state": n.DrainState,
		"capacity":    v1NodeCapacity{CPUs: n.Capacity.CPUs, MemoryMB: n.Capacity.MemoryMB, DiskMB: n.Capacity.DiskMB},
		"used":        v1NodeUsage{Sandboxes: u.Sandboxes, CPUs: u.CPUs, MemoryMB: u.MemoryMB, DiskMB: u.DiskMB},
		"sandboxes":   u.Sandboxes,
	})
}

// --- POST /v1/nodes/{id}/drain | /undrain -------------------------------
//
// Drain refuses NEW placements on the node (the create path checks the
// executor's drain state); running sandboxes are untouched until a later
// milestone adds eviction. Both endpoints are admin-only via roles.go.

func (s *Server) v1DrainNode(w http.ResponseWriter, r *http.Request) {
	s.setNodeDrain(w, r, store.DrainDrain)
}

func (s *Server) v1UndrainNode(w http.ResponseWriter, r *http.Request) {
	s.setNodeDrain(w, r, store.DrainActive)
}

func (s *Server) setNodeDrain(w http.ResponseWriter, r *http.Request, state string) {
	id := r.PathValue("id")
	n, err := s.Store.GetNode(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeV1Err(w, http.StatusNotFound, "not_found", "no such node")
		return
	}
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if n.DrainState != state {
		if err := s.Store.SetNodeDrain(r.Context(), id, state); err != nil {
			writeV1Err(w, http.StatusInternalServerError, "internal", "drain: "+err.Error())
			return
		}
	}
	action := "node.undrain"
	if state == store.DrainDrain {
		action = "node.drain"
	}
	usage, _ := s.fleetUsage(r.Context())
	s.auditAction(r, audit.Entry{Action: action, Target: id,
		Detail: map[string]any{"live_sandboxes": usage[id].Sandboxes}})
	updated, err := s.Store.GetNode(r.Context(), id)
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"node": s.v1NodeFromRow(updated, usage[id])})
}
