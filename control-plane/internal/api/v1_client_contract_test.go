package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// These tests pin the JSON SHAPES the console (and any /v1 client) renders.
// sandboxd is the contract; a client mocks exactly these shapes. If a field is
// renamed/removed here, the client's fixtures must change too — so this is where
// such a break should fail first.

// GET /v1/sandboxes/{id} → the object the console's app-detail screen reads.
func TestV1SandboxResponseShape(t *testing.T) {
	s, id := newSandboxRowServer(t, 3000)
	r := httptest.NewRequest("GET", "/v1/sandboxes/"+id, nil)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	s.v1GetSandbox(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d: %s", w.Code, w.Body)
	}
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	for _, k := range []string{"id", "status", "image", "ports", "preview", "visibility", "idle_policy", "created_at"} {
		if _, ok := m[k]; !ok {
			t.Errorf("v1 sandbox response missing %q: %s", k, w.Body)
		}
	}
	prev, ok := m["preview"].(map[string]any)
	if !ok {
		t.Fatalf("preview is not an object: %s", w.Body)
	}
	for _, k := range []string{"url", "status"} {
		if _, ok := prev[k]; !ok {
			t.Errorf("preview missing %q", k)
		}
	}
}
