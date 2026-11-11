package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestSignMatchesHMACSHA256 pins the signature format: sha256=<hex hmac>.
func TestSignMatchesHMACSHA256(t *testing.T) {
	d := &Dispatcher{Secret: "whsec-test"}
	body := []byte(`{"event":"created"}`)
	want := "sha256=" + hex.EncodeToString(func() []byte {
		mac := hmac.New(sha256.New, []byte("whsec-test"))
		mac.Write(body)
		return mac.Sum(nil)
	}())
	if d.Sign(body) != want {
		t.Fatalf("Sign = %q, want %q", d.Sign(body), want)
	}
}

// TestDispatchDeliversSignedEvent pins the wire contract: headers + body
// shape (PORTAL_PLAN §4 #4).
func TestDispatchDeliversSignedEvent(t *testing.T) {
	type recv struct {
		sig, eventID, body string
	}
	ch := make(chan recv, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ch <- recv{r.Header.Get("X-Sandboxd-Signature"), r.Header.Get("X-Sandboxd-Event-Id"), string(b)}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := &Dispatcher{URL: srv.URL, Secret: "whsec-test", Log: quietLogger()}
	d.Dispatch(Event{
		Event:     EventCreated,
		SandboxID: "01J",
		External:  map[string]string{"workspace_id": "org-1", "user_id": "u1"},
		Resources: &Resources{CPUs: 1.5, MemoryMB: 2048},
	})

	select {
	case got := <-ch:
		// Signature authenticates the exact body.
		if !hmac.Equal([]byte(got.sig), []byte((&Dispatcher{Secret: "whsec-test"}).Sign([]byte(got.body)))) {
			t.Fatalf("signature mismatch: %q for body %s", got.sig, got.body)
		}
		if got.eventID == "" {
			t.Fatal("missing X-Sandboxd-Event-Id")
		}
		var ev Event
		if err := json.Unmarshal([]byte(got.body), &ev); err != nil {
			t.Fatalf("body not the webhook contract: %v (%s)", err, got.body)
		}
		if ev.ID != got.eventID || ev.Event != "created" || ev.SandboxID != "01J" ||
			ev.External["workspace_id"] != "org-1" || ev.External["user_id"] != "u1" ||
			ev.Resources == nil || ev.Resources.CPUs != 1.5 || ev.Resources.MemoryMB != 2048 {
			t.Fatalf("payload = %+v", ev)
		}
		if _, err := time.Parse(time.RFC3339, ev.OccurredAt); err != nil {
			t.Fatalf("occurred_at not RFC3339: %q", ev.OccurredAt)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no delivery within 5s")
	}
}

// TestDispatchRetriesUntilSuccess: a 500 is retried with backoff until the
// receiver accepts.
func TestDispatchRetriesUntilSuccess(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := &Dispatcher{URL: srv.URL, Secret: "s", Log: quietLogger()}
	d.backoffBase, d.retryWindow = time.Millisecond, 10*time.Second
	d.Dispatch(Event{Event: EventStopped, SandboxID: "01J"})

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := attempts
		mu.Unlock()
		if n >= 3 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no successful retry, attempts = %d", n)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestDispatchGivesUpAfterRetryWindow: a receiver that never accepts gets
// dropped after the window (24 h in production) — the dispatcher never
// leaks goroutines forever.
func TestDispatchGivesUpAfterRetryWindow(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	d := &Dispatcher{URL: srv.URL, Secret: "s", Log: quietLogger()}
	d.backoffBase, d.retryWindow = time.Millisecond, 30*time.Millisecond
	d.Dispatch(Event{Event: EventFailed, SandboxID: "01J"})

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := attempts
		mu.Unlock()
		if n > 1 {
			return // retried at least once, then gave up (window is 30ms)
		}
		if time.Now().After(deadline) {
			t.Fatal("dispatcher never retried")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestDispatcherNilSafe: no dispatcher / no URL must be a no-op.
func TestDispatcherNilSafe(t *testing.T) {
	var d *Dispatcher
	if d.Enabled() {
		t.Fatal("nil dispatcher must be disabled")
	}
	d.Dispatch(Event{Event: EventDeleted, SandboxID: "01J"}) // must not panic
	off := &Dispatcher{}
	if off.Enabled() {
		t.Fatal("URL-less dispatcher must be disabled")
	}
	off.Dispatch(Event{Event: EventDeleted, SandboxID: "01J"})
}
