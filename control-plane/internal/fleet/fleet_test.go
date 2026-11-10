package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

// stubNodeStore answers ListNodes from a fixed slice.
type stubNodeStore struct {
	nodes []*store.Node
}

func (s stubNodeStore) ListNodes(context.Context) ([]*store.Node, error) { return s.nodes, nil }

func localNode(drain string) *store.Node {
	return &store.Node{NodeID: "local", Role: RoleLocal, DrainState: drain,
		HeartbeatAt: time.Now()}
}

func workerNode(id, drain string, hbAge time.Duration) *store.Node {
	return &store.Node{NodeID: id, Role: RoleWorker, DrainState: drain,
		HeartbeatAt: time.Now().Add(-hbAge)}
}

func TestExecutor(t *testing.T) {
	ctx := context.Background()

	t.Run("empty fleet is unplaced", func(t *testing.T) {
		id, err := Executor(ctx, stubNodeStore{})
		if err != nil || id != "" {
			t.Fatalf("Executor = %q, %v; want \"\", nil", id, err)
		}
	})

	t.Run("active local node is the executor", func(t *testing.T) {
		id, err := Executor(ctx, stubNodeStore{nodes: []*store.Node{
			localNode(DrainActive), workerNode("w1", DrainActive, 0),
		}})
		if err != nil || id != "local" {
			t.Fatalf("Executor = %q, %v; want local, nil", id, err)
		}
	})

	t.Run("drained local node blocks placement", func(t *testing.T) {
		id, err := Executor(ctx, stubNodeStore{nodes: []*store.Node{localNode(DrainDrain)}})
		var drainErr *DrainError
		if !errors.As(err, &drainErr) {
			t.Fatalf("want DrainError, got %v", err)
		}
		if drainErr.NodeID != "local" {
			t.Errorf("DrainError.NodeID = %q, want local", drainErr.NodeID)
		}
		if id != "" {
			t.Errorf("Executor returned node %q on drain", id)
		}
	})

	t.Run("fleet without a local row is unplaced", func(t *testing.T) {
		id, err := Executor(ctx, stubNodeStore{nodes: []*store.Node{
			workerNode("w1", DrainActive, 0), workerNode("w2", DrainDrain, 0),
		}})
		if err != nil || id != "" {
			t.Fatalf("Executor = %q, %v; want \"\", nil (M6: workers are not executors)", id, err)
		}
	})
}

func TestStatus(t *testing.T) {
	now := time.Now()
	local := localNode(DrainActive)
	if got := Status(local, now, DefaultLease); got != StatusOnline {
		t.Errorf("local node status = %q, want always online", got)
	}
	young := workerNode("w1", DrainActive, 10*time.Second)
	if got := Status(young, now, DefaultLease); got != StatusOnline {
		t.Errorf("fresh heartbeat status = %q, want online", got)
	}
	stale := workerNode("w2", DrainActive, DefaultLease+time.Second)
	if got := Status(stale, now, DefaultLease); got != StatusOffline {
		t.Errorf("stale heartbeat status = %q, want offline", got)
	}
	// A zero lease falls back to the default instead of everything-offline.
	if got := Status(young, now, 0); got != StatusOnline {
		t.Errorf("zero lease status = %q, want online via default lease", got)
	}
}

func TestSum(t *testing.T) {
	now := time.Now()
	nodes := []*store.Node{
		func() *store.Node {
			n := localNode(DrainActive)
			n.Capacity = store.Capacity{CPUs: 8, MemoryMB: 16384}
			return n
		}(),
		func() *store.Node {
			n := workerNode("w1", DrainDrain, 0)
			n.Capacity = store.Capacity{CPUs: 4, MemoryMB: 8192, DiskMB: 1024}
			return n
		}(),
		workerNode("w2", DrainActive, DefaultLease+time.Minute), // offline
	}
	usage := map[string]store.Usage{
		"local": {Sandboxes: 2, CPUs: 1.5, MemoryMB: 2048},
		"w1":    {Sandboxes: 1, MemoryMB: 512},
	}
	got := Sum(nodes, usage, now, DefaultLease)
	if got.Nodes != 3 || got.Online != 2 || got.Offline != 1 || got.Draining != 1 {
		t.Errorf("totals = %+v, want nodes=3 online=2 offline=1 draining=1", got)
	}
	if got.Capacity.CPUs != 12 || got.Capacity.MemoryMB != 24576 || got.Capacity.DiskMB != 1024 {
		t.Errorf("capacity totals = %+v", got.Capacity)
	}
	if got.Used.Sandboxes != 3 || got.Used.CPUs != 1.5 || got.Used.MemoryMB != 2560 {
		t.Errorf("used totals = %+v", got.Used)
	}
}

func TestUsageOfPrefersWorkerReport(t *testing.T) {
	n := workerNode("w1", DrainActive, 0)
	n.Used = store.Capacity{CPUs: 2, MemoryMB: 4096}
	got := UsageOf(n, store.Usage{Sandboxes: 5, MemoryMB: 111})
	if got.CPUs != 2 || got.MemoryMB != 4096 || got.Sandboxes != 5 {
		t.Errorf("UsageOf = %+v, want worker-reported cpus/memory with derived sandboxes", got)
	}
	n.Used = store.Capacity{}
	got = UsageOf(n, store.Usage{Sandboxes: 5, MemoryMB: 111})
	if got.MemoryMB != 111 {
		t.Errorf("UsageOf fallback = %+v, want derived", got)
	}
}

func TestClientRegisterAndHeartbeat(t *testing.T) {
	var gotAuth, gotPath string
	var reg RegisterRequest
	var beat HeartbeatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		switch r.URL.Path {
		case "/internal/nodes/register":
			_ = json.NewDecoder(r.Body).Decode(&reg)
			_ = json.NewEncoder(w).Encode(RegisterResponse{NodeID: reg.NodeID, DrainState: DrainActive, Now: "now"})
		case "/internal/nodes/heartbeat":
			_ = json.NewDecoder(r.Body).Decode(&beat)
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(HeartbeatResponse{NodeID: beat.NodeID, DrainState: DrainDrain, Now: "now"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, Token: "wsecret"}
	regResp, err := c.Register(context.Background(), RegisterRequest{
		NodeID: "w1", Capacity: store.Capacity{CPUs: 2, MemoryMB: 4096},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if regResp.NodeID != "w1" || regResp.DrainState != DrainActive {
		t.Errorf("register response = %+v", regResp)
	}
	if gotPath != "/internal/nodes/register" || gotAuth != "Bearer wsecret" {
		t.Errorf("register request = %s %s", gotPath, gotAuth)
	}
	if reg.Capacity.MemoryMB != 4096 {
		t.Errorf("register body capacity = %+v", reg.Capacity)
	}

	beatResp, err := c.Heartbeat(context.Background(), HeartbeatRequest{NodeID: "w1"})
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if beatResp.DrainState != DrainDrain {
		t.Errorf("heartbeat drain state = %q, want drain", beatResp.DrainState)
	}
}

func TestClientAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"unknown node: register again"}}`))
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Token: "wsecret"}
	_, err := c.Heartbeat(context.Background(), HeartbeatRequest{NodeID: "ghost"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound {
		t.Fatalf("want APIError 404, got %v", err)
	}
}
