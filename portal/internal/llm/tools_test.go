// A3.0 tests for StreamTools (AGENT_PLAN_A3 §5 + phase A3.0 "Xong khi"):
// - arguments assembled from ≥ 3 deltas
// - parallel tool calls keep index order
// - fallback parses the ```tool fence and rejects 2 blocks / unknown tool
// - retry keeps the same X-Request-Id (429 then OK)
// - X-WebHub-Memory: 0 present when a message has role "tool"
// - 400 with tools → ErrToolsUnsupported (no retry)
// - the legacy Stream path is untouched (llm_test.go stays green).
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tastyeffectco/sandboxd/portal/internal/llm/llmtest"
)

func webhubClient(url string) *Client {
	return &Client{
		BaseURL: url + "/v1", APIKey: "wh_test", Model: "glm-4.6",
		Flavor: "webhub", RetryBackoff: []time.Duration{0, 0, 0},
	}
}

func simpleTools() []Tool {
	return []Tool{{
		Name:        "get_weather",
		Description: "Get current weather",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"},"unit":{"type":"string"}},"required":["city"]}`),
	}}
}

func reqWith(msgs ...Msg) ToolRequest {
	return ToolRequest{
		Messages:  msgs,
		Tools:     simpleTools(),
		UserID:    "11111111-1111-1111-1111-111111111111",
		SessionID: "22222222-2222-2222-2222-222222222222",
		RequestID: "run1:1",
	}
}

