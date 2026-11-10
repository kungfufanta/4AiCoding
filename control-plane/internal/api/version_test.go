package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/auth"
)

// GET /version is a public build-identity probe: JSON, uncacheable, and
// reachable without credentials even when auth is enforced.
func TestVersionEndpoint(t *testing.T) {
	s := &Server{Instance: InstanceInfo{Version: "v1.2.3", GitCommit: "abc1234"}}

	mw := auth.NewMiddleware(&auth.Config{Disabled: false}, nil, nil, nil)
	h := mw.Wrap(http.HandlerFunc(s.handleVersion))

	req := httptest.NewRequest("GET", "/version", nil)
	req.RemoteAddr = "203.0.113.9:5000" // not loopback; no Authorization header
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("cache-control = %q, want no-store", cc)
	}
	var got map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if got["version"] != "v1.2.3" || got["commit"] != "abc1234" {
		t.Fatalf("body = %v", got)
	}
	if len(got) != 2 {
		t.Fatalf("unexpected extra fields: %v", got)
	}
}
