package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

// testID is a fresh ULID for sandbox rows.
func testID() string {
	u, err := ulid.New(ulid.Now(), rand.Reader)
	if err != nil {
		panic(err)
	}
	return u.String()
}

// testNullStr mirrors the api package's nullStr helper.
func testNullStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

func mkNode(id, role string, cap Capacity) *Node {
	return &Node{NodeID: id, Role: role, Capacity: cap, Version: "test", Address: "http://" + id}
}

func TestUpsertNodeRegisterAndRefresh(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	if err := st.UpsertNode(ctx, mkNode("w1", RoleWorker, Capacity{CPUs: 4, MemoryMB: 8192})); err != nil {
		t.Fatalf("register: %v", err)
	}
	n, err := st.GetNode(ctx, "w1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if n.Role != RoleWorker || n.DrainState != DrainActive || n.Capacity.CPUs != 4 {
		t.Fatalf("node = %+v", n)
	}
	if n.Capacity.MemoryMB != 8192 || n.Capacity.DiskMB != 0 {
		t.Errorf("capacity = %+v", n.Capacity)
	}

	// Drain, then re-register: the drain must survive a worker restart.
	if err := st.SetNodeDrain(ctx, "w1", DrainDrain); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if err := st.UpsertNode(ctx, mkNode("w1", RoleWorker, Capacity{CPUs: 8})); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	n, _ = st.GetNode(ctx, "w1")
	if n.DrainState != DrainDrain {
		t.Errorf("drain state after re-register = %q, want drain preserved", n.DrainState)
	}
	if n.Capacity.CPUs != 8 {
		t.Errorf("capacity after re-register = %+v, want refreshed", n.Capacity)
	}
}

func TestNodeHeartbeatUpdatesAndReturnsState(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	if err := st.UpsertNode(ctx, mkNode("w1", RoleWorker, Capacity{CPUs: 4})); err != nil {
		t.Fatal(err)
	}
	before, _ := st.GetNode(ctx, "w1")
	time.Sleep(time.Second) // heartbeat_at has second resolution; force a visible delta

	cap := Capacity{CPUs: 6, MemoryMB: 12288}
	n, err := st.NodeHeartbeat(ctx, "w1", &cap, nil)
	if err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	if !n.HeartbeatAt.After(before.HeartbeatAt) {
		t.Errorf("heartbeat_at did not advance: %v -> %v", before.HeartbeatAt, n.HeartbeatAt)
	}
	if n.Capacity.CPUs != 6 || n.Capacity.MemoryMB != 12288 {
		t.Errorf("capacity after heartbeat = %+v", n.Capacity)
	}
	if n.DrainState != DrainActive {
		t.Errorf("drain state = %q", n.DrainState)
	}

	if _, err := st.NodeHeartbeat(ctx, "ghost", nil, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("heartbeat unknown node = %v, want ErrNotFound", err)
	}
}

func TestSetNodeDrainAndDelete(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	if err := st.UpsertNode(ctx, mkNode("w1", RoleWorker, Capacity{})); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertNode(ctx, mkNode("local", RoleLocal, Capacity{})); err != nil {
		t.Fatal(err)
	}
	if err := st.SetNodeDrain(ctx, "w1", DrainDrain); err != nil {
		t.Fatalf("drain: %v", err)
	}
	nodes, err := st.ListNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 {
		t.Fatalf("list = %d nodes, want 2", len(nodes))
	}
	// The local node sorts first.
	if nodes[0].NodeID != "local" || nodes[0].Role != RoleLocal {
		t.Errorf("first node = %s (%s), want local", nodes[0].NodeID, nodes[0].Role)
	}

	// The local node cannot be deleted; a worker can.
	if err := st.DeleteNode(ctx, "local"); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete local = %v, want ErrNotFound", err)
	}
	if err := st.DeleteNode(ctx, "w1"); err != nil {
		t.Errorf("delete worker: %v", err)
	}
	if _, err := st.GetNode(ctx, "w1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("get after delete = %v, want ErrNotFound", err)
	}

	// Unknown ids and bad states are rejected.
	if err := st.SetNodeDrain(ctx, "ghost", DrainDrain); !errors.Is(err, ErrNotFound) {
		t.Errorf("drain unknown = %v, want ErrNotFound", err)
	}
	if err := st.SetNodeDrain(ctx, "local", "bogus"); !errors.Is(err, ErrConflict) {
		t.Errorf("bad drain state = %v, want ErrConflict", err)
	}
	if err := st.UpsertNode(ctx, mkNode("x", "boss", Capacity{})); !errors.Is(err, ErrConflict) {
		t.Errorf("bad role = %v, want ErrConflict", err)
	}
}

func TestPlacementLifecycle(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	sb := &Sandbox{ID: testID(), Status: "running", NodeID: "local",
		ExternalUserID: testNullStr("local"), Spec: Spec{CPUs: 1.5, MemoryMB: 2048}}
	if err := st.Create(ctx, sb); err != nil {
		t.Fatalf("create with placement: %v", err)
	}
	p, err := st.PlacementFor(ctx, sb.ID)
	if err != nil {
		t.Fatalf("placement: %v", err)
	}
	if p.NodeID != "local" || p.State != "placed" {
		t.Errorf("placement = %+v", p)
	}
	// fill() surfaces the node id on the sandbox row.
	got, err := st.Get(ctx, sb.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.NodeID != "local" {
		t.Errorf("sandbox.NodeID = %q, want local", got.NodeID)
	}

	// Usage counts live sandboxes only, with explicit spec resources.
	sb2 := &Sandbox{ID: testID(), Status: "stopped", NodeID: "local", ExternalUserID: testNullStr("local"),
		Spec: Spec{CPUs: 9, MemoryMB: 9999}}
	if err := st.Create(ctx, sb2); err != nil {
		t.Fatal(err)
	}
	usage, err := st.NodeUsage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	u := usage["local"]
	if u.Sandboxes != 1 || u.CPUs != 1.5 || u.MemoryMB != 2048 {
		t.Errorf("usage = %+v, want 1 sandbox @ 1.5 cpu / 2048 MB (stopped row excluded)", u)
	}

	// Deleting the sandbox cascades the placement.
	if err := st.Delete(ctx, sb.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PlacementFor(ctx, sb.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("placement after delete = %v, want ErrNotFound (cascade)", err)
	}
	usage, _ = st.NodeUsage(ctx)
	if _, ok := usage["local"]; ok {
		t.Errorf("usage after delete = %+v, want empty", usage)
	}

	// A create without NodeID stays unplaced (legacy behaviour).
	sb3 := &Sandbox{ID: testID(), Status: "running", ExternalUserID: testNullStr("local")}
	if err := st.Create(ctx, sb3); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PlacementFor(ctx, sb3.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("unplaced create got a placement: %v", err)
	}
}

func TestValidNodeID(t *testing.T) {
	good := []string{"local", "w", "worker-1", "a.b", "node-01.eu-west", "0"}
	bad := []string{"", "-lead", ".lead", "UPPER", "sp ace", "sl/ash", "under_score",
		string(make([]byte, 64)), "tab\t"}
	for _, id := range good {
		if !ValidNodeID(id) {
			t.Errorf("ValidNodeID(%q) = false, want true", id)
		}
	}
	for _, id := range bad {
		if ValidNodeID(id) {
			t.Errorf("ValidNodeID(%q) = true, want false", id)
		}
	}
}
