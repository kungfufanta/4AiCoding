// Package fleet is the control-plane logic for the worker fleet (M6,
// PLAN §2 "Chế độ B" + §9): membership states, the placement decision,
// and the HTTP client a worker process uses to register and heartbeat.
//
// M6 boundary: the ONLY node that executes sandboxes is the control
// plane itself (role 'local'). Workers register, heartbeat, report
// capacity and can be drained — their placements become executable in
// M7 when the worker runtime lands (k3s B1). The Executor decision below
// is already fleet-shaped so M7 extends it, not rewrites it.
package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

// Re-exported so handlers/CLI code never import store for the constants.
const (
	RoleLocal   = store.RoleLocal
	RoleWorker  = store.RoleWorker
	DrainActive = store.DrainActive
	DrainDrain  = store.DrainDrain
)

// Node statuses (derived, not stored): a worker is online while its last
// heartbeat is within the lease; the local node is always online (we are
// answering, therefore it is up).
const (
	StatusOnline  = "online"
	StatusOffline = "offline"
)

// DefaultLease is how long after the last heartbeat a worker is still
// considered online. Workers heartbeat every 10s by default, so the lease
// tolerates 8 missed beats before the fleet marks them offline.
const DefaultLease = 90 * time.Second

// DrainError is the create-time refusal when the node that would run the
// sandbox is draining (PLAN M6: "chặn tạo sandbox khi node đang drain").
type DrainError struct {
	NodeID string
	Role   string
}

func (e *DrainError) Error() string {
	return fmt.Sprintf("node %s is draining; new sandboxes are refused", e.NodeID)
}

// Status derives a node's liveness. Pure so tests can inject the clock.
func Status(n *store.Node, now time.Time, lease time.Duration) string {
	if n.Role == RoleLocal {
		return StatusOnline
	}
	if lease <= 0 {
		lease = DefaultLease
	}
	if n.HeartbeatAt.IsZero() {
		return StatusOffline
	}
	if now.Sub(n.HeartbeatAt) <= lease {
		return StatusOnline
	}
	return StatusOffline
}

// nodeStore is the slice of the store the placement decision needs.
type nodeStore interface {
	ListNodes(ctx context.Context) ([]*store.Node, error)
}

// Executor returns the node a new sandbox must be placed on.
//
//   - Empty fleet (no rows): returns "" — unplaced. Unit tests and the
//     boot window before EnsureLocalNode keep working.
//   - The role='local' node is the M6 executor. Drained → DrainError
//     (the caller turns that into a 503, and creation is blocked).
//   - Workers are ignored for placement in M6 (see package comment);
//     their drain state is enforced at the API/fleet level so M7 only
//     widens the candidate set.
func Executor(ctx context.Context, st nodeStore) (string, error) {
	rows, err := st.ListNodes(ctx)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", nil
	}
	for _, n := range rows {
		if n.Role != RoleLocal {
			continue
		}
		if n.DrainState == DrainDrain {
			return "", &DrainError{NodeID: n.NodeID, Role: n.Role}
		}
		return n.NodeID, nil
	}
	return "", nil
}

// UsageOf resolves a node's effective usage: the worker's own report when
// it carries one, else the placement-derived sums from the store.
func UsageOf(n *store.Node, derived store.Usage) store.Usage {
	if n.Used != (store.Capacity{}) {
		return store.Usage{
			Sandboxes: derived.Sandboxes,
			CPUs:      n.Used.CPUs,
			MemoryMB:  n.Used.MemoryMB,
			DiskMB:    n.Used.DiskMB,
		}
	}
	return derived
}

// Totals aggregates the fleet for GET /v1/capacity.
type Totals struct {
	Capacity store.Capacity
	Used     store.Usage
	Nodes    int `json:"nodes"`
	Online   int `json:"online"`
	Draining int `json:"draining"`
	Offline  int `json:"offline"`
}

