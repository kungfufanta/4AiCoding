package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Per-sandbox image selection is not supported yet (M3a) (the image is instance-wide via
// SANDBOXD_IMAGE). Every create body must reject an `image` field with a clear
// 400 instead of silently dropping it.
func TestImageFieldRejected(t *testing.T) {
	s := &Server{Store: memStore(t)}

	check := func(name string, h http.HandlerFunc, path, body string) {
		t.Run(name, func(t *testing.T) {
			w := httptest.NewRecorder()
			h(w, reqAs("POST", path, body, "tenantA"))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("%s: got %d, want 400; body=%s", name, w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "per-sandbox image selection is not supported") {
				t.Errorf("%s: missing explanatory message: %s", name, w.Body.String())
			}
		})
	}

	check("v1CreateSandbox", s.v1CreateSandbox, "/v1/sandboxes",
		`{"project":{"id":"p","user_id":"u"},"image":"sandboxd-go:test"}`)
	check("handleCreate", s.handleCreate, "/sandbox",
		`{"ports":[3000],"image":"sandboxd-go:test"}`)
}
