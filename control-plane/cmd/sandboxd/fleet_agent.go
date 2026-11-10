// fleet_agent.go — the register + heartbeat loop a fleet member runs against
// the control plane. Used by `sandboxd worker` (M6, heartbeat only) and by a
// sandboxd daemon in executor mode (M7b, SANDBOXD_CONTROL_PLANE_URL set).
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/api"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/fleet"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

// runFleetAgent registers (with backoff — the control plane may not be up
// yet), then heartbeats every interval until ctx ends. A 404 heartbeat means
// the control plane forgot the node: register again. used (optional)
// reports current usage on each beat; onBeat (optional) receives every
// successful heartbeat reply.
func runFleetAgent(ctx context.Context, log *slog.Logger, client *fleet.Client, req fleet.RegisterRequest,
	interval time.Duration, used func(context.Context) *store.Capacity, onBeat func(*fleet.HeartbeatResponse)) {
	delay := registerBackoffStart
	for {
		resp, err := client.Register(ctx, req)
		if err == nil {
			log.Info("worker registered", "node_id", resp.NodeID, "drain_state", resp.DrainState)
			break
		}
		if ctx.Err() != nil {
			return
		}
		log.Warn("register failed; retrying", "err", err.Error(), "retry_in", delay.String())
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return
		}
		delay = min(delay*2, registerBackoffMax)
	}

	drained := ""
	capacity := req.Capacity
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		beat := fleet.HeartbeatRequest{NodeID: req.NodeID, Capacity: &capacity}
		if used != nil {
			beat.Used = used(ctx)
		}
		resp, err := client.Heartbeat(ctx, beat)
		if ctx.Err() != nil {
			return
		}
		switch {
		case err == nil:
			if resp.DrainState != drained {
				drained = resp.DrainState
				if drained == fleet.DrainDrain {
					log.Warn("worker DRAINED by the control plane — refuse new sandboxes here", "node_id", req.NodeID)
				} else {
					log.Info("worker is active", "node_id", req.NodeID)
				}
			}
			if onBeat != nil {
				onBeat(resp)
			}
		default:
			log.Warn("heartbeat failed", "err", err.Error())
			if apiErr, ok := err.(*fleet.APIError); ok && apiErr.Status == 404 {
				log.Info("control plane does not know this node; re-registering", "node_id", req.NodeID)
				if _, rerr := client.Register(ctx, req); rerr != nil {
					log.Warn("re-register failed", "err", rerr.Error())
				}
			}
		}
		select {
		case <-t.C:
		case <-ctx.Done():
			log.Info("worker stopping", "node_id", req.NodeID)
			return
		}
	}
}

// --- executor mode (M7b) ------------------------------------------------------

// executorConfig is a sandboxd daemon that runs sandboxes for a control
// plane: SANDBOXD_CONTROL_PLANE_URL + SANDBOXD_WORKER_TOKEN +
// SANDBOXD_NODE_ID + SANDBOXD_NODE_ADDRESS.
type executorConfig struct {
	ControlPlaneURL string
	WorkerToken     string
	NodeID          string
	Address         string
	APIToken        string // this node's credential for the control plane
	Interval        time.Duration
}

// loadNodeAPIToken returns this node's API token, generating and saving it
// (0600) on first start so it survives restarts — the control plane binds
// the node id to it.
func loadNodeAPIToken(stateDir string) (string, error) {
	path := filepath.Join(stateDir, "node-api-token")
	if b, err := os.ReadFile(path); err == nil {
		if tok := strings.TrimSpace(string(b)); len(tok) >= 32 {
			return tok, nil
		}
		return "", fmt.Errorf("%s is malformed; delete it and reset the node on the control plane", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	tok := "sk_node_" + hex.EncodeToString(buf)
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", err
	}
	return tok, nil
}

// runExecutorAgent joins the fleet as an executor: it registers this node
// (with its API token), reports usage, applies the drain state to its own
// local node and the control plane's policy to its own store.
func runExecutorAgent(ctx context.Context, log *slog.Logger, st *store.Store, srv *api.Server, cfg executorConfig) {
	var capacity store.Capacity
	if n, err := st.GetNode(ctx, cfg.NodeID); err == nil {
		capacity = n.Capacity
	}
	req := fleet.RegisterRequest{
		NodeID:   cfg.NodeID,
		Address:  cfg.Address,
		Version:  buildIdentString(),
		Capacity: capacity,
		APIToken: cfg.APIToken,
	}
	client := &fleet.Client{BaseURL: strings.TrimRight(cfg.ControlPlaneURL, "/"), Token: cfg.WorkerToken}
	used := func(ctx context.Context) *store.Capacity {
		usage, err := st.NodeUsage(ctx)
		if err != nil {
			return nil
		}
		u := usage[cfg.NodeID]
		return &store.Capacity{CPUs: u.CPUs, MemoryMB: u.MemoryMB}
	}
	onBeat := func(resp *fleet.HeartbeatResponse) {
		if n, err := st.GetNode(ctx, cfg.NodeID); err == nil && n.DrainState != resp.DrainState {
			if err := st.SetNodeDrain(ctx, cfg.NodeID, resp.DrainState); err != nil {
				log.Warn("executor: apply drain state failed", "err", err.Error())
			}
		}
		if len(resp.Policy) > 0 {
			if changed, err := srv.ApplyFleetPolicy(ctx, resp.Policy); err != nil {
				log.Warn("executor: apply control plane policy failed", "err", err.Error())
			} else if changed {
				log.Info("executor: applied the control plane's policy")
			}
		}
	}
	log.Info("executor: joining the fleet", "node_id", cfg.NodeID, "control_plane", cfg.ControlPlaneURL, "address", cfg.Address)
	runFleetAgent(ctx, log, client, req, cfg.Interval, used, onBeat)
}
