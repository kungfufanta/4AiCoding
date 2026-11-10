// Package httpapi serves the Portal HTTP surface. S0 keeps it to the
// health probes (/healthz, /readyz per PORTAL_PLAN 6/S0); milestones
// S1+ mount the BFF and the public /v1 API on the same mux.
package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Server wires the dependencies for the HTTP handlers.
type Server struct {
	Log  *slog.Logger
	Pool *pgxpool.Pool
}

// Handler returns the root mux with every route registered. Callers
// may mount more handlers (the SPA in main) on the returned mux.
func (s *Server) Handler() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	return mux
}

// handleHealthz answers 200 always: the process is up. It touches no
// dependency on purpose (kubernetes uses it to decide restarts).
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReadyz answers 200 only when Postgres answers within 2s:
// readiness gates traffic behind the service.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if s.Pool == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "unavailable",
			"error":  "database not connected",
		})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), pingReadyTimeout)
	defer cancel()
	if err := s.Pool.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "unavailable",
			"error":  "database ping: " + err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

const pingReadyTimeout = 2 * time.Second

// NotFound writes the control-plane-style error envelope for unknown
// routes: {"error":{"code":"...","message":"..."}} (PORTAL_PLAN 5).
func NotFound(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusNotFound, map[string]any{
		"error": map[string]string{"code": "not_found", "message": "route not found"},
	})
}
