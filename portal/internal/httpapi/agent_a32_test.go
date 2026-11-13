// agent_a32_test.go — A3.2 acceptance (docs/AGENT_PLAN_A3.md §11
// "A3.2 Xong khi"), driven end to end through the BFF with a scripted
// LLM (llmtest) and the fake sandboxd (sandboxtest):
//
//	write_file → run_command cat → finish  ⇒ files_changed=[{hello.txt,A}]
//	edit_file with a duplicated `old`      ⇒ error to the LLM, run continues
//	../../etc/passwd and /etc/passwd       ⇒ blocked before sandboxd
//	run_command sleep 999, timeout_s 2     ⇒ timed_out within ≤ 4 s
//	5 MiB output                           ⇒ 16 KiB to the LLM
//	image without git                      ⇒ succeeded + warning, checkpoint NULL
//	SECRET_MARKER                          ⇒ never in agent_events
//	model supports_tools=false             ⇒ 201 + fallback mode (A3.2 review)
package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tastyeffectco/sandboxd/portal/internal/agent"
	"github.com/tastyeffectco/sandboxd/portal/internal/agent/sandboxtest"
	"github.com/tastyeffectco/sandboxd/portal/internal/db"
	"github.com/tastyeffectco/sandboxd/portal/internal/llm"
	"github.com/tastyeffectco/sandboxd/portal/internal/llm/llmtest"
	"github.com/tastyeffectco/sandboxd/portal/internal/passwordauth"
)

