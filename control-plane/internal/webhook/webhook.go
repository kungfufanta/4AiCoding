// Package webhook delivers sandbox lifecycle events (created, started,
// stopped, deleted, failed) to a configured HTTPS endpoint — the Portal's
// S3a contract (PORTAL_PLAN §4 #4): POST JSON
// {id, event, sandbox_id, external, resources, occurred_at} with
//
//	X-Sandboxd-Signature: sha256=<hmac-sha256(secret, body)>
//	X-Sandboxd-Event-Id:  <event id>
//
// Delivery is asynchronous and retried with exponential backoff until the
// 24-hour retry window closes; a delivery that never succeeds is logged and
// dropped (the app_events timeline keeps the record either way). The
// dispatcher is nil-safe: without SANDBOXD_WEBHOOK_URL nothing is sent.
//
// The signature header authenticates the BODY. A receiver must also enforce
// its own replay window (compare occurred_at / the event id against recent
// deliveries) — the Portal side does this when it consumes the webhook.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/oklog/ulid/v2"
)

// Event names — the `event` field of the payload. Stable contract: add
// new ones, don't rename.
const (
	EventCreated = "created"
	EventStarted = "started"
	EventStopped = "stopped"
	EventDeleted = "deleted"
	EventFailed  = "failed"
)

// Resources is the sandbox's effective resource shape in the payload.
type Resources struct {
	CPUs     float64 `json:"cpus"`
	MemoryMB int     `json:"memory_mb"`
}

// Event is one lifecycle webhook body. External carries the caller's
// identity tags (user_id / project_id / workspace_id) verbatim.
type Event struct {
	ID         string            `json:"id"`
	Event      string            `json:"event"`
	SandboxID  string            `json:"sandbox_id"`
	External   map[string]string `json:"external,omitempty"`
	Resources  *Resources        `json:"resources,omitempty"`
	OccurredAt string            `json:"occurred_at"`
}

// Dispatcher posts signed events to URL. Safe for concurrent use.
type Dispatcher struct {
	URL    string
	Secret string
	Log    *slog.Logger

	// Client delivers the POST. nil = per-attempt 10 s timeout.
	Client *http.Client

	// Retry tunables (exported for tests; zero = defaults).
	backoffBase time.Duration // first retry delay (default 1 s)
	backoffMax  time.Duration // delay cap (default 1 h)
	retryWindow time.Duration // total retry budget (default 24 h)
}

// Enabled reports whether the dispatcher has somewhere to send to.
func (d *Dispatcher) Enabled() bool { return d != nil && d.URL != "" }

// SetTestBackoff overrides the retry schedule for tests: the first delay
// and the total retry budget. Production code never calls this (defaults:
// 1 s base, 24 h window, 1 h per-attempt cap).
func (d *Dispatcher) SetTestBackoff(base, window time.Duration) {
	d.backoffBase, d.retryWindow = base, window
}

// Sign returns the X-Sandboxd-Signature value for body.
func (d *Dispatcher) Sign(body []byte) string {
	mac := hmac.New(sha256.New, []byte(d.Secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// Dispatch delivers ev asynchronously: it returns immediately and the
// caller's request path is never blocked or failed by delivery. A nil
// dispatcher or an empty URL is a no-op.
func (d *Dispatcher) Dispatch(ev Event) {
	if !d.Enabled() {
		return
	}
	if ev.ID == "" {
		ev.ID = ulid.MustNew(ulid.Now(), rand.Reader).String()
	}
	if ev.OccurredAt == "" {
		ev.OccurredAt = time.Now().UTC().Format(time.RFC3339)
	}
	body, err := json.Marshal(ev)
	if err != nil {
		if d.Log != nil {
			d.Log.Warn("webhook: marshal failed (dropping)", "event", ev.Event, "err", err.Error())
		}
		return
	}
	go d.deliver(body)
}

// deliver POSTs body until a 2xx, retrying with exponential backoff until
// the retry window closes. Runs in its own goroutine.
func (d *Dispatcher) deliver(body []byte) {
	ctx := context.Background()
	base := d.backoffBase
	if base <= 0 {
		base = time.Second
	}
	maxWait := d.backoffMax
	if maxWait <= 0 {
		maxWait = time.Hour
	}
	window := d.retryWindow
	if window <= 0 {
		window = 24 * time.Hour
	}
	deadline := time.Now().Add(window)

	sig := d.Sign(body)
	for attempt := 0; ; attempt++ {
		if d.post(ctx, sig, body) {
			return
		}
		wait := base << attempt // 1s, 2s, 4s, …
		if wait > maxWait || wait <= 0 {
			wait = maxWait
		}
		if time.Now().Add(wait).After(deadline) {
			if d.Log != nil {
				d.Log.Warn("webhook: retry window exhausted, dropping event",
					"event_id", eventIDOf(body), "attempts", attempt+1)
			}
			return
		}
		time.Sleep(wait)
	}
}

// post performs one delivery attempt. true = accepted (any 2xx), or a
// permanent misconfiguration that must not be retried.
func (d *Dispatcher) post(ctx context.Context, sig string, body []byte) bool {
	client := d.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.URL, bytes.NewReader(body))
	if err != nil {
		if d.Log != nil {
			d.Log.Warn("webhook: bad URL, dropping event", "err", err.Error())
		}
		return true // never retry a misconfiguration
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Sandboxd-Signature", sig)
	req.Header.Set("X-Sandboxd-Event-Id", eventIDOf(body))

	resp, err := client.Do(req)
	if err != nil {
		if d.Log != nil {
			d.Log.Warn("webhook: delivery failed (will retry)", "err", err.Error())
		}
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return true
	}
	if d.Log != nil {
		d.Log.Warn("webhook: receiver rejected delivery (will retry)",
			"status", resp.StatusCode)
	}
	return false
}

// eventIDOf extracts the event id from a marshalled body without a second
// marshal. Parsing failure returns "" (the header is then omitted).
func eventIDOf(body []byte) string {
	var probe struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(body, &probe) != nil {
		return ""
	}
	return probe.ID
}

// String renders the dispatcher state for startup logs without leaking the
// secret.
func (d *Dispatcher) String() string {
	if d == nil || d.URL == "" {
		return "disabled"
	}
	if d.Secret == "" {
		return "enabled (secret NOT SET — receiver cannot verify signatures)"
	}
	return "enabled (secret set)"
}
