// notifier_test.go — the A3.2 review fix: subscribers filter by run
// id IN the notifier, so one SSE follower (or one run's heartbeat) is
// not woken by every event of every run.
package agent

import (
	"testing"
	"time"
)

// counter totals the wakes one subscriber received.
type counter struct {
	ch <-chan NotifyMsg
	n  int
}

func (c *counter) collect() {
	for {
		select {
		case _, ok := <-c.ch:
			if !ok {
				return
			}
			c.n++
		default:
			return
		}
	}
}

func TestNotifierRunFilter(t *testing.T) {
	n := &Notifier{log: testLogger(), subs: map[int]sub{}}

	all := &counter{ch: newSub(n, nil)}
	a := &counter{ch: newSub(n, []string{"run-a"})}
	b := &counter{ch: newSub(n, []string{"run-b"})}
	ab := &counter{ch: newSub(n, []string{"run-a", "run-b"})}

	// An agent_events notify for run A wakes the unfiltered follower,
	// run A's follower and the two-run follower — NOT run B's.
	n.broadcast(NotifyMsg{Channel: NotifyEvents, Payload: "run-a:7", RunID: "run-a"})
	waitFor(t, all, 1, "unfiltered follower")
	waitFor(t, a, 1, "run-a follower")
	waitFor(t, ab, 1, "two-run follower")
	b.collect()
	if b.n != 0 {
		t.Fatalf("run-b follower woken by run-a event (%d times)", b.n)
	}

	// A queued-run notify for run B.
	n.broadcast(NotifyMsg{Channel: NotifyQueued, Payload: "run-b", RunID: "run-b"})
	waitFor(t, b, 1, "run-b follower")
	waitFor(t, all, 2, "unfiltered follower")
	waitFor(t, ab, 2, "two-run follower")
	a.collect()
	if a.n != 1 {
		t.Fatalf("run-a follower got extra wakes (%d total)", a.n)
	}

	// An unknown run: only the unfiltered follower.
	n.broadcast(NotifyMsg{Channel: NotifyEvents, Payload: "run-x:1", RunID: "run-x"})
	waitFor(t, all, 3, "unfiltered follower")
	ab.collect()
	if ab.n != 2 {
		t.Fatalf("two-run follower got extra wakes (%d total)", ab.n)
	}
}

func waitFor(t *testing.T, c *counter, want int, name string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		c.collect()
		if c.n >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%s received %d wakes, want %d", name, c.n, want)
}

// newSub registers a subscriber directly (broadcast is package
// internal; tests bypass the LISTEN connection).
func newSub(n *Notifier, runIDs []string) <-chan NotifyMsg {
	ch, stop := n.SubscribeRun(runIDs...)
	// SubscribeRun returns a cancel func we must keep alive for the
	// test's lifetime — store it on the notifier's GC by never calling
	// stop: the test Notifier is not started (no LISTEN loop), so
	// nothing else closes the channel.
	_ = stop
	return ch
}

// TestSubscribeRunNoIDs — a SubscribeRun with no ids behaves like an
// unfiltered subscribe (defensive; nothing passes zero ids today).
func TestSubscribeRunNoIDs(t *testing.T) {
	n := &Notifier{log: testLogger(), subs: map[int]sub{}}
	ch := newSub(n, nil)
	n.broadcast(NotifyMsg{Channel: NotifyEvents, Payload: "r:1", RunID: "r"})
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("SubscribeRun() with zero ids filtered everything out")
	}
}
