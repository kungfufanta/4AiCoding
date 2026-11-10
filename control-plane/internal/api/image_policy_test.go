package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A sandbox may only name the instance image or one on the allowlist
// (SANDBOXD_IMAGE_ALLOWLIST). Both create paths must reject any other image
// with a clear 400 before anything touches docker.
func TestImageOutsideAllowlistRejected(t *testing.T) {
	s := &Server{Store: memStore(t)}

	check := func(name string, h http.HandlerFunc, path, body string) {
		t.Run(name, func(t *testing.T) {
			w := httptest.NewRecorder()
			h(w, reqAs("POST", path, body, "tenantA"))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("%s: got %d, want 400; body=%s", name, w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "is not allowed on this instance") {
				t.Errorf("%s: missing explanatory message: %s", name, w.Body.String())
			}
		})
	}

	check("v1CreateSandbox", s.v1CreateSandbox, "/v1/sandboxes",
		`{"image":"sandboxd-go:test"}`)
	check("handleCreate", s.handleCreate, "/sandbox",
		`{"ports":[3000],"image":"sandboxd-go:test"}`)
}
