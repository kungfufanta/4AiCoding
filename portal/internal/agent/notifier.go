// notifier.go — LISTEN/NOTIFY wake-up bus (decision §2-8: "LISTEN/
// NOTIFY ... Fallback poll 1 s khi LISTEN lỗi. Replay luôn đọc từ
// bảng, NOTIFY chỉ để đánh thức").
//
// One dedicated connection (never taken from the RLS pool) LISTENs on
// the three channels the migration triggers and the API write to:
//
//	agent_events      payload "<run_id>:<seq>"  — a new event exists
//	agent_runs_queued payload "<run_id>"        — a run is queued
//	agent_cancel      payload "<run_id>"        — cancel was requested
//
// Subscribers can filter by run id: the broadcast drops messages for
// other runs BEFORE they reach the channel, so one SSE follower (or
// one run's heartbeat) is not woken by every event of every run — it
// re-reads the table only when its own run has something new. A Portal
// at this scale has few concurrent runs; per-run subscriptions still
// cost nothing extra (they share the one LISTEN connection). If LISTEN
// dies the connection is redialled with a small backoff — and every
// consumer keeps its own poll timer, so a notify blackout degrades
// latency, not correctness.
package agent

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Channels the migration triggers + RequestCancel publish on.
const (
	NotifyEvents = "agent_events"
	NotifyQueued = "agent_runs_queued"
	NotifyCancel = "agent_cancel"
)

// NotifyMsg is one notification payload.
type NotifyMsg struct {
	Channel string // agent_events | agent_runs_queued | agent_cancel
	Payload string
	// RunID is the payload's leading UUID for every channel; the
	// agent_events payload is "<run_id>:<seq>".
	RunID string
}

// Notifier fans LISTEN notifications out to subscribers.
type Notifier struct {
	log *slog.Logger

	mu     sync.Mutex
	subs   map[int]sub
	nextID int
	closed bool
}

// sub is one subscriber: its wake channel plus the optional run-id
// filter ("" = receive every notification). Filtering here — at the
// broadcast — keeps unrelated events from waking a subscriber that
// would only re-read the table for its own run.
type sub struct {
	ch     chan NotifyMsg
	runIDs map[string]bool // empty set = no filter
}

// NewNotifier opens its own connection, starts LISTEN and keeps it
// alive until ctx is done. listengURL is the raw Postgres URL (the
// connection deliberately skips the pool's SET ROLE: LISTEN needs no
// table privileges and must survive pool churn).
func NewNotifier(ctx context.Context, log *slog.Logger, listenURL string) *Notifier {
	n := &Notifier{log: log, subs: map[int]sub{}}
	go n.loop(ctx, listenURL)
	return n
}

// Subscribe returns a channel that receives EVERY notification until
// the returned cancel func is called (or the notifier's ctx ends).
// The claim loop uses this — queued runs carry any run id. The channel
// is buffered; a slow reader simply coalesces wakes — the consumer
// always re-reads the table.
func (n *Notifier) Subscribe() (<-chan NotifyMsg, func()) {
	return n.subscribe(nil)
}

// SubscribeRun returns a channel that receives only notifications for
// the given run (payload "<run_id>" or "<run_id>:<seq>"). One SSE
// follower of one run uses this: events of other runs no longer wake
// it into a pointless re-read. Multiple run ids may be passed (e.g. a
// follower of a whole conversation).
func (n *Notifier) SubscribeRun(runIDs ...string) (<-chan NotifyMsg, func()) {
	set := make(map[string]bool, len(runIDs))
	for _, id := range runIDs {
		if id != "" {
			set[id] = true
		}
	}
	return n.subscribe(set)
}

// subscribe registers one subscriber (runIDs nil = no filter).
func (n *Notifier) subscribe(runIDs map[string]bool) (<-chan NotifyMsg, func()) {
	n.mu.Lock()
	defer n.mu.Unlock()
	id := n.nextID
	n.nextID++
	ch := make(chan NotifyMsg, 64)
	n.subs[id] = sub{ch: ch, runIDs: runIDs}
	return ch, func() {
		n.mu.Lock()
		defer n.mu.Unlock()
		if s, ok := n.subs[id]; ok {
			delete(n.subs, id)
			close(s.ch)
		}
	}
}

// broadcast delivers msg to every subscriber whose run filter (if
// any) matches; a full subscriber channel drops the wake (poll
// fallback covers it).
func (n *Notifier) broadcast(msg NotifyMsg) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return
	}
	for _, s := range n.subs {
		if len(s.runIDs) > 0 && !s.runIDs[msg.RunID] {
			continue // another run's event: do not wake this subscriber
		}
		select {
		case s.ch <- msg:
		default:
		}
	}
}

// Close marks the notifier dead and drains subscribers so consumers
// fall back to polling instead of waiting for a wake that never
// comes (used by tests and process shutdown; the ctx end also calls
// it).
func (n *Notifier) Close() { n.close() }

// close marks the notifier dead and drains subscribers so consumers
// fall back to polling instead of waiting for a wake that never comes.
func (n *Notifier) close() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return
	}
	n.closed = true
	for id, s := range n.subs {
		close(s.ch)
		delete(n.subs, id)
	}
}

func (n *Notifier) loop(ctx context.Context, listenURL string) {
	defer n.close()
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		conn, err := pgx.Connect(ctx, listenURL)
		if err != nil {
			n.log.Warn("agent notifier connect failed (poll fallback still works)",
				"error", err.Error())
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 15*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		if err := n.listenAll(ctx, conn); err != nil {
			_ = conn.Close(ctx)
			continue
		}
		// Graceful end (ctx cancelled) — stop; otherwise the LISTEN
		// died and we redial.
		_ = conn.Close(ctx)
		if ctx.Err() != nil {
			return
		}
	}
}

// listenAll subscribes the connection and forwards notifications until
// the connection or the process ctx dies.
func (n *Notifier) listenAll(ctx context.Context, conn *pgx.Conn) error {
	for _, ch := range []string{NotifyEvents, NotifyQueued, NotifyCancel} {
		if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{ch}.Sanitize()); err != nil {
			n.log.Warn("agent notifier LISTEN failed", "channel", ch, "error", err.Error())
			return err
		}
	}
	n.log.Info("agent notifier listening",
		"channels", strings.Join([]string{NotifyEvents, NotifyQueued, NotifyCancel}, ","))
	for {
		notification, err := conn.WaitForNotification(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			n.log.Warn("agent notifier lost (redialling)", "error", err.Error())
			return err
		}
		n.broadcast(NotifyMsg{
			Channel: notification.Channel,
			Payload: notification.Payload,
			RunID:   strings.SplitN(notification.Payload, ":", 2)[0],
		})
	}
}
