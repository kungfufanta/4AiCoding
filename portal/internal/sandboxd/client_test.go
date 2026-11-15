package sandboxd

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// fake sandboxd: records the last create body and serves canned
// responses. Close returns the server; callers wire Client.BaseURL.
func fakeSandboxd(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func TestCreateTagsTenant(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	srv := fakeSandboxd(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"01HX","status":"running","image":"ubuntu:24.04",
                        "resources":{"cpus":1,"memory_mb":512},"ports":[],"egress":[],"egress_mode":"allowlist",
                        "created_at":"2026-11-11T00:00:00Z"}`))
	})
	c := New(srv.URL, "test-key", 5*time.Second)
	sb, err := c.Create(context.Background(), CreateInput{
		OrgID: "org-uuid-1", ProjectID: "proj-uuid-2", UserID: "key-1",
		Image: "ubuntu:24.04", MemoryMB: 512, CPUs: 1, Ports: []int{3000},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sb.ID != "01HX" || sb.Status != "running" || sb.Resources.MemoryMB != 512 {
		t.Errorf("sandbox decoded wrong: %+v", sb)
	}
	if gotPath != "/v1/sandboxes" {
		t.Errorf("path = %s", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("auth header = %q", gotAuth)
	}
	ext, _ := gotBody["external"].(map[string]any)
	if ext == nil || ext["workspace_id"] != "org-uuid-1" || ext["project_id"] != "proj-uuid-2" || ext["user_id"] != "key-1" {
		t.Errorf("tenant tags missing: %v", gotBody)
	}
	if _, ok := gotBody["timeout_s"]; ok {
		t.Error("create must not carry exec fields")
	}
}

// GETs retry once on 503 (then succeed); mutations do not retry.
func TestRetryPolicy(t *testing.T) {
	var gets atomic.Int64
	var posts atomic.Int64
	srv := fakeSandboxd(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if gets.Add(1) == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte(`{"id":"01","status":"running"}`))
		case http.MethodPost:
			posts.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	c := New(srv.URL, "", time.Second)
	if _, err := c.Get(context.Background(), "01"); err != nil {
		t.Errorf("Get after one 503: %v", err)
	}
	if gets.Load() != 2 {
		t.Errorf("GET attempts = %d, want 2 (one retry)", gets.Load())
	}
	err := c.Stop(context.Background(), "01")
	var apiErr *APIError
	if err == nil {
		t.Fatal("Stop should surface the 503")
	}
	if !errorsAs(err, &apiErr) || apiErr.Status != http.StatusServiceUnavailable {
		t.Errorf("error = %v, want sandboxd 503", err)
	}
	if posts.Load() != 1 {
		t.Errorf("POST attempts = %d, want 1 (no retry on mutations)", posts.Load())
	}
}

// A sandboxd error envelope relays status + code + message.
func TestAPIErrorDecoded(t *testing.T) {
	srv := fakeSandboxd(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"workspace_required","message":"tag it"}}`))
	})
	c := New(srv.URL, "", time.Second)
	_, err := c.Create(context.Background(), CreateInput{})
	var apiErr *APIError
	if !errorsAs(err, &apiErr) {
		t.Fatalf("err = %v, want APIError", err)
	}
	if apiErr.Status != 400 || apiErr.Code != "workspace_required" || apiErr.Message != "tag it" {
		t.Errorf("APIError = %+v", apiErr)
	}
}

