package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// An empty image means "sandboxd's instance image" (the one image the
// instance policy always allows). "Bật Agent" sends no image; a guessed
// image like ubuntu:24.04 was refused by the live policy.
func TestCreateSandboxBodyImageOptional(t *testing.T) {
	cases := []struct {
		name  string
		image string
		ok    bool
	}{
		{"empty = instance image", "", true},
		{"repo:tag", "python:3.12-alpine", true},
		{"malformed", "-bad image", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := createSandboxBody{Image: c.image}
			w := httptest.NewRecorder()
			if got := b.validate(w); got != c.ok {
				t.Fatalf("validate(%q) = %v, want %v (body %s)", c.image, got, c.ok, w.Body.String())
			}
			if !c.ok && w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", w.Code)
			}
		})
	}
}
