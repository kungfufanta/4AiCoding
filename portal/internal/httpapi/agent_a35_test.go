// agent_a35_test.go — A3.5 (docs/AGENT_PLAN_A3.md §11): the revert
// rename fix + the run-end metrics log.
//
//	rename a.txt → b.txt inside a run ⇒ files_changed carries the
//	  --no-renames split D a.txt + A b.txt (NEVER one R row — under R
//	  the new file landed in neither revert list and survived revert)
//	revert of that run ⇒ a.txt back byte for byte, b.txt gone, user
//	  files untouched, post-revert diff against the ref empty
//	every finished run logs exactly ONE info line with run_id, org_id,
//	  status, error_code, steps, input_tokens, output_tokens,
//	  duration_ms, tool_mode
package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tastyeffectco/sandboxd/portal/internal/agent"
	"github.com/tastyeffectco/sandboxd/portal/internal/agent/sandboxtest"
	"github.com/tastyeffectco/sandboxd/portal/internal/llm"
	"github.com/tastyeffectco/sandboxd/portal/internal/llm/llmtest"
	"github.com/tastyeffectco/sandboxd/portal/internal/sandboxd"
)

// TestAgentA35RevertRename — the A3.3 rename bug, pinned at the API
// level: the agent moves a.txt to b.txt; files_changed must carry the
// --no-renames split (D a.txt + A b.txt), and revert must put a.txt
// back and delete b.txt — leaving the post-revert diff empty.
func TestAgentA35RevertRename(t *testing.T) {
	srv, sbx, notify := setupA33(t)
	sfx := fmt.Sprint(time.Now().UnixNano())
	_, carol, carolCSRF := login(t, srv, "a35-carol-"+sfx, "a35-carol-"+sfx+"@example.test", "Carol")
	rec := do(t, srv, http.MethodPost, "/api/orgs", `{"name":"A35 Rename `+sfx+`"}`, carol, carolCSRF)
	orgID := extractString(t, rec.Body.String(), "id")
	sandboxID := seedAgentSandbox(t, srv, orgID, "ren")
	chat := seedChat(t, srv, orgID, sandboxID, carol, carolCSRF)

	// The file that will be renamed must exist BEFORE the run so it is
	// tracked in the checkpoint tree — that is what makes the rename
	// visible as D+A (a file created and renamed inside the same run
	// only ever shows A b.txt). notes.txt is the user's bystander.
	appDir := filepath.Join(sbx.Root(sandboxtest.DefaultID), "workspace", "app")
	if err := os.WriteFile(filepath.Join(appDir, "a.txt"), []byte("nội dung gốc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "notes.txt"), []byte("user notes"), 0o644); err != nil {
		t.Fatal(err)
	}

	scripted := llmtest.NewServer(
		llmtest.Scenario{Chunks: toolCallChunks("c1", "run_command",
			`{"command":"mv a.txt b.txt"}`)},
		llmtest.Scenario{Chunks: toolCallChunks("c2", "finish",
			`{"summary":"đã đổi tên a.txt thành b.txt"}`)},
	)
	defer scripted.Close()
	srv.LLM = &llm.Client{BaseURL: scripted.URL, Model: "glm-4.6", Flavor: "webhub", APIKey: "wh_test"}
	startAgentWorker(t, srv, notify, agent.EnvSource{Fallback: srv.LLM}, sbx.Client())

	runID := createRun(t, srv, orgID, chat, []*http.Cookie{carol, carolCSRF}, `{"prompt":"đổi tên a.txt thành b.txt"}`)
	waitForRunStatus(t, srv, orgID, runID, []*http.Cookie{carol, carolCSRF}, 15*time.Second)

	// files_changed: exactly the split — and never an R row.
	_, res := resultOf(t, srv, orgID, runID)
	raw, _ := json.Marshal(res["files_changed"])
	var files []agent.FileChange
	if err := json.Unmarshal(raw, &files); err != nil {
		t.Fatalf("decode files_changed %s: %v", raw, err)
	}
	if len(files) != 2 {
		t.Fatalf("files_changed = %s; want exactly [D a.txt, A b.txt]", raw)
	}
	got := map[string]string{}
	for _, f := range files {
		if f.Status == "R" {
			t.Fatalf("rename detection survived: row %v (want the --no-renames D+A split)", f)
		}
		got[f.Path] = f.Status
	}
	if got["a.txt"] != "D" || got["b.txt"] != "A" {
		t.Fatalf("files_changed = %v; want a.txt D + b.txt A", got)
	}
	// After the run: the rename happened on disk.
	if b, err := os.ReadFile(filepath.Join(appDir, "b.txt")); err != nil || string(b) != "nội dung gốc\n" {
		t.Fatalf("b.txt after run = %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(appDir, "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("a.txt still exists after the run (err=%v)", err)
	}

	// Revert: a.txt restored byte for byte, b.txt deleted, bystander safe.
	rec = do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/agent/runs/"+runID+"/revert", "", carol, carolCSRF)
	if rec.Code != http.StatusOK {
		t.Fatalf("revert = %d: %s, want 200", rec.Code, rec.Body.String())
	}
	if b, err := os.ReadFile(filepath.Join(appDir, "a.txt")); err != nil || string(b) != "nội dung gốc\n" {
		t.Fatalf("a.txt after revert = %q, %v; want the original bytes back", b, err)
	}
	if _, err := os.Stat(filepath.Join(appDir, "b.txt")); !os.IsNotExist(err) {
		t.Fatalf("b.txt still exists after revert (err=%v) — the A3.3 rename bug", err)
	}
	if b, err := os.ReadFile(filepath.Join(appDir, "notes.txt")); err != nil || string(b) != "user notes" {
		t.Fatalf("notes.txt after revert = %q, %v; a user file the run never touched must survive", b, err)
	}

	// The reverted event: 1 restored (the D side), 1 removed (the A
	// side) and an empty post-revert diff (files_remaining).
	restored, removed, remaining := 0, 0, -1
	for _, e := range eventDatas(t, eventsOf(t, srv, orgID, runID), "reverted") {
		restored = int(e["files_restored"].(float64))
		removed = int(e["files_removed"].(float64))
		remaining = len(e["files_remaining"].([]any))
	}
	if restored != 1 || removed != 1 || remaining != 0 {
		t.Fatalf("reverted event = restored %d, removed %d, remaining %d; want 1/1/0",
			restored, removed, remaining)
	}
}

// logCapture is a slog handler collecting records for assertions.
type logCapture struct {
	mu  sync.Mutex
	rec []slog.Record
}

func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }
func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rec = append(c.rec, r.Clone())
	return nil
}
func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *logCapture) WithGroup(string) slog.Handler      { return c }

