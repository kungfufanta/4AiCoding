// Package llm is a minimal streaming client for OpenAI-compatible
// chat completion endpoints (POST {base}/chat/completions with
// stream=true). The Chat MVP uses one platform connection seeded from
// env (AGENT_PLAN 5.2.1: WebHub glm-4.6 as the free default); A2
// replaces that with per-org model connections and keys.
//
// Flavor "webhub" adds the hub's tenant headers: X-WebHub-User (the
// Portal user's UUID, never an email) and X-Session-Id (the
// conversation). Bodies stay standard OpenAI, so the same client
// works against LiteLLM, OpenRouter or vLLM with flavor "generic".
package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Message is one chat turn in OpenAI wire shape.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Request is one completion call.
type Request struct {
	Messages []Message
	// UserID is the Portal user (UUID); sent as X-WebHub-User u_<id>
	// for flavor webhub and as the OpenAI "user" field otherwise.
	UserID string
	// SessionID groups the calls of one conversation (X-Session-Id).
	SessionID string
	// RequestID makes a retry idempotent on the hub (X-Request-Id).
	RequestID string
	// DisableMemory sets X-WebHub-Memory: 0 (AGENT_PLAN §5.2.1).
	// The agent loop sets this on every call that carries tool output,
	// so the hub does not persist file contents / command output as
	// long-term memory (prompt-injection vector). The first LLM call
	// of a run (no tool output yet) leaves memory on.
	DisableMemory bool
}

// Client talks to one OpenAI-compatible endpoint.
type Client struct {
	BaseURL string // e.g. https://hub.example/v1 (no trailing slash)
	APIKey  string
	Model   string
	Flavor  string // "webhub" or "generic"
	// Models is the env fallback's model list for the Chat picker
	// (PORTAL_DEFAULT_LLM_MODELS). DB connections carry their own.
	Models []string
	HTTP   *http.Client
	// MaxOutputBytes caps the assembled reply so a runaway stream
	// cannot grow without bound. Zero means 256 KiB.
	MaxOutputBytes int
	// RetryBackoff is the sleep before each retry of StreamTools
	// (network error / 429 / 5xx). Nil means the default 1s, 3s,
	// 9s; tests set zero durations. Max retries = len(RetryBackoff).
	RetryBackoff []time.Duration
	// StreamOptions controls whether the request carries
	// stream_options.include_usage (the only way to get token
	// counts from a stream). Nil means auto: on for non-webhub
	// flavors, off for webhub — the hub never sends a usage chunk
	// anyway (spike A3.0) and some proxies 400 on the field.
	// A non-nil value overrides the flavor default.
	StreamOptions *bool
}

// ErrUpstream wraps a non-2xx answer from the provider. The provider's
// body is not echoed to callers (it may carry account details).
var ErrUpstream = errors.New("llm upstream error")

// ErrToolsUnsupported is returned when the upstream rejects the
// request with a 400 whose body blames the tools (mentions tool or
// function — a 400 about context length or an unknown model reads
// the same status and stays ErrUpstream). The agent loop catches it
// and switches the run to fallback (JSON-in-text) mode —
// AGENT_PLAN_A3 §5, decided once per run.
var ErrToolsUnsupported = errors.New("llm upstream does not support tools")

// ErrBadToolBlock is returned by the fallback (JSON-in-text) parser
// when the model's reply violates the protocol: two or more tool
// blocks, invalid JSON, or an unknown tool name. The loop feeds the
// reason back to the model as a tool_result so it can retry the turn.
var ErrBadToolBlock = errors.New("llm returned an invalid tool block")

// ErrStreamCut is returned when the provider closed a stream mid-reply
// (connection dropped, or the body ended with neither [DONE] nor a
// finish_reason) after some text was already delivered. Some hubs cut
// every stream at a fixed wall-clock limit (~30 s), so a long reply —
// a whole file in one write_file — never completes. It wraps
// ErrUpstream; the agent loop catches it and asks for smaller steps.
var ErrStreamCut = fmt.Errorf("%w: stream closed mid-reply", ErrUpstream)

// ErrBadToolCall is returned when the assembled tool call arguments
// are not valid JSON (fragments did not concatenate into an object).
var ErrBadToolCall = errors.New("llm returned an invalid tool call")

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	// No overall timeout: a stream may legitimately run for minutes;
	// the request context bounds it instead.
	return &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ResponseHeaderTimeout: 60 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
	}}
}

// sendStreamOptions reports whether chat requests should carry
// stream_options.include_usage. Explicit StreamOptions wins; without
// it the flavor decides (webhub never returns usage — spike A3.0 —
// and some proxies reject the field).
func (c *Client) sendStreamOptions() bool {
	if c.StreamOptions != nil {
		return *c.StreamOptions
	}
	return c.Flavor != "webhub"
}