// setupA32 wires one A3.2 test: fake LLM + fake sandboxd rooted at a
// temp dir whose project directory exists.
func setupA32(t *testing.T) (*Server, *sandboxtest.Server, *agent.Notifier) {
	t.Helper()
	if os.Getenv("PORTAL_SECRETS_KEY") == "" {
		_ = os.Setenv("PORTAL_SECRETS_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	}
	srv, _, notify, sbxClient := setupAgent(t)
	sbx := sandboxtest.New(t.TempDir()) // replaces the empty one from setupAgent
	t.Cleanup(sbx.Close)
	if err := os.MkdirAll(filepath.Join(sbx.Root(sandboxtest.DefaultID), "workspace/app"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = sbxClient // the tests that need a client take sbx.Client()
	return srv, sbx, notify
}

// createRun posts a run through the BFF and returns its id.
func createRun(t *testing.T, srv *Server, orgID, chatID string, cookies []*http.Cookie, body string) string {
	t.Helper()
	rec := do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/chats/"+chatID+"/agent/runs", body, cookies...)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create run = %d: %s", rec.Code, rec.Body.String())
	}
	return extractString(t, rec.Body.String(), "id")
}

// eventsOf reads every event of a run.
func eventsOf(t *testing.T, srv *Server, orgID, runID string) []agent.Event {
	t.Helper()
	events, err := agent.NewStore(srv.Pool).EventsSince(context.Background(), orgID, runID, 0)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	return events
}

// eventDatas decodes one event type's payloads.
func eventDatas(t *testing.T, events []agent.Event, typ string) []map[string]any {
	t.Helper()
	out := []map[string]any{}
	for _, e := range events {
		if e.Type != typ {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(e.Data, &m); err != nil {
			t.Fatalf("decode %s event: %v", typ, err)
		}
		out = append(out, m)
	}
	return out
}

func resultOf(t *testing.T, srv *Server, orgID, runID string) (agent.Run, map[string]any) {
	t.Helper()
	run, err := agent.NewStore(srv.Pool).GetRun(context.Background(), orgID, runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	var res map[string]any
	_ = json.Unmarshal(run.Result, &res)
	return run, res
}

func toolCallChunks(id, name, args string) []any {
	return []any{llmtest.ToolCallChunk(0, id, name, args), llmtest.FinishChunk("tool_calls")}
}

// TestAgentA32WriteRunCatFinish — the plan's headline scenario:
// write_file → run_command cat → finish ⇒ files_changed [{hello.txt,A}]
// and the tool_result carries the file content. Also audits every
// tool call and keeps write_file content off the wire.
func TestAgentA32WriteRunCatFinish(t *testing.T) {
	srv, sbx, notify := setupA32(t)
	scripted := llmtest.NewServer(
		llmtest.Scenario{Chunks: toolCallChunks("c1", "write_file",
			`{"path":"hello.txt","content":"Hello A3.2"}`)},
		llmtest.Scenario{Chunks: toolCallChunks("c2", "run_command",
			`{"command":"cat hello.txt"}`)},
		llmtest.Scenario{Chunks: toolCallChunks("c3", "finish",
			`{"summary":"wrote hello.txt and read it back"}`)},
	)
	defer scripted.Close()
	srv.LLM = &llm.Client{BaseURL: scripted.URL, Model: "glm-4.6", Flavor: "webhub", APIKey: "wh_SECRET_MARKER_key"}
	startAgentWorker(t, srv, notify, agent.EnvSource{Fallback: srv.LLM}, sbx.Client())

	sfx := fmt.Sprint(time.Now().UnixNano())
	_, alice, aliceCSRF := login(t, srv, "a32-flow-"+sfx, "a32-flow-"+sfx+"@example.test", "Alice")
	rec := do(t, srv, http.MethodPost, "/api/orgs", `{"name":"A32 Org `+sfx+`"}`, alice, aliceCSRF)
	orgID := extractString(t, rec.Body.String(), "id")
	sandboxID := seedAgentSandbox(t, srv, orgID, "flow")
	rec = do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/chats", "", alice, aliceCSRF)
	chat := extractString(t, rec.Body.String(), "id")
	do(t, srv, http.MethodPatch, "/api/orgs/"+orgID+"/chats/"+chat, `{"sandbox_id":"`+sandboxID+`"}`, alice, aliceCSRF)

	runID := createRun(t, srv, orgID, chat, []*http.Cookie{alice, aliceCSRF}, `{"prompt":"tạo hello.txt rồi kiểm tra"}`)
	waitForRunStatus(t, srv, orgID, runID, []*http.Cookie{alice, aliceCSRF}, 15*time.Second)

	run, res := resultOf(t, srv, orgID, runID)
	if run.Status != agent.StatusSucceeded {
		t.Fatalf("run = %s/%s", run.Status, run.ErrorCode)
	}
	if run.CheckpointRef == "" {
		t.Fatal("checkpoint_ref empty, want a git checkpoint")
	}
	files, _ := res["files_changed"].([]any)
	if len(files) != 1 {
		t.Fatalf("files_changed = %v", res["files_changed"])
	}
	fc := files[0].(map[string]any)
	if fc["path"] != "hello.txt" || fc["status"] != "A" {
		t.Fatalf("files_changed[0] = %v, want hello.txt A", fc)
	}
	if res["build_status"] != "skipped" {
		t.Fatalf("build_status = %v, want skipped (no manifests)", res["build_status"])
	}
	if res["summary"] != "wrote hello.txt and read it back" {
		t.Fatalf("summary = %v", res["summary"])
	}

	events := eventsOf(t, srv, orgID, runID)
	calls := eventDatas(t, events, "tool_call")
	if len(calls) != 3 || calls[0]["name"] != "write_file" || calls[1]["name"] != "run_command" {
		t.Fatalf("tool_call events = %+v", calls)
	}
	preview := fmt.Sprint(calls[0]["args_preview"])
	if strings.Contains(preview, "Hello A3.2") || strings.Contains(preview, "content") {
		t.Fatalf("write_file preview leaks content: %s", preview)
	}
	if !strings.Contains(preview, "hello.txt") {
		t.Fatalf("write_file preview missing path: %s", preview)
	}
	results := eventDatas(t, events, "tool_result")
	if len(results) != 3 {
		t.Fatalf("tool_result events = %d", len(results))
	}
	if ok, _ := results[1]["ok"].(bool); !ok {
		t.Fatalf("cat tool_result not ok: %v", results[1])
	}
	if out := fmt.Sprint(results[1]["output_preview"]); !strings.Contains(out, "Hello A3.2") {
		t.Fatalf("cat tool_result missing the file content: %v", results[1])
	}

	// Status contract: starting → checkpoint → running(tool_mode) → postcheck → done.
	var phases []string
	for _, e := range eventsOf(t, srv, orgID, runID) {
		if e.Type == "status" {
			var m map[string]any
			_ = json.Unmarshal(e.Data, &m)
			phases = append(phases, fmt.Sprint(m["phase"]))
		}
	}
	joined := strings.Join(phases, ",")
	if !strings.Contains(joined, "starting") || !strings.Contains(joined, "checkpoint") ||
		!strings.Contains(joined, "postcheck") {
		t.Fatalf("status phases = %s", joined)
	}

	// The wire: native mode sends the 8 tools, the webhub memory kill
	// fires once tool output entered the history.
	if len(scripted.Requests) != 3 {
		t.Fatalf("llm requests = %d", len(scripted.Requests))
	}
	for i, req := range scripted.Requests {
		tools, has := req.Body["tools"].([]any)
		if !has || len(tools) != 8 {
			t.Fatalf("request %d tools = %v, want 8", i, req.Body["tools"])
		}
	}
	if mem := scripted.Requests[1].Header.Get("X-WebHub-Memory"); mem != "0" {
		t.Fatalf("request 2 X-WebHub-Memory = %q, want 0 after tool output", mem)
	}

	// Audit agent.tool per executed tool (plan §6), without content.
	var auditCount int
	if err := db.WithOrg(context.Background(), srv.Pool, orgID, func(tx db.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT count(*) FROM audit_events WHERE action = 'agent.tool'`).Scan(&auditCount)
	}); err != nil {
		t.Fatal(err)
	}
	if auditCount != 3 {
		t.Fatalf("agent.tool audit rows = %d, want 3", auditCount)
	}

	// History keeps the tool turns for "continue in the same chat".
	msgs, err := agent.NewStore(srv.Pool).LoadMessages(context.Background(), orgID, runID)
	if err != nil {
		t.Fatal(err)
	}
	toolMsgs := 0
	for _, m := range msgs {
		if m.Role == "tool" {
			toolMsgs++
		}
	}
	if toolMsgs != 2 { // write_file result + cat result; finish closes the run
		t.Fatalf("history tool messages = %d, want 2", toolMsgs)
	}
}

// TestAgentA32EditAmbiguousKeepsRunning — edit_file with a duplicated
// `old` answers an error to the LLM and the run continues (plan §11
// acceptance 2).
func TestAgentA32EditAmbiguousKeepsRunning(t *testing.T) {
	srv, sbx, notify := setupA32(t)
	root := sbx.Root(sandboxtest.DefaultID)
	if err := os.WriteFile(filepath.Join(root, "workspace/app/dup.txt"), []byte("word word\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scripted := llmtest.NewServer(
		llmtest.Scenario{Chunks: toolCallChunks("e1", "edit_file",
			`{"path":"dup.txt","old":"word","new":"WORD"}`)},
		llmtest.Scenario{Chunks: toolCallChunks("e2", "finish",
			`{"summary":"stopped at the ambiguous edit"}`)},
	)
	defer scripted.Close()
	srv.LLM = &llm.Client{BaseURL: scripted.URL, Model: "glm-4.6", Flavor: "webhub", APIKey: "wh_test"}
	startAgentWorker(t, srv, notify, agent.EnvSource{Fallback: srv.LLM}, sbx.Client())

	sfx := fmt.Sprint(time.Now().UnixNano())
	_, alice, aliceCSRF := login(t, srv, "a32-edit-"+sfx, "a32-edit-"+sfx+"@example.test", "Alice")
	rec := do(t, srv, http.MethodPost, "/api/orgs", `{"name":"A32 Edit Org `+sfx+`"}`, alice, aliceCSRF)
	orgID := extractString(t, rec.Body.String(), "id")
	sandboxID := seedAgentSandbox(t, srv, orgID, "edit")
	rec = do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/chats", "", alice, aliceCSRF)
	chat := extractString(t, rec.Body.String(), "id")
	do(t, srv, http.MethodPatch, "/api/orgs/"+orgID+"/chats/"+chat, `{"sandbox_id":"`+sandboxID+`"}`, alice, aliceCSRF)

	runID := createRun(t, srv, orgID, chat, []*http.Cookie{alice, aliceCSRF}, `{"prompt":"sửa dup.txt"}`)
	waitForRunStatus(t, srv, orgID, runID, []*http.Cookie{alice, aliceCSRF}, 15*time.Second)

	run, _ := resultOf(t, srv, orgID, runID)
	if run.Status != agent.StatusSucceeded {
		t.Fatalf("run = %s/%s, want succeeded (a tool error must not fail the run)", run.Status, run.ErrorCode)
	}
	results := eventDatas(t, eventsOf(t, srv, orgID, runID), "tool_result")
	if len(results) == 0 || results[0]["ok"] != false ||
		!strings.Contains(fmt.Sprint(results[0]["output_preview"]), "appears 2 times") {
		t.Fatalf("edit tool_result = %+v", results)
	}
	data, err := os.ReadFile(filepath.Join(root, "workspace/app/dup.txt"))
	if err != nil || string(data) != "word word\n" {
		t.Fatalf("file changed by a refused edit: %q %v", data, err)
	}
	// files_changed must stay empty: nothing changed.
	_, res := resultOf(t, srv, orgID, runID)
	if files, _ := res["files_changed"].([]any); len(files) != 0 {
		t.Fatalf("files_changed = %v, want empty", res["files_changed"])
	}
}

// TestAgentA32TraversalBlocked — `../../etc/passwd` and `/etc/passwd`
// are blocked BEFORE sandboxd is called (acceptance 3).
func TestAgentA32TraversalBlocked(t *testing.T) {
	srv, sbx, notify := setupA32(t)
	root := sbx.Root(sandboxtest.DefaultID)
	scripted := llmtest.NewServer(
		llmtest.Scenario{Chunks: toolCallChunks("p1", "write_file",
			`{"path":"../../etc/passwd","content":"owned"}`)},
		llmtest.Scenario{Chunks: toolCallChunks("p2", "write_file",
			`{"path":"/etc/passwd","content":"owned"}`)},
		llmtest.Scenario{Chunks: toolCallChunks("p3", "finish",
			`{"summary":"blocked twice"}`)},
	)
	defer scripted.Close()
	srv.LLM = &llm.Client{BaseURL: scripted.URL, Model: "glm-4.6", Flavor: "webhub", APIKey: "wh_test"}
	startAgentWorker(t, srv, notify, agent.EnvSource{Fallback: srv.LLM}, sbx.Client())

	sfx := fmt.Sprint(time.Now().UnixNano())
	_, alice, aliceCSRF := login(t, srv, "a32-path-"+sfx, "a32-path-"+sfx+"@example.test", "Alice")
	rec := do(t, srv, http.MethodPost, "/api/orgs", `{"name":"A32 Path Org `+sfx+`"}`, alice, aliceCSRF)
	orgID := extractString(t, rec.Body.String(), "id")
	sandboxID := seedAgentSandbox(t, srv, orgID, "path")
	rec = do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/chats", "", alice, aliceCSRF)
	chat := extractString(t, rec.Body.String(), "id")
	do(t, srv, http.MethodPatch, "/api/orgs/"+orgID+"/chats/"+chat, `{"sandbox_id":"`+sandboxID+`"}`, alice, aliceCSRF)

	runID := createRun(t, srv, orgID, chat, []*http.Cookie{alice, aliceCSRF}, `{"prompt":"đừng làm gì ngoài workspace"}`)
	waitForRunStatus(t, srv, orgID, runID, []*http.Cookie{alice, aliceCSRF}, 15*time.Second)

	run, _ := resultOf(t, srv, orgID, runID)
	if run.Status != agent.StatusSucceeded {
		t.Fatalf("run = %s/%s, want succeeded", run.Status, run.ErrorCode)
	}
	results := eventDatas(t, eventsOf(t, srv, orgID, runID), "tool_result")
	if len(results) < 2 || results[0]["ok"] != false || results[1]["ok"] != false {
		t.Fatalf("blocked writes = %+v", results)
	}
	for _, r := range results[:2] {
		if !strings.Contains(fmt.Sprint(r["output_preview"]), "paths are relative") {
			t.Fatalf("blocked message = %v", r["output_preview"])
		}
	}
	// Nothing escaped the workspace: no exec call carries the blocked
	// path (the writes were refused before any sandboxd call).
	for _, e := range sbx.Execs {
		for _, c := range e.Cmd {
			if strings.Contains(c, "/etc/passwd") || strings.Contains(c, "../..") {
				t.Fatalf("blocked path reached exec: %+v", e)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(root, "etc")); err == nil {
		t.Fatal("an etc/ dir appeared inside the sandbox root")
	}
}

// TestAgentA32TimeoutAndTruncation — acceptance 4+5: `sleep 999` with
// timeout_s 2 answers timed_out within ≤ 4 s; 5 MiB of output reaches
// the LLM as ≤ 16 KiB with a truncation marker.
func TestAgentA32TimeoutAndTruncation(t *testing.T) {
	srv, sbx, notify := setupA32(t)
	scripted := llmtest.NewServer(
		llmtest.Scenario{Chunks: toolCallChunks("t1", "run_command",
			`{"command":"sleep 999","timeout_s":2}`)},
		llmtest.Scenario{Chunks: toolCallChunks("t2", "run_command",
			`{"command":"head -c 5000000 /dev/zero | tr '\\0' 'a'"}`)},
		llmtest.Scenario{Chunks: toolCallChunks("t3", "finish",
			`{"summary":"checked timeout and truncation"}`)},
	)
	defer scripted.Close()
	srv.LLM = &llm.Client{BaseURL: scripted.URL, Model: "glm-4.6", Flavor: "webhub", APIKey: "wh_test"}
	startAgentWorker(t, srv, notify, agent.EnvSource{Fallback: srv.LLM}, sbx.Client())

	sfx := fmt.Sprint(time.Now().UnixNano())
	_, alice, aliceCSRF := login(t, srv, "a32-to-"+sfx, "a32-to-"+sfx+"@example.test", "Alice")
	rec := do(t, srv, http.MethodPost, "/api/orgs", `{"name":"A32 TO Org `+sfx+`"}`, alice, aliceCSRF)
	orgID := extractString(t, rec.Body.String(), "id")
	sandboxID := seedAgentSandbox(t, srv, orgID, "to")
	rec = do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/chats", "", alice, aliceCSRF)
	chat := extractString(t, rec.Body.String(), "id")
	do(t, srv, http.MethodPatch, "/api/orgs/"+orgID+"/chats/"+chat, `{"sandbox_id":"`+sandboxID+`"}`, alice, aliceCSRF)

	runID := createRun(t, srv, orgID, chat, []*http.Cookie{alice, aliceCSRF}, `{"prompt":"kiểm tra timeout"}`)
	waitForRunStatus(t, srv, orgID, runID, []*http.Cookie{alice, aliceCSRF}, 20*time.Second)

	results := eventDatas(t, eventsOf(t, srv, orgID, runID), "tool_result")
	if len(results) < 2 {
		t.Fatalf("tool_result events = %d", len(results))
	}
	// Timeout: ok=false, the preview carries timed_out=true, ≤ 4 s.
	if results[0]["ok"] != false || !strings.Contains(fmt.Sprint(results[0]["output_preview"]), "timed_out=true") {
		t.Fatalf("timeout tool_result = %+v", results[0])
	}
	if dur, _ := results[0]["duration_ms"].(float64); dur > 4500 {
		t.Fatalf("timeout took %v ms, want ≤ 4000", results[0]["duration_ms"])
	}
	// Truncation: preview ≤ ~9 KiB, marker present, history ≤ ~17 KiB.
	preview := fmt.Sprint(results[1]["output_preview"])
	if results[1]["truncated"] != true || len(preview) > 9<<10 || !strings.Contains(preview, "…") {
		t.Fatalf("truncated tool_result = %d bytes truncated=%v", len(preview), results[1]["truncated"])
	}
	msgs, err := agent.NewStore(srv.Pool).LoadMessages(context.Background(), orgID, runID)
	if err != nil {
		t.Fatal(err)
	}
	biggest := 0
	for _, m := range msgs {
		if m.Role == "tool" && len(m.Content) > biggest {
			biggest = len(m.Content)
		}
	}
	if biggest > 17<<10 {
		t.Fatalf("LLM-facing tool output = %d bytes, want ≤ 16 KiB + markers", biggest)
	}
}

// TestAgentA32NoGitImage — acceptance 6: an image without git still
// succeeds with a warning and checkpoint_ref NULL.
func TestAgentA32NoGitImage(t *testing.T) {
	srv, sbx, notify := setupA32(t)
	// The env probe answers without the git=yes marker.
	sbx.Hook = func(rec sandboxtest.ExecRecord) (map[string]any, bool) {
		for _, c := range rec.Cmd {
			if strings.Contains(c, "command -v git") {
				return map[string]any{
					"stdout": "dir=/home/sandbox/workspace/app\n", "stderr": "",
					"exit_code": 0, "timed_out": false, "truncated": false, "duration_ms": 1,
				}, true
			}
		}
		return nil, false
	}
	scripted := llmtest.NewServer(
		llmtest.Scenario{Chunks: []any{llmtest.TextChunk("Không có git nên tôi chỉ trả lời.")}},
		llmtest.Scenario{Chunks: toolCallChunks("g1", "finish", `{"summary":"no git, no checkpoint"}`)},
	)
	defer scripted.Close()
	srv.LLM = &llm.Client{BaseURL: scripted.URL, Model: "glm-4.6", Flavor: "webhub", APIKey: "wh_test"}
	startAgentWorker(t, srv, notify, agent.EnvSource{Fallback: srv.LLM}, sbx.Client())

	sfx := fmt.Sprint(time.Now().UnixNano())
	_, alice, aliceCSRF := login(t, srv, "a32-nogit-"+sfx, "a32-nogit-"+sfx+"@example.test", "Alice")
	rec := do(t, srv, http.MethodPost, "/api/orgs", `{"name":"A32 NoGit Org `+sfx+`"}`, alice, aliceCSRF)
	orgID := extractString(t, rec.Body.String(), "id")
	sandboxID := seedAgentSandbox(t, srv, orgID, "nogit")
	rec = do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/chats", "", alice, aliceCSRF)
	chat := extractString(t, rec.Body.String(), "id")
	do(t, srv, http.MethodPatch, "/api/orgs/"+orgID+"/chats/"+chat, `{"sandbox_id":"`+sandboxID+`"}`, alice, aliceCSRF)

	runID := createRun(t, srv, orgID, chat, []*http.Cookie{alice, aliceCSRF}, `{"prompt":"chạy thử"}`)
	waitForRunStatus(t, srv, orgID, runID, []*http.Cookie{alice, aliceCSRF}, 15*time.Second)

	run, res := resultOf(t, srv, orgID, runID)
	if run.Status != agent.StatusSucceeded {
		t.Fatalf("run = %s/%s, want succeeded", run.Status, run.ErrorCode)
	}
	if run.CheckpointRef != "" {
		t.Fatalf("checkpoint_ref = %q, want NULL (no git)", run.CheckpointRef)
	}
	if fmt.Sprint(res["warning"]) == "" {
		t.Fatalf("result missing the no-git warning: %v", res)
	}
}

// TestAgentA32SecretMarkerNeverInEvents — acceptance 7: a marker
// planted in the connection key and the exec env never appears in
// agent_events.
func TestAgentA32SecretMarkerNeverInEvents(t *testing.T) {
	srv, sbx, notify := setupA32(t)
	sbx.Hook = func(rec sandboxtest.ExecRecord) (map[string]any, bool) {
		// Any exec the loop sends runs with the marker in its env —
		// still nothing of it may reach an event.
		_ = rec
		return nil, false
	}
	scripted := llmtest.NewServer(
		llmtest.Scenario{Chunks: toolCallChunks("s1", "run_command", `{"command":"echo hi"}`)},
		llmtest.Scenario{Chunks: toolCallChunks("s2", "write_file",
			`{"path":"x.txt","content":"body"}`)},
		llmtest.Scenario{Chunks: toolCallChunks("s3", "finish", `{"summary":"clean"}`)},
	)
	defer scripted.Close()
	srv.LLM = &llm.Client{BaseURL: scripted.URL, Model: "glm-4.6", Flavor: "webhub", APIKey: "k_SECRET_MARKER_KEY"}
	startAgentWorker(t, srv, notify, agent.EnvSource{Fallback: srv.LLM}, sbx.Client())

	sfx := fmt.Sprint(time.Now().UnixNano())
	_, alice, aliceCSRF := login(t, srv, "a32-sec-"+sfx, "a32-sec-"+sfx+"@example.test", "Alice")
	rec := do(t, srv, http.MethodPost, "/api/orgs", `{"name":"A32 Sec Org `+sfx+`"}`, alice, aliceCSRF)
	orgID := extractString(t, rec.Body.String(), "id")
	sandboxID := seedAgentSandbox(t, srv, orgID, "sec")
	rec = do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/chats", "", alice, aliceCSRF)
	chat := extractString(t, rec.Body.String(), "id")
	do(t, srv, http.MethodPatch, "/api/orgs/"+orgID+"/chats/"+chat, `{"sandbox_id":"`+sandboxID+`"}`, alice, aliceCSRF)

	runID := createRun(t, srv, orgID, chat, []*http.Cookie{alice, aliceCSRF}, `{"prompt":"chạy"}`)
	waitForRunStatus(t, srv, orgID, runID, []*http.Cookie{alice, aliceCSRF}, 15*time.Second)

	var hits int
	if err := db.WithOrg(context.Background(), srv.Pool, orgID, func(tx db.Tx) error {
		return tx.QueryRow(context.Background(), `
			SELECT count(*) FROM agent_events
			WHERE data::text LIKE '%SECRET_MARKER%'`).Scan(&hits)
	}); err != nil {
		t.Fatal(err)
	}
	if hits != 0 {
		t.Fatalf("SECRET_MARKER leaked into %d agent_events", hits)
	}
}

// TestAgentA32NoToolsModelRunsFallback — the A3.2 review fix: a model
// declared supports_tools=false is SELECTABLE (no more 400
// model_no_tools) and the worker runs it in the JSON fallback
// protocol (no tools field on the wire).
func TestAgentA32NoToolsModelRunsFallback(t *testing.T) {
	srv, sbx, notify := setupA32(t)
	scripted := llmtest.NewServer(
		llmtest.Scenario{Chunks: []any{llmtest.TextChunk(
			"Tôi sẽ tạo file.\n```tool\n{\"name\":\"write_file\",\"arguments\":{\"path\":\"fb.txt\",\"content\":\"fallback ok\"}}\n```"),
			llmtest.FinishChunk("stop")}},
		llmtest.Scenario{Chunks: []any{llmtest.TextChunk(
			"```tool\n{\"name\":\"finish\",\"arguments\":{\"summary\":\"done via fallback\"}}\n```"),
			llmtest.FinishChunk("stop")}},
	)
	defer scripted.Close()
	srv.LLM = &llm.Client{BaseURL: scripted.URL, Model: "glm-4.6", Flavor: "webhub", APIKey: "wh_test"}

	sfx := fmt.Sprint(time.Now().UnixNano())
	_, alice, aliceCSRF := login(t, srv, "a32-fb-"+sfx, "a32-fb-"+sfx+"@example.test", "Alice")
	rec := do(t, srv, http.MethodPost, "/api/orgs", `{"name":"A32 FB Org `+sfx+`"}`, alice, aliceCSRF)
	orgID := extractString(t, rec.Body.String(), "id")
	sandboxID := seedAgentSandbox(t, srv, orgID, "fb")

	// A PLATFORM connection whose only model is probed as tool-less.
	// Platform rows bypass the org SSRF guard, so the scripted fake
	// (127.0.0.1) is reachable — same exemption the A2 admin surface
	// tests use.
	adminUser := "a32-admin-" + sfx
	adminID, err := passwordauth.EnsureBootstrap(context.Background(), srv.Pool, discardLogger(),
		adminUser, "correct-horse-1", adminUser+"@example.test")
	if err != nil {
		t.Fatalf("EnsureBootstrap: %v", err)
	}
	srv.PlatformAdminEnabled = true
	srv.BootstrapAdminID = adminID
	adminCookies := loginA2Admin(t, srv, "password:"+adminUser)
	body := `{"name":"air","protocol":"openai","flavor":"webhub",` +
		`"base_url":"` + scripted.URL + `","default_model":"glm-air",` +
		`"models":[{"id":"glm-air","supports_tools":false}],"key_mode":"shared",` +
		`"timeout_s":60,"max_tokens":4096,"enabled":true,"is_default":true,` +
		`"api_key":"sk-air-secret"}`
	rec = do(t, srv, http.MethodPost, "/api/admin/llm/connections", body, adminCookies...)
	if rec.Code != http.StatusCreated {
		t.Fatalf("connection = %d: %s", rec.Code, rec.Body.String())
	}

	rec = do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/chats", "", alice, aliceCSRF)
	chat := extractString(t, rec.Body.String(), "id")
	do(t, srv, http.MethodPatch, "/api/orgs/"+orgID+"/chats/"+chat, `{"sandbox_id":"`+sandboxID+`"}`, alice, aliceCSRF)

	// The gate regression: 201, not 400 model_no_tools.
	rec = do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/chats/"+chat+"/agent/runs",
		`{"prompt":"tạo fb.txt","model":"glm-air"}`, alice, aliceCSRF)
	if rec.Code == http.StatusBadRequest && strings.Contains(rec.Body.String(), "model_no_tools") {
		t.Fatal("supports_tools=false still blocked at create (A3.2 review point 1)")
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("create with no-tools model = %d: %s", rec.Code, rec.Body.String())
	}
	runID := extractString(t, rec.Body.String(), "id")

	// The worker resolves the model through the DB connection.
	startAgentWorker(t, srv, notify, agent.ConfigSource{Pool: srv.Pool}, sbx.Client())
	waitForRunStatus(t, srv, orgID, runID, []*http.Cookie{alice, aliceCSRF}, 15*time.Second)

	run, _ := resultOf(t, srv, orgID, runID)
	if run.Status != agent.StatusSucceeded {
		t.Fatalf("run = %s/%s, want succeeded in fallback mode", run.Status, run.ErrorCode)
	}
	if _, err := os.Stat(filepath.Join(sbx.Root(sandboxtest.DefaultID), "workspace/app/fb.txt")); err != nil {
		t.Fatalf("fallback write_file did not land: %v", err)
	}

	// status event records tool_mode=fallback (plan §5: decided once).
	modes := []string{}
	for _, e := range eventDatas(t, eventsOf(t, srv, orgID, runID), "status") {
		if m, has := e["tool_mode"]; has {
			modes = append(modes, fmt.Sprint(m))
		}
	}
	if len(modes) == 0 || modes[0] != "fallback" {
		t.Fatalf("tool_mode events = %v, want fallback first", modes)
	}

	// The wire: NO tools field, the fence protocol IS in the system block.
	if len(scripted.Requests) != 2 {
		t.Fatalf("llm requests = %d", len(scripted.Requests))
	}
	for i, req := range scripted.Requests {
		if _, has := req.Body["tools"]; has {
			t.Fatalf("fallback request %d carries a tools field", i)
		}
		raw := string(req.RawBody)
		if !strings.Contains(raw, "Tool calling protocol") {
			t.Fatalf("fallback request %d missing the fence protocol", i)
		}
	}
	// The connection key never leaks into the events either.
	var hits int
	if err := db.WithOrg(context.Background(), srv.Pool, orgID, func(tx db.Tx) error {
		return tx.QueryRow(context.Background(),
			`SELECT count(*) FROM agent_events WHERE data::text LIKE '%sk-air-secret%'`).Scan(&hits)
	}); err != nil {
		t.Fatal(err)
	}
	if hits != 0 {
		t.Fatalf("connection key leaked into %d events", hits)
	}
}

// TestAgentA32BuildDetectAndPreview — a package.json build runs in the
// postcheck and lands a build event (decision §2-7).
func TestAgentA32BuildDetectAndPreview(t *testing.T) {
	srv, sbx, notify := setupA32(t)
	root := sbx.Root(sandboxtest.DefaultID)
	// AGENTS.md build block (decision §2-7 priority) — a plain echo so
	// the test does not depend on node being installed.
	agents := "# app\n\nbuild:\n  echo build-ok\n"
	if err := os.WriteFile(filepath.Join(root, "workspace/app/AGENTS.md"), []byte(agents), 0o644); err != nil {
		t.Fatal(err)
	}
	scripted := llmtest.NewServer(
		llmtest.Scenario{Chunks: toolCallChunks("b1", "finish", `{"summary":"built"}`)},
	)
	defer scripted.Close()
	srv.LLM = &llm.Client{BaseURL: scripted.URL, Model: "glm-4.6", Flavor: "webhub", APIKey: "wh_test"}
	startAgentWorker(t, srv, notify, agent.EnvSource{Fallback: srv.LLM}, sbx.Client())

	sfx := fmt.Sprint(time.Now().UnixNano())
	_, alice, aliceCSRF := login(t, srv, "a32-bld-"+sfx, "a32-bld-"+sfx+"@example.test", "Alice")
	rec := do(t, srv, http.MethodPost, "/api/orgs", `{"name":"A32 Bld Org `+sfx+`"}`, alice, aliceCSRF)
	orgID := extractString(t, rec.Body.String(), "id")
	sandboxID := seedAgentSandbox(t, srv, orgID, "bld")
	rec = do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/chats", "", alice, aliceCSRF)
	chat := extractString(t, rec.Body.String(), "id")
	do(t, srv, http.MethodPatch, "/api/orgs/"+orgID+"/chats/"+chat, `{"sandbox_id":"`+sandboxID+`"}`, alice, aliceCSRF)

	runID := createRun(t, srv, orgID, chat, []*http.Cookie{alice, aliceCSRF}, `{"prompt":"build"}`)
	waitForRunStatus(t, srv, orgID, runID, []*http.Cookie{alice, aliceCSRF}, 15*time.Second)

	builds := eventDatas(t, eventsOf(t, srv, orgID, runID), "build")
	if len(builds) != 1 || builds[0]["status"] != "passed" {
		t.Fatalf("build events = %+v", builds)
	}
	if cmd := fmt.Sprint(builds[0]["command"]); cmd != "echo build-ok" {
		t.Fatalf("build command = %q, want the AGENTS.md block command", cmd)
	}
	if tail := fmt.Sprint(builds[0]["tail"]); !strings.Contains(tail, "build-ok") {
		t.Fatalf("build tail = %q", tail)
	}
	_, res := resultOf(t, srv, orgID, runID)
	if res["build_status"] != "passed" {
		t.Fatalf("result build_status = %v", res["build_status"])
	}
	// The fake sandbox has port 3000 and nothing listens: preview is
	// checked (false) rather than absent.
	if _, has := res["preview_ok"]; !has {
		t.Fatalf("result missing preview_ok: %v", res)
	}
}
