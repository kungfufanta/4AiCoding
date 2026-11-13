// Package agent — worker.go: the run executor (plan §7, decision
// §2-4 "worker cùng binary, goroutine trong process portal").
//
// Worker = a claim loop + concurrent run goroutines:
//
//   - claim: SECURITY DEFINER agent_claim_next (SKIP LOCKED, lease),
//     woken by NOTIFY agent_runs_queued and a poll ticker (the
//     fallback that keeps runs flowing when LISTEN is down);
//   - heartbeat: renews the lease every Heartbeat (20 s default);
//   - cancel: NOTIFY agent_cancel wakes the run immediately, and a
//     1 s poll on cancel_requested is the fallback — both cancel the
//     run context, the loop finalizes status cancelled;
//   - sweeper: every SweepInterval, agent_sweep_lost reaps runs whose
//     lease expired more than SweepGrace ago → failed/worker_lost
//     (plan §7 "Worker mất lease ... Không resume");
//   - shutdown: stop claiming, cancel active runs, mark them
//     failed/worker_shutdown.
package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tastyeffectco/sandboxd/portal/internal/llm"
	"github.com/tastyeffectco/sandboxd/portal/internal/sandboxd"
)

// Worker defaults (plan §2-3/§7, decision §2-8).
const (
	DefaultLease      = 60 * time.Second // lease length
	DefaultHeartbeat  = 20 * time.Second // lease renewal cadence
	DefaultClaimEvery = time.Second      // queue poll cadence
	DefaultCancelPoll = time.Second      // cancel_requested poll cadence
	DefaultSweepEvery = 15 * time.Second // sweeper cadence
	DefaultSweepGrace = 30 * time.Second // lease overdue before worker_lost
	// DefaultShutdownGrace: on SIGTERM active runs get this long
	// before their contexts are cancelled (helm
	// terminationGracePeriodSeconds: 30, A3.5).
	DefaultShutdownGrace = 20 * time.Second
)

// StartPool wires N workers onto one pool/notifier/sandboxd and
// starts them. Worker ids are "<process>-<n>" — unique per pod because
// the lease owner text is what the sweeper keys off (any unique string
// works; the pid suffix disambiguates two pools in one test process).
func StartPool(ctx context.Context, log *slog.Logger, pool *pgxpool.Pool, notify *Notifier, src LLMSource, sbx *sandboxd.Client, n int) []*Worker {
	if n < 1 {
		n = 1
	}
	out := make([]*Worker, 0, n)
	for i := 0; i < n; i++ {
		w := &Worker{
			ID:       fmt.Sprintf("portal-%d-%d", os.Getpid(), i),
			Log:      log,
			Store:    NewStore(pool),
			LLM:      src,
			Sandboxd: sbx,
			Notify:   notify,
		}
		w.Start(ctx)
		out = append(out, w)
	}
	return out
}

// Worker executes claimed runs.
type Worker struct {
	ID    string // lease_owner identity, unique per process/goroutine
	Log   *slog.Logger
	Store *Store
	LLM   LLMSource
	// Sandboxd is the client the tools execute through (A3.2). nil →
	// every run fails sandbox_unavailable: without a sandbox backend
	// the agent cannot work honestly.
	Sandboxd *sandboxd.Client
	// Notify wakes the claim loop (agent_runs_queued) and the active
	// runs (agent_cancel); nil = poll-only (still correct).
	Notify *Notifier

	// Tunables (tests shrink them; zero = default).
	Lease          time.Duration
	Heartbeat      time.Duration
	ClaimEvery     time.Duration
	CancelPoll     time.Duration
	SweepEvery     time.Duration
	SweepGrace     time.Duration
	ShutdownGraceD time.Duration
	MaxConcurrent  int // 0 = 1

	ctx      context.Context
	shutdown atomicBool
	wg       sync.WaitGroup
	mu       sync.Mutex
	active   map[string]context.CancelFunc // run_id → cancel
}

type atomicBool struct {
	mu sync.Mutex
	v  bool
}

func (b *atomicBool) set(v bool) { b.mu.Lock(); b.v = v; b.mu.Unlock() }
func (b *atomicBool) get() bool  { b.mu.Lock(); defer b.mu.Unlock(); return b.v }

func (w *Worker) shuttingDown() bool { return w.shutdown.get() }

type tuning struct {
	lease, heartbeat, claimEvery, cancelPoll, sweepEvery, sweepGrace, shutdownGrace time.Duration
	maxConcurrent                                                                   int
}