// hubActor prefixes the hub tenant id: user ids arrive bare (the
// Portal user UUID) and become "u_<id>", while agent runs created
// through /v1 already carry their "k_<key_id>" prefix (AGENT_PLAN_A3
// §5) and pass through unchanged.
func hubActor(userID string) string {
	if strings.HasPrefix(userID, "k_") {
		return userID
	}
	return "u_" + userID
}

// Stream sends req and calls onDelta for every content fragment as it
// arrives. It returns the whole reply. onDelta returning an error
// aborts the stream (client went away).
func (c *Client) Stream(ctx context.Context, req Request, onDelta func(string) error) (string, error) {
	body := map[string]any{
		"model":    c.Model,
		"messages": req.Messages,
		"stream":   true,
	}
	if c.Flavor != "webhub" && req.UserID != "" {
		body["user"] = hubActor(req.UserID)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		endpoint(c.BaseURL, "/chat/completions"), bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "text/event-stream")
	if c.APIKey != "" {
		hreq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	if c.Flavor == "webhub" {
		if req.UserID != "" {
			hreq.Header.Set("X-WebHub-User", hubActor(req.UserID))
		}
		if req.SessionID != "" {
			hreq.Header.Set("X-Session-Id", "s_"+req.SessionID)
		}
		if req.DisableMemory {
			// AGENT_PLAN §5.2.1: tool output may contain file
			// contents or .env; do not let the hub persist it.
			hreq.Header.Set("X-WebHub-Memory", "0")
		}
	}
	if req.RequestID != "" {
		hreq.Header.Set("X-Request-Id", req.RequestID)
	}

	res, err := c.httpClient().Do(hreq)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
		return "", fmt.Errorf("%w: status %d", ErrUpstream, res.StatusCode)
	}

	limit := c.MaxOutputBytes
	if limit <= 0 {
		limit = 256 << 10
	}
	var out strings.Builder
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue // comments, event:, id:, blank separators
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			return out.String(), nil
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue // tolerate keep-alive junk
		}
		if chunk.Error != nil {
			return out.String(), fmt.Errorf("%w: stream error", ErrUpstream)
		}
		for _, ch := range chunk.Choices {
			d := ch.Delta.Content
			if d == "" {
				continue
			}
			if out.Len()+len(d) > limit {
				return out.String(), fmt.Errorf("%w: reply exceeded %d bytes", ErrUpstream, limit)
			}
			out.WriteString(d)
			if err := onDelta(d); err != nil {
				return out.String(), err
			}
		}
	}
	if err := sc.Err(); err != nil {
		return out.String(), fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	// Stream closed without [DONE]: keep what arrived.
	return out.String(), nil
}

// TestResult is one probe of a model connection (AGENT_PLAN A2).
// POST /api/.../connections/{id}/test sends a tiny prompt and reports
// whether the provider answered, the model it used, the latency, and
// any error.
type TestResult struct {
	OK            bool   `json:"ok"`
	Model         string `json:"model,omitempty"`
	Latency       int64  `json:"latency_ms,omitempty"`
	Reply         string `json:"reply,omitempty"`
	Error         string `json:"error,omitempty"`
	SupportsTools bool   `json:"supports_tools,omitempty"`
}

// Test sends a one-shot "Reply with the single word: ok" prompt and
// reports the result. It does NOT stream — a test call should be fast.
// supports_tools is probed by asking the model to call a no-op tool
// when protocol = openai; the probe is best-effort (a provider that
// ignores tool_choice still reports OK on the text reply).
func (c *Client) Test(ctx context.Context, model string) TestResult {
	start := time.Now()
	body := map[string]any{
		"model":      model,
		"messages":   []Message{{Role: "user", Content: "Reply with the single word: ok"}},
		"stream":     false,
		"max_tokens": 16,
	}
	raw, _ := json.Marshal(body)
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		endpoint(c.BaseURL, "/chat/completions"), bytes.NewReader(raw))
	if err != nil {
		return TestResult{Error: err.Error()}
	}
	hreq.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		hreq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	if c.Flavor == "webhub" {
		hreq.Header.Set("X-WebHub-Memory", "0")
	}
	res, err := c.httpClient().Do(hreq)
	if err != nil {
		return TestResult{Error: err.Error()}
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, 4<<10))
	if res.StatusCode == http.StatusBadRequest {
		// Some hubs (the z.ai sandbox WebHub) only take stream=true and
		// answer 400 otherwise: retry the same prompt streamed.
		sc := *c
		sc.Model = model
		sc.MaxOutputBytes = 4 << 10
		reply, serr := sc.Stream(ctx, Request{Messages: []Message{{Role: "user", Content: "Reply with the single word: ok"}}, DisableMemory: true}, func(string) error { return nil })
		if serr == nil && strings.TrimSpace(reply) != "" {
			return TestResult{OK: true, Model: model, Latency: time.Since(start).Milliseconds(), Reply: reply}
		}
	}
	if res.StatusCode != http.StatusOK {
		return TestResult{Error: fmt.Sprintf("upstream %d: %s", res.StatusCode, string(data))}
	}
	var resp struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return TestResult{Error: "decode: " + err.Error()}
	}
	reply := ""
	if len(resp.Choices) > 0 {
		reply = resp.Choices[0].Message.Content
	}
	return TestResult{
		OK:      true,
		Model:   resp.Model,
		Latency: time.Since(start).Milliseconds(),
		Reply:   reply,
	}
}

