// agent_a34_test.go — A3.4 backend surface: the conversation JSON now
// carries sandbox_id (the Chat header renders the attach dropdown from
// it) and the sandbox-scoped agent-runs list powers the composer lock
// ("Agent đang chạy (từ API)" when a /v1-run or another chat's run
// holds the sandbox). Real Postgres; no worker needed — the runs stay
// queued and that is exactly the state the lock cares about.
package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/tastyeffectco/sandboxd/portal/internal/agent"
	"github.com/tastyeffectco/sandboxd/portal/internal/db"
)

func createChatRow(t *testing.T, srv *Server, orgID string, cookies ...*http.Cookie) string {
	t.Helper()
	rec := do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/chats", "", cookies...)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create chat = %d: %s", rec.Code, rec.Body.String())
	}
	return extractString(t, rec.Body.String(), "id")
}

func insertQueuedRun(t *testing.T, srv *Server, orgID, userID, sandboxID, conversationID, prompt string) string {
	t.Helper()
	store := agent.NewStore(srv.Pool)
	// chat_conversations RLS pins org AND user — the insert must ride
	// alice's identity exactly like the BFF create handler does.
	ctx := db.WithUserID(context.Background(), userID)
	run, _, err := store.CreateRun(ctx, agent.CreateIn{
		OrgID:          orgID,
		SandboxID:      sandboxID,
		ConversationID: conversationID,
		Model:          "glm-4.6",
		Prompt:         prompt,
		MaxSteps:       5,
	})
	if err != nil {
		t.Fatalf("insert queued run: %v", err)
	}
	return run.ID
}

func decodeRuns(t *testing.T, body string) []map[string]any {
	t.Helper()
	var runs []map[string]any
	if err := json.Unmarshal([]byte(body), &runs); err != nil {
		t.Fatalf("decode runs: %v (%s)", err, body)
	}
	return runs
}

// finishRun marks a run terminal so the next CreateRun on the same
// sandbox passes the single-active gate (plan §2: one active run per
// sandbox).
func finishRun(t *testing.T, srv *Server, orgID, runID string) {
	t.Helper()
	store := agent.NewStore(srv.Pool)
	err := store.SetFinal(context.Background(), orgID, runID, agent.FinalState{
		Status:    agent.StatusSucceeded,
		SandboxID: "",
		Steps:     1,
	})
	if err != nil {
		t.Fatalf("finish run %s: %v", runID, err)
	}
}

// TestBFFChatSandboxID — create/list return sandbox_id ("" before
// attach, the id after) so the SPA header can render the attach
// dropdown without a second round trip.
func TestBFFChatSandboxID(t *testing.T) {
	srv := setupBFF(t)
	cleanAgent(t)

	sfx := fmt.Sprint(time.Now().UnixNano())
	aliceID, alice, aliceCSRF := login(t, srv, "a34-alice-"+sfx, "a34-alice-"+sfx+"@example.test", "Alice")
	rec := do(t, srv, http.MethodPost, "/api/orgs", `{"name":"A34 Org `+sfx+`"}`, alice, aliceCSRF)
	orgID := extractString(t, rec.Body.String(), "id")
	sandboxID := seedAgentSandbox(t, srv, orgID, "sandboxid")

	rec = do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/chats", "", alice, aliceCSRF)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create chat = %d: %s", rec.Code, rec.Body.String())
	}
	if got := extractString(t, rec.Body.String(), "sandbox_id"); got != "" {
		t.Fatalf("new chat sandbox_id = %q, want empty", got)
	}
	chat := extractString(t, rec.Body.String(), "id")

	rec = do(t, srv, http.MethodPatch, "/api/orgs/"+orgID+"/chats/"+chat, `{"sandbox_id":"`+sandboxID+`"}`, alice, aliceCSRF)
	if rec.Code != http.StatusOK {
		t.Fatalf("attach = %d: %s", rec.Code, rec.Body.String())
	}
	if got := extractString(t, rec.Body.String(), "sandbox_id"); got != sandboxID {
		t.Fatalf("attach response sandbox_id = %q, want %q", got, sandboxID)
	}

	rec = do(t, srv, http.MethodGet, "/api/orgs/"+orgID+"/chats", "", alice, aliceCSRF)
	if rec.Code != http.StatusOK {
		t.Fatalf("list chats = %d: %s", rec.Code, rec.Body.String())
	}
	var chats []map[string]any
	if err := json.Unmarshal([]byte(rec.Body.String()), &chats); err != nil {
		t.Fatalf("decode chats: %v", err)
	}
	found := false
	for _, c := range chats {
		if c["id"] == chat {
			found = true
			if c["sandbox_id"] != sandboxID {
				t.Fatalf("list sandbox_id = %v, want %v", c["sandbox_id"], sandboxID)
			}
		}
	}
	if !found {
		t.Fatalf("chat %s missing from list", chat)
	}
	_ = aliceID
}

