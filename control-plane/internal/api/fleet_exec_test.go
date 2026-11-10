package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/auth"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/policy"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/secrets"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

// fakeWorker is a minimal executor worker API: it records every request
// and keeps sandboxes in memory.
type fakeWorker struct {
	t     *testing.T
	token string
	mu    sync.Mutex
	reqs  []*http.Request
	boxes map[string]map[string]any
}

func (f *fakeWorker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, r.Clone(context.Background()))
	if r.Header.Get("Authorization") != "Bearer "+f.token {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == "POST" && r.URL.Path == "/v1/sandboxes":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		id := "01REMOTE0000000000000000" + string(rune('A'+len(f.boxes)))
		sb := map[string]any{"id": id, "status": "running", "node": "w1",
			"lifecycle": map[string]any{"keep_alive": body["lifecycle"] != nil}}
		f.boxes[id] = sb
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(sb)
	case r.Method == "GET" && r.URL.Path == "/v1/sandboxes":
		list := []any{}
		for _, sb := range f.boxes {
			list = append(list, sb)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"sandboxes": list})
	case strings.HasPrefix(r.URL.Path, "/v1/sandboxes/"):
		id := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/sandboxes/"), "/")[0]
		sb, ok := f.boxes[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch r.Method {
		case "DELETE":
			delete(f.boxes, id)
			w.WriteHeader(http.StatusNoContent)
		default:
			_ = json.NewEncoder(w).Encode(sb)
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeWorker) last() *http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqs[len(f.reqs)-1]
}

// execServer is a control plane with a local node and one executor worker
// (w1) backed by a fakeWorker with more free memory than the local node.
func execServer(t *testing.T) (*Server, *fakeWorker) {
	t.Helper()
	s, _ := nodeServer(t)
	c, err := secrets.Load("", filepath.Join(t.TempDir(), "secrets.key"))
	if err != nil {
		t.Fatal(err)
	}
	s.Secrets = c
	fw := &fakeWorker{t: t, token: "sk_node_test_token_0123456789abcdef", boxes: map[string]map[string]any{}}
	ts := httptest.NewServer(fw)
	t.Cleanup(ts.Close)
	w := internalCall(s.internalNodeRegister, `{"node_id":"w1","address":"`+ts.URL+`","capacity":{"cpus":8,"memory_mb":65536},"api_token":"`+fw.token+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("register = %d: %s", w.Code, w.Body)
	}
	s.Handler()
	return s, fw
}

func asAdmin(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Cookie", "sandboxd_session=secret")
	return r.WithContext(auth.WithActor(r.Context(), auth.Actor{Kind: "user", Name: "admin", User: "admin", Role: auth.RoleAdmin}))
}

func serve(s *Server, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.inner.ServeHTTP(w, r)
	return w
}

