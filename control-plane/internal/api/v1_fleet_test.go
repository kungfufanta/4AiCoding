package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/auth"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/docker"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/fleet"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/loopback"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

// nodeServer returns a Server whose store has a role='local' node plus
// optional workers already registered.
func nodeServer(t *testing.T, workers ...string) (*Server, string) {
	t.Helper()
	st := fileStore(t)
	t.Cleanup(func() { st.Close() })
	lm := loopback.New()
	lm.Root = t.TempDir()
	s := &Server{Store: st, Loopback: lm, Docker: docker.NewClient(),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := st.UpsertNode(context.Background(), &store.Node{
		NodeID: "local", Role: store.RoleLocal, Capacity: store.Capacity{CPUs: 8, MemoryMB: 16384},
	}); err != nil {
		t.Fatal(err)
	}
	for _, w := range workers {
		if err := st.UpsertNode(context.Background(), &store.Node{
			NodeID: w, Role: store.RoleWorker, Capacity: store.Capacity{CPUs: 4, MemoryMB: 8192},
		}); err != nil {
			t.Fatal(err)
		}
	}
	return s, "local"
}

// internalCall invokes an /internal handler with the worker actor the auth
// middleware attaches for a valid worker token (the middleware itself is
// covered by internal/auth tests).
func internalCall(h http.HandlerFunc, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/internal/nodes/x", strings.NewReader(body))
	r = r.WithContext(auth.WithActor(r.Context(), auth.Actor{
		Kind: auth.ActorWorker, Name: "worker", Role: "worker",
	}))
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

func TestInternalNodeRegister(t *testing.T) {
	s, _ := nodeServer(t)

	// Valid register creates the node with its capacity.
	w := internalCall(s.internalNodeRegister, `{"node_id":"w1","address":"http://w1:9000","version":"m6","capacity":{"cpus":4,"memory_mb":8192,"disk_mb":1024}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("register = %d: %s", w.Code, w.Body)
	}
	var rr struct {
		NodeID     string `json:"node_id"`
		DrainState string `json:"drain_state"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &rr)
	if rr.NodeID != "w1" || rr.DrainState != store.DrainActive {
		t.Errorf("register response = %+v", rr)
	}
	n, err := s.Store.GetNode(context.Background(), "w1")
	if err != nil {
		t.Fatalf("node not stored: %v", err)
	}
	if n.Capacity.CPUs != 4 || n.Capacity.MemoryMB != 8192 || n.Capacity.DiskMB != 1024 {
		t.Errorf("capacity = %+v", n.Capacity)
	}
	if n.Role != store.RoleWorker || n.Address != "http://w1:9000" {
		t.Errorf("node = %+v", n)
	}

	// Re-register refreshes capacity but keeps the drain state.
	if err := s.Store.SetNodeDrain(context.Background(), "w1", store.DrainDrain); err != nil {
		t.Fatal(err)
	}
	w = internalCall(s.internalNodeRegister, `{"node_id":"w1","capacity":{"cpus":2}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("re-register = %d: %s", w.Code, w.Body)
	}
	n, _ = s.Store.GetNode(context.Background(), "w1")
	if n.DrainState != store.DrainDrain || n.Capacity.CPUs != 2 {
		t.Errorf("after re-register: drain=%q cpus=%v, want drain preserved + capacity refreshed",
			n.DrainState, n.Capacity.CPUs)
	}

	// Validation: bad ids and unknown fields.
	for _, body := range []string{
		`{"node_id":"BAD ID"}`,
		`{"node_id":""}`,
		`{"node_id":"w2","capacity":{"cpus":-1}}`,
		`{"node_id":"w2","nope":1}`,
		`not json`,
	} {
		w := internalCall(s.internalNodeRegister, body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("register %s = %d, want 400", body, w.Code)
		}
	}
}

func TestInternalNodeHeartbeat(t *testing.T) {
	s, _ := nodeServer(t, "w1")

	if err := s.Store.SetNodeDrain(context.Background(), "w1", store.DrainDrain); err != nil {
		t.Fatal(err)
	}
	w := internalCall(s.internalNodeHeartbeat, `{"node_id":"w1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("heartbeat = %d: %s", w.Code, w.Body)
	}
	var hr struct {
		DrainState string `json:"drain_state"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &hr)
	if hr.DrainState != store.DrainDrain {
		t.Errorf("heartbeat drain state = %q, want the worker to learn it is drained", hr.DrainState)
	}

	// Unknown node -> 404 so the worker re-registers.
	w = internalCall(s.internalNodeHeartbeat, `{"node_id":"ghost"}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("heartbeat unknown = %d, want 404", w.Code)
	}
	// Bad payload -> 400.
	w = internalCall(s.internalNodeHeartbeat, `{"node_id":"w1","used":"lots"}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("heartbeat bad payload = %d, want 400", w.Code)
	}
}

func TestV1NodesAndCapacity(t *testing.T) {
	s, _ := nodeServer(t, "w1", "w2")

	// A running sandbox placed on local shows up in usage.
	id := newULID()
	if err := s.Store.Create(context.Background(), &store.Sandbox{
		ID: id, Status: "running", NodeID: "local", ExternalUserID: nullStr("local"),
		Spec: store.Spec{CPUs: 0.5, MemoryMB: 512},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Store.SetNodeDrain(context.Background(), "w2", store.DrainDrain); err != nil {
		t.Fatal(err)
	}
	// w2 stops heartbeating: backdate its heartbeat past the default lease
	// while w1 keeps beating, exactly like a lost worker.
	_, err := s.Store.DB().ExecContext(context.Background(),
		`UPDATE nodes SET heartbeat_at = heartbeat_at - 120 WHERE node_id = 'w2'`)
	if err != nil {
		t.Fatal(err)
	}

	w := v1Call(s.v1ListNodes, "GET", "/v1/nodes", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("list nodes = %d: %s", w.Code, w.Body)
	}
	var list struct {
		Nodes []v1Node `json:"nodes"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Nodes) != 3 {
		t.Fatalf("nodes = %d, want 3", len(list.Nodes))
	}
	if list.Nodes[0].NodeID != "local" || list.Nodes[0].Role != store.RoleLocal {
		t.Errorf("first node = %+v, want local first", list.Nodes[0])
	}
	byID := map[string]v1Node{}
	for _, n := range list.Nodes {
		byID[n.NodeID] = n
	}
	if byID["local"].Status != fleet.StatusOnline {
		t.Errorf("local status = %q, want online", byID["local"].Status)
	}
	if byID["w1"].Status != fleet.StatusOnline || byID["w2"].Status != fleet.StatusOffline {
		t.Errorf("worker statuses = %q / %q, want online / offline", byID["w1"].Status, byID["w2"].Status)
	}
	if byID["w2"].DrainState != store.DrainDrain {
		t.Errorf("w2 drain = %q", byID["w2"].DrainState)
	}
	if byID["local"].Used.Sandboxes != 1 || byID["local"].Used.CPUs != 0.5 || byID["local"].Used.MemoryMB != 512 {
		t.Errorf("local usage = %+v", byID["local"].Used)
	}
	if byID["w1"].Capacity.CPUs != 4 {
		t.Errorf("w1 capacity = %+v", byID["w1"].Capacity)
	}

	// Fleet capacity totals.
	w = v1Call(s.v1FleetCapacity, "GET", "/v1/capacity", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("capacity = %d: %s", w.Code, w.Body)
	}
	var cap struct {
		Capacity v1NodeCapacity `json:"capacity"`
		Used     v1NodeUsage    `json:"used"`
		Nodes    int            `json:"nodes"`
		Online   int            `json:"online"`
		Offline  int            `json:"offline"`
		Draining int            `json:"draining"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &cap)
	if cap.Nodes != 3 || cap.Online != 2 || cap.Offline != 1 || cap.Draining != 1 {
		t.Errorf("capacity totals = %+v", cap)
	}
	if cap.Capacity.CPUs != 16 || cap.Capacity.MemoryMB != 32768 {
		t.Errorf("capacity = %+v", cap.Capacity)
	}
	if cap.Used.Sandboxes != 1 || cap.Used.MemoryMB != 512 {
		t.Errorf("used = %+v", cap.Used)
	}

	// Per-node capacity endpoint.
	w = v1Call(s.v1NodeCapacity, "GET", "/v1/nodes/{id}/capacity", "w1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("node capacity = %d: %s", w.Code, w.Body)
	}
	w = v1Call(s.v1NodeCapacity, "GET", "/v1/nodes/{id}/capacity", "ghost", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("node capacity unknown = %d, want 404", w.Code)
	}
}

func TestDrainUndrainFlow(t *testing.T) {
	s, _ := nodeServer(t, "w1")

	// Drain records state + audit.
	w := v1Call(s.v1DrainNode, "POST", "/v1/nodes/{id}/drain", "w1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("drain = %d: %s", w.Code, w.Body)
	}
	n, _ := s.Store.GetNode(context.Background(), "w1")
	if n.DrainState != store.DrainDrain {
		t.Fatalf("drain state = %q", n.DrainState)
	}
	// Drain is idempotent.
	w = v1Call(s.v1DrainNode, "POST", "/v1/nodes/{id}/drain", "w1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("re-drain = %d", w.Code)
	}
	// Undrain restores.
	w = v1Call(s.v1UndrainNode, "POST", "/v1/nodes/{id}/undrain", "w1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("undrain = %d", w.Code)
	}
	n, _ = s.Store.GetNode(context.Background(), "w1")
	if n.DrainState != store.DrainActive {
		t.Fatalf("undrain state = %q", n.DrainState)
	}
	// Unknown node -> 404.
	w = v1Call(s.v1DrainNode, "POST", "/v1/nodes/{id}/drain", "ghost", "")
	if w.Code != http.StatusNotFound {
		t.Errorf("drain unknown = %d, want 404", w.Code)
	}
}

// TestDrainBlocksCreate pins the M6 rule: when the node that would run the
// sandbox is draining, POST /v1/sandboxes returns 503 and no row is written.
func TestDrainBlocksCreate(t *testing.T) {
	s, _ := nodeServer(t)
	if err := s.Store.SetNodeDrain(context.Background(), "local", store.DrainDrain); err != nil {
		t.Fatal(err)
	}
	w := v1Call(s.v1CreateSandbox, "POST", "/v1/sandboxes", "", `{"ports":[3000]}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("create on drained node = %d: %s, want 503", w.Code, w.Body)
	}
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &e)
	if e.Error.Code != "sandbox_capacity" {
		t.Errorf("503 code = %q, want sandbox_capacity", e.Error.Code)
	}
	if !strings.Contains(e.Error.Message, "draining") || !strings.Contains(e.Error.Message, "local") {
		t.Errorf("503 message = %q, want the node id + drain reason", e.Error.Message)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("503 missing Retry-After")
	}
	rows, _ := s.Store.List(context.Background())
	if len(rows) != 0 {
		t.Errorf("drained create wrote %d rows, want 0", len(rows))
	}

	// Undraining re-opens creation (the request proceeds past placement and
	// fails later on the missing Docker workspace plumbing, NOT with a 503).
	if err := s.Store.SetNodeDrain(context.Background(), "local", store.DrainActive); err != nil {
		t.Fatal(err)
	}
	w = v1Call(s.v1CreateSandbox, "POST", "/v1/sandboxes", "", `{"ports":[3000]}`)
	if w.Code == http.StatusServiceUnavailable {
		t.Fatalf("create after undrain = 503: %s", w.Body)
	}
}

// TestCreateRecordsPlacement covers the happy path end to end at the unit
// level by driving handleCreate's placement through the store directly:
// a sandbox created with a fleet present carries its node in /v1.
func TestCreateRecordsPlacement(t *testing.T) {
	s, _ := nodeServer(t)
	// Simulate exactly what handleCreate persists: a create with NodeID set
	// (the handler passes fleet.Executor's pick into store.Create).
	id := newULID()
	if err := s.Store.Create(context.Background(), &store.Sandbox{
		ID: id, Status: "running", NodeID: "local", ExternalUserID: nullStr("local"),
	}); err != nil {
		t.Fatal(err)
	}
	w := v1Call(s.v1GetSandbox, "GET", "/v1/sandboxes/{id}", id, "")
	if w.Code != http.StatusOK {
		t.Fatalf("get = %d: %s", w.Code, w.Body)
	}
	var sb struct {
		Node string `json:"node"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &sb)
	if sb.Node != "local" {
		t.Errorf("v1 sandbox node = %q, want local", sb.Node)
	}
	// And the node's usage counts it.
	w = v1Call(s.v1FleetCapacity, "GET", "/v1/capacity", "", "")
	var cap struct {
		Used struct {
			Sandboxes int `json:"sandboxes"`
		} `json:"used"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &cap)
	if cap.Used.Sandboxes != 1 {
		t.Errorf("capacity used.sandboxes = %d, want 1", cap.Used.Sandboxes)
	}
}
