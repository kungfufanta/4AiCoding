package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/audit"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/upgrade"
)

// GET /v1/upgrade — current upgrade state (survives the control-plane restart:
// it is a file under the data dir).
func (s *Server) v1UpgradeStatus(w http.ResponseWriter, r *http.Request) {
	if s.Upgrade == nil {
		writeJSON(w, http.StatusOK, upgrade.State{Phase: "unavailable",
			Message: "in-place upgrade is not configured on this install — use ./upgrade.sh"})
		return
	}
	writeJSON(w, http.StatusOK, s.Upgrade.Status(r.Context()))
}

// POST /v1/upgrade {"target":"vX.Y.Z"} — launch the detached upgrader.
// 202 + state on success; 409 already running / unsupported install;
// 400 bad target; 507 low disk.
func (s *Server) v1UpgradeStart(w http.ResponseWriter, r *http.Request) {
	if s.Upgrade == nil {
		writeV1Err(w, http.StatusConflict, "unsupported",
			"in-place upgrade is not configured on this install — run ./upgrade.sh on the server")
		return
	}
	var body struct {
		Target string `json:"target"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4*1024))
	if err := dec.Decode(&body); err != nil || body.Target == "" {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "target (release tag) is required")
		return
	}
	st, err := s.Upgrade.Start(r.Context(), body.Target)
	switch {
	case err == nil:
		s.auditAction(r, audit.Entry{Action: "upgrade.started", Target: body.Target})
		writeJSON(w, http.StatusAccepted, st)
	case errors.Is(err, upgrade.ErrRunning):
		writeV1Err(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, upgrade.ErrNoSrcDir):
		writeV1Err(w, http.StatusConflict, "unsupported", err.Error())
	case errors.Is(err, upgrade.ErrBadTarget):
		writeV1Err(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, upgrade.ErrLowDisk):
		writeV1Err(w, http.StatusInsufficientStorage, "low_disk", err.Error())
	default:
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
	}
}