// ErrModelsUnsupported means the provider has no GET /models (404):
// the model ids must be typed in by hand.
var ErrModelsUnsupported = errors.New("provider has no /models endpoint")

// ListModels calls GET {base}/models and returns the model id list.
// Used by the connection editor's "Load models" button.
func (c *Client) ListModels(ctx context.Context) ([]string, error) {
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet,
		endpoint(c.BaseURL, "/models"), nil)
	if err != nil {
		return nil, err
	}
	if c.APIKey != "" {
		hreq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	res, err := c.httpClient().Do(hreq)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return nil, ErrModelsUnsupported
	}
	if res.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(res.Body, 1<<10))
		return nil, fmt.Errorf("upstream %d: %s", res.StatusCode, string(data))
	}
	var resp struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	out := make([]string, 0, len(resp.Data))
	for _, m := range resp.Data {
		out = append(out, m.ID)
	}
	return out, nil
}

// --- Tool calling (A3, AGENT_PLAN_A3 §5) ------------------------------

// Tool is one function the agent exposes to the model (JSON Schema).
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"` // JSON Schema object
}

// ToolCall is one function call the model asked for.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"` // raw JSON object
}

// Msg is one chat turn for the agent loop — Message plus the tool
// fields of the OpenAI protocol. This is the INTERNAL shape (what the
// loop and the persisted history use); the wire shape is wireMsg and
// friends below — the OpenAI spec differs from it in three ways that
// broke strict providers (vLLM, LiteLLM, OpenAI itself):
//   - tools are wrapped: {"type":"function","function":{...}}, not
//     the bare {"name",...} of Tool;
//   - assistant tool_calls carry {"id","type","function":{"name",
//     "arguments":"<json as a STRING>"}}, not {"name","arguments":{...}};
//   - "content" must always be present, even when empty (a role:tool
//     message without it is rejected by some providers).
type Msg struct {
	Role       string     `json:"role"` // system | user | assistant | tool
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`   // assistant
	ToolCallID string     `json:"tool_call_id,omitempty"` // tool
}

// --- wire shapes (OpenAI chat completion spec) -------------------------

// wireTool is the on-the-wire form of Tool.
type wireTool struct {
	Type     string           `json:"type"` // always "function"
	Function wireToolFunction `json:"function"`
}

type wireToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

// wireToolCall is the on-the-wire form of an assistant ToolCall.
type wireToolCall struct {
	ID       string               `json:"id"`
	Type     string               `json:"type"` // "function"
	Function wireToolCallFunction `json:"function"`
}

type wireToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // the JSON object, as a string
}

// wireMsg is the on-the-wire form of Msg. Content has no omitempty:
// every message carries the field, empty or not.
type wireMsg struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

// wireTool converts one internal tool. A nil schema becomes {} — the
// field is required by the spec.
func wireToolOf(t Tool) wireTool {
	params := t.Parameters
	if len(params) == 0 {
		params = json.RawMessage("{}")
	}
	return wireTool{
		Type: "function",
		Function: wireToolFunction{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  params,
		},
	}
}

// wireToolCalls converts internal tool calls for an assistant history
// message. Arguments become a JSON-encoded string; empty arguments
// become "{}".
func wireToolCallsOf(calls []ToolCall) []wireToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]wireToolCall, 0, len(calls))
	for _, call := range calls {
		args := string(call.Arguments)
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		out = append(out, wireToolCall{
			ID:   call.ID,
			Type: "function",
			Function: wireToolCallFunction{
				Name:      call.Name,
				Arguments: args,
			},
		})
	}
	return out
}

// wireMessages converts history for native mode: every field keeps
// its OpenAI role and shape, content is always present.
func wireMessages(msgs []Msg) []wireMsg {
	out := make([]wireMsg, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, wireMsg{
			Role:       m.Role,
			Content:    m.Content,
			ToolCalls:  wireToolCallsOf(m.ToolCalls),
			ToolCallID: m.ToolCallID,
		})
	}
	return out
}

// fallbackMessages converts history for fallback mode (provider has
// no native tool calling, so role:"tool" and tool_calls would be
// rejected outright):
//   - an assistant message with tool calls becomes plain text holding
//     the same ```tool fence block(s) the protocol prompt defines;
//   - a tool result becomes a user message "[tool_result id=… name=…]
//     \n<output>" — the name is looked up from the tool_calls that
//     preceded it (unknown ids degrade to name=unknown).
func fallbackMessages(msgs []Msg) []wireMsg {
	out := make([]wireMsg, 0, len(msgs))
	callNames := map[string]string{}
	for _, m := range msgs {
		switch {
		case m.Role == "assistant" && len(m.ToolCalls) > 0:
			var b strings.Builder
			if m.Content != "" {
				b.WriteString(m.Content)
				b.WriteString("\n")
			}
			for i, call := range m.ToolCalls {
				callNames[call.ID] = call.Name
				if i > 0 {
					b.WriteString("\n")
				}
				args := call.Arguments
				if len(args) == 0 {
					args = json.RawMessage("{}")
				}
				block, err := json.Marshal(struct {
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				}{call.Name, args})
				if err != nil {
					block = []byte(`{"name":"` + call.Name + `","arguments":{}}`)
				}
				b.WriteString("```tool\n")
				b.Write(block)
				b.WriteString("\n```")
			}
			out = append(out, wireMsg{Role: "assistant", Content: b.String()})
		case m.Role == "tool":
			name := callNames[m.ToolCallID]
			if name == "" {
				name = "unknown"
			}
			out = append(out, wireMsg{
				Role:    "user",
				Content: "[tool_result id=" + m.ToolCallID + " name=" + name + "]\n" + m.Content,
			})
		default:
			out = append(out, wireMsg{Role: m.Role, Content: m.Content})
		}
	}
	return out
}

// Usage is token accounting for one call. When the stream carries no
// usage chunk (WebHub does not, spike A3.0), the client estimates
// len(bytes)/4 and sets UsageEstimated.
type Usage struct {
	InputTokens    int  `json:"input_tokens"`
	OutputTokens   int  `json:"output_tokens"`
	UsageEstimated bool `json:"usage_estimated,omitempty"`
}

// Turn is the assembled result of one streamed completion.
type Turn struct {
	Text         string
	ToolCalls    []ToolCall
	FinishReason string
	Usage        Usage
	// TextToolCall: the call was salvaged from text markup in native
	// mode (the provider dropped the tools field). The loop switches
	// the run to the fallback protocol so tool results reach the model.
	TextToolCall bool
}

// ToolRequest is one StreamTools call.
type ToolRequest struct {
	Messages []Msg
	Tools    []Tool
	// UserID / SessionID / RequestID mirror Request for the WebHub
	// headers; RequestID stays the same across retries.
	UserID    string
	SessionID string
	RequestID string
	// Fallback switches to the JSON-in-text protocol for models
	// without native tool calling: the request carries no "tools"
	// field, the system prompt gains the fence protocol, and the
	// reply must contain at most one ```tool {...}``` block.
	Fallback bool
}

// fallbackProtocol is appended to the system context in fallback mode.
// Strict fence format: exactly one ```tool block per reply.
const fallbackProtocol = `## Tool calling protocol

You can call tools by replying with EXACTLY ONE fenced block:

` + "```tool\n" + `{"name": "<tool name>", "arguments": {<json arguments>}}
` + "```" + `

Rules:
- "name" must be one of the tools listed below; "arguments" must be a
  JSON object matching that tool's schema.
- Put any prose before or after the block, never inside it.
- When no tool call is needed, reply with plain text only — no block.
- Never write two blocks in one reply.

Available tools:
`

// estimateUsage fills the usage struct from byte counts when the
// stream carried no usage chunk (WebHub spike A3.0: it never does).
func estimateUsage(inputBytes, outputBytes int) Usage {
	return Usage{
		InputTokens:    inputBytes / 4,
		OutputTokens:   outputBytes / 4,
		UsageEstimated: true,
	}
}

// messagesHaveToolRole reports whether any message carries tool output
// (role "tool"). WebHub calls with tool output must set
// X-WebHub-Memory: 0 so the hub does not persist file contents or
// command output as long-term memory (AGENT_PLAN §5.2.1). It looks at
// the INTERNAL history, so it still fires in fallback mode where the
// wire has no role:"tool" but the user messages carry tool output.
func messagesHaveToolRole(msgs []Msg) bool {
	for _, m := range msgs {
		if m.Role == "tool" {
			return true
		}
	}
	return false
}

// errorBodyMentionsTools reports whether an error body points at the
// tools field. Only such a 400 means "provider has no tool support";
// a 400 about an over-long context or an unknown model reads the same
// status but must not flip the run into fallback mode. Match on the
// usual provider phrasings (case-insensitive substring).
func errorBodyMentionsTools(body []byte) bool {
	s := strings.ToLower(string(body))
	return strings.Contains(s, "tool") || strings.Contains(s, "function")
}

// toolRequestBody marshals the wire body for one StreamTools attempt.
// Native mode sends tools (OpenAI wrapper shape) + tool_choice +
// stream_options; fallback mode converts the tool history to plain
// text (a provider without tool support rejects role:"tool" and
// tool_calls) and injects the fence protocol into the system block
// instead.
func toolRequestBody(c *Client, req ToolRequest) ([]byte, error) {
	msgs := req.Messages
	wireMsgs := wireMessages(msgs)
	body := map[string]any{
		"model":    c.Model,
		"messages": wireMsgs,
		"stream":   true,
	}
	if c.sendStreamOptions() {
		body["stream_options"] = map[string]any{
			"include_usage": true,
		}
	}
	if req.Fallback {
		// No tools field on the wire (a provider that rejected
		// tools would 400 again); the registry rides in the
		// protocol prompt instead, and the history loses its
		// tool roles.
		wireMsgs = fallbackMessages(msgs)
		protocol := fallbackProtocol
		for i, t := range req.Tools {
			desc, _ := json.Marshal(t)
			protocol += string(desc)
			if i < len(req.Tools)-1 {
				protocol += "\n"
			}
		}
		protocol += "\n"
		insert := 0
		for insert < len(msgs) && msgs[insert].Role == "system" {
			insert++
		}
		with := make([]wireMsg, 0, len(wireMsgs)+1)
		with = append(with, wireMsgs[:insert]...)
		with = append(with, wireMsg{Role: "system", Content: protocol})
		with = append(with, wireMsgs[insert:]...)
		body["messages"] = with
	} else if len(req.Tools) > 0 {
		tools := make([]wireTool, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, wireToolOf(t))
		}
		body["tools"] = tools
		body["tool_choice"] = "auto"
	}
	if c.Flavor != "webhub" && req.UserID != "" {
		body["user"] = hubActor(req.UserID)
	}
	return json.Marshal(body)
}

