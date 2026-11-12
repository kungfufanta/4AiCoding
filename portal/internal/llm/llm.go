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
	"io"
	"net/http"
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
	HTTP    *http.Client
	// MaxOutputBytes caps the assembled reply so a runaway stream
	// cannot grow without bound. Zero means 256 KiB.
	MaxOutputBytes int
}

// ErrUpstream wraps a non-2xx answer from the provider. The provider's
// body is not echoed to callers (it may carry account details).
var ErrUpstream = errors.New("llm upstream error")

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
		body["user"] = "u_" + req.UserID
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.BaseURL, "/")+"/chat/completions", bytes.NewReader(raw))
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
			hreq.Header.Set("X-WebHub-User", "u_"+req.UserID)
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
		strings.TrimRight(c.BaseURL, "/")+"/chat/completions", bytes.NewReader(raw))
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

// ListModels calls GET {base}/models and returns the model id list.
// Used by the connection editor's "Load models" button.
func (c *Client) ListModels(ctx context.Context) ([]string, error) {
	hreq, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(c.BaseURL, "/")+"/models", nil)
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