// Exec with no optional fields sends exactly {"cmd": …} — the legacy
// contract sandboxd accepts today (A0 adds the extra fields later).
func TestExecLegacyBody(t *testing.T) {
	var gotBody map[string]any
	srv := fakeSandboxd(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"stdout":"hi","stderr":"","exit_code":0}`))
	})
	c := New(srv.URL, "", time.Second)
	out, err := c.Exec(context.Background(), "01", ExecInput{Cmd: []string{"echo", "hi"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if out.Stdout != "hi" || out.ExitCode != 0 {
		t.Errorf("result = %+v", out)
	}
	if len(gotBody) != 1 || gotBody["cmd"] == nil {
		t.Errorf("legacy exec body must be {cmd} only, got %v", gotBody)
	}
}

// Exec with v2 fields maps them (compatible with the A0 sandboxd).
func TestExecV2Fields(t *testing.T) {
	var gotBody map[string]any
	srv := fakeSandboxd(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"stdout":"","stderr":"","exit_code":124,"timed_out":true}`))
	})
	c := New(srv.URL, "", time.Second)
	out, err := c.Exec(context.Background(), "01", ExecInput{
		Cmd: []string{"sleep", "999"}, TimeoutS: 30, MaxOutputBytes: 4096,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !out.TimedOut || out.ExitCode != 124 {
		t.Errorf("result = %+v", out)
	}
	if gotBody["timeout_s"] != float64(30) || gotBody["max_output_bytes"] != float64(4096) {
		t.Errorf("v2 fields not mapped: %v", gotBody)
	}
}

// errorsAs is errors.As without importing it in every test.
func errorsAs(err error, target *(*APIError)) bool {
	for err != nil {
		if e, ok := err.(*APIError); ok {
			*target = e
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// TestStreamsOutliveCallTimeout: the per-call timeout must not cut a
// followed log stream or a long exec — those are bounded by the
// caller's context (http.Client.Timeout also covers body reads).
func TestStreamsOutliveCallTimeout(t *testing.T) {
	srv := fakeSandboxd(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/sandboxes/sb1/exec" {
			time.Sleep(300 * time.Millisecond)
			_, _ = w.Write([]byte(`{"stdout":"done","exit_code":0}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 3; i++ {
			_, _ = w.Write([]byte("data: tick\n\n"))
			w.(http.Flusher).Flush()
			time.Sleep(150 * time.Millisecond)
		}
	})
	c := New(srv.URL, "", 100*time.Millisecond)
	res, err := c.Logs(context.Background(), "sb1", 10, true)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil {
		t.Fatalf("follow stream cut by the call timeout: %v (got %q)", err, body)
	}
	out, err := c.Exec(context.Background(), "sb1", ExecInput{Cmd: []string{"sleep", "1"}})
	if err != nil || out.Stdout != "done" {
		t.Fatalf("exec cut by the call timeout: %v %+v", err, out)
	}
}

// Health returns nil on 2xx and an APIError on 5xx; a closed server
// surfaces as a transport error. The BFF caches the result for 30 s
// (issue #82), so this is the one call per cache window.
func TestHealth(t *testing.T) {
	// 200 → nil.
	srvOK := fakeSandboxd(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	c := New(srvOK.URL, "test-key", 2*time.Second)
	if err := c.Health(context.Background()); err != nil {
		t.Errorf("Health on 200 = %v, want nil", err)
	}

	// 503 → APIError.
	srvBusy := fakeSandboxd(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	c2 := New(srvBusy.URL, "", 2*time.Second)
	err := c2.Health(context.Background())
	var apiErr *APIError
	if !errorsAs(err, &apiErr) || apiErr.Status != http.StatusServiceUnavailable {
		t.Errorf("Health on 503 = %v, want APIError{503}", err)
	}

	// Closed server → transport error (not APIError).
	srvDead := fakeSandboxd(t, func(w http.ResponseWriter, r *http.Request) {})
	srvDead.Close()
	c3 := New(srvDead.URL, "", 2*time.Second)
	if err := c3.Health(context.Background()); err == nil {
		t.Error("Health on dead server = nil, want transport error")
	}

	// Auth header carried.
	var gotAuth string
	srvAuth := fakeSandboxd(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	})
	c4 := New(srvAuth.URL, "bearer-test", 2*time.Second)
	_ = c4.Health(context.Background())
	if gotAuth != "Bearer bearer-test" {
		t.Errorf("Health auth = %q, want Bearer bearer-test", gotAuth)
	}
}