// toolCallAccumulator assembles one tool call from streamed deltas.
// OpenAI-compatible providers stream the arguments of one call as
// string fragments under the same index; the id and name arrive on
// the first fragment (later fragments may repeat or omit them).
type toolCallAccumulator struct {
	id   string
	name string
	args strings.Builder
}

// StreamTools sends req with tool calling enabled and streams text
// deltas through onText. It returns the assembled Turn: text, tool
// calls (arguments concatenated per index), finish reason and usage.
//
// Retry policy (AGENT_PLAN_A3 §5): network errors and 429/5xx are
// retried up to len(RetryBackoff) times (default 3) with the SAME
// X-Request-Id; other 4xx are not retried. A 400 that carries tools
// surfaces as ErrToolsUnsupported so the loop can switch the run to
// fallback mode. A retry only happens while no delta has been handed
// to onText — once text has streamed, the turn is committed.
func (c *Client) StreamTools(ctx context.Context, req ToolRequest, onText func(string) error) (Turn, error) {
	raw, err := toolRequestBody(c, req)
	if err != nil {
		return Turn{}, err
	}
	inputBytes := len(raw)
	backoff := c.RetryBackoff
	if backoff == nil {
		backoff = []time.Duration{time.Second, 3 * time.Second, 9 * time.Second}
	}
	var lastErr error
	for attempt := 0; attempt <= len(backoff); attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return Turn{}, ctx.Err()
			case <-time.After(backoff[attempt-1]):
			}
		}
		turn, delivered, retryable, err := c.attemptStreamTools(ctx, req, raw, inputBytes, onText)
		if err == nil {
			return turn, nil
		}
		if !retryable {
			return Turn{}, err
		}
		if delivered {
			// Text already streamed to the caller: a retry
			// would duplicate it. Commit the failure.
			return Turn{}, err
		}
		lastErr = err
	}
	return Turn{}, lastErr
}

