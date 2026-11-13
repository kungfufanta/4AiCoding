// agent_a34_test.go — A3.4 BFF surface (docs/AGENT_PLAN_A3.md §10):
// the chat DTO carries the pinned sandbox (reload path), chat
// messages carry the agent_run_id of worker notes (the Chat page
// hides them where it renders the run card), the run carries
// created_by (Revert visibility), and the sandbox runs endpoint
// answers the org's own runs and 404s a foreign org — no existence
// leak.
package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tastyeffectco/sandboxd/portal/internal/agent"
)

func TestAgentA34ChatDTOAndSandboxRuns(t *testing.T) {
	srv, _, notify, sbx := setupAgent(t)
	startAgentWorker(t, srv, notify, agent.EnvSource{Fallback: srv.LLM}, sbx)

	sfx := fmt.Sprint(time.Now().UnixNano())
	aliceID, alice, aliceCSRF := login(t, srv, "agent34-alice-"+sfx, "agent34-alice-"+sfx+"@example.test", "Alice")
	rec := do(t, srv, http.MethodPost, "/api/orgs", `{"name":"A34 Org `+sfx+`"}`, alice, aliceCSRF)
	orgID := extractString(t, rec.Body.String(), "id")
	sandboxID := seedAgentSandbox(t, srv, orgID, "a34")

	// A new chat has no sandbox: the field stays off the wire.
	rec = do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/chats", "", alice, aliceCSRF)
	if rec.Code != http.StatusCreated || strings.Contains(rec.Body.String(), "sandbox_id") {
		t.Fatalf("create chat = %d: %s", rec.Code, rec.Body.String())
	}
	chat := extractString(t, rec.Body.String(), "id")

	// Attach → the chat list carries sandbox_id back (the reload
	// path needs it before the chat has any runs).
	rec = do(t, srv, http.MethodPatch, "/api/orgs/"+orgID+"/chats/"+chat, `{"sandbox_id":"`+sandboxID+`"}`, alice, aliceCSRF)
	if rec.Code != http.StatusOK {
		t.Fatalf("attach = %d: %s", rec.Code, rec.Body.String())
	}
	rec = do(t, srv, http.MethodGet, "/api/orgs/"+orgID+"/chats", "", alice, aliceCSRF)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"sandbox_id":"`+sandboxID+`"`) {
		t.Fatalf("chat list = %d: %s", rec.Code, rec.Body.String())
	}

	// A full run: the row carries created_by = the session user.
	rec = do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/chats/"+chat+"/agent/runs", `{"prompt":"tạo trang login"}`, alice, aliceCSRF)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create run = %d: %s", rec.Code, rec.Body.String())
	}
	runID := extractString(t, rec.Body.String(), "id")
	final := waitForRunStatus(t, srv, orgID, runID, []*http.Cookie{alice, aliceCSRF}, 15*time.Second)
	if !strings.Contains(final, `"status":"succeeded"`) || !strings.Contains(final, `"created_by":"`+aliceID+`"`) {
		t.Fatalf("final = %s", final)
	}

	// The worker's summary note lands in chat_messages with the run
	// id — exactly one, so the page can hide it where the run card
	// already renders the summary.
	rec = do(t, srv, http.MethodGet, "/api/orgs/"+orgID+"/chats/"+chat+"/messages", "", alice, aliceCSRF)
	if rec.Code != http.StatusOK {
		t.Fatalf("messages = %d: %s", rec.Code, rec.Body.String())
	}
	var msgs []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &msgs); err != nil {
		t.Fatalf("messages json: %v (%s)", err, rec.Body.String())
	}
	notes := 0
	for _, m := range msgs {
		if m["agent_run_id"] == runID {
			notes++
		}
	}
	if notes != 1 {
		t.Fatalf("notes with agent_run_id = %d, want 1 (messages: %v)", notes, msgs)
	}

	// Sandbox runs endpoint: the org sees its own run, newest first.
	rec = do(t, srv, http.MethodGet, "/api/orgs/"+orgID+"/sandboxes/"+sandboxID+"/agent/runs", "", alice, aliceCSRF)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), runID) {
		t.Fatalf("sandbox runs = %d: %s", rec.Code, rec.Body.String())
	}

	// A foreign org gets 404 for the same sandbox (no leak); so does
	// an unknown sandbox id inside the org.
	_, bob, bobCSRF := login(t, srv, "agent34-bob-"+sfx, "agent34-bob-"+sfx+"@example.test", "Bob")
	rec = do(t, srv, http.MethodPost, "/api/orgs", `{"name":"A34 Other `+sfx+`"}`, bob, bobCSRF)
	otherOrg := extractString(t, rec.Body.String(), "id")
	if rec := do(t, srv, http.MethodGet, "/api/orgs/"+otherOrg+"/sandboxes/"+sandboxID+"/agent/runs", "", bob, bobCSRF); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-org sandbox runs = %d, want 404", rec.Code)
	}
	if rec := do(t, srv, http.MethodGet, "/api/orgs/"+orgID+"/sandboxes/SBXNOPE/agent/runs", "", alice, aliceCSRF); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown sandbox = %d, want 404", rec.Code)
	}
}