func (w *Worker) cfg() tuning {
	t := tuning{
		lease:         firstDuration(w.Lease, DefaultLease),
		heartbeat:     firstDuration(w.Heartbeat, DefaultHeartbeat),
		claimEvery:    firstDuration(w.ClaimEvery, DefaultClaimEvery),
		cancelPoll:    firstDuration(w.CancelPoll, DefaultCancelPoll),
		sweepEvery:    firstDuration(w.SweepEvery, DefaultSweepEvery),
		sweepGrace:    firstDuration(w.SweepGrace, DefaultSweepGrace),
		shutdownGrace: firstDuration(w.ShutdownGraceD, DefaultShutdownGrace),
		maxConcurrent: 1,
	}
	if w.MaxConcurrent > 0 {
		t.maxConcurrent = w.MaxConcurrent
	}
	return t
}

func firstDuration(v, def time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return def
}

// Start runs the worker until ctx is done. It returns immediately;
// Wait blocks until every active run has finalized.
func (w *Worker) Start(ctx context.Context) {
	w.ctx = ctx
	w.mu.Lock()
	if w.active == nil {
		w.active = map[string]context.CancelFunc{}
	}
	w.mu.Unlock()
	w.wg.Add(1)
	go w.serve()
}

// Wait blocks until the worker and all its active runs are done.
func (w *Worker) Wait() { w.wg.Wait() }

// serve is the main loop: claim until ctx dies; a sweeper goroutine
// reaps lost runs alongside.
func (w *Worker) serve() {
	defer w.wg.Done()
	t := w.cfg()

	wake, stopWake := w.subscribe()
	defer stopWake()

	sweepDone := make(chan struct{})
	go func() {
		defer close(sweepDone)
		w.sweepLoop(t.sweepEvery, t.sweepGrace)
	}()

	w.claimLoop(t.claimEvery, t.lease, t.maxConcurrent, wake)

	<-sweepDone
}

// subscribe returns a channel of notifications plus a cancel func;
// nil notifier → a never-firing channel.
func (w *Worker) subscribe() (<-chan NotifyMsg, func()) {
	if w.Notify == nil {
		ch := make(chan NotifyMsg)
		return ch, func() {}
	}
	return w.Notify.Subscribe()
}

func (w *Worker) claimLoop(claimEvery, lease time.Duration, maxConcurrent int, wake <-chan NotifyMsg) {
	ticker := time.NewTicker(claimEvery)
	defer ticker.Stop()
	for {
		if w.ctx.Err() != nil {
			return
		}
		// Claim while a slot is free. The per-sandbox single-active
		// gate is the DB's partial unique index; this loop only
		// spreads runs across the N workers.
		for w.activeCount() < maxConcurrent {
			claimed, ok, err := w.Store.ClaimNext(w.ctx, w.ID, lease)
			if err != nil {
				if w.ctx.Err() != nil {
					return
				}
				w.Log.Warn("agent claim failed", "worker", w.ID, "error", err.Error())
				break
			}
			if !ok {
				break
			}
			w.startRun(claimed)
		}
		select {
		case <-w.ctx.Done():
			return
		case <-ticker.C:
		case _, open := <-wake:
			if !open {
				wake = nil // notifier died: polling continues
			}
		}
	}
}

func (w *Worker) activeCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.active)
}

// startRun launches one run goroutine with its own cancel context,
// heartbeat and cancel watcher.
func (w *Worker) startRun(claimed ClaimedRun) {
	t := w.cfg()
	runCtx, cancel := context.WithCancel(w.ctx)

	w.mu.Lock()
	w.active[claimed.ID] = cancel
	w.mu.Unlock()

	w.Log.Info("agent run claimed", "run_id", claimed.ID, "worker", w.ID,
		"org", claimed.OrgID, "sandbox", claimed.SandboxID, "model", claimed.Model)

	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				w.Log.Error("agent run panicked", "run_id", claimed.ID, "panic", fmt.Sprint(r))
				// The loop died mid-flight: write the terminal row so
				// the run does not wait for the sweeper.
				_ = w.Store.SetFinal(context.WithoutCancel(runCtx), claimed.OrgID, claimed.ID, FinalState{
					Status:    StatusFailed,
					ErrorCode: ErrInternal,
					DoneData:  map[string]any{"status": StatusFailed, "error_code": ErrInternal},
				})
			}
			w.mu.Lock()
			delete(w.active, claimed.ID)
			w.mu.Unlock()
			cancel()
		}()

		// Heartbeat + cancel watcher live only for this run.
		hbDone := make(chan struct{})
		go func() {
			defer close(hbDone)
			w.heartbeatLoop(runCtx, claimed, t.heartbeat, t.lease, t.cancelPoll, cancel)
		}()

		// The one-line metrics log for the finished run lives in
		// executeRun (right after SetFinal — A3.5 plan §11): it is
		// richer (org_id, duration_ms, tool_mode) and the "exactly
		// one line per run" contract means there must not be a
		// second one here.
		_, err := w.executeRun(runCtx, claimed)
		// Wait for the heartbeat goroutine before releasing the slot.
		<-hbDone
		if err != nil {
			w.Log.Error("agent run finalize failed (sweeper will reap)",
				"run_id", claimed.ID, "error", err.Error())
			return
		}
	}()
}

