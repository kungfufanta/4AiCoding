// agent_a33_test.go — A3.3 acceptance (docs/AGENT_PLAN_A3.md §11
// "A3.3 Xong khi"), driven end to end through the BFF and /v1 with a
// scripted LLM (llmtest) and the fake sandboxd (real git + bash):
//
//	revert of "hello.txt + rewrote README" ⇒ hello.txt gone,
//	  README back to the user's bytes, user files untouched
//	revert of a non-latest run    ⇒ 409 not_latest_run
//	member (not creator/admin)    ⇒ 403; foreign org ⇒ 404
//	already reverted              ⇒ 409 already_reverted
//	max_tokens exceeded           ⇒ failed/token_budget
//	usage_events                  ⇒ agent_tokens/agent_steps sums
//	failed run in a chat          ⇒ "(run failed: …)" assistant note
package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
)

// setupA33 wires one A3.3 test: fake LLM + fake sandboxd rooted at a
// temp dir whose project directory exists, with the SERVER's sandbox
// gateway pointed at the fake (the revert handlers exec through it).
func setupA33(t *testing.T) (*Server, *sandboxtest.Server, *agent.Notifier) {
	t.Helper()
	srv, _, notify, _ := setupAgent(t)
	sbx := sandboxtest.New(t.TempDir())
	t.Cleanup(sbx.Close)
	if err := os.MkdirAll(filepath.Join(sbx.Root(sandboxtest.DefaultID), "workspace/app"), 0o755); err != nil {
		t.Fatal(err)
	}
	srv.Sandboxd = sbx.Client()
	return srv, sbx, notify
}

// seedChat makes a chat and attaches the sandbox (plan §9.1 flow).
func seedChat(t *testing.T, srv *Server, orgID, sandboxID string, cookies ...*http.Cookie) string {
	t.Helper()
	rec := do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/chats", "", cookies...)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create chat = %d: %s", rec.Code, rec.Body.String())
	}
	chat := extractString(t, rec.Body.String(), "id")
	rec = do(t, srv, http.MethodPatch, "/api/orgs/"+orgID+"/chats/"+chat,
		`{"sandbox_id":"`+sandboxID+`"}`, cookies...)
	if rec.Code != http.StatusOK {
		t.Fatalf("attach sandbox = %d: %s", rec.Code, rec.Body.String())
	}
	return chat
}

