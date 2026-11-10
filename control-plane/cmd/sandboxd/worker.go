// worker.go — the `sandboxd worker` subcommand (M6, PLAN §2 "Chế độ B").
//
// A worker is the same binary in a different role: it does NOT serve the
// API and does NOT touch Docker. It registers with the control plane
// (worker token), then heartbeats its capacity until it is signalled to
// stop. The heartbeat response tells it the control plane's desired
// state — above all drain_state, which a worker must respect before
// accepting work (execution lands on workers from M7; M6 is membership).
//
// Usage:
//
//	sandboxd worker \
//	  --control-plane-url http://sandboxd-ctl:9000 \
//	  --worker-token $SANDBOXD_WORKER_TOKEN \
//	  --node-id worker-1 \
//	  --capacity-cpus 8 --capacity-memory-mb 16384 --capacity-disk-mb 51200
//
// Every flag has a SANDBOXD_* env fallback (see flags below), so the
// same systemd unit / docker run line works for both.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/fleet"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/logging"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/reaper"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

const (
	defaultHeartbeatInterval = 10 * time.Second
	// registerBackoffStart/Max bound the retry ladder while the control
	// plane is unreachable at boot (it may simply not be up yet).
	registerBackoffStart = 2 * time.Second
	registerBackoffMax   = 60 * time.Second
)

