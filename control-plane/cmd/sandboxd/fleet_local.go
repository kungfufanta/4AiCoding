// fleet_local.go — the control plane's own fleet membership (M6). The
// all-in-one topology executes sandboxes on the control plane host, so
// the control plane registers itself as a role='local' node: it shows up
// in GET /v1/nodes, contributes capacity to /v1/capacity and obeys drain
// exactly like a worker (draining it refuses new sandbox creates).
package main

import (
	"context"
	"log/slog"
	"runtime"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/api"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/hosttelemetry"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/reaper"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

// ensureLocalNode upserts the role='local' node row at boot and returns
// its id ("" when registration failed — placement degrades to unplaced).
// Re-registering never clears a drain an operator set (see
// store.UpsertNode). Capacity defaults come from env (SANDBOXD_CAPACITY_*),
// falling back to the machine: NumCPU and MemTotal. Unreportable
// dimensions stay 0 (= unreported).
func ensureLocalNode(ctx context.Context, st *store.Store, log *slog.Logger) string {
	nodeID := envDefault("SANDBOXD_NODE_ID", "local")
	if !store.ValidNodeID(nodeID) {
		log.Error("startup: invalid SANDBOXD_NODE_ID; fleet placement disabled", "node_id", nodeID)
		return ""
	}
	capacity := store.Capacity{
		CPUs:     floatFromEnv("SANDBOXD_CAPACITY_CPUS", float64(runtime.NumCPU())),
		MemoryMB: intFromEnv("SANDBOXD_CAPACITY_MEMORY_MB", 0),
		DiskMB:   intFromEnv("SANDBOXD_CAPACITY_DISK_MB", 0),
	}
	if capacity.MemoryMB == 0 {
		if mi, err := reaper.ReadMemInfo(); err == nil {
			capacity.MemoryMB = int(mi.Total / 1024)
		}
	}
	n := &store.Node{
		NodeID:   nodeID,
		Role:     store.RoleLocal,
		Version:  buildIdentString(),
		Capacity: capacity,
	}
	if err := st.UpsertNode(ctx, n); err != nil {
		// Fleet rows are additive: a failure here must not keep the
		// control plane down — placement degrades to unplaced.
		log.Error("startup: local node register failed; fleet features degraded", "err", err.Error())
		return ""
	}
	log.Info("startup: local node registered in fleet",
		"node_id", nodeID,
		"capacity_cpus", capacity.CPUs,
		"capacity_memory_mb", capacity.MemoryMB,
		"capacity_disk_mb", capacity.DiskMB,
	)
	return nodeID
}

// bumpLocalNodeLoop keeps the local node's heartbeat fresh. The local node
// reports itself online regardless (we are the process answering), but a
// moving heartbeat makes /v1/nodes honest for external inspection and
// keeps one code path for lease expiry.
//
// Each tick also samples this host (tel, nil = telemetry off) into the
// node's metrics, exactly like a remote worker's heartbeat does.
func bumpLocalNodeLoop(ctx context.Context, st *store.Store, srv *api.Server, log *slog.Logger, nodeID string, tel *hosttelemetry.Collector) {
	if nodeID == "" {
		return
	}
	t := time.NewTicker(localHeartbeatInterval)
	defer t.Stop()
	bump := func() {
		if _, err := st.NodeHeartbeat(ctx, nodeID, nil, nil); err != nil && ctx.Err() == nil {
			log.Warn("local node heartbeat failed", "err", err.Error())
		}
		if snap := tel.Collect(); snap != nil {
			if err := srv.RecordNodeTelemetry(ctx, nodeID, snap); err != nil && ctx.Err() == nil {
				log.Warn("local node telemetry failed", "err", err.Error())
			}
		}
	}
	bump() // first tick immediately after boot
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			bump()
		}
	}
}

// localHeartbeatInterval is the local node's heartbeat + telemetry period
// (the same default as a worker's, so node charts have the same step).
const localHeartbeatInterval = 10 * time.Second

func buildIdentString() string {
	v, c := buildIdent()
	return v + " (" + c + ")"
}