// heartbeatLoop renews the lease and watches for cancellation. Any of
// these cancels the run context:
//   - NOTIFY agent_cancel with this run's id (instant path);
//   - cancel_requested seen by the 1 s poll (fallback path);
//   - a lease renewal rejection (reaped/lost ownership — the loop
//     aborts; SetFinal keeps the first outcome anyway).
func (w *Worker) heartbeatLoop(runCtx context.Context, claimed ClaimedRun, heartbeat, lease, cancelPoll time.Duration, cancel context.CancelFunc) {
	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()
	poller := time.NewTicker(cancelPoll)
	defer poller.Stop()
	var notify <-chan NotifyMsg
	var stopNotify func()
	if w.Notify != nil {
		// Filter by run id in the notifier (A3.2 review): one run's
		// heartbeat is not woken by other runs' events.
		notify, stopNotify = w.Notify.SubscribeRun(claimed.ID)
		defer stopNotify()
	}
	for {
		// Renew first: a fresh run must not be sweepable for a full
		// heartbeat period.
		ok, err := w.Store.RenewLease(runCtx, claimed.OrgID, claimed.ID, w.ID, lease)
		if err != nil || !ok {
			w.Log.Warn("agent lease renewal failed (stopping run)",
				"run_id", claimed.ID, "error", fmt.Sprint(err))
			cancel()
			return
		}
		select {
		case <-runCtx.Done():
			return
		case <-ticker.C:
		case <-poller.C:
			want, err := w.Store.CancelRequested(runCtx, claimed.OrgID, claimed.ID)
			if err == nil && want {
				w.Log.Info("agent run cancel requested (poll)", "run_id", claimed.ID)
				cancel()
				return
			}
		case msg, open := <-notify:
			if !open {
				notify = nil // notifier died: the poll fallback carries on
				continue
			}
			if msg.Channel == NotifyCancel && msg.RunID == claimed.ID {
				w.Log.Info("agent run cancel requested (notify)", "run_id", claimed.ID)
				cancel()
				return
			}
		}
	}
}

// sweepLoop reaps runs whose worker died (lease overdue). Runs are
// never resumed (plan §7) — the user's "continue" creates a new run.
func (w *Worker) sweepLoop(every, grace time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		if w.ctx.Err() != nil {
			return
		}
		swept, err := w.Store.SweepLost(w.ctx, int(grace.Seconds()))
		if err != nil && w.ctx.Err() == nil {
			w.Log.Warn("agent sweep failed", "error", err.Error())
		}
		for _, s := range swept {
			w.Log.Warn("agent run reaped as worker_lost", "run_id", s.ID,
				"org", s.OrgID, "sandbox", s.SandboxID)
		}
		select {
		case <-w.ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Shutdown stops claiming and cancels every active run: first a grace
// period for runs to finish on their own, then the contexts. Marks
// them failed/worker_shutdown through the loop's cancelledResult.
// Used by the standalone subcommand and by main on SIGTERM.
func (w *Worker) Shutdown(grace time.Duration) {
	w.shutdown.set(true)
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if w.activeCount() == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	w.mu.Lock()
	for _, cancel := range w.active {
		cancel()
	}
	w.mu.Unlock()
}

// EnvSource is the env-based fallback LLM source (Chat MVP client
// from PORTAL_DEFAULT_LLM_*): used when the org has no DB connection.
type EnvSource struct {
	Fallback *llm.Client
}

// ErrNoLLM — the run cannot proceed without any model connection.
var ErrNoLLM = fmt.Errorf("no llm connection configured for this org")

// Client implements LLMSource. The env client has no model metadata:
// tools are assumed supported (the fallback flip covers a provider
// that disagrees).
func (e EnvSource) Client(_ context.Context, run Run) (LLMChoice, error) {
	if e.Fallback == nil {
		return LLMChoice{}, ErrNoLLM
	}
	c := *e.Fallback
	c.Model = run.Model
	return LLMChoice{Client: &c, SupportsTools: true}, nil
}