// v1Do issues a /v1 request with a Bearer key.
func v1Do(t *testing.T, srv *Server, method, path, body, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// chatNote queries the assistant note a run left in chat_messages
// (the "(run …)" line) — direct DB, the chat API shape is UI's job.
// RLS pins org AND user on chat_messages, so the creator's id rides
// along (WithUserID), exactly like the worker's SaveSummary.
func chatNote(t *testing.T, srv *Server, orgID, userID, runID string) string {
	t.Helper()
	note := ""
	ctx := db.WithUserID(context.Background(), userID)
	err := db.WithOrg(ctx, srv.Pool, orgID, func(tx db.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT content FROM chat_messages WHERE agent_run_id = $1::uuid AND role = 'assistant'`,
			runID).Scan(&note)
	})
	if err != nil {
		t.Fatalf("chat note: %v", err)
	}
	return note
}

// TestAgentA33RevertRestoresFiles — the headline scenario: the run
// creates hello.txt and rewrites README.md; revert removes exactly
// hello.txt, restores the user's README bytes and never touches the
// user's other files.
func TestAgentA33RevertRestoresFiles(t *testing.T) {
	srv, sbx, notify := setupA33(t)
	sfx := fmt.Sprint(time.Now().UnixNano())
	_, alice, aliceCSRF := login(t, srv, "a33-alice-"+sfx, "a33-alice-"+sfx+"@example.test", "Alice")
	rec := do(t, srv, http.MethodPost, "/api/orgs", `{"name":"A33 Revert `+sfx+`"}`, alice, aliceCSRF)
	orgID := extractString(t, rec.Body.String(), "id")
	sandboxID := seedAgentSandbox(t, srv, orgID, "rev")
	chat := seedChat(t, srv, orgID, sandboxID, alice, aliceCSRF)

	// The user's files BEFORE the run.
	appDir := filepath.Join(sbx.Root(sandboxtest.DefaultID), "workspace", "app")
	if err := os.WriteFile(filepath.Join(appDir, "README.md"), []byte("user original README"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "notes.txt"), []byte("user notes"), 0o644); err != nil {
		t.Fatal(err)
	}

	scripted := llmtest.NewServer(
		llmtest.Scenario{Chunks: toolCallChunks("c1", "write_file",
			`{"path":"hello.txt","content":"Hello A3.3"}`)},
		llmtest.Scenario{Chunks: toolCallChunks("c2", "write_file",
			`{"path":"README.md","content":"agent rewrote the README"}`)},
		llmtest.Scenario{Chunks: toolCallChunks("c3", "finish",
			`{"summary":"created hello.txt and rewrote README.md"}`)},
	)
	defer scripted.Close()
	srv.LLM = &llm.Client{BaseURL: scripted.URL, Model: "glm-4.6", Flavor: "webhub", APIKey: "wh_test"}
	startAgentWorker(t, srv, notify, agent.EnvSource{Fallback: srv.LLM}, sbx.Client())

	runID := createRun(t, srv, orgID, chat, []*http.Cookie{alice, aliceCSRF}, `{"prompt":"viết hello và sửa README"}`)
	waitForRunStatus(t, srv, orgID, runID, []*http.Cookie{alice, aliceCSRF}, 15*time.Second)

	_, res := resultOf(t, srv, orgID, runID)
	filesJSON, _ := json.Marshal(res["files_changed"])
	files := string(filesJSON)
	if !strings.Contains(files, `"path":"hello.txt"`) || !strings.Contains(files, `"status":"A"`) {
		t.Fatalf("files_changed missing hello.txt A: %s", files)
	}
	if !strings.Contains(files, `"path":"README.md"`) || !strings.Contains(files, `"status":"M"`) {
		t.Fatalf("files_changed missing README.md M: %s", files)
	}
	if b, err := os.ReadFile(filepath.Join(appDir, "hello.txt")); err != nil || string(b) != "Hello A3.3" {
		t.Fatalf("hello.txt after run = %q, %v", b, err)
	}

	// The creator reverts.
	rec = do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/agent/runs/"+runID+"/revert", "", alice, aliceCSRF)
	if rec.Code != http.StatusOK {
		t.Fatalf("revert = %d: %s, want 200", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"reverted_at"`) {
		t.Fatalf("revert response missing reverted_at: %s", rec.Body.String())
	}

	// Filesystem assertions — the "Xong khi" sentence, byte for byte.
	if _, err := os.Stat(filepath.Join(appDir, "hello.txt")); !os.IsNotExist(err) {
		t.Fatalf("hello.txt still exists after revert (err=%v)", err)
	}
	if b, err := os.ReadFile(filepath.Join(appDir, "README.md")); err != nil || string(b) != "user original README" {
		t.Fatalf("README.md after revert = %q, %v; want the user's original bytes", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(appDir, "notes.txt")); err != nil || string(b) != "user notes" {
		t.Fatalf("notes.txt after revert = %q, %v; a user file the run never touched must survive", b, err)
	}

	// The reverted event carries counts and an empty files_remaining.
	restored := 0
	removed := 0
	remaining := -1
	for _, e := range eventDatas(t, eventsOf(t, srv, orgID, runID), "reverted") {
		restored = int(e["files_restored"].(float64))
		removed = int(e["files_removed"].(float64))
		remaining = len(e["files_remaining"].([]any))
	}
	if restored != 1 || removed != 1 || remaining != 0 {
		t.Fatalf("reverted event = restored %d, removed %d, remaining %d; want 1/1/0",
			restored, removed, remaining)
	}

	// A second revert answers 409 already_reverted.
	if rec := do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/agent/runs/"+runID+"/revert", "", alice, aliceCSRF); rec.Code != http.StatusConflict {
		t.Fatalf("second revert = %d, want 409 already_reverted", rec.Code)
	}
}

// TestAgentA33RevertNotLatest — only the newest run of the sandbox is
// revertible (plan §8); the older one answers 409 not_latest_run.
func TestAgentA33RevertNotLatest(t *testing.T) {
	srv, sbx, notify := setupA33(t)
	sfx := fmt.Sprint(time.Now().UnixNano())
	_, alice, aliceCSRF := login(t, srv, "a33-late-"+sfx, "a33-late-"+sfx+"@example.test", "Alice")
	rec := do(t, srv, http.MethodPost, "/api/orgs", `{"name":"A33 NotLatest `+sfx+`"}`, alice, aliceCSRF)
	orgID := extractString(t, rec.Body.String(), "id")
	sandboxID := seedAgentSandbox(t, srv, orgID, "late")
	chat := seedChat(t, srv, orgID, sandboxID, alice, aliceCSRF)

	scripted := llmtest.NewServer(
		llmtest.Scenario{Chunks: toolCallChunks("c1", "write_file",
			`{"path":"first.txt","content":"first run"}`)},
		llmtest.Scenario{Chunks: toolCallChunks("c2", "finish", `{"summary":"first"}`)},
		llmtest.Scenario{Chunks: []any{llmtest.TextChunk("second run done")}},
	)
	defer scripted.Close()
	srv.LLM = &llm.Client{BaseURL: scripted.URL, Model: "glm-4.6", Flavor: "webhub", APIKey: "wh_test"}
	startAgentWorker(t, srv, notify, agent.EnvSource{Fallback: srv.LLM}, sbx.Client())

	run1 := createRun(t, srv, orgID, chat, []*http.Cookie{alice, aliceCSRF}, `{"prompt":"run một"}`)
	waitForRunStatus(t, srv, orgID, run1, []*http.Cookie{alice, aliceCSRF}, 15*time.Second)
	run2 := createRun(t, srv, orgID, chat, []*http.Cookie{alice, aliceCSRF}, `{"prompt":"run hai"}`)
	waitForRunStatus(t, srv, orgID, run2, []*http.Cookie{alice, aliceCSRF}, 15*time.Second)

	rec = do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/agent/runs/"+run1+"/revert", "", alice, aliceCSRF)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "not_latest_run") {
		t.Fatalf("revert of older run = %d: %s, want 409 not_latest_run", rec.Code, rec.Body.String())
	}
	// The newest run still reverts, and first.txt (created by run 1,
	// tracked in run 2's checkpoint) SURVIVES a run-2 revert.
	rec = do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/agent/runs/"+run2+"/revert", "", alice, aliceCSRF)
	if rec.Code != http.StatusOK {
		t.Fatalf("revert of latest run = %d: %s, want 200", rec.Code, rec.Body.String())
	}
	appDir := filepath.Join(sbx.Root(sandboxtest.DefaultID), "workspace", "app")
	if b, err := os.ReadFile(filepath.Join(appDir, "first.txt")); err != nil || string(b) != "first run" {
		t.Fatalf("first.txt after latest-run revert = %q, %v; an older run's file belongs to the tree", b, err)
	}
}

// TestAgentA33RevertPermissions — plan §2-6: creator or org admin;
// a plain member gets 403, a non-member 404 (no existence leak).
func TestAgentA33RevertPermissions(t *testing.T) {
	srv, sbx, notify := setupA33(t)
	sfx := fmt.Sprint(time.Now().UnixNano())
	_, alice, aliceCSRF := login(t, srv, "a33-own-"+sfx, "a33-own-"+sfx+"@example.test", "Alice")
	_, mem, memCSRF := login(t, srv, "a33-mem-"+sfx, "a33-mem-"+sfx+"@example.test", "Mem")
	_, adm, admCSRF := login(t, srv, "a33-adm-"+sfx, "a33-adm-"+sfx+"@example.test", "Adm")
	_, out, outCSRF := login(t, srv, "a33-out-"+sfx, "a33-out-"+sfx+"@example.test", "Out")

	rec := do(t, srv, http.MethodPost, "/api/orgs", `{"name":"A33 Perm `+sfx+`"}`, alice, aliceCSRF)
	orgID := extractString(t, rec.Body.String(), "id")
	sandboxID := seedAgentSandbox(t, srv, orgID, "perm")
	chat := seedChat(t, srv, orgID, sandboxID, alice, aliceCSRF)

	// mem = member, adm = admin (invite → accept).
	join := func(email, role string, c, csrf *http.Cookie) {
		rec := do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/invites",
			`{"email":"`+email+`","role":"`+role+`"}`, alice, aliceCSRF)
		if rec.Code != http.StatusCreated {
			t.Fatalf("invite %s = %d: %s", email, rec.Code, rec.Body.String())
		}
		u := extractString(t, rec.Body.String(), "url")
		token := u[strings.Index(u, "token=")+len("token="):]
		if rec := do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/invites/accept",
			`{"token":"`+token+`"}`, c, csrf); rec.Code != http.StatusOK {
			t.Fatalf("accept %s = %d: %s", email, rec.Code, rec.Body.String())
		}
	}
	join("a33-mem-"+sfx+"@example.test", "member", mem, memCSRF)
	join("a33-adm-"+sfx+"@example.test", "admin", adm, admCSRF)

	scripted := llmtest.NewServer(
		llmtest.Scenario{Chunks: []any{llmtest.TextChunk("done quickly")}},
	)
	defer scripted.Close()
	srv.LLM = &llm.Client{BaseURL: scripted.URL, Model: "glm-4.6", Flavor: "webhub", APIKey: "wh_test"}
	startAgentWorker(t, srv, notify, agent.EnvSource{Fallback: srv.LLM}, sbx.Client())

	runID := createRun(t, srv, orgID, chat, []*http.Cookie{alice, aliceCSRF}, `{"prompt":"gì cũng được"}`)
	waitForRunStatus(t, srv, orgID, runID, []*http.Cookie{alice, aliceCSRF}, 15*time.Second)

	// Non-member → 404.
	if rec := do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/agent/runs/"+runID+"/revert", "", out, outCSRF); rec.Code != http.StatusNotFound {
		t.Fatalf("non-member revert = %d, want 404", rec.Code)
	}
	// Member, not the creator → 403.
	if rec := do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/agent/runs/"+runID+"/revert", "", mem, memCSRF); rec.Code != http.StatusForbidden {
		t.Fatalf("member revert = %d: %s, want 403", rec.Code, rec.Body.String())
	}
	// Admin, not the creator → 200.
	if rec := do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/agent/runs/"+runID+"/revert", "", adm, admCSRF); rec.Code != http.StatusOK {
		t.Fatalf("admin revert = %d: %s, want 200", rec.Code, rec.Body.String())
	}
}