// Sum computes fleet totals over the given nodes (usage from the
// placement-derived map, adjusted per node by UsageOf).
func Sum(nodes []*store.Node, usage map[string]store.Usage, now time.Time, lease time.Duration) Totals {
	var t Totals
	for _, n := range nodes {
		t.Nodes++
		switch Status(n, now, lease) {
		case StatusOnline:
			t.Online++
		default:
			t.Offline++
		}
		if n.DrainState == DrainDrain {
			t.Draining++
		}
		t.Capacity.CPUs += n.Capacity.CPUs
		t.Capacity.MemoryMB += n.Capacity.MemoryMB
		t.Capacity.DiskMB += n.Capacity.DiskMB
		u := UsageOf(n, usage[n.NodeID])
		t.Used.Sandboxes += u.Sandboxes
		t.Used.CPUs += u.CPUs
		t.Used.MemoryMB += u.MemoryMB
		t.Used.DiskMB += u.DiskMB
	}
	return t
}

// --- worker-side client (used by `sandboxd worker`) --------------------

// Client is the fleet client a worker process talks to the control plane
// with. It is deliberately tiny: register + heartbeat, both with the
// worker token.
type Client struct {
	BaseURL string // e.g. http://sandboxd-ctl:9000
	Token   string // SANDBOXD_WORKER_TOKEN on the control plane
	HTTP    *http.Client
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

// Register announces the worker to the fleet (idempotent — an existing
// node is refreshed, its drain state preserved server-side).
func (c *Client) Register(ctx context.Context, req RegisterRequest) (*RegisterResponse, error) {
	return postJSON[*RegisterResponse](ctx, c, "/internal/nodes/register", req)
}

// Heartbeat stamps liveness and returns the node's desired state — in
// particular drain_state, so a drained worker learns it must not accept
// new sandboxes.
func (c *Client) Heartbeat(ctx context.Context, req HeartbeatRequest) (*HeartbeatResponse, error) {
	return postJSON[*HeartbeatResponse](ctx, c, "/internal/nodes/heartbeat", req)
}

type RegisterRequest struct {
	NodeID   string         `json:"node_id"`
	Address  string         `json:"address,omitempty"`
	Version  string         `json:"version,omitempty"`
	Capacity store.Capacity `json:"capacity,omitempty"`
	// APIToken (M7b) is the credential the control plane must use to call
	// this worker's API. Only executor workers send one; once stored, a
	// re-register of the same node id must present the same token.
	APIToken string `json:"api_token,omitempty"`
}

type RegisterResponse struct {
	NodeID     string `json:"node_id"`
	DrainState string `json:"drain_state"`
	Now        string `json:"now"`
}

type HeartbeatRequest struct {
	NodeID   string          `json:"node_id"`
	Capacity *store.Capacity `json:"capacity,omitempty"`
	Used     *store.Capacity `json:"used,omitempty"`
}

type HeartbeatResponse struct {
	NodeID     string `json:"node_id"`
	DrainState string `json:"drain_state"`
	Now        string `json:"now"`
	// Policy (M7b) is the control plane's admin policy. An executor worker
	// applies it, so sandboxes on every node obey the same policy.
	Policy json.RawMessage `json:"policy,omitempty"`
}

// APIError is a non-2xx from the control plane.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("control plane returned %d: %s", e.Status, e.Body)
}

func postJSON[T any](ctx context.Context, c *Client, path string, body any) (T, error) {
	var zero T
	b, err := json.Marshal(body)
	if err != nil {
		return zero, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(b))
	if err != nil {
		return zero, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return zero, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return zero, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return zero, &APIError{Status: resp.StatusCode, Body: string(raw)}
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		return zero, fmt.Errorf("decode %s response: %w", path, err)
	}
	return out, nil
}

// ErrReRegister reports a heartbeat for a node the control plane does not
// know (it lost its DB, or GC'd the node); the worker must register again.
var ErrReRegister = errors.New("unknown node: register again")