func runWorker(args []string) int {
	log := logging.NewLogger()

	fs := flag.NewFlagSet("worker", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var (
		controlPlaneURL = fs.String("control-plane-url", "", "base URL of the control plane API (env SANDBOXD_CONTROL_PLANE_URL)")
		workerToken     = fs.String("worker-token", "", "worker token the control plane expects (env SANDBOXD_WORKER_TOKEN)")
		nodeID          = fs.String("node-id", "", "stable id of this worker (env SANDBOXD_NODE_ID; default the hostname)")
		address         = fs.String("address", "", "base URL other machines can reach this worker on (env SANDBOXD_NODE_ADDRESS)")
		cpus            = fs.Float64("capacity-cpus", 0, "advertised CPU cores (env SANDBOXD_CAPACITY_CPUS; default runtime.NumCPU())")
		memoryMB        = fs.Int("capacity-memory-mb", 0, "advertised memory MiB (env SANDBOXD_CAPACITY_MEMORY_MB; default MemTotal)")
		diskMB          = fs.Int("capacity-disk-mb", 0, "advertised workspace disk MiB (env SANDBOXD_CAPACITY_DISK_MB)")
		intervalSec     = fs.Int("heartbeat-interval-sec", 0, "heartbeat period seconds (env SANDBOXD_HEARTBEAT_INTERVAL_SECONDS; default 10)")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	env := func(k, flagVal string) string {
		if flagVal != "" {
			return flagVal
		}
		return os.Getenv(k)
	}
	num := func(flagVal float64, envKey string, def float64) float64 {
		if flagVal > 0 {
			return flagVal
		}
		if v := os.Getenv(envKey); v != "" {
			var f float64
			if _, err := fmt.Sscanf(v, "%g", &f); err == nil && f > 0 {
				return f
			}
		}
		return def
	}

	*controlPlaneURL = env("SANDBOXD_CONTROL_PLANE_URL", *controlPlaneURL)
	*workerToken = env("SANDBOXD_WORKER_TOKEN", *workerToken)
	*nodeID = env("SANDBOXD_NODE_ID", *nodeID)
	*address = env("SANDBOXD_NODE_ADDRESS", *address)
	interval := defaultHeartbeatInterval
	if *intervalSec > 0 {
		interval = time.Duration(*intervalSec) * time.Second
	} else if v := os.Getenv("SANDBOXD_HEARTBEAT_INTERVAL_SECONDS"); v != "" {
		if n := intFromEnv("SANDBOXD_HEARTBEAT_INTERVAL_SECONDS", 10); n > 0 {
			interval = time.Duration(n) * time.Second
		}
	}

	if *controlPlaneURL == "" || *workerToken == "" {
		fmt.Fprintln(os.Stderr, "sandboxd worker: --control-plane-url and --worker-token are required (or SANDBOXD_CONTROL_PLANE_URL / SANDBOXD_WORKER_TOKEN)")
		return 2
	}
	if *nodeID == "" {
		host, err := os.Hostname()
		if err != nil || host == "" {
			host = fmt.Sprintf("worker-%d", os.Getpid())
		}
		*nodeID = host
	}
	if !store.ValidNodeID(*nodeID) {
		fmt.Fprintf(os.Stderr, "sandboxd worker: invalid node id %q (want [a-z0-9][a-z0-9.-]{0,62})\n", *nodeID)
		return 2
	}

	// Capacity defaults describe the machine we run on. Both are best-effort:
	// an unreported dimension stays 0 (= unreported) rather than a lie.
	capacity := store.Capacity{
		CPUs:     num(*cpus, "SANDBOXD_CAPACITY_CPUS", float64(runtime.NumCPU())),
		MemoryMB: int(num(float64(*memoryMB), "SANDBOXD_CAPACITY_MEMORY_MB", 0)),
		DiskMB:   int(num(float64(*diskMB), "SANDBOXD_CAPACITY_DISK_MB", 0)),
	}
	if capacity.MemoryMB == 0 {
		if mi, err := reaper.ReadMemInfo(); err == nil {
			capacity.MemoryMB = int(mi.Total / 1024)
		}
	}

	version, commit := buildIdent()
	client := &fleet.Client{BaseURL: *controlPlaneURL, Token: *workerToken}
	log.Info("worker starting",
		"node_id", *nodeID,
		"control_plane", *controlPlaneURL,
		"address", *address,
		"capacity_cpus", capacity.CPUs,
		"capacity_memory_mb", capacity.MemoryMB,
		"capacity_disk_mb", capacity.DiskMB,
		"heartbeat_interval", interval.String(),
		"version", version, "commit", commit,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Info("worker: signal received", "signal", sig.String())
		cancel()
	}()

	req := fleet.RegisterRequest{
		NodeID:   *nodeID,
		Address:  *address,
		Version:  fmt.Sprintf("%s (%s)", version, commit),
		Capacity: capacity,
	}
	// Register with backoff — the control plane may not be up yet; a worker
	// joining before it must not give up.
	delay := registerBackoffStart
	for {
		resp, err := client.Register(ctx, req)
		if err == nil {
			log.Info("worker registered", "node_id", resp.NodeID, "drain_state", resp.DrainState)
			break
		}
		if ctx.Err() != nil {
			return 0
		}
		log.Warn("register failed; retrying", "err", err.Error(), "retry_in", delay.String())
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return 0
		}
		delay *= 2
		if delay > registerBackoffMax {
			delay = registerBackoffMax
		}
	}

	// Heartbeat loop. A 404 means the control plane lost our row (fresh DB):
	// re-register once, then keep heartbeating.
	drained := ""
	beat := fleet.HeartbeatRequest{
		NodeID:   *nodeID,
		Capacity: &capacity,
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		resp, err := client.Heartbeat(ctx, beat)
		if ctx.Err() != nil {
			return 0
		}
		switch {
		case err == nil:
			if resp.DrainState != drained {
				drained = resp.DrainState
				if drained == fleet.DrainDrain {
					log.Warn("worker DRAINED by the control plane — refuse new sandboxes here", "node_id", *nodeID)
				} else {
					log.Info("worker is active", "node_id", *nodeID)
				}
			}
		default:
			log.Warn("heartbeat failed", "err", err.Error())
			if apiErr, ok := err.(*fleet.APIError); ok && apiErr.Status == 404 {
				log.Info("control plane does not know this node; re-registering", "node_id", *nodeID)
				if _, rerr := client.Register(ctx, req); rerr != nil {
					log.Warn("re-register failed", "err", rerr.Error())
				}
			}
		}
		select {
		case <-t.C:
		case <-ctx.Done():
			log.Info("worker stopping", "node_id", *nodeID)
			return 0
		}
	}
}