// TestAgentA33RevertV1 — the /v1 surface: agent:run scope reverts,
// a read-only key 403s, a foreign org's key 404s.
func TestAgentA33RevertV1(t *testing.T) {
	srv, sbx, notify := setupA33(t)
	sfx := fmt.Sprint(time.Now().UnixNano())
	_, alice, aliceCSRF := login(t, srv, "a33-v1-"+sfx, "a33-v1-"+sfx+"@example.test", "Alice")
	_, bob, bobCSRF := login(t, srv, "a33-v1b-"+sfx, "a33-v1b-"+sfx+"@example.test", "Bob")
	rec := do(t, srv, http.MethodPost, "/api/orgs", `{"name":"A33 V1 `+sfx+`"}`, alice, aliceCSRF)
	orgA := extractString(t, rec.Body.String(), "id")
	rec = do(t, srv, http.MethodPost, "/api/orgs", `{"name":"A33 V1 B `+sfx+`"}`, bob, bobCSRF)
	orgB := extractString(t, rec.Body.String(), "id")
	sandboxA := seedAgentSandbox(t, srv, orgA, "v1")

	keyA, _ := createKey(t, srv, []*http.Cookie{alice, aliceCSRF}, orgA, "agent-a",
		`["agent:run","agent:read"]`)
	keyARead, _ := createKey(t, srv, []*http.Cookie{alice, aliceCSRF}, orgA, "read-a",
		`["agent:read"]`)
	keyB, _ := createKey(t, srv, []*http.Cookie{bob, bobCSRF}, orgB, "agent-b",
		`["agent:run","agent:read"]`)

	scripted := llmtest.NewServer(
		llmtest.Scenario{Chunks: []any{llmtest.TextChunk("v1 run done")}},
	)
	defer scripted.Close()
	srv.LLM = &llm.Client{BaseURL: scripted.URL, Model: "glm-4.6", Flavor: "webhub", APIKey: "wh_test"}
	startAgentWorker(t, srv, notify, agent.EnvSource{Fallback: srv.LLM}, sbx.Client())

	// Create through /v1 (agent token-free key): run without a chat.
	req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes/"+sandboxA+"/agent/runs",
		strings.NewReader(`{"prompt":"v1 revert test"}`))
	req.Header.Set("Authorization", "Bearer "+keyA)
	recV1 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recV1, req)
	if recV1.Code != http.StatusCreated {
		t.Fatalf("v1 create = %d: %s", recV1.Code, recV1.Body.String())
	}
	runID := extractString(t, recV1.Body.String(), "id")
	waitForRunStatus(t, srv, orgA, runID, []*http.Cookie{alice, aliceCSRF}, 15*time.Second)

	// Read-only key → 403 (scope agent:run required).
	if rec := v1Do(t, srv, http.MethodPost, "/v1/agent/runs/"+runID+"/revert", "", keyARead); rec.Code != http.StatusForbidden {
		t.Fatalf("read-only key revert = %d, want 403", rec.Code)
	}
	// Foreign org's key → 404.
	if rec := v1Do(t, srv, http.MethodPost, "/v1/agent/runs/"+runID+"/revert", "", keyB); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-org key revert = %d, want 404", rec.Code)
	}
	// The org's key with agent:run → 200.
	if rec := v1Do(t, srv, http.MethodPost, "/v1/agent/runs/"+runID+"/revert", "", keyA); rec.Code != http.StatusOK {
		t.Fatalf("v1 revert = %d: %s, want 200", rec.Code, rec.Body.String())
	}
}