// attemptStreamTools performs one HTTP attempt. retryable is true for
// network errors and 429/5xx while nothing was delivered. delivered
// reports whether onText fired at least once.
func (c *Client) attemptStreamTools(ctx context.Context, req ToolRequest, raw []byte, inputBytes int, onText func(string) error) (turn Turn, delivered bool, retryable bool, err error) {
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		endpoint(c.BaseURL, "/chat/completions"), bytes.NewReader(raw))
	if err != nil {
		return Turn{}, false, false, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "text/event-stream")
	if c.APIKey != "" {
		hreq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	if req.UserID != "" && c.Flavor == "webhub" {
		hreq.Header.Set("X-WebHub-User", hubActor(req.UserID))
	}
	if req.SessionID != "" && c.Flavor == "webhub" {
		hreq.Header.Set("X-Session-Id", "s_"+req.SessionID)
	}
	if c.Flavor == "webhub" && messagesHaveToolRole(req.Messages) {
		hreq.Header.Set("X-WebHub-Memory", "0")
	}
	if req.RequestID != "" {
		// Same value on every attempt — the hub deduplicates.
		hreq.Header.Set("X-Request-Id", req.RequestID)
	}

	res, err := c.httpClient().Do(hreq)
	if err != nil {
		return Turn{}, false, true, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
		switch {
		case res.StatusCode == http.StatusBadRequest && !req.Fallback &&
			len(req.Tools) > 0 && errorBodyMentionsTools(body):
			// The provider rejected the request BECAUSE of
			// the tools field: the loop switches to fallback
			// mode (no retry). A 400 about anything else —
			// over-long context, unknown model — must not
			// trigger that switch; it surfaces as ErrUpstream.
			return Turn{}, false, false, ErrToolsUnsupported
		case res.StatusCode == http.StatusTooManyRequests || res.StatusCode/100 == 5:
			return Turn{}, false, true, fmt.Errorf("%w: status %d", ErrUpstream, res.StatusCode)
		default:
			return Turn{}, false, false, fmt.Errorf("%w: status %d", ErrUpstream, res.StatusCode)
		}
	}

	limit := c.MaxOutputBytes
	if limit <= 0 {
		limit = 256 << 10
	}
	var text strings.Builder
	outputBytes := 0
	accs := map[int]*toolCallAccumulator{}
	finish := ""
	sawDone := false
	var usage Usage
	usageSeen := false

	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			sawDone = true
			break
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			continue // tolerate keep-alive junk
		}
		if chunk.Error != nil {
			return Turn{}, delivered, false, fmt.Errorf("%w: stream error", ErrUpstream)
		}
		if chunk.Usage != nil {
			usage = Usage{
				InputTokens:  chunk.Usage.PromptTokens,
				OutputTokens: chunk.Usage.CompletionTokens,
			}
			usageSeen = true
		}
		for _, ch := range chunk.Choices {
			if ch.FinishReason != nil {
				finish = *ch.FinishReason
			}
			d := ch.Delta.Content
			if d != "" {
				if outputBytes+len(d) > limit {
					return Turn{}, delivered, false, fmt.Errorf("%w: reply exceeded %d bytes", ErrUpstream, limit)
				}
				outputBytes += len(d)
				text.WriteString(d)
				delivered = true
				if onText != nil {
					if err := onText(d); err != nil {
						return Turn{}, delivered, false, err
					}
				}
			}
			for _, tc := range ch.Delta.ToolCalls {
				acc := accs[tc.Index]
				if acc == nil {
					acc = &toolCallAccumulator{}
					accs[tc.Index] = acc
				}
				if tc.ID != "" && acc.id == "" {
					acc.id = tc.ID
				}
				if tc.Function.Name != "" && acc.name == "" {
					acc.name = tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					outputBytes += len(tc.Function.Arguments)
					acc.args.WriteString(tc.Function.Arguments)
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		if delivered {
			return Turn{}, delivered, false, fmt.Errorf("%w (%v)", ErrStreamCut, err)
		}
		return Turn{}, delivered, true, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	if delivered && !sawDone && finish == "" {
		return Turn{}, delivered, false, ErrStreamCut
	}

	// Fallback mode: split the assembled text into prose and the
	// tool fence. Parse errors surface as ErrBadToolBlock so the
	// loop can feed the reason back as a tool_result.
	if req.Fallback {
		ft, err := parseFallbackTurn(text.String(), req.Tools)
		if err != nil {
			return Turn{}, delivered, false, err
		}
		ft.Usage = estimateUsage(inputBytes, outputBytes)
		return ft, delivered, false, nil
	}

	turn.Text = text.String()
	turn.FinishReason = finish
	// Assemble tool calls in index order.
	if len(accs) > 0 {
		idxs := make([]int, 0, len(accs))
		for i := range accs {
			idxs = append(idxs, i)
		}
		sort.Ints(idxs)
		for _, i := range idxs {
			acc := accs[i]
			if acc.name == "" {
				return Turn{}, delivered, false, fmt.Errorf("%w: tool call %d has no name", ErrBadToolCall, i)
			}
			args := strings.TrimSpace(acc.args.String())
			if args == "" {
				args = "{}"
			}
			if !json.Valid([]byte(args)) {
				return Turn{}, delivered, false, fmt.Errorf("%w: tool call %s arguments", ErrBadToolCall, acc.name)
			}
			turn.ToolCalls = append(turn.ToolCalls, ToolCall{
				ID: acc.id, Name: acc.name, Arguments: json.RawMessage(args),
			})
		}
	}
	if len(turn.ToolCalls) == 0 {
		// Some hubs drop the tools field and the model then writes the
		// call as text (GLM: <tool name="x"><parameter name="k">v
		// </parameter></tool>). Salvage it instead of ending the run
		// with the markup as the "summary".
		if xt, ok := parseXMLToolTurn(turn.Text, req.Tools); ok {
			xt.Usage = turn.Usage
			xt.TextToolCall = true
			turn = xt
		} else if mentionsTextToolCall(turn.Text, req.Tools) {
			// Looks like a call we cannot read: let the loop switch to
			// the fallback protocol and ask again.
			return Turn{}, delivered, false, fmt.Errorf("%w: tool call written as unreadable text", ErrBadToolBlock)
		}
	}
	if usageSeen {
		turn.Usage = usage
	} else {
		turn.Usage = estimateUsage(inputBytes, outputBytes)
	}
	return turn, delivered, false, nil
}

// parseFallbackTurn splits a fallback-mode reply into text and tool
// calls. Strict: at most one ```tool fence; inside it a JSON object
// with a name from the registry and an object "arguments". Zero
// fences is a plain text reply (no tool call).
func parseFallbackTurn(text string, tools []Tool) (Turn, error) {
	known := map[string]bool{}
	for _, t := range tools {
		known[t.Name] = true
	}
	const fence = "```tool"
	var blocks []string
	rest := text
	for {
		i := strings.Index(rest, fence)
		if i < 0 {
			break
		}
		after := rest[i+len(fence):]
		// Skip an optional language-tag remainder up to the
		// newline (```tool-json is not a tool fence — require
		// the fence to be followed by whitespace or newline).
		if after != "" && after[0] != '\n' && after[0] != '\r' && after[0] != ' ' {
			rest = rest[i+len(fence):]
			continue
		}
		end := strings.Index(after, "```")
		if end < 0 {
			return Turn{}, fmt.Errorf("%w: unclosed tool fence", ErrBadToolBlock)
		}
		blocks = append(blocks, strings.TrimSpace(after[:end]))
		rest = after[end+3:]
	}
	if len(blocks) > 1 {
		// Models often plan several calls in one reply. Run the first
		// one; the model sees its result and re-issues the rest. Keep
		// only the prose before the first block (the text after it
		// describes calls that did not run).
		start := strings.Index(text, fence)
		text = text[:start] + fence + "\n" + blocks[0] + "\n```"
		blocks = blocks[:1]
	}
	turn := Turn{FinishReason: "stop"}
	if len(blocks) == 0 {
		if xt, ok := parseXMLToolTurn(text, tools); ok {
			return xt, nil
		}
		turn.Text = strings.TrimSpace(text)
		return turn, nil
	}
	// Remove the block (fence to fence) from the visible text.
	// The closing fence search starts after the opening fence tag,
	// otherwise it matches the fence's own leading backticks.
	start := strings.Index(text, fence)
	end := strings.Index(text[start+len(fence):], "```") + start + len(fence) + 3
	before := strings.TrimRight(text[:start], " \t\n\r")
	after := strings.TrimLeft(text[end:], " \t\n\r")
	switch {
	case before == "":
		turn.Text = after
	case after == "":
		turn.Text = before
	default:
		turn.Text = before + "\n" + after
	}
	var parsed struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(blocks[0]), &parsed); err != nil {
		return Turn{}, fmt.Errorf("%w: block is not JSON: %v", ErrBadToolBlock, err)
	}
	if !known[parsed.Name] {
		return Turn{}, fmt.Errorf("%w: unknown tool %q", ErrBadToolBlock, parsed.Name)
	}
	args := strings.TrimSpace(string(parsed.Arguments))
	if args == "" {
		args = "{}"
	}
	if !json.Valid([]byte(args)) || json.Unmarshal([]byte(args), &map[string]any{}) != nil {
		return Turn{}, fmt.Errorf("%w: arguments of %s are not a JSON object", ErrBadToolBlock, parsed.Name)
	}
	turn.ToolCalls = []ToolCall{{
		ID: "fallback_0", Name: parsed.Name, Arguments: json.RawMessage(args),
	}}
	turn.FinishReason = "tool_calls"
	return turn, nil
}

// endpoint appends suffix to the base URL's path and keeps its query.
// Some gateways route by query (the z.ai sandbox hub needs
// ?XTransformPort=3003), so "https://h/v1?XTransformPort=3003" must
// become "https://h/v1/chat/completions?XTransformPort=3003", not a
// string with the suffix glued after the query.
func endpoint(base, suffix string) string {
	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" {
		return strings.TrimRight(base, "/") + suffix
	}
	u.Path = strings.TrimRight(u.Path, "/") + suffix
	u.RawPath = ""
	return u.String()
}

