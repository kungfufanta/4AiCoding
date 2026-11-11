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