// TestAgentA33TokenBudgetAndUsage — max_tokens exceeded ⇒
// failed/token_budget, and usage_events carries the run's exact
// token/steps totals (plan §11 "A3.3 Xong khi" criteria 4+5).
func TestAgentA33TokenBudgetAndUsage(t *testing.T) {
	srv, sbx, notify := setupA33(t)
	sfx := fmt.Sprint(time.Now().UnixNano())
	_, alice, aliceCSRF := login(t, srv, "a33-budget-"+sfx, "a33-budget-"+sfx+"@example.test", "Alice")
	rec := do(t, srv, http.MethodPost, "/api/orgs", `{"name":"A33 Budget `+sfx+`"}`, alice, aliceCSRF)
	orgID := extractString(t, rec.Body.String(), "id")
	sandboxID := seedAgentSandbox(t, srv, orgID, "budget")

	scripted := llmtest.NewServer(
		llmtest.Scenario{Chunks: []any{
			llmtest.UsageChunk(50000, 60000),
			llmtest.FinishChunk("stop"),
		}},
	)
	defer scripted.Close()
	srv.LLM = &llm.Client{BaseURL: scripted.URL, Model: "glm-4.6", Flavor: "webhub", APIKey: "wh_test"}
	startAgentWorker(t, srv, notify, agent.EnvSource{Fallback: srv.LLM}, sbx.Client())

	// Created directly (the BFF does not expose max_tokens): a 100
	// token budget blown by the first usage chunk.
	ctx := context.Background()
	created, _, err := agent.NewStore(srv.Pool).CreateRun(ctx, agent.CreateIn{
		OrgID: orgID, SandboxID: sandboxID, Prompt: "too expensive",
		Model: "glm-4.6", MaxSteps: 5, MaxTokens: 100,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	waitForRunStatus(t, srv, orgID, created.ID, []*http.Cookie{alice, aliceCSRF}, 15*time.Second)

	rec = do(t, srv, http.MethodGet, "/api/orgs/"+orgID+"/agent/runs/"+created.ID, "", alice, aliceCSRF)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"error_code":"token_budget"`) {
		t.Fatalf("run = %d: %s, want failed/token_budget", rec.Code, rec.Body.String())
	}

	// usage_events: exactly one agent_tokens + one agent_steps row for
	// the run, with the run's own totals.
	type row struct {
		Kind     string
		Quantity int64
		Unit     string
	}
	rows := []row{}
	if err := db.WithOrg(ctx, srv.Pool, orgID, func(tx db.Tx) error {
		rs, err := tx.Query(ctx, `
                        SELECT kind, quantity, unit FROM usage_events
                        WHERE sandbox_id = $1 AND kind LIKE 'agent_%' ORDER BY kind`, sandboxID)
		if err != nil {
			return err
		}
		defer rs.Close()
		for rs.Next() {
			var r row
			if err := rs.Scan(&r.Kind, &r.Quantity, &r.Unit); err != nil {
				return err
			}
			rows = append(rows, r)
		}
		return rs.Err()
	}); err != nil {
		t.Fatalf("usage query: %v", err)
	}
	if len(rows) != 2 ||
		rows[0] != (row{"agent_steps", 1, "steps"}) ||
		rows[1] != (row{"agent_tokens", 110000, "tokens"}) {
		t.Fatalf("usage rows = %+v, want [steps 1 / tokens 110000]", rows)
	}
}

// TestAgentA33FailedRunNote — a failed run in a chat leaves the
// "(run failed: …)" assistant note (A3.3 "tóm tắt run trước khi
// Tiếp tục": the next run of the conversation sees what happened).
func TestAgentA33FailedRunNote(t *testing.T) {
	srv, sbx, notify := setupA33(t)
	sfx := fmt.Sprint(time.Now().UnixNano())
	aliceID, alice, aliceCSRF := login(t, srv, "a33-note-"+sfx, "a33-note-"+sfx+"@example.test", "Alice")
	rec := do(t, srv, http.MethodPost, "/api/orgs", `{"name":"A33 Note `+sfx+`"}`, alice, aliceCSRF)
	orgID := extractString(t, rec.Body.String(), "id")
	sandboxID := seedAgentSandbox(t, srv, orgID, "note")
	chat := seedChat(t, srv, orgID, sandboxID, alice, aliceCSRF)

	// Two 400s that mention tools: the first flips the run to fallback
	// mode (plan §5), the second is then a plain llm_error → failed.
	scripted := llmtest.NewServer(
		llmtest.Scenario{Status: 400, Body: `{"error":{"message":"tools are not accepted"}}`},
		llmtest.Scenario{Status: 400, Body: `{"error":{"message":"tools are not accepted"}}`},
	)
	defer scripted.Close()
	srv.LLM = &llm.Client{BaseURL: scripted.URL, Model: "glm-4.6", Flavor: "webhub", APIKey: "wh_test"}
	startAgentWorker(t, srv, notify, agent.EnvSource{Fallback: srv.LLM}, sbx.Client())

	runID := createRun(t, srv, orgID, chat, []*http.Cookie{alice, aliceCSRF}, `{"prompt":"sẽ thất bại"}`)
	waitForRunStatus(t, srv, orgID, runID, []*http.Cookie{alice, aliceCSRF}, 15*time.Second)

	rec = do(t, srv, http.MethodGet, "/api/orgs/"+orgID+"/agent/runs/"+runID, "", alice, aliceCSRF)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"error_code":"llm_error"`) {
		t.Fatalf("run = %d: %s, want failed/llm_error", rec.Code, rec.Body.String())
	}
	if note := chatNote(t, srv, orgID, aliceID, runID); note != "(run failed: llm_error)" {
		t.Fatalf("chat note = %q, want %q", note, "(run failed: llm_error)")
	}
}