func TestRegisterBindsNodeToken(t *testing.T) {
	s, fw := execServer(t)
	// Another worker-token holder cannot take over w1 (and redirect traffic).
	w := internalCall(s.internalNodeRegister, `{"node_id":"w1","address":"http://evil:9000","api_token":"sk_node_other_token_000000000000000"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("hijack register = %d, want 409: %s", w.Code, w.Body)
	}
	w = internalCall(s.internalNodeRegister, `{"node_id":"w1","address":"http://evil:9000"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("tokenless re-register = %d, want 409", w.Code)
	}
	// The same worker (same token) re-registers fine.
	n, _ := s.Store.GetNode(context.Background(), "w1")
	w = internalCall(s.internalNodeRegister, `{"node_id":"w1","address":"`+n.Address+`","api_token":"`+fw.token+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("same-token register = %d: %s", w.Code, w.Body)
	}
	// The token is stored encrypted, never in clear.
	enc, _ := s.Store.NodeAPIToken(context.Background(), "w1")
	if enc == "" || strings.Contains(enc, fw.token) {
		t.Fatalf("stored token = %q", enc)
	}
}

func TestCreateRoutesToWorkerAndForwards(t *testing.T) {
	s, fw := execServer(t)

	// The worker has more free memory than the local node: it gets the sandbox.
	w := serve(s, asAdmin("POST", "/v1/sandboxes", `{"network":{"ports":[{"container":3000}]}}`))
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", w.Code, w.Body)
	}
	var sb struct {
		ID   string `json:"id"`
		Node string `json:"node"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &sb)
	if sb.Node != "w1" {
		t.Fatalf("create response = %s", w.Body)
	}
	if rs, err := s.Store.RemoteSandboxFor(context.Background(), sb.ID); err != nil || rs.NodeID != "w1" {
		t.Fatalf("remote placement = %+v, %v", rs, err)
	}
	var sent map[string]any
	body, _ := io.ReadAll(fw.last().Body)
	_ = json.Unmarshal(body, &sent)
	if sent["network"] != nil && sent["network"].(map[string]any)["ports"] != nil {
		t.Errorf("network.ports must be folded into ports, sent %s", body)
	}

	// GET is forwarded with the node credential and without the caller's.
	w = serve(s, asAdmin("GET", "/v1/sandboxes/"+sb.ID, ""))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), sb.ID) {
		t.Fatalf("get = %d: %s", w.Code, w.Body)
	}
	last := fw.last()
	if last.Header.Get("Cookie") != "" || last.Header.Get("Authorization") != "Bearer "+fw.token {
		t.Errorf("forwarded headers: cookie=%q auth=%q", last.Header.Get("Cookie"), last.Header.Get("Authorization"))
	}

	// The list merges the worker's sandboxes.
	w = serve(s, asAdmin("GET", "/v1/sandboxes", ""))
	if !strings.Contains(w.Body.String(), sb.ID) {
		t.Fatalf("list misses the remote sandbox: %s", w.Body)
	}

	// The control plane's node view counts it.
	if u, _ := s.fleetUsage(context.Background()); u["w1"].Sandboxes != 1 {
		t.Errorf("fleet usage w1 = %+v", u["w1"])
	}

	// A node that still runs sandboxes cannot be removed.
	if w := serve(s, asAdmin("DELETE", "/v1/nodes/w1", "")); w.Code != http.StatusConflict {
		t.Errorf("delete busy node = %d, want 409", w.Code)
	}

	// DELETE is forwarded and forgets the placement.
	w = serve(s, asAdmin("DELETE", "/v1/sandboxes/"+sb.ID, ""))
	if w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d: %s", w.Code, w.Body)
	}
	if _, err := s.Store.RemoteSandboxFor(context.Background(), sb.ID); err != store.ErrNotFound {
		t.Errorf("placement still recorded: %v", err)
	}
	if w := serve(s, asAdmin("DELETE", "/v1/nodes/w1", "")); w.Code != http.StatusNoContent {
		t.Errorf("delete idle node = %d: %s", w.Code, w.Body)
	}
}

func TestDrainedWorkerGetsNoSandbox(t *testing.T) {
	s, fw := execServer(t)
	if err := s.Store.SetNodeDrain(context.Background(), "w1", store.DrainDrain); err != nil {
		t.Fatal(err)
	}
	en, err := s.pickNode(context.Background())
	if err != nil || en != nil {
		t.Fatalf("pickNode with w1 drained = %+v, %v; want local", en, err)
	}
	_ = fw
}

func TestRemoteKeepAliveDeleteNeedsApproval(t *testing.T) {
	s, fw := execServer(t)
	p, _ := s.policy(context.Background())
	p.Approvals = []string{policy.ApproveDeleteKeepAlive}
	if _, err := s.savePolicy(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	w := serve(s, asAdmin("POST", "/v1/sandboxes", `{"lifecycle":{"keep_alive":true}}`))
	var sb struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &sb)
	if w.Code != http.StatusCreated || sb.ID == "" {
		t.Fatalf("create = %d: %s", w.Code, w.Body)
	}

	// Parked on the control plane: 202, nothing deleted on the worker.
	w = serve(s, asAdmin("DELETE", "/v1/sandboxes/"+sb.ID, ""))
	if w.Code != http.StatusAccepted {
		t.Fatalf("delete keep-alive = %d, want 202: %s", w.Code, w.Body)
	}
	var ap struct {
		ApprovalID string `json:"approval_id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &ap)
	if _, ok := fw.boxes[sb.ID]; !ok {
		t.Fatal("sandbox deleted before approval")
	}
	r := asAdmin("POST", "/v1/approvals/"+ap.ApprovalID+"/approve", `{}`)
	if w := serve(s, r); w.Code != http.StatusOK {
		t.Fatalf("approve = %d: %s", w.Code, w.Body)
	}
	// Retried with the approval: forwarded with the fleet grant.
	r = asAdmin("DELETE", "/v1/sandboxes/"+sb.ID, "")
	r.Header.Set(ApprovalHeader, ap.ApprovalID)
	if w := serve(s, r); w.Code != http.StatusNoContent {
		t.Fatalf("approved delete = %d: %s", w.Code, w.Body)
	}
	last := fw.last()
	if last.Method != "DELETE" || last.Header.Get(FleetGrantHeader) != policy.ApproveDeleteKeepAlive ||
		last.Header.Get(ApprovalHeader) != "" {
		t.Errorf("forwarded delete: method=%s grant=%q approval=%q", last.Method,
			last.Header.Get(FleetGrantHeader), last.Header.Get(ApprovalHeader))
	}
}

func TestFleetGrantOnlyFromFleetActor(t *testing.T) {
	s, _ := nodeServer(t)
	mk := func(kind string) *http.Request {
		r := httptest.NewRequest("DELETE", "/v1/sandboxes/x", nil)
		r.Header.Set(FleetGrantHeader, policy.ApproveDeleteKeepAlive)
		return r.WithContext(auth.WithActor(r.Context(), auth.Actor{Kind: kind, Name: auth.FleetActorName, Role: auth.RoleAdmin}))
	}
	if stop := s.requestApproval(httptest.NewRecorder(), mk(auth.ActorFleet), policy.ApproveDeleteKeepAlive, "sandbox:x", nil); stop {
		t.Error("fleet actor with a grant must proceed")
	}
	w := httptest.NewRecorder()
	if stop := s.requestApproval(w, mk("service"), policy.ApproveDeleteKeepAlive, "sandbox:x", nil); !stop || w.Code != http.StatusAccepted {
		t.Errorf("a non-fleet caller's grant header must be ignored: stop=%v code=%d", stop, w.Code)
	}
}

func TestApplyFleetPolicy(t *testing.T) {
	s, _ := nodeServer(t)
	p, _ := s.policy(context.Background())
	p.Approvals = []string{policy.ApprovePublic}
	raw, _ := json.Marshal(p)
	changed, err := s.ApplyFleetPolicy(context.Background(), raw)
	if err != nil || !changed {
		t.Fatalf("apply = %v, %v", changed, err)
	}
	if changed, _ := s.ApplyFleetPolicy(context.Background(), raw); changed {
		t.Error("re-applying the same policy reported a change")
	}
	got, _ := s.policy(context.Background())
	if !got.RequiresApproval(policy.ApprovePublic) {
		t.Errorf("policy not applied: %+v", got.Approvals)
	}
}
