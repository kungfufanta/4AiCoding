package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

const telemetryJSON = `{"collected_at":"2026-11-11T10:00:00Z","hostname":"k3s-1","cpu":{"cores":4,"usage_percent":37.5,"load1":1.2},` +
	`"memory":{"total_mb":8192,"used_mb":3000,"available_mb":5192},"disk":{"path":"/","total_mb":102400,"used_mb":20000,"free_mb":82400},` +
	`"network":{"interfaces":["eth0"],"rx_bytes":100,"tx_bytes":200,"rx_bps":10,"tx_bps":20}}`

func TestHeartbeatStoresTelemetryAndMetricsEndpoint(t *testing.T) {
	s, _ := nodeServer(t, "w1")

	w := internalCall(s.internalNodeHeartbeat, `{"node_id":"w1","telemetry":`+telemetryJSON+`}`)
	if w.Code != http.StatusOK {
		t.Fatalf("heartbeat = %d: %s", w.Code, w.Body)
	}
	n, err := s.Store.GetNode(context.Background(), "w1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(n.Telemetry), `"total_mb":8192`) {
		t.Fatalf("telemetry not stored: %s", n.Telemetry)
	}

	// Unknown fields inside telemetry are dropped (re-encoded), bad JSON is a 400.
	w = internalCall(s.internalNodeHeartbeat, `{"node_id":"w1","telemetry":{"cpu":{"cores":2},"evil":"x"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("heartbeat w/ extra field = %d: %s", w.Code, w.Body)
	}
	n, _ = s.Store.GetNode(context.Background(), "w1")
	if strings.Contains(string(n.Telemetry), "evil") {
		t.Fatalf("unknown telemetry field stored: %s", n.Telemetry)
	}
	w = internalCall(s.internalNodeHeartbeat, `{"node_id":"w1","telemetry":"nope"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("bad telemetry = %d, want 400", w.Code)
	}
	w = internalCall(s.internalNodeHeartbeat, `{"node_id":"w1","telemetry":{"hostname":"`+strings.Repeat("a", 20000)+`"}}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("huge telemetry = %d, want 400", w.Code)
	}

	w = v1Call(s.v1NodeMetrics, "GET", "/v1/nodes/w1/metrics", "w1", "")
	if w.Code != http.StatusOK {
		t.Fatalf("metrics = %d: %s", w.Code, w.Body)
	}
	var m struct {
		Node    v1Node            `json:"node"`
		History []nodeMetricPoint `json:"history"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	if len(m.History) != 1 || m.History[0].CPUPercent != 37.5 || m.History[0].MemTotalMB != 8192 {
		t.Fatalf("history = %+v (second beat within the step must not add a point)", m.History)
	}
	if len(m.Node.Telemetry) == 0 {
		t.Fatal("node.telemetry missing")
	}
	if w := v1Call(s.v1NodeMetrics, "GET", "/v1/nodes/nope/metrics", "nope", ""); w.Code != http.StatusNotFound {
		t.Fatalf("unknown node metrics = %d", w.Code)
	}
}

func TestSetValuesCapacityOnRealValuesFile(t *testing.T) {
	src, err := os.ReadFile("../../../deploy/k3s-1-values.yaml")
	if err != nil {
		t.Skip("deploy values file not found:", err)
	}
	want := store.Capacity{CPUs: 2.5, MemoryMB: 4096, DiskMB: 81920}
	out, err := setValuesCapacity(src, want)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := readValuesCapacity(out)
	if got != want {
		t.Fatalf("got %+v", got)
	}
	// Only the three capacity lines change.
	d := lineDiff(src, out)
	if len(d) != 6 {
		t.Fatalf("diff = %q", d)
	}
	if !strings.HasPrefix(string(out), "# Production worker configuration") {
		t.Fatal("leading comment lost")
	}
}

func TestSetValuesCapacityKeepsCommentsAndAddsMissing(t *testing.T) {
	src := "image:\n  tag: x # pinned\ncapacity:   # advertised\n    cpus: 1   # cores\n\n    memoryMB: 512\nresources: {}\n"
	out, err := setValuesCapacity([]byte(src), store.Capacity{CPUs: 4, MemoryMB: 2048, DiskMB: 100})
	if err != nil {
		t.Fatal(err)
	}
	wantOut := "image:\n  tag: x # pinned\ncapacity:   # advertised\n    cpus: 4   # cores\n\n    memoryMB: 2048\n    diskMB: 100\nresources: {}\n"
	if string(out) != wantOut {
		t.Fatalf("got:\n%s\nwant:\n%s", out, wantOut)
	}

	// No capacity block at all: one is appended.
	out, err = setValuesCapacity([]byte("kind: DaemonSet\n"), store.Capacity{CPUs: 2, MemoryMB: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := readValuesCapacity(out); c.CPUs != 2 || c.MemoryMB != 1024 {
		t.Fatalf("appended block = %s", out)
	}
}

func TestValidateNodeChange(t *testing.T) {
	g := &GitOpsConfig{}
	tel, _ := decodeTelemetry(json.RawMessage(telemetryJSON))
	ok := nodeChangeReq{CPUs: 2, MemoryMB: 4096, DiskMB: 51200}
	if msg := validateNodeChange(ok, g.max(), tel); msg != "" {
		t.Fatalf("valid request refused: %s", msg)
	}
	for name, req := range map[string]nodeChangeReq{
		"zero cpu":        {CPUs: 0, MemoryMB: 1024},
		"3 decimals":      {CPUs: 1.234, MemoryMB: 1024},
		"tiny memory":     {CPUs: 1, MemoryMB: 64},
		"more cores":      {CPUs: 8, MemoryMB: 1024},
		"more memory":     {CPUs: 1, MemoryMB: 16384},
		"more disk":       {CPUs: 1, MemoryMB: 1024, DiskMB: 200000},
		"negative disk":   {CPUs: 1, MemoryMB: 1024, DiskMB: -1},
		"above ceiling":   {CPUs: 300, MemoryMB: 1024},
		"reason too long": {CPUs: 1, MemoryMB: 1024, Reason: strings.Repeat("x", 1001)},
	} {
		var tt = tel
		if name == "above ceiling" {
			tt = nil
		}
		if msg := validateNodeChange(req, g.max(), tt); msg == "" {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestParseNodeFiles(t *testing.T) {
	got := parseNodeFiles(" k3s-1=deploy/k3s-1-values.yaml, bad id=x.yaml, w2=/etc/passwd.yaml, w3=../x.yaml, w4=deploy/w4.yml")
	if len(got) != 2 || got["k3s-1"] != "deploy/k3s-1-values.yaml" || got["w4"] != "deploy/w4.yml" {
		t.Fatalf("got %v", got)
	}
}

// fakeGitHub is the slice of the GitHub REST API the change request uses.
type fakeGitHub struct {
	mu      sync.Mutex
	file    string
	calls   []string
	commit  map[string]string
	prBody  map[string]any
	authHdr string
}

func (f *fakeGitHub) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		f.authHdr = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/contents/deploy/w1-values.yaml":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"content": base64.StdEncoding.EncodeToString([]byte(f.file)), "encoding": "base64", "sha": "blob1"})
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/git/ref/heads/main":
			_, _ = w.Write([]byte(`{"object":{"sha":"base1"}}`))
		case r.Method == "POST" && r.URL.Path == "/repos/o/r/git/refs":
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == "PUT" && r.URL.Path == "/repos/o/r/contents/deploy/w1-values.yaml":
			_ = json.Unmarshal(body, &f.commit)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == "POST" && r.URL.Path == "/repos/o/r/pulls":
			_ = json.Unmarshal(body, &f.prBody)
			w.WriteHeader(201)
			_, _ = w.Write([]byte(`{"number":77,"title":"t","state":"open","html_url":"https://github.com/o/r/pull/77","head":{"ref":"capacity/w1-1"}}`))
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/pulls":
			_, _ = w.Write([]byte(`[{"number":77,"state":"closed","merged_at":"2026-11-11T00:00:00Z","head":{"ref":"capacity/w1-1"}},{"number":5,"state":"open","head":{"ref":"feature/x"}}]`))
		default:
			t.Errorf("unexpected github call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	})
}

func TestNodeChangeRequestFlow(t *testing.T) {
	s, _ := nodeServer(t, "w1")
	ctx := context.Background()
	if err := s.Store.SetNodeTelemetry(ctx, "w1", json.RawMessage(telemetryJSON)); err != nil {
		t.Fatal(err)
	}
	fake := &fakeGitHub{file: "# worker\ncapacity:\n  cpus: 1\n  memoryMB: 1024\n  diskMB: 40960\n"}
	gh := httptest.NewServer(fake.handler(t))
	defer gh.Close()
	pv := map[string]string{"id": "w1"}

	// Not mapped → 409.
	w := callAs(s.v1CreateNodeChange, "user", "POST", "/", `{"cpus":2,"memory_mb":2048}`, nil, pv)
	if w.Code != http.StatusConflict || v1ErrCodeOf(t, w) != "gitops_not_configured" {
		t.Fatalf("unmapped = %d %s", w.Code, w.Body)
	}

	// Mapped without a token: dry run previews, a real request is refused.
	s.GitOps = &GitOpsConfig{Repo: "o/r", APIURL: gh.URL, NodeFiles: map[string]string{"w1": "deploy/w1-values.yaml"}}
	w = callAs(s.v1CreateNodeChange, "user", "POST", "/", `{"cpus":2,"memory_mb":2048,"disk_mb":40960,"dry_run":true}`, nil, pv)
	if w.Code != http.StatusOK {
		t.Fatalf("dry run = %d %s", w.Code, w.Body)
	}
	var prev nodeChangeResp
	_ = json.Unmarshal(w.Body.Bytes(), &prev)
	if prev.CanOpenPR || prev.Current.CPUs != 1 || prev.Proposed.MemoryMB != 2048 || len(prev.Diff) != 4 ||
		!strings.Contains(prev.Content, "memoryMB: 2048") {
		t.Fatalf("preview = %+v", prev)
	}
	w = callAs(s.v1CreateNodeChange, "user", "POST", "/", `{"cpus":2,"memory_mb":2048}`, nil, pv)
	if w.Code != http.StatusConflict {
		t.Fatalf("no token real request = %d %s", w.Code, w.Body)
	}

	// More than the host has → 400 before GitHub is touched.
	s.GitOps.Token = "ghp_test"
	w = callAs(s.v1CreateNodeChange, "user", "POST", "/", `{"cpus":16,"memory_mb":2048}`, nil, pv)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "grow the machine") {
		t.Fatalf("over host = %d %s", w.Code, w.Body)
	}
	// Same as current → no_change.
	w = callAs(s.v1CreateNodeChange, "user", "POST", "/", `{"cpus":1,"memory_mb":1024,"disk_mb":40960}`, nil, pv)
	if w.Code != http.StatusConflict || v1ErrCodeOf(t, w) != "no_change" {
		t.Fatalf("no change = %d %s", w.Code, w.Body)
	}

	// Real request opens a PR.
	w = callAs(s.v1CreateNodeChange, "user", "POST", "/", `{"cpus":2,"memory_mb":2048,"disk_mb":40960,"reason":"VM grew to 4 vCPU @everyone"}`, nil, pv)
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	var created nodeChangeResp
	_ = json.Unmarshal(w.Body.Bytes(), &created)
	if created.PR == nil || created.PR.Number != 77 || created.PR.URL == "" {
		t.Fatalf("pr = %+v", created.PR)
	}
	if fake.authHdr != "Bearer ghp_test" {
		t.Fatalf("auth header = %q", fake.authHdr)
	}
	if fake.commit["branch"] == "" || !strings.HasPrefix(fake.commit["branch"], "capacity/w1-") || fake.commit["sha"] != "blob1" {
		t.Fatalf("commit = %v", fake.commit)
	}
	newFile, _ := base64.StdEncoding.DecodeString(fake.commit["content"])
	if !strings.Contains(string(newFile), "cpus: 2\n") || !strings.HasPrefix(string(newFile), "# worker\n") {
		t.Fatalf("committed file = %s", newFile)
	}
	if body, _ := fake.prBody["body"].(string); strings.Contains(body, "@everyone") || !strings.Contains(body, "Revert this PR") {
		t.Fatalf("pr body = %s", body)
	}

	// List shows only this node's capacity PRs, merged state resolved.
	w = callAs(s.v1ListNodeChanges, "user", "GET", "/", "", nil, pv)
	var list struct {
		Current  v1NodeCapacity `json:"current"`
		Requests []nodeChangePR `json:"requests"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &list)
	if len(list.Requests) != 1 || list.Requests[0].State != "merged" || list.Current.MemoryMB != 1024 {
		t.Fatalf("list = %s", w.Body)
	}
}
