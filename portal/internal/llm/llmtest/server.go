// Package llmtest is a scripted OpenAI-compatible chat/completions
// fake for tests (AGENT_PLAN_A3 §5). Each incoming request pops the
// next Scenario from the queue and streams its chunks as SSE; the
// request headers and body are recorded for assertions.
package llmtest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
)

// Scenario is one scripted HTTP response.
type Scenario struct {
	// Status is the HTTP status written before streaming (0 = 200).
	Status int
	// Chunks are raw SSE data payloads: full chunk objects such as
	// {"choices":[{"delta":{...}}]} or a final {"usage":{...}}.
	Chunks []any
	// RawChunks are written verbatim (keep-alives, broken JSON, ...).
	RawChunks []string
	// Disconnect closes the stream without [DONE] after the chunks
	// have been written (simulates a mid-stream network drop).
	Disconnect bool
}

// Recorded is one request the fake received.
type Recorded struct {
	Method string
	Path   string
	Header http.Header
	Body   map[string]any
}

// Server is the scripted fake.
type Server struct {
	*httptest.Server
	mu       sync.Mutex
	queue    []Scenario
	Requests []Recorded
}

// NewServer starts the fake with the given scenario queue. When the
// queue runs dry every further request gets 500 (so a retry test that
// expects fewer attempts than scenarios fails loudly).
func NewServer(scenarios ...Scenario) *Server {
	s := &Server{queue: scenarios}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		rec := Recorded{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone()}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		rec.Body = body
		s.Requests = append(s.Requests, rec)
		var sc Scenario
		if len(s.queue) > 0 {
			sc, s.queue = s.queue[0], s.queue[1:]
		} else {
			sc = Scenario{Status: 500}
		}
		s.mu.Unlock()

		if sc.Status != 0 && sc.Status != http.StatusOK {
			w.WriteHeader(sc.Status)
			_, _ = w.Write([]byte(`{"error":{"message":"scripted"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, c := range sc.RawChunks {
			_, _ = w.Write([]byte(c))
			if flusher != nil {
				flusher.Flush()
			}
		}
		for _, c := range sc.Chunks {
			b, err := json.Marshal(c)
			if err != nil {
				continue
			}
			_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
		}
		if sc.Disconnect {
			// Drop the connection without [DONE].
			if hijacker, ok := w.(http.Hijacker); ok {
				conn, buf, _ := hijacker.Hijack()
				_ = buf.Flush()
				_ = conn.Close()
			}
			return
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}))
	return s
}

// PopCount reports how many scenarios are still queued.
func (s *Server) PopCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.queue)
}

// --- chunk builders --------------------------------------------------

// TextChunk is one content delta.
func TextChunk(text string) map[string]any {
	return map[string]any{
		"choices": []any{map[string]any{
			"delta": map[string]any{"content": text},
		}},
	}
}

// ToolCallChunk is one tool_calls delta fragment. An empty id or name
// is omitted from the wire (matching providers that only send them on
// the first fragment).
func ToolCallChunk(index int, id, name, argsFragment string) map[string]any {
	fn := map[string]any{}
	if name != "" {
		fn["name"] = name
	}
	if argsFragment != "" {
		fn["arguments"] = argsFragment
	}
	tc := map[string]any{"index": index, "function": fn}
	if id != "" {
		tc["id"] = id
		tc["type"] = "function"
	}
	return map[string]any{
		"choices": []any{map[string]any{
			"delta": map[string]any{"tool_calls": []any{tc}},
		}},
	}
}

// FinishChunk sets the finish reason (the delta is empty).
func FinishChunk(reason string) map[string]any {
	return map[string]any{
		"choices": []any{map[string]any{
			"delta":         map[string]any{},
			"finish_reason": reason,
		}},
	}
}

// UsageChunk is the terminal usage chunk (include_usage providers).
func UsageChunk(in, out int) map[string]any {
	return map[string]any{
		"choices": []any{},
		"usage":   map[string]any{"prompt_tokens": in, "completion_tokens": out},
	}
}

// ToolBody marshals one tool definition for tests.
func ToolBody(name, desc, params string) map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        name,
			"description": desc,
			"parameters":  json.RawMessage(params),
		},
	}
}