// TestBFFSandboxAgentRuns — the sandbox-scoped list every surface's
// runs (chat A, chat B, /v1 without conversation); outsider and
// foreign-sandbox get 404.
func TestBFFSandboxAgentRuns(t *testing.T) {
	srv := setupBFF(t)
	cleanAgent(t)

	sfx := fmt.Sprint(time.Now().UnixNano())
	aliceID, alice, aliceCSRF := login(t, srv, "a34-sbx-"+sfx, "a34-sbx-"+sfx+"@example.test", "Alice")
	rec := do(t, srv, http.MethodPost, "/api/orgs", `{"name":"A34 SBX `+sfx+`"}`, alice, aliceCSRF)
	orgID := extractString(t, rec.Body.String(), "id")
	sandboxID := seedAgentSandbox(t, srv, orgID, "sbxruns")

	_, bob, bobCSRF := login(t, srv, "a34-bob-"+sfx, "a34-bob-"+sfx+"@example.test", "Bob")
	rec = do(t, srv, http.MethodPost, "/api/orgs", `{"name":"A34 Other `+sfx+`"}`, bob, bobCSRF)
	org2 := extractString(t, rec.Body.String(), "id")

	chatA := createChatRow(t, srv, orgID, alice, aliceCSRF)
	chatB := createChatRow(t, srv, orgID, alice, aliceCSRF)

	runA := insertQueuedRun(t, srv, orgID, aliceID, sandboxID, chatA, "run của chat A")
	finishRun(t, srv, orgID, runA) // terminal — frees the single-active slot
	runB := insertQueuedRun(t, srv, orgID, aliceID, sandboxID, chatB, "run của chat B")
	finishRun(t, srv, orgID, runB)
	runAPI := insertQueuedRun(t, srv, orgID, aliceID, sandboxID, "", "run từ /v1")
	// runAPI stays queued — the one active run the composer lock reacts to.

	rec = do(t, srv, http.MethodGet, "/api/orgs/"+orgID+"/sandboxes/"+sandboxID+"/agent/runs", "", alice, aliceCSRF)
	if rec.Code != http.StatusOK {
		t.Fatalf("sandbox runs = %d: %s", rec.Code, rec.Body.String())
	}
	runs := decodeRuns(t, rec.Body.String())
	if len(runs) != 3 {
		t.Fatalf("got %d runs, want 3: %s", len(runs), rec.Body.String())
	}
	// Newest first (ListRunsBySandbox orders created_at DESC): the
	// /v1 run came last, so it leads.
	if runs[0]["id"] != runAPI {
		t.Fatalf("first run = %v, want %v", runs[0]["id"], runAPI)
	}
	saw := map[string]map[string]any{}
	for _, r := range runs {
		saw[r["id"].(string)] = r
	}
	if conv := saw[runA]["conversation_id"]; conv != chatA {
		t.Fatalf("run A conversation = %v, want %v", conv, chatA)
	}
	if conv := saw[runB]["conversation_id"]; conv != chatB {
		t.Fatalf("run B conversation = %v, want %v", conv, chatB)
	}
	if conv := saw[runAPI]["conversation_id"]; conv != "" && conv != nil {
		t.Fatalf("api run conversation = %v, want empty", conv)
	}
	if st := saw[runAPI]["status"]; st != "queued" {
		t.Fatalf("api run status = %v, want queued", st)
	}

	// The chat-scoped list still hides the other chat's runs.
	rec = do(t, srv, http.MethodGet, "/api/orgs/"+orgID+"/chats/"+chatA+"/agent/runs", "", alice, aliceCSRF)
	runs = decodeRuns(t, rec.Body.String())
	if len(runs) != 1 || runs[0]["id"] != runA {
		t.Fatalf("chat A runs = %s, want only %s", rec.Body.String(), runA)
	}

	// Outsider: 404 (org membership first, no existence leak).
	rec = do(t, srv, http.MethodGet, "/api/orgs/"+orgID+"/sandboxes/"+sandboxID+"/agent/runs", "", bob, bobCSRF)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("outsider = %d, want 404", rec.Code)
	}
	// The same sandbox id under bob's own org id: not visible → 404.
	rec = do(t, srv, http.MethodGet, "/api/orgs/"+org2+"/sandboxes/"+sandboxID+"/agent/runs", "", bob, bobCSRF)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign org = %d, want 404", rec.Code)
	}
	// Unknown sandbox id → 404.
	rec = do(t, srv, http.MethodGet, "/api/orgs/"+orgID+"/sandboxes/SBXNOPE/agent/runs", "", alice, aliceCSRF)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown sandbox = %d, want 404", rec.Code)
	}
}