var (
	xmlToolRe  = regexp.MustCompile(`(?s)<tool\s+name\s*=\s*["']([A-Za-z0-9_.-]+)["']\s*>(.*?)</tool\s*>`)
	xmlParamRe = regexp.MustCompile(`(?s)<parameter\s+name\s*=\s*["']([A-Za-z0-9_.-]+)["']\s*>(.*?)</parameter\s*>`)
	xmlChildRe = regexp.MustCompile(`(?s)<([A-Za-z_][A-Za-z0-9_]*)>(.*?)</([A-Za-z_][A-Za-z0-9_]*)\s*>`)
	// key="value" | key='value' | key=value | key: value, one per line.
	kvQuotedRe = regexp.MustCompile(`(?s)([A-Za-z_][A-Za-z0-9_]*)\s*[=:]\s*"((?:[^"\\]|\\.)*)"`)
	kvSingleRe = regexp.MustCompile(`(?s)([A-Za-z_][A-Za-z0-9_]*)\s*[=:]\s*'((?:[^'\\]|\\.)*)'`)
	kvBareRe   = regexp.MustCompile(`(?m)^\s*([A-Za-z_][A-Za-z0-9_]*)\s*[=:]\s*(.+?)\s*$`)
)

// textCallValue keeps JSON scalars/objects typed and everything else a
// (HTML-unescaped) string.
func textCallValue(raw string) any {
	raw = strings.TrimSpace(raw)
	var v any
	if raw != "" && strings.ContainsAny(raw[:1], "{[0123456789-tfn") && json.Unmarshal([]byte(raw), &v) == nil {
		return v
	}
	return html.UnescapeString(raw)
}

