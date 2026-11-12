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
// Subscribers get every notification and filter by payload (a Portal
// at this scale has few concurrent runs; per-run subscriptions would
// cost one LISTEN connection each). If LISTEN dies the connection is
// redialled with a small backoff — and every consumer keeps its own
// poll timer, so a notify blackout degrades latency, not correctness.
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
	subs   map[int]chan NotifyMsg
	nextID int
	closed bool
}

// NewNotifier opens its own connection, starts LISTEN and keeps it
// alive until ctx is done. listengURL is the raw Postgres URL (the
// connection deliberately skips the pool's SET ROLE: LISTEN needs no
// table privileges and must survive pool churn).
func NewNotifier(ctx context.Context, log *slog.Logger, listenURL string) *Notifier {
	n := &Notifier{log: log, subs: map[int]chan NotifyMsg{}}
	go n.loop(ctx, listenURL)
	return n
}

// Subscribe returns a channel that receives notifications until the
// returned cancel func is called (or the notifier's ctx ends). The
// channel is buffered; a slow reader simply coalesces wakes — the
// consumer always re-reads the table.
func (n *Notifier) Subscribe() (<-chan NotifyMsg, func()) {
	n.mu.Lock()
	defer n.mu.Unlock()
	id := n.nextID
	n.nextID++
	ch := make(chan NotifyMsg, 64)
	n.subs[id] = ch
	return ch, func() {
		n.mu.Lock()
		defer n.mu.Unlock()
		if c, ok := n.subs[id]; ok {
			delete(n.subs, id)
			close(c)
		}
	}
}

// broadcast delivers msg to every subscriber; a full subscriber
// channel drops the wake (poll fallback covers it).
func (n *Notifier) broadcast(msg NotifyMsg) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return
	}
	for _, ch := range n.subs {
		select {
		case ch <- msg:
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
	for id, ch := range n.subs {
		close(ch)
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