// TestStreamToolsAssemblesArgumentsFromSplitDeltas — the OpenAI spec
// streams one call's arguments as string fragments; ≥3 fragments must
// concatenate into valid JSON.
func TestStreamToolsAssemblesArgumentsFromSplitDeltas(t *testing.T) {
	srv := llmtest.NewServer(llmtest.Scenario{Chunks: []any{
		llmtest.TextChunk("Checking "),
		llmtest.TextChunk("the weather.\n"),
		llmtest.ToolCallChunk(0, "call_a", "get_weather", ""),
		llmtest.ToolCallChunk(0, "", "", `{"city":`),
		llmtest.ToolCallChunk(0, "", "", `"Hanoi",`),
		llmtest.ToolCallChunk(0, "", "", `"unit":"c"}`),
		llmtest.FinishChunk("tool_calls"),
	}})
	defer srv.Close()
	c := webhubClient(srv.URL)

	var text string
	turn, err := c.StreamTools(context.Background(), reqWith(Msg{Role: "user", Content: "weather?"}),
		func(d string) error { text += d; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if text != "Checking the weather.\n" {
		t.Fatalf("text = %q", text)
	}
	if turn.FinishReason != "tool_calls" {
		t.Fatalf("finish = %q", turn.FinishReason)
	}
	if len(turn.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v", turn.ToolCalls)
	}
	tc := turn.ToolCalls[0]
	if tc.ID != "call_a" || tc.Name != "get_weather" {
		t.Fatalf("id/name = %q/%q", tc.ID, tc.Name)
	}
	var args map[string]any
	if err := json.Unmarshal(tc.Arguments, &args); err != nil {
		t.Fatalf("arguments not valid JSON: %v (%s)", err, tc.Arguments)
	}
	if args["city"] != "Hanoi" || args["unit"] != "c" {
		t.Fatalf("arguments = %s", tc.Arguments)
	}
	if turn.Usage.UsageEstimated {
		t.Log("usage estimated — acceptable for WebHub (no usage chunk)")
	}
}

// TestStreamToolsParallelCallsKeepIndexOrder — two calls streaming in
// interleaved fragments must come out sorted by index.
func TestStreamToolsParallelCallsKeepIndexOrder(t *testing.T) {
	srv := llmtest.NewServer(llmtest.Scenario{Chunks: []any{
		llmtest.ToolCallChunk(1, "call_b", "get_weather", `{"city":"Os`),
		llmtest.ToolCallChunk(0, "call_a", "get_weather", `{"city":"Ha`),
		llmtest.ToolCallChunk(1, "", "", `noi"}`),
		llmtest.ToolCallChunk(0, "", "", `ei"}`),
		llmtest.FinishChunk("tool_calls"),
	}})
	defer srv.Close()
	c := webhubClient(srv.URL)
	turn, err := c.StreamTools(context.Background(), reqWith(Msg{Role: "user", Content: "x"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(turn.ToolCalls) != 2 {
		t.Fatalf("tool calls = %+v", turn.ToolCalls)
	}
	if turn.ToolCalls[0].ID != "call_a" || string(turn.ToolCalls[0].Arguments) != `{"city":"Haei"}` {
		t.Fatalf("call 0 = %+v", turn.ToolCalls[0])
	}
	if turn.ToolCalls[1].ID != "call_b" || string(turn.ToolCalls[1].Arguments) != `{"city":"Osnoi"}` {
		t.Fatalf("call 1 = %+v", turn.ToolCalls[1])
	}
}

// TestStreamToolsUsageFromStreamChunk — a provider that honors
// include_usage reports exact numbers (not estimated).
func TestStreamToolsUsageFromStreamChunk(t *testing.T) {
	srv := llmtest.NewServer(llmtest.Scenario{Chunks: []any{
		llmtest.TextChunk("pong"),
		llmtest.FinishChunk("stop"),
		llmtest.UsageChunk(161, 28),
	}})
	defer srv.Close()
	c := webhubClient(srv.URL)
	turn, err := c.StreamTools(context.Background(), reqWith(Msg{Role: "user", Content: "hi"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if turn.Usage.UsageEstimated || turn.Usage.InputTokens != 161 || turn.Usage.OutputTokens != 28 {
		t.Fatalf("usage = %+v", turn.Usage)
	}
}

// TestStreamToolsUsageEstimatedWhenAbsent — WebHub never sends a usage
// chunk (spike A3.0); the estimate kicks in and is marked.
func TestStreamToolsUsageEstimatedWhenAbsent(t *testing.T) {
	srv := llmtest.NewServer(llmtest.Scenario{Chunks: []any{
		llmtest.TextChunk("pong"),
		llmtest.FinishChunk("stop"),
	}})
	defer srv.Close()
	c := webhubClient(srv.URL)
	turn, err := c.StreamTools(context.Background(), reqWith(Msg{Role: "user", Content: "hi"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !turn.Usage.UsageEstimated || turn.Usage.InputTokens <= 0 || turn.Usage.OutputTokens <= 0 {
		t.Fatalf("usage = %+v, want estimated positive", turn.Usage)
	}
}

// TestStreamToolsRetryKeepsRequestID — 429 twice then OK: three
// requests, all carrying the same X-Request-Id.
func TestStreamToolsRetryKeepsRequestID(t *testing.T) {
	srv := llmtest.NewServer(
		llmtest.Scenario{Status: 429},
		llmtest.Scenario{Status: 429},
		llmtest.Scenario{Chunks: []any{
			llmtest.TextChunk("ok"), llmtest.FinishChunk("stop"),
		}},
	)
	defer srv.Close()
	c := webhubClient(srv.URL)
	turn, err := c.StreamTools(context.Background(), reqWith(Msg{Role: "user", Content: "hi"}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if turn.Text != "ok" {
		t.Fatalf("text = %q", turn.Text)
	}
	if got := len(srv.Requests); got != 3 {
		t.Fatalf("requests = %d, want 3", got)
	}
	for i, r := range srv.Requests {
		if r.Header.Get("X-Request-Id") != "run1:1" {
			t.Fatalf("attempt %d X-Request-Id = %q", i, r.Header.Get("X-Request-Id"))
		}
	}
}

// TestStreamToolsNoRetryOn4xx — 401 must surface after one request.
func TestStreamToolsNoRetryOn4xx(t *testing.T) {
	srv := llmtest.NewServer(llmtest.Scenario{Status: 401})
	defer srv.Close()
	c := webhubClient(srv.URL)
	_, err := c.StreamTools(context.Background(), reqWith(Msg{Role: "user", Content: "hi"}), nil)
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("err = %v", err)
	}
	if got := len(srv.Requests); got != 1 {
		t.Fatalf("requests = %d, want 1 (4xx never retried)", got)
	}
}

// TestStreamTools400WithToolsIsUnsupported — the provider rejecting
// the tools field (400) must surface as ErrToolsUnsupported with no
// retry, so the run can switch to fallback mode.
func TestStreamTools400WithToolsIsUnsupported(t *testing.T) {
	srv := llmtest.NewServer(llmtest.Scenario{Status: 400})
	defer srv.Close()
	c := webhubClient(srv.URL)
	_, err := c.StreamTools(context.Background(), reqWith(Msg{Role: "user", Content: "hi"}), nil)
	if !errors.Is(err, ErrToolsUnsupported) {
		t.Fatalf("err = %v, want ErrToolsUnsupported", err)
	}
	if got := len(srv.Requests); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
}

// TestStreamToolsMemoryHeaderOnToolRole — any role:"tool" message in
// the conversation forces X-WebHub-Memory: 0 (tool output must not be
// persisted as hub memory).
func TestStreamToolsMemoryHeaderOnToolRole(t *testing.T) {
	withTool := llmtest.NewServer(llmtest.Scenario{Chunks: []any{
		llmtest.TextChunk("ok"), llmtest.FinishChunk("stop"),
	}})
	defer withTool.Close()
	c := webhubClient(withTool.URL)
	req := reqWith(
		Msg{Role: "user", Content: "weather?"},
		Msg{Role: "assistant", Content: "", ToolCalls: []ToolCall{{ID: "call_a", Name: "get_weather", Arguments: json.RawMessage(`{"city":"Hanoi"}`)}}},
		Msg{Role: "tool", ToolCallID: "call_a", Content: "sunny"},
	)
	if _, err := c.StreamTools(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	if h := withTool.Requests[0].Header.Get("X-WebHub-Memory"); h != "0" {
		t.Fatalf("X-WebHub-Memory = %q, want 0", h)
	}

	without := llmtest.NewServer(llmtest.Scenario{Chunks: []any{
		llmtest.TextChunk("ok"), llmtest.FinishChunk("stop"),
	}})
	defer without.Close()
	c2 := webhubClient(without.URL)
	if _, err := c2.StreamTools(context.Background(), reqWith(Msg{Role: "user", Content: "hi"}), nil); err != nil {
		t.Fatal(err)
	}
	if h := without.Requests[0].Header.Get("X-WebHub-Memory"); h != "" {
		t.Fatalf("X-WebHub-Memory = %q, want empty on the first call", h)
	}
}

// TestStreamToolsNativeBodyShape — the wire body carries tools,
// tool_choice auto and stream_options.include_usage; webhub flavor
// never puts user in the body.
func TestStreamToolsNativeBodyShape(t *testing.T) {
	srv := llmtest.NewServer(llmtest.Scenario{Chunks: []any{
		llmtest.TextChunk("ok"), llmtest.FinishChunk("stop"),
	}})
	defer srv.Close()
	c := webhubClient(srv.URL)
	if _, err := c.StreamTools(context.Background(), reqWith(Msg{Role: "user", Content: "hi"}), nil); err != nil {
		t.Fatal(err)
	}
	body := srv.Requests[0].Body
	if _, ok := body["tools"]; !ok {
		t.Fatal("body missing tools")
	}
	if body["tool_choice"] != "auto" {
		t.Fatalf("tool_choice = %v", body["tool_choice"])
	}
	so, ok := body["stream_options"].(map[string]any)
	if !ok || so["include_usage"] != true {
		t.Fatalf("stream_options = %v", body["stream_options"])
	}
	if _, ok := body["user"]; ok {
		t.Fatal("webhub flavor must keep user out of the body")
	}
}

// --- fallback (JSON-in-text) mode -------------------------------------

// TestFallbackParsesSingleToolBlock — prose + one fence → text + one
// call; the block itself is stripped from the visible text.
func TestFallbackParsesSingleToolBlock(t *testing.T) {
	srv := llmtest.NewServer(llmtest.Scenario{Chunks: []any{
		llmtest.TextChunk("Let me check.\n"),
		llmtest.TextChunk("```tool\n"),
		llmtest.TextChunk(`{"name":"get_weather","arguments":{"city":"Hanoi"}}`),
		llmtest.TextChunk("\n```\n"),
		llmtest.TextChunk("One moment."),
	}})
	defer srv.Close()
	c := webhubClient(srv.URL)
	req := reqWith(Msg{Role: "user", Content: "weather?"})
	req.Fallback = true
	turn, err := c.StreamTools(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if turn.Text != "Let me check.\nOne moment." {
		t.Fatalf("text = %q", turn.Text)
	}
	if len(turn.ToolCalls) != 1 || turn.ToolCalls[0].Name != "get_weather" {
		t.Fatalf("calls = %+v", turn.ToolCalls)
	}
	if string(turn.ToolCalls[0].Arguments) != `{"city":"Hanoi"}` {
		t.Fatalf("args = %s", turn.ToolCalls[0].Arguments)
	}
	if turn.FinishReason != "tool_calls" {
		t.Fatalf("finish = %q", turn.FinishReason)
	}
	// The wire body must NOT carry the tools field in fallback mode.
	if _, ok := srv.Requests[0].Body["tools"]; ok {
		t.Fatal("fallback body must not carry tools")
	}
	// And the protocol prompt rides in the system block.
	msgs, _ := json.Marshal(srv.Requests[0].Body["messages"])
	if !strings.Contains(string(msgs), "Tool calling protocol") || !strings.Contains(string(msgs), "get_weather") {
		t.Fatal("fallback protocol prompt missing from system messages")
	}
}

// TestFallbackPlainText — no fence is a plain reply.
func TestFallbackPlainText(t *testing.T) {
	turn, err := parseFallbackTurn("Just an answer.", simpleTools())
	if err != nil {
		t.Fatal(err)
	}
	if turn.Text != "Just an answer." || len(turn.ToolCalls) != 0 {
		t.Fatalf("turn = %+v", turn)
	}
}

// TestFallbackRejectsTwoBlocks — the protocol says exactly one.
func TestFallbackRejectsTwoBlocks(t *testing.T) {
	_, err := parseFallbackTurn("a ```tool\n{\"name\":\"get_weather\",\"arguments\":{}}\n``` b ```tool\n{\"name\":\"get_weather\",\"arguments\":{}}\n```", simpleTools())
	if !errors.Is(err, ErrBadToolBlock) {
		t.Fatalf("err = %v", err)
	}
}

// TestFallbackRejectsUnknownTool — the name must be in the registry.
func TestFallbackRejectsUnknownTool(t *testing.T) {
	_, err := parseFallbackTurn("```tool\n{\"name\":\"delete_everything\",\"arguments\":{}}\n```", simpleTools())
	if !errors.Is(err, ErrBadToolBlock) {
		t.Fatalf("err = %v", err)
	}
}

// TestFallbackRejectsInvalidJSON — broken JSON inside the fence.
func TestFallbackRejectsInvalidJSON(t *testing.T) {
	_, err := parseFallbackTurn("```tool\n{\"name\":\"get_weather\",\"arguments\":{\"city\":}}\n```", simpleTools())
	if !errors.Is(err, ErrBadToolBlock) {
		t.Fatalf("err = %v", err)
	}
}

// TestFallbackRejectsUnclosedFence — the model wrote half a fence.
func TestFallbackRejectsUnclosedFence(t *testing.T) {
	_, err := parseFallbackTurn("```tool\n{\"name\":\"get_weather\"}", simpleTools())
	if !errors.Is(err, ErrBadToolBlock) {
		t.Fatalf("err = %v", err)
	}
}

// TestFallbackArgumentsMustBeObject — arguments = "abc" is invalid.
func TestFallbackArgumentsMustBeObject(t *testing.T) {
	_, err := parseFallbackTurn("```tool\n{\"name\":\"get_weather\",\"arguments\":\"city\"}\n```", simpleTools())
	if !errors.Is(err, ErrBadToolBlock) {
		t.Fatalf("err = %v", err)
	}
}
