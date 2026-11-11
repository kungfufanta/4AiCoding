// v1_node_metrics.go — host telemetry of fleet members. Every member sends
// a hosttelemetry.Snapshot with its heartbeat (workers over /internal, the
// local node in-process); the latest one is stored on the node row and a
// short in-memory history feeds the console's Node Resources charts.
//
// GET /v1/nodes/{id}/metrics returns both. The data comes from the worker
// image itself, so it works whether the worker runs on a VPS, under Docker
// or as a k3s pod — no Kubernetes Metrics API or Prometheus needed.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/hosttelemetry"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

const (
	// maxTelemetryBytes bounds one snapshot on the wire (a real one is
	// well under 1 KiB; the interface list is the only variable part).
	maxTelemetryBytes = 16 << 10
	// nodeHistStep / nodeHistWindow: one point per step for the window
	// (1 h at the default 10 s heartbeat = 360 points per node).
	nodeHistStep   = 5 * time.Second
	nodeHistWindow = time.Hour
)

// nodeMetricPoint is one sample of a node's history.
type nodeMetricPoint struct {
	T           string  `json:"t"`
	CPUPercent  float64 `json:"cpu_percent"`
	Load1       float64 `json:"load1"`
	MemUsedMB   int64   `json:"mem_used_mb"`
	MemTotalMB  int64   `json:"mem_total_mb"`
	DiskUsedMB  int64   `json:"disk_used_mb"`
	DiskTotalMB int64   `json:"disk_total_mb"`
	RxBps       float64 `json:"rx_bps"`
	TxBps       float64 `json:"tx_bps"`
}

// nodeHistory is the per-node ring of recent samples. It lives in memory:
// a control plane restart starts the charts over, the latest snapshot
// itself survives on the node row.
type nodeHistory struct {
	mu     sync.Mutex
	points map[string][]nodeMetricPoint
	last   map[string]time.Time
}

func (h *nodeHistory) add(nodeID string, now time.Time, s *hosttelemetry.Snapshot) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.points == nil {
		h.points = map[string][]nodeMetricPoint{}
		h.last = map[string]time.Time{}
	}
	if now.Sub(h.last[nodeID]) < nodeHistStep {
		return
	}
	h.last[nodeID] = now
	pts := append(h.points[nodeID], nodeMetricPoint{
		T:           now.UTC().Format(time.RFC3339),
		CPUPercent:  s.CPU.UsagePercent,
		Load1:       s.CPU.Load1,
		MemUsedMB:   s.Memory.UsedMB,
		MemTotalMB:  s.Memory.TotalMB,
		DiskUsedMB:  s.Disk.UsedMB,
		DiskTotalMB: s.Disk.TotalMB,
		RxBps:       s.Network.RxBps,
		TxBps:       s.Network.TxBps,
	})
	cut := now.Add(-nodeHistWindow).UTC().Format(time.RFC3339)
	i := 0
	for i < len(pts) && pts[i].T < cut {
		i++
	}
	h.points[nodeID] = pts[i:]
}

func (h *nodeHistory) get(nodeID string) []nodeMetricPoint {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]nodeMetricPoint{}, h.points[nodeID]...)
}

func (h *nodeHistory) forget(nodeID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.points, nodeID)
	delete(h.last, nodeID)
}

// decodeTelemetry validates a snapshot from the wire and re-encodes it, so
// only known fields are ever stored. nil, nil for an absent field.
func decodeTelemetry(raw json.RawMessage) (*hosttelemetry.Snapshot, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if len(raw) > maxTelemetryBytes {
		return nil, errors.New("telemetry too large")
	}
	var s hosttelemetry.Snapshot
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, errors.New("invalid telemetry: " + err.Error())
	}
	if len(s.Network.Interfaces) > 64 {
		s.Network.Interfaces = s.Network.Interfaces[:64]
	}
	return &s, nil
}

// RecordNodeTelemetry stores a node's latest host sample and appends it to
// the node's history. main.go calls it for the local node; the heartbeat
// handler for workers.
func (s *Server) RecordNodeTelemetry(ctx context.Context, nodeID string, snap *hosttelemetry.Snapshot) error {
	if snap == nil {
		return nil
	}
	b, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	if err := s.Store.SetNodeTelemetry(ctx, nodeID, b); err != nil {
		return err
	}
	s.nodeHist.add(nodeID, time.Now(), snap)
	return nil
}

// --- GET /v1/nodes/{id}/metrics -------------------------------------------

func (s *Server) v1NodeMetrics(w http.ResponseWriter, r *http.Request) {
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
	writeJSON(w, http.StatusOK, map[string]any{
		"node":    s.v1NodeFromRow(n, usage[n.NodeID]),
		"history": s.nodeHist.get(n.NodeID),
		"step":    nodeHistStep.String(),
		"window":  nodeHistWindow.String(),
		"now":     time.Now().UTC().Format(time.RFC3339),
	})
}