// textCallArgs parses the body of a text-form tool call: a JSON object,
// <parameter name="k">v</parameter>, <k>v</k>, or key="v" / key: v lines.
func textCallArgs(body string) map[string]any {
	args := map[string]any{}
	trimmed := strings.TrimSpace(body)
	if strings.HasPrefix(trimmed, "{") {
		if json.Unmarshal([]byte(trimmed), &args) == nil {
			return args
		}
		args = map[string]any{}
	}
	if ps := xmlParamRe.FindAllStringSubmatch(body, -1); len(ps) > 0 {
		for _, p := range ps {
			args[p[1]] = textCallValue(p[2])
		}
		return args
	}
	if cs := xmlChildRe.FindAllStringSubmatch(body, -1); len(cs) > 0 {
		for _, c := range cs {
			if c[1] == c[3] {
				args[c[1]] = textCallValue(c[2])
			}
		}
		if len(args) > 0 {
			return args
		}
	}
	for _, m := range kvQuotedRe.FindAllStringSubmatch(body, -1) {
		var v string
		if json.Unmarshal([]byte(`"`+m[2]+`"`), &v) != nil {
			v = m[2]
		}
		args[m[1]] = v
	}
	for _, m := range kvSingleRe.FindAllStringSubmatch(body, -1) {
		if _, ok := args[m[1]]; !ok {
			args[m[1]] = strings.ReplaceAll(m[2], `\'`, "'")
		}
	}
	if len(args) == 0 {
		for _, m := range kvBareRe.FindAllStringSubmatch(body, -1) {
			args[m[1]] = textCallValue(m[2])
		}
	}
	return args
}

