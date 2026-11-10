package httpapi

import (
	"context"
	"net/http"
	"time"
)

const pingReadyTimeout = 2 * time.Second

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

// NotFound writes the control-plane-style error envelope for unknown
// routes: {"error":{"code":"...","message":"..."}} (PORTAL_PLAN 5).
func NotFound(w http.ResponseWriter, _ *http.Request) {
	writeErr(w, http.StatusNotFound, "not_found", "route not found")
}
