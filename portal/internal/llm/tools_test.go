// A3.0 tests for StreamTools (AGENT_PLAN_A3 §5 + phase A3.0 "Xong khi"):
//   - arguments assembled from ≥ 3 deltas
//   - parallel tool calls keep index order
//   - fallback parses the ```tool fence and rejects 2 blocks / unknown tool
//   - retry keeps the same X-Request-Id (429 then OK)
//   - X-WebHub-Memory: 0 present when a message has role "tool"
//   - wire shape matches the OpenAI spec (tools wrapped in
//     {"type":"function","function":{...}}, assistant tool_calls with
//     "arguments" as a JSON string, "content" always present) — locked
//     by golden files plus structural asserts
//   - fallback converts tool history to plain text (no role:"tool",
//     no tool_calls reaches a tool-less provider)
//   - 400 classifies by body: tool/function mention →
//     ErrToolsUnsupported, anything else → ErrUpstream (no fallback flip)
//   - stream_options only when the flavor wants it or it is configured
//   - the legacy Stream path is untouched (llm_test.go stays green).
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
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

// TestStreamTools400WithToolsIsUnsupported — a 400 whose body blames
// the tools field must surface as ErrToolsUnsupported with no retry,
// so the run can switch to fallback mode.
func TestStreamTools400WithToolsIsUnsupported(t *testing.T) {
	srv := llmtest.NewServer(llmtest.Scenario{
		Status: 400,
		Body:   `{"error":{"message":"'tools' is not supported by this model","type":"invalid_request_error"}}`,
	})
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

// TestStreamTools400WithoutToolMentionIsUpstream — a 400 about
// anything else (context length, unknown model) must NOT flip the run
// into fallback mode: it stays ErrUpstream and is not retried.
func TestStreamTools400WithoutToolMentionIsUpstream(t *testing.T) {
	cases := []struct{ name, body string }{
		{
			name: "context too long",
			body: `{"error":{"message":"This model's maximum context length is 8192 tokens, however you requested 12000"}}`,
		},
		{
			name: "unknown model",
			body: `{"error":{"message":"model 'gpt-9' not found"}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := llmtest.NewServer(llmtest.Scenario{Status: 400, Body: tc.body})
			defer srv.Close()
			c := webhubClient(srv.URL)
			_, err := c.StreamTools(context.Background(), reqWith(Msg{Role: "user", Content: "hi"}), nil)
			if !errors.Is(err, ErrUpstream) {
				t.Fatalf("err = %v, want ErrUpstream", err)
			}
			if errors.Is(err, ErrToolsUnsupported) {
				t.Fatalf("err = %v, must not be ErrToolsUnsupported", err)
			}
			if got := len(srv.Requests); got != 1 {
				t.Fatalf("requests = %d, want 1", got)
			}
		})
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

// TestStreamToolsNativeBodyShape — the wire body carries tools in the
// OpenAI wrapper shape (type:function + function object), tool_choice
// auto; webhub keeps user out of the body and omits stream_options by
// default (the hub never sends usage and some proxies 400 on the
// field).
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
	tools, ok := body["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %v", body["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["type"] != "function" {
		t.Fatalf("tool type = %v, want function", tool["type"])
	}
	fn, ok := tool["function"].(map[string]any)
	if !ok {
		t.Fatalf("tool function = %v", tool["function"])
	}
	if fn["name"] != "get_weather" || fn["description"] != "Get current weather" {
		t.Fatalf("function = %v", fn)
	}
	if _, ok := fn["parameters"].(map[string]any); !ok {
		t.Fatalf("parameters = %v", fn["parameters"])
	}
	if body["tool_choice"] != "auto" {
		t.Fatalf("tool_choice = %v", body["tool_choice"])
	}
	if _, ok := body["stream_options"]; ok {
		t.Fatal("webhub flavor must omit stream_options by default")
	}
	if _, ok := body["user"]; ok {
		t.Fatal("webhub flavor must keep user out of the body")
	}
}

// TestStreamOptionsGating — stream_options.include_usage rides by
// default only on non-webhub flavors; Client.StreamOptions overrides
// the flavor default in both directions.
func TestStreamOptionsGating(t *testing.T) {
	on := true
	off := false
	cases := []struct {
		name        string
		flavor      string
		streamOpts  *bool
		wantPresent bool
	}{
		{name: "webhub default off", flavor: "webhub", streamOpts: nil, wantPresent: false},
		{name: "generic default on", flavor: "generic", streamOpts: nil, wantPresent: true},
		{name: "webhub forced on", flavor: "webhub", streamOpts: &on, wantPresent: true},
		{name: "generic forced off", flavor: "generic", streamOpts: &off, wantPresent: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := llmtest.NewServer(llmtest.Scenario{Chunks: []any{
				llmtest.TextChunk("ok"), llmtest.FinishChunk("stop"),
			}})
			defer srv.Close()
			c := webhubClient(srv.URL)
			c.Flavor = tc.flavor
			c.StreamOptions = tc.streamOpts
			if _, err := c.StreamTools(context.Background(), reqWith(Msg{Role: "user", Content: "hi"}), nil); err != nil {
				t.Fatal(err)
			}
			_, got := srv.Requests[0].Body["stream_options"]
			if got != tc.wantPresent {
				t.Fatalf("stream_options present = %v, want %v", got, tc.wantPresent)
			}
		})
	}
}

// TestNativeAssistantToolCallsWireShape — assistant history messages
// marshal tool_calls in the OpenAI shape: {id, type:"function",
// function:{name, arguments:"<json string>"}} — NOT the internal
// {id, name, arguments:{...}} that vLLM/LiteLLM reject.
func TestNativeAssistantToolCallsWireShape(t *testing.T) {
	srv := llmtest.NewServer(llmtest.Scenario{Chunks: []any{
		llmtest.TextChunk("ok"), llmtest.FinishChunk("stop"),
	}})
	defer srv.Close()
	c := webhubClient(srv.URL)
	req := reqWith(
		Msg{Role: "user", Content: "weather?"},
		Msg{Role: "assistant", Content: "", ToolCalls: []ToolCall{
			{ID: "call_a", Name: "get_weather", Arguments: json.RawMessage(`{"city":"Hanoi"}`)},
		}},
		Msg{Role: "tool", ToolCallID: "call_a", Content: "sunny"},
	)
	if _, err := c.StreamTools(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	msgs := srv.Requests[0].Body["messages"].([]any)
	assistant := msgs[1].(map[string]any)
	calls, ok := assistant["tool_calls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("tool_calls = %v", assistant["tool_calls"])
	}
	call := calls[0].(map[string]any)
	if call["id"] != "call_a" {
		t.Fatalf("id = %v", call["id"])
	}
	if call["type"] != "function" {
		t.Fatalf("type = %v", call["type"])
	}
	fn, ok := call["function"].(map[string]any)
	if !ok {
		t.Fatalf("function = %v", call["function"])
	}
	if fn["name"] != "get_weather" {
		t.Fatalf("name = %v", fn["name"])
	}
	if fn["arguments"] != `{"city":"Hanoi"}` {
		t.Fatalf("arguments = %v, want the JSON object as a STRING", fn["arguments"])
	}
	if _, has := assistant["content"]; !has {
		t.Fatal("assistant content field missing")
	}
	tool := msgs[2].(map[string]any)
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_a" {
		t.Fatalf("tool msg = %v", tool)
	}
}

// TestWireKeepsEmptyContentField — a role:"tool" message with empty
// content still carries "content":"" on the wire (omitempty on the
// internal Msg dropped the field and some providers reject that).
func TestWireKeepsEmptyContentField(t *testing.T) {
	srv := llmtest.NewServer(llmtest.Scenario{Chunks: []any{
		llmtest.TextChunk("ok"), llmtest.FinishChunk("stop"),
	}})
	defer srv.Close()
	c := webhubClient(srv.URL)
	req := reqWith(
		Msg{Role: "user", Content: "hi"},
		Msg{Role: "assistant", Content: "", ToolCalls: []ToolCall{
			{ID: "call_a", Name: "get_weather", Arguments: json.RawMessage(`{"city":"Hanoi"}`)},
		}},
		Msg{Role: "tool", ToolCallID: "call_a", Content: ""},
	)
	if _, err := c.StreamTools(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	msgs := srv.Requests[0].Body["messages"].([]any)
	assistant := msgs[1].(map[string]any)
	if v, ok := assistant["content"]; !ok || v != "" {
		t.Fatalf("assistant content = %q (present=%v)", v, ok)
	}
	tool := msgs[2].(map[string]any)
	if v, ok := tool["content"]; !ok || v != "" {
		t.Fatalf("tool content = %q (present=%v), want the field present and empty", v, ok)
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

// --- fallback history conversion ---------------------------------------

// TestFallbackConvertsToolHistoryToPlainText — a run can START in
// fallback mode on a chat whose history already holds native tool
// turns (previous run ran native, or the connection changed). A
// provider without tool support must never see role:"tool",
// tool_calls or tool_call_id on the wire: the assistant call becomes
// a ```tool fence in plain text and the result becomes a user message
// "[tool_result id=… name=…]".
func TestFallbackConvertsToolHistoryToPlainText(t *testing.T) {
	srv := llmtest.NewServer(llmtest.Scenario{Chunks: []any{
		llmtest.TextChunk("ok"), llmtest.FinishChunk("stop"),
	}})
	defer srv.Close()
	c := webhubClient(srv.URL)
	req := reqWith(
		Msg{Role: "system", Content: "sys prompt"},
		Msg{Role: "user", Content: "weather?"},
		Msg{Role: "assistant", Content: "Checking.", ToolCalls: []ToolCall{
			{ID: "call_a", Name: "get_weather", Arguments: json.RawMessage(`{"city":"Hanoi"}`)},
		}},
		Msg{Role: "tool", ToolCallID: "call_a", Content: "sunny, 28C"},
	)
	req.Fallback = true
	if _, err := c.StreamTools(context.Background(), req, nil); err != nil {
		t.Fatal(err)
	}
	body := srv.Requests[0].Body
	if _, ok := body["tools"]; ok {
		t.Fatal("fallback body must not carry tools")
	}
	msgs := body["messages"].([]any)
	if len(msgs) != 5 {
		t.Fatalf("messages = %d, want 5 (system, protocol, user, converted assistant, converted tool)", len(msgs))
	}
	var roles []string
	for _, m := range msgs {
		roles = append(roles, m.(map[string]any)["role"].(string))
	}
	want := []string{"system", "system", "user", "assistant", "user"}
	if fmt.Sprint(roles) != fmt.Sprint(want) {
		t.Fatalf("roles = %v, want %v", roles, want)
	}
	raw, _ := json.Marshal(msgs)
	s := string(raw)
	// Assistant tool call became a protocol fence (arguments still
	// a JSON object, name preserved).
	if !strings.Contains(s, "```tool\\n{\\\"name\\\":\\\"get_weather\\\",\\\"arguments\\\":{\\\"city\\\":\\\"Hanoi\\\"}}\\n```") {
		t.Fatalf("assistant fence missing: %s", s)
	}
	// Original prose survives before the fence.
	if !strings.Contains(s, "Checking.\\n```tool") {
		t.Fatalf("assistant prose missing before fence: %s", s)
	}
	// Tool result became a user message with the marker line.
	if !strings.Contains(s, "[tool_result id=call_a name=get_weather]\\nsunny, 28C") {
		t.Fatalf("tool_result marker missing: %s", s)
	}
	// Nothing tool-native remains on the wire.
	for _, banned := range []string{"tool_calls", "tool_call_id", `\"role\":\"tool\"`} {
		if strings.Contains(s, banned) {
			t.Fatalf("fallback wire must not contain %q: %s", banned, s)
		}
	}
	if !strings.Contains(s, "Tool calling protocol") {
		t.Fatal("protocol prompt missing")
	}
}

// TestFallbackToolResultWithoutMatchingCall — a role:"tool" whose id
// has no matching assistant call in the history degrades to
// name=unknown instead of an empty marker.
func TestFallbackToolResultWithoutMatchingCall(t *testing.T) {
	wire := fallbackMessages([]Msg{
		{Role: "user", Content: "hi"},
		{Role: "tool", ToolCallID: "call_z", Content: "out"},
	})
	if len(wire) != 2 || wire[1].Role != "user" {
		t.Fatalf("wire = %+v", wire)
	}
	want := "[tool_result id=call_z name=unknown]\nout"
	if wire[1].Content != want {
		t.Fatalf("content = %q, want %q", wire[1].Content, want)
	}
}

// --- golden wire bodies -------------------------------------------------

// updateGolden lets `go test ./internal/llm -update` rewrite the
// golden files after an intentional protocol change.
var updateGolden = flag.Bool("update", false, "rewrite golden wire-body files")

// goldenRequest is the fixed history every golden body is built from:
// system, user, an assistant tool call, the tool result. It exercises
// every wire-shape trap at once (tool wrapper, tool_calls string
// arguments, empty assistant content, memory-relevant history). IDs
// come from reqWith (user 1111…, session 2222…, request run1:1).
func goldenRequest() ToolRequest {
	return reqWith(
		Msg{Role: "system", Content: "You are a coding agent."},
		Msg{Role: "user", Content: "Weather in Hanoi?"},
		Msg{Role: "assistant", Content: "", ToolCalls: []ToolCall{
			{ID: "call_a", Name: "get_weather", Arguments: json.RawMessage(`{"city":"Hanoi"}`)},
		}},
		Msg{Role: "tool", ToolCallID: "call_a", Content: "sunny, 28C"},
	)
}

func assertGoldenBody(t *testing.T, name string, raw []byte) {
	t.Helper()
	var indented bytes.Buffer
	if err := json.Indent(&indented, raw, "", "  "); err != nil {
		t.Fatalf("indent: %v", err)
	}
	got := indented.String() + "\n"
	path := filepath.Join("testdata", name+".golden.json")
	if *updateGolden {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("golden missing: %v — run `go test ./internal/llm -update`", err)
	}
	if got != string(want) {
		t.Fatalf("wire body drifted from golden %s:\n--- got ---\n%s\n--- want ---\n%s", path, got, want)
	}
}

// TestGoldenNativeBodyGeneric — generic flavor, native mode: tools in
// the OpenAI wrapper, tool_choice auto, stream_options on (auto),
// user in the body, assistant tool_calls with arguments as a JSON
// string, content on every message.
func TestGoldenNativeBodyGeneric(t *testing.T) {
	raw, err := toolRequestBody(&Client{Model: "glm-4.6", Flavor: "generic"}, goldenRequest())
	if err != nil {
		t.Fatal(err)
	}
	assertGoldenBody(t, "native_generic", raw)
}

// TestGoldenNativeBodyWebhub — webhub flavor: no user field, no
// stream_options (never returns usage — spike A3.0 — and some proxies
// 400 on the field).
func TestGoldenNativeBodyWebhub(t *testing.T) {
	raw, err := toolRequestBody(&Client{Model: "glm-4.6", Flavor: "webhub"}, goldenRequest())
	if err != nil {
		t.Fatal(err)
	}
	assertGoldenBody(t, "native_webhub", raw)
}

// TestGoldenFallbackBodyWebhub — fallback mode: no tools field, the
// fence protocol rides in a system message, the tool history becomes
// plain text (assistant fence + [tool_result …] user message).
func TestGoldenFallbackBodyWebhub(t *testing.T) {
	raw, err := toolRequestBody(&Client{Model: "glm-4.6", Flavor: "webhub"}, func() ToolRequest {
		req := goldenRequest()
		req.Fallback = true
		return req
	}())
	if err != nil {
		t.Fatal(err)
	}
	assertGoldenBody(t, "fallback_webhub", raw)
}