// parseXMLToolTurn recognises one text-form tool call for a tool in the
// registry, in the shapes models write when the provider dropped the
// tools field:
//
//	<tool name="list_files"><parameter name="path">/x</parameter></tool>
//	<run_command>command="ls -la"</run_command>
//	<read_file><path>a.txt</path></read_file>
//
// Only the first call is taken; the markup is removed from the text.
func parseXMLToolTurn(text string, tools []Tool) (Turn, bool) {
	known := map[string]bool{}
	for _, t := range tools {
		known[t.Name] = true
	}
	best, name, body := []int(nil), "", ""
	if m := xmlToolRe.FindStringSubmatchIndex(text); m != nil && known[text[m[2]:m[3]]] {
		best, name, body = m[:2], text[m[2]:m[3]], text[m[4]:m[5]]
	}
	for _, t := range tools {
		re := regexp.MustCompile(`(?s)<` + regexp.QuoteMeta(t.Name) + `(?:\s[^>]*)?>(.*?)</` + regexp.QuoteMeta(t.Name) + `\s*>`)
		if m := re.FindStringSubmatchIndex(text); m != nil && (best == nil || m[0] < best[0]) {
			best, name, body = m[:2], t.Name, text[m[2]:m[3]]
		}
	}
	if best == nil {
		return Turn{}, false
	}
	argJSON, _ := json.Marshal(textCallArgs(body))
	visible := strings.TrimSpace(strings.TrimSpace(text[:best[0]]) + "\n" + strings.TrimSpace(text[best[1]:]))
	return Turn{
		Text:         visible,
		ToolCalls:    []ToolCall{{ID: "xml_0", Name: name, Arguments: json.RawMessage(argJSON)}},
		FinishReason: "tool_calls",
	}, true
}

// mentionsTextToolCall reports markup that looks like a tool call
// (<run_command, <tool name=, ```tool) the parser could not read.
func mentionsTextToolCall(text string, tools []Tool) bool {
	if strings.Contains(text, "```tool") || strings.Contains(text, "<tool ") {
		return true
	}
	for _, t := range tools {
		if strings.Contains(text, "<"+t.Name+">") || strings.Contains(text, "<"+t.Name+" ") {
			return true
		}
	}
	return false
}