func (c *logCapture) records(msg string) []slog.Record {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []slog.Record{}
	for _, r := range c.rec {
		if r.Message == msg {
			out = append(out, r)
		}
	}
	return out
}

func attrOf(r slog.Record, key string) (slog.Value, bool) {
	found := slog.Value{}
	ok := false
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			found, ok = a.Value, true
			return false
		}
		return true
	})
	return found, ok
}

// startAgentWorkerCapture is startAgentWorker with a capturing logger
// (the metrics line is the test subject here).
func startAgentWorkerCapture(t *testing.T, srv *Server, notify *agent.Notifier, src agent.LLMSource, sbx *sandboxd.Client) (*logCapture, func()) {
	t.Helper()
	cap := &logCapture{}
	w := &agent.Worker{
		ID:         fmt.Sprintf("httpapi-cap-%d", time.Now().UnixNano()%100000),
		Log:        slog.New(cap),
		Store:      agent.NewStore(srv.Pool),
		LLM:        src,
		Sandboxd:   sbx,
		Notify:     notify,
		Lease:      3 * time.Second,
		Heartbeat:  300 * time.Millisecond,
		ClaimEvery: 100 * time.Millisecond,
		CancelPoll: 200 * time.Millisecond,
		SweepEvery: 250 * time.Millisecond,
		SweepGrace: time.Second,
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	stop := func() {
		cancel()
		w.Shutdown(0)
		w.Wait()
	}
	t.Cleanup(stop)
	return cap, stop
}

// TestAgentA35MetricsLog — plan §11 A3.5: when a run ends, the worker
// logs EXACTLY ONE info line carrying run_id, org_id, status,
// error_code, steps, input_tokens, output_tokens, duration_ms and
// tool_mode.
func TestAgentA35MetricsLog(t *testing.T) {
	srv, sbx, notify := setupA33(t)
	sfx := fmt.Sprint(time.Now().UnixNano())
	_, dave, daveCSRF := login(t, srv, "a35-dave-"+sfx, "a35-dave-"+sfx+"@example.test", "Dave")
	rec := do(t, srv, http.MethodPost, "/api/orgs", `{"name":"A35 Metrics `+sfx+`"}`, dave, daveCSRF)
	orgID := extractString(t, rec.Body.String(), "id")
	sandboxID := seedAgentSandbox(t, srv, orgID, "met")
	chat := seedChat(t, srv, orgID, sandboxID, dave, daveCSRF)

	scripted := llmtest.NewServer(
		llmtest.Scenario{Chunks: []any{
			llmtest.TextChunk("xong việc rồi"),
			llmtest.FinishChunk("stop"),
		}},
	)
	defer scripted.Close()
	srv.LLM = &llm.Client{BaseURL: scripted.URL, Model: "glm-4.6", Flavor: "webhub", APIKey: "wh_test"}
	cap, _ := startAgentWorkerCapture(t, srv, notify, agent.EnvSource{Fallback: srv.LLM}, sbx.Client())

	runID := createRun(t, srv, orgID, chat, []*http.Cookie{dave, daveCSRF}, `{"prompt":"làm việc nhẹ"}`)
	waitForRunStatus(t, srv, orgID, runID, []*http.Cookie{dave, daveCSRF}, 15*time.Second)

	finished := cap.records("agent run finished")
	var line *slog.Record
	for i := range finished {
		if v, ok := attrOf(finished[i], "run_id"); ok && v.String() == runID {
			line = &finished[i]
		}
	}
	if line == nil {
		t.Fatalf("no metrics line for run %s among %d finished-run lines", runID, len(finished))
	}
	if line.Level != slog.LevelInfo {
		t.Errorf("metrics line level = %v, want Info", line.Level)
	}
	for key, want := range map[string]string{
		"org_id":     orgID,
		"status":     "succeeded",
		"error_code": "",
		"tool_mode":  "native",
	} {
		if v, ok := attrOf(*line, key); !ok || v.String() != want {
			t.Errorf("metrics %s = %q (present=%v), want %q", key, v, ok, want)
		}
	}
	if v, ok := attrOf(*line, "steps"); !ok || v.Int64() != 1 {
		t.Errorf("metrics steps = %v (present=%v), want 1", v, ok)
	}
	for _, key := range []string{"input_tokens", "output_tokens", "duration_ms"} {
		if v, ok := attrOf(*line, key); !ok || v.Kind() != slog.KindInt64 {
			t.Errorf("metrics %s missing or not an int: %v (present=%v)", key, v, ok)
		}
	}
	if v, _ := attrOf(*line, "duration_ms"); v.Int64() < 0 {
		t.Errorf("metrics duration_ms = %d, want >= 0", v.Int64())
	}
}
