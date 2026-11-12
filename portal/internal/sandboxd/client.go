// Package sandboxd is the Portal's client for the sandboxd control
// plane (PORTAL_PLAN 6/S3): create/read/exec/logs/actions on sandboxes,
// tagged with the caller's tenant (external.workspace_id = org id).
//
// Reliability contract: every call carries a timeout (Client.Timeout,
// PORTAL_SANDBOXD_TIMEOUT); read-only GETs retry once on a transport
// error or 502/503/504 (they cannot duplicate anything). Mutations
// never retry here — the caller's Idempotency-Key handling is what
// makes client-visible retries safe, and a blind retry of a create
// could build two sandboxes.
package sandboxd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Sandbox mirrors sandboxd's v1Sandbox (control-plane/internal/api/v1.go).
// Only the fields the Portal consumes are named; unknown fields are
// ignored so an older Portal keeps working against a newer sandboxd.
type Sandbox struct {
	ID           string            `json:"id"`
	Status       string            `json:"status"`
	Image        string            `json:"image"`
	Command      []string          `json:"command,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	Resources    Resources         `json:"resources"`
	Lifecycle    Lifecycle         `json:"lifecycle"`
	Egress       []string          `json:"egress"`
	EgressMode   string            `json:"egress_mode"`
	EgressDeny   []string          `json:"egress_deny"`
	Ports        []int             `json:"ports"`
	Preview      *Preview          `json:"preview,omitempty"`
	Visibility   string            `json:"visibility"`
	IdlePolicy   string            `json:"idle_policy"`
	Node         string            `json:"node,omitempty"`
	External     *External         `json:"external,omitempty"`
	ErrorMessage string            `json:"error_message,omitempty"`
	LastActiveAt string            `json:"last_active_at,omitempty"`
	CreatedAt    string            `json:"created_at"`
	UpdatedAt    string            `json:"updated_at,omitempty"`
}

type Resources struct {
	CPUs     float64 `json:"cpus"`
	MemoryMB int     `json:"memory_mb"`
}

type Lifecycle struct {
	IdleTimeoutSecs int  `json:"idle_timeout_secs"`
	KeepAlive       bool `json:"keep_alive"`
}

type Preview struct {
	URL    string `json:"url"`
	Port   int    `json:"port"`
	Status string `json:"status"`
}

type External struct {
	UserID      string `json:"user_id,omitempty"`
	ProjectID   string `json:"project_id,omitempty"`
	WorkspaceID string `json:"workspace_id,omitempty"`
}

// CreateInput is what a Portal caller may ask for. OrgID is the
// required tenant tag (it is what makes the sandbox visible to the org
// and only to it); everything else maps onto sandboxd's v1 create
// request.
type CreateInput struct {
	OrgID       string
	ProjectID   string
	UserID      string // api key id or user id, for the audit trail
	Image       string
	Command     []string
	Env         map[string]string
	CPUs        float64
	MemoryMB    int
	Ports       []int
	EgressMode  string // "" = sandboxd default (allowlist)
	EgressAllow []string
	RepoURL     string
	Branch      string
}

// ExecInput/ExecResult mirror the v1 exec v2 fields (AGENT_PLAN A0):
// in-container timeout, bounded output, timed_out/truncated markers.
type ExecInput struct {
	Cmd            []string
	Cwd            string
	Env            map[string]string
	Stdin          []byte
	TimeoutS       int
	MaxOutputBytes int64
}

type ExecResult struct {
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	ExitCode   int    `json:"exit_code"`
	TimedOut   bool   `json:"timed_out"`
	Truncated  bool   `json:"truncated"`
	DurationMs int64  `json:"duration_ms"`
}

// APIError is a sandboxd error response, relayed with its original
// status and machine code ({"error":{"code","message"}}).
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("sandboxd %d %s: %s", e.Status, e.Code, e.Message)
}

// Client talks to one sandboxd control plane. Safe for concurrent use.
type Client struct {
	// BaseURL, e.g. http://sandboxd:9191 (no trailing slash).
	BaseURL string
	// APIKey is the Bearer value sent on every call. Empty is valid
	// against a dev sandboxd without its auth store; production keys
	// carry the require_external flag (P3a).
	APIKey string
	// HTTP bounds one call; nil = 30 s.
	HTTP *http.Client
	// Stream carries calls whose length the caller bounds by context:
	// SSE log/metrics follows and exec (the gateway caps it at
	// maxExecSeconds). http.Client.Timeout covers reading the body, so
	// running these on HTTP would cut every stream at the 30 s default.
	Stream *http.Client
	Log    *slog.Logger
}

// streamClient returns the client for context-bounded long calls.
func (c *Client) streamClient() *http.Client {
	if c.Stream != nil {
		return c.Stream
	}
	return &http.Client{}
}

// httpClient returns the per-call-timeout client.
func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// New builds a client with the process-wide timeout.
func New(baseURL, apiKey string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		HTTP:    &http.Client{Timeout: timeout},
		Stream:  &http.Client{},
	}
}

// do runs one request and decodes the JSON response into out (nil out
// = discard). Retries once for idempotent GETs on transport errors and
// 502/503/504.
func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	return c.doWith(ctx, c.httpClient(), method, path, body, out)
}

// doWith is do on an explicit client (exec uses the stream client).
func (c *Client) doWith(ctx context.Context, hc *http.Client, method, path string, body any, out any) error {
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		payload = b
	}
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt == 1 && (method != http.MethodGet || !retryable(lastErr)) {
			return lastErr
		}
		if attempt == 1 {
			time.Sleep(250 * time.Millisecond)
		}
		err := c.once(ctx, hc, method, path, payload, out)
		if err == nil {
			return nil
		}
		lastErr = err
	}
	return lastErr
}

func retryable(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status == http.StatusBadGateway ||
			apiErr.Status == http.StatusServiceUnavailable ||
			apiErr.Status == http.StatusGatewayTimeout
	}
	return true // transport error (timeout, connection refused…)
}

func (c *Client) once(ctx context.Context, hc *http.Client, method, path string, payload []byte, out any) error {
	var rdr io.Reader
	if payload != nil {
		rdr = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		return err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	res, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return err
	}
	if res.StatusCode >= 400 {
		return decodeErr(res.StatusCode, data)
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

func decodeErr(status int, body []byte) error {
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err == nil && env.Error.Code != "" {
		return &APIError{Status: status, Code: env.Error.Code, Message: env.Error.Message}
	}
	// sandboxd's internal delegates occasionally emit the legacy
	// {"error": "…"} shape from the internal handler.
	var legacy struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &legacy); err == nil && legacy.Error != "" {
		return &APIError{Status: status, Code: "internal", Message: legacy.Error}
	}
	return &APIError{Status: status, Code: "internal", Message: strings.TrimSpace(string(body))}
}

// --- lifecycle -------------------------------------------------------

// Create places a sandbox tagged with the tenant. sandboxd clamps the
// resources to the instance policy; the response carries the applied
// values, so callers should read limits from the returned Sandbox
// rather than echo the request.
func (c *Client) Create(ctx context.Context, in CreateInput) (Sandbox, error) {
	body := map[string]any{
		"external": map[string]string{
			"user_id":      in.UserID,
			"project_id":   in.ProjectID,
			"workspace_id": in.OrgID,
		},
	}
	if in.Image != "" {
		body["image"] = in.Image
	}
	if len(in.Command) > 0 {
		body["command"] = in.Command
	}
	if len(in.Env) > 0 {
		body["env"] = in.Env
	}
	if in.CPUs > 0 || in.MemoryMB > 0 {
		body["resources"] = map[string]any{"cpus": in.CPUs, "memory_mb": in.MemoryMB}
	}
	if len(in.Ports) > 0 {
		body["ports"] = in.Ports
	}
	if in.EgressMode != "" || len(in.EgressAllow) > 0 {
		body["network"] = map[string]any{
			"egress_mode": in.EgressMode,
			"egress":      in.EgressAllow,
		}
	}
	if in.RepoURL != "" {
		body["workspace"] = map[string]string{"repo_url": in.RepoURL, "branch": in.Branch}
	}
	var sb Sandbox
	if err := c.do(ctx, http.MethodPost, "/v1/sandboxes", body, &sb); err != nil {
		return Sandbox{}, err
	}
	return sb, nil
}

// Get one sandbox (any tag) — the gateway checks ownership BEFORE
// calling this, so a hit is safe to relay.
func (c *Client) Get(ctx context.Context, id string) (Sandbox, error) {
	var sb Sandbox
	if err := c.do(ctx, http.MethodGet, "/v1/sandboxes/"+url.PathEscape(id), nil, &sb); err != nil {
		return Sandbox{}, err
	}
	return sb, nil
}

// ListByWorkspace returns the sandboxes tagged for one workspace
// (Portal org). sandboxd filters server-side on both local and fleet
// nodes; unreachable workers are reported as warnings.
func (c *Client) ListByWorkspace(ctx context.Context, workspaceID string) ([]Sandbox, []string, error) {
	var resp struct {
		Sandboxes        []Sandbox `json:"sandboxes"`
		UnreachableNodes []string  `json:"unreachable_nodes"`
	}
	q := url.Values{"external_workspace_id": {workspaceID}}
	if err := c.do(ctx, http.MethodGet, "/v1/sandboxes?"+q.Encode(), nil, &resp); err != nil {
		return nil, nil, err
	}
	out := resp.Sandboxes
	if out == nil {
		out = []Sandbox{}
	}
	return out, resp.UnreachableNodes, nil
}

// Start / Stop / Delete — the actions the plan exposes. Delete is the
// full destroy (container + workspace + row).
func (c *Client) Start(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(id)+"/start", map[string]any{}, nil)
}

func (c *Client) Stop(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(id)+"/stop", map[string]any{}, nil)
}

func (c *Client) Delete(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/v1/sandboxes/"+url.PathEscape(id), nil, nil)
}

// Exec runs a command with the v2 semantics. The gateway caps
// TimeoutS and MaxOutputBytes above what sandboxd accepts anyway.
func (c *Client) Exec(ctx context.Context, id string, in ExecInput) (ExecResult, error) {
	body := map[string]any{"cmd": in.Cmd}
	if in.Cwd != "" {
		body["cwd"] = in.Cwd
	}
	if len(in.Env) > 0 {
		body["env"] = in.Env
	}
	if in.Stdin != nil {
		// sandboxd exec v2 takes stdin as a plain JSON string (A0).
		body["stdin"] = string(in.Stdin)
	}
	if in.TimeoutS > 0 {
		body["timeout_s"] = in.TimeoutS
	}
	if in.MaxOutputBytes > 0 {
		body["max_output_bytes"] = in.MaxOutputBytes
	}
	var out ExecResult
	if err := c.doWith(ctx, c.streamClient(), http.MethodPost, "/v1/sandboxes/"+url.PathEscape(id)+"/exec", body, &out); err != nil {
		return ExecResult{}, err
	}
	return out, nil
}

// Logs opens the log stream: text/plain backlog (follow=false) or an
// SSE stream (follow=true). The caller owns closing the body; the
// request context cuts the stream.
func (c *Client) Logs(ctx context.Context, id string, tail int, follow bool) (*http.Response, error) {
	q := url.Values{"tail": {fmt.Sprint(tail)}}
	if follow {
		q.Set("follow", "true")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.BaseURL+"/v1/sandboxes/"+url.PathEscape(id)+"/logs?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	req.Header.Set("Accept", "text/plain, text/event-stream")
	hc := c.httpClient()
	if follow {
		hc = c.streamClient()
	}
	res, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 400 {
		defer res.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		return nil, decodeErr(res.StatusCode, data)
	}
	return res, nil
}

// Metrics fetches one stats sample; MetricsStream opens the SSE feed
// (interval seconds, 1..60). Both return raw payloads for the BFF to
// relay byte-for-byte.
func (c *Client) Metrics(ctx context.Context, id string, out any) error {
	return c.do(ctx, http.MethodGet, "/v1/sandboxes/"+url.PathEscape(id)+"/metrics", nil, out)
}

func (c *Client) MetricsStream(ctx context.Context, id string, interval int) (*http.Response, error) {
	q := ""
	if interval > 0 {
		q = "?interval=" + fmt.Sprint(interval)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.BaseURL+"/v1/sandboxes/"+url.PathEscape(id)+"/metrics/stream"+q, nil)
	if err != nil {
		return nil, err
	}
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	res, err := c.streamClient().Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 400 {
		defer res.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		return nil, decodeErr(res.StatusCode, data)
	}
	return res, nil
}

// Raw issues an arbitrary request to sandboxd and returns the raw
// response. Used by the A1 files gateway: the request body, query
// string, and headers are forwarded verbatim; the response body is
// relayed byte-for-byte to the Portal caller. The caller owns closing
// res.Body.
//
// Files reads can be large (2 MiB per call) but not stream-long, so
// the per-call HTTP client (c.httpClient) bounds them. PUT/DELETE/move
// are short. Use streamClient() only when the call is stream-shaped
// (none of the files routes are); the gateway caps the read body via
// io.LimitReader to 8 MiB as a safety net.
func (c *Client) Raw(ctx context.Context, method, path string, body []byte, contentType string) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil && contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	res, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 400 {
		defer res.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		return nil, decodeErr(res.StatusCode, data)
	}
	return res, nil
}

// ExecStream opens the exec v2 endpoint in NDJSON streaming mode
// (?stream=ndjson) and returns the raw response. The caller reads
// newline-delimited JSON lines and relays them; the request context
// bounds the stream. Use this instead of Exec when the client wants
// ?stream=ndjson. (AGENT_PLAN A1.)
func (c *Client) ExecStream(ctx context.Context, id string, in ExecInput) (*http.Response, error) {
	body := map[string]any{"cmd": in.Cmd}
	if in.Cwd != "" {
		body["cwd"] = in.Cwd
	}
	if len(in.Env) > 0 {
		body["env"] = in.Env
	}
	if in.Stdin != nil {
		body["stdin"] = string(in.Stdin)
	}
	if in.TimeoutS > 0 {
		body["timeout_s"] = in.TimeoutS
	}
	if in.MaxOutputBytes > 0 {
		body["max_output_bytes"] = in.MaxOutputBytes
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode exec stream request: %w", err)
	}
	q := url.Values{"stream": {"ndjson"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/v1/sandboxes/"+url.PathEscape(id)+"/exec?"+q.Encode(),
		bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	req.Header.Set("Accept", "application/x-ndjson")
	res, err := c.streamClient().Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 400 {
		defer res.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		return nil, decodeErr(res.StatusCode, data)
	}
	return res, nil
}
