package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tastyeffectco/sandboxd/portal/internal/agent/sandboxtest"
	"github.com/tastyeffectco/sandboxd/portal/internal/llm"
	"github.com/tastyeffectco/sandboxd/portal/internal/llm/llmtest"
)

// The z.ai hub drops the tools field and cuts every stream at ~30 s.
// A run there used to end after step 1: the model pasted the page in a
// ``` fence (no tool call → "final summary"), or its long reply was cut
// (llm_error). Now the loop reminds it once to use the tools, absorbs
// the cut with a "smaller steps" nudge, and the run succeeds.
func TestRunSurvivesFenceAnswerAndStreamCut(t *testing.T) {
	handle := setupDB(t)
	st := NewStore(handle.Pool)
	orgID, userID, sandboxID := seedOrg(t, st, "hubcut")

	toolBlock := func(name, args string) string {
		return "```tool\n{\"name\":\"" + name + "\",\"arguments\":" + args + "}\n```"
	}
	fake := llmtest.NewServer(
		// 1. native mode, the model writes the page into the chat.
		llmtest.Scenario{Chunks: []any{
			llmtest.TextChunk("Here is the page:\n```html\n<h1>Cà phê</h1>\n```"),
			llmtest.FinishChunk("stop"),
		}},
		// 2. after the reminder: a long reply the provider cuts.
		llmtest.Scenario{Chunks: []any{llmtest.TextChunk("```tool\n{\"name\":\"write_file\",\"argu")}, Disconnect: true},
		// 3. small steps: first part, then append.
		llmtest.Scenario{Chunks: []any{llmtest.TextChunk(toolBlock("write_file", `{"path":"index.html","content":"<h1>Cà phê</h1>\n"}`))}},
		llmtest.Scenario{Chunks: []any{llmtest.TextChunk(toolBlock("write_file", `{"path":"index.html","content":"<p>ngon</p>\n","append":true}`))}},
		llmtest.Scenario{Chunks: []any{llmtest.TextChunk(toolBlock("finish", `{"summary":"index.html xong"}`))}},
	)
	defer fake.Close()
	client := &llm.Client{BaseURL: fake.URL, APIKey: "k", Model: "glm-4-plus", Flavor: "generic"}

	w, sbx := newWorker(t, st, fixedSource{client}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	w.Start(ctx)
	defer func() { cancel(); w.Shutdown(0); w.Wait() }()

	run, _, err := st.CreateRun(ctx, CreateIn{
		OrgID: orgID, SandboxID: sandboxID, CreatedByUser: userID,
		Prompt: "landing page cà phê", Model: "glm-4-plus", MaxSteps: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	fin := waitForStatus(t, st, orgID, run.ID, 20*time.Second)
	if fin.Status != StatusSucceeded {
		t.Fatalf("status = %s (%s), want succeeded", fin.Status, fin.ErrorCode)
	}

	var found string
	_ = filepath.Walk(sbx.Root(sandboxtest.DefaultID), func(p string, info os.FileInfo, err error) error {
		if err == nil && info.Name() == "index.html" {
			found = p
		}
		return nil
	})
	if found == "" {
		t.Fatalf("index.html was not written; sandbox roots: %s", sbx.Root(sandboxtest.DefaultID))
	}
	b, _ := os.ReadFile(found)
	if got := string(b); got != "<h1>Cà phê</h1>\n<p>ngon</p>\n" {
		t.Fatalf("index.html = %q, want both parts", got)
	}

	// The second request must carry the reminder, the fourth the cut nudge.
	if len(fake.Requests) < 3 {
		t.Fatalf("requests = %d", len(fake.Requests))
	}
	if !strings.Contains(string(fake.Requests[1].RawBody), "You did not call any tool") {
		t.Errorf("request 2 lacks the no-tool reminder")
	}
	if !strings.Contains(string(fake.Requests[2].RawBody), "was cut off") {
		t.Errorf("request 3 lacks the stream-cut nudge")
	}
}
