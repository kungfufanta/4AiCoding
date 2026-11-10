package httpapi

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthzAlwaysOK(t *testing.T) {
	s := &Server{Log: slog.Default(), Pool: nil} // must not touch the pool
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want 200", rec.Code)
	}
	if want := `{"status":"ok"}` + "\n"; rec.Body.String() != want {
		t.Errorf("body = %q, want %q", rec.Body.String(), want)
	}
}

func TestReadyzWithoutPoolIs503(t *testing.T) {
	// A nil pool panics on Ping; the handler must not — model a dead
	// database by a pool whose Ping fails. For S0 the nil case is
	// guarded: readiness answers 503 instead of crashing the probe.
	s := &Server{Log: slog.Default(), Pool: nil}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET /readyz with nil pool = %d, want 503", rec.Code)
	}
}

func TestNotFoundEnvelope(t *testing.T) {
	rec := httptest.NewRecorder()
	NotFound(rec, httptest.NewRequest(http.MethodGet, "/nope", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("NotFound status = %d, want 404", rec.Code)
	}
	want := `{"error":{"code":"not_found","message":"route not found"}}` + "\n"
	if rec.Body.String() != want {
		t.Errorf("body = %q, want %q", rec.Body.String(), want)
	}
}
