// Dashboard BFF for sandboxes (PORTAL_PLAN_V2 P4 UI): the session-
// authenticated surface the Sandboxes pages call. Same core as the
// public /v1 gateway — one org's sandboxes, ownership 404s, live
// sandboxd data — but the actor is a signed-in user and members (not
// just admins) operate sandboxes.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/tastyeffectco/sandboxd/portal/internal/db"
	"github.com/tastyeffectco/sandboxd/portal/internal/quota"
	"github.com/tastyeffectco/sandboxd/portal/internal/sandboxd"
)

// handleBFFListSandboxes — GET /api/orgs/{orgID}/sandboxes?project_id=.
// Answers 200 with configured:false instead of an error when the
// sandbox backend is not wired up (dev/preview stacks), so the page
// can render its "not connected" empty state.
func (s *Server) handleBFFListSandboxes(w http.ResponseWriter, r *http.Request) {
	orgID := pathOrgID(r)
	if _, ok := s.requireMember(w, r, orgID); !ok {
		return
	}
	if s.Sandboxd == nil {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false, "sandboxes": []any{}})
		return
	}
	projectFilter := r.URL.Query().Get("project_id")
	if projectFilter != "" && !s.projectInOrg(r.Context(), orgID, projectFilter) {
		writeErr(w, http.StatusBadRequest, "bad_request", "unknown project for this org")
		return
	}
	sandboxes, unreachable, err := s.Sandboxd.ListByWorkspace(r.Context(), orgID)
	if err != nil {
		s.writeSandboxdErr(w, err)
		return
	}
	if projectFilter != "" {
		kept := sandboxes[:0]
		for _, sb := range sandboxes {
			if sb.External != nil && sb.External.ProjectID == projectFilter {
				kept = append(kept, sb)
			}
		}
		sandboxes = kept
	}
	projects := s.projectMap(r.Context(), orgID)
	out := make([]sandboxJSON, 0, len(sandboxes))
	for _, sb := range sandboxes {
		out = append(out, sandboxJSON{Sandbox: sb, ProjectID: projects[sb.ID]})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"configured":        true,
		"sandboxes":         out,
		"unreachable_nodes": unreachable,
	})
}

// handleBFFCreateSandbox — POST /api/orgs/{orgID}/sandboxes.
func (s *Server) handleBFFCreateSandbox(w http.ResponseWriter, r *http.Request) {
	orgID := pathOrgID(r)
	if _, ok := s.requireMember(w, r, orgID); !ok {
		return
	}
	if !s.requireSandboxd(w) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 65536)
	var body createSandboxBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if !body.validate(w) {
		return
	}
	projectID := body.ProjectID
	if projectID != "" && !s.projectInOrg(r.Context(), orgID, projectID) {
		writeErr(w, http.StatusBadRequest, "bad_request", "unknown project for this org")
		return
	}
	userID := db.UserID(r.Context())
	req := admitReq(&body)
	release, err := s.admitRun(r.Context(), orgID, req)
	if s.writeAdmitErr(w, orgID, err) {
		return
	}
	applyAdmitted(&body, req)
	sb, err := s.gatewayCreate(r.Context(), orgID, projectID, "user", userID, body)
	release()
	if err != nil {
		s.writeSandboxdErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sandboxJSON{Sandbox: sb, ProjectID: projectID})
}

// requireOwnedSandbox resolves + authorizes one sandbox for the
// session caller and returns its project pointer.
func (s *Server) requireOwnedSandbox(w http.ResponseWriter, r *http.Request, orgID, id string) (string, bool) {
	if _, ok := s.requireMember(w, r, orgID); !ok {
		return "", false
	}
	projectID, ok := s.sandboxOwned(r.Context(), orgID, id)
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "sandbox not found")
		return "", false
	}
	return projectID, true
}

// handleBFFGetSandbox — GET /api/orgs/{orgID}/sandboxes/{id}.
func (s *Server) handleBFFGetSandbox(w http.ResponseWriter, r *http.Request) {
	orgID := pathOrgID(r)
	id := r.PathValue("id")
	if _, ok := s.requireOwnedSandbox(w, r, orgID, id); !ok {
		return
	}
	if s.Sandboxd == nil {
		writeErr(w, http.StatusServiceUnavailable, "sandboxd_unavailable", "sandbox backend not configured")
		return
	}
	s.getSandboxCommon(w, r, orgID, id)
}

// handleBFFExecSandbox — POST /api/orgs/{orgID}/sandboxes/{id}/exec.
// Legacy {cmd} until AGENT_PLAN A0 merges (the v2 timeout fields would
// be rejected by today's sandboxd); the 120 s ceiling is the request
// context, not an in-container kill.
func (s *Server) handleBFFExecSandbox(w http.ResponseWriter, r *http.Request) {
	orgID := pathOrgID(r)
	id := r.PathValue("id")
	if _, ok := s.requireOwnedSandbox(w, r, orgID, id); !ok {
		return
	}
	if !s.requireSandboxd(w) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16384)
	var body struct {
		Cmd string `json:"cmd"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", `body must be {"cmd": "…"}`)
		return
	}
	argv, ok := splitCmd(body.Cmd)
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad_request", "cmd is empty or has an unterminated quote")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), maxExecSeconds*time.Second)
	defer cancel()
	out, err := s.Sandboxd.Exec(ctx, id, sandboxd.ExecInput{Cmd: argv})
	if err != nil {
		s.writeSandboxdErr(w, err)
		return
	}
	s.auditSandbox(r.Context(), orgID, "user", db.UserID(r.Context()), "sandbox.exec", id, nil)
	writeJSON(w, http.StatusOK, out)
}

// handleBFFSandboxLogs — GET …/logs?tail=&follow= (text or SSE relay).
func (s *Server) handleBFFSandboxLogs(w http.ResponseWriter, r *http.Request) {
	orgID := pathOrgID(r)
	id := r.PathValue("id")
	if _, ok := s.requireOwnedSandbox(w, r, orgID, id); !ok {
		return
	}
	if !s.requireSandboxd(w) {
		return
	}
	follow := r.URL.Query().Get("follow") == "true"
	tail := parseIntDefault(r.URL.Query().Get("tail"), 500, 10000)
	s.proxyLogs(w, r, orgID, id, tail, follow)
}

// handleBFFSandboxMetricsStream — GET …/metrics/stream?interval= (SSE
// relay; the browser consumes it with native EventSource, which
// carries the session cookie).
func (s *Server) handleBFFSandboxMetricsStream(w http.ResponseWriter, r *http.Request) {
	orgID := pathOrgID(r)
	id := r.PathValue("id")
	if _, ok := s.requireOwnedSandbox(w, r, orgID, id); !ok {
		return
	}
	if !s.requireSandboxd(w) {
		return
	}
	interval := parseIntDefault(r.URL.Query().Get("interval"), 2, 60)
	res, err := s.Sandboxd.MetricsStream(r.Context(), id, interval)
	if err != nil {
		s.writeSandboxdErr(w, err)
		return
	}
	defer res.Body.Close()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 16*1024)
	for {
		n, rerr := res.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if rerr != nil {
			return
		}
	}
}

// handleBFFSandboxAction — POST …/start, POST …/stop, DELETE …/{id}.
func (s *Server) handleBFFSandboxAction(w http.ResponseWriter, r *http.Request) {
	orgID := pathOrgID(r)
	id := r.PathValue("id")
	if _, ok := s.requireOwnedSandbox(w, r, orgID, id); !ok {
		return
	}
	if !s.requireSandboxd(w) {
		return
	}
	userID := db.UserID(r.Context())
	var err error
	action := ""
	switch {
	case r.Method == http.MethodPost && endsWith(r.URL.Path, "/start"):
		release, aerr := s.admitRun(r.Context(), orgID, &quota.CreateRequest{})
		if s.writeAdmitErr(w, orgID, aerr) {
			return
		}
		action, err = "sandbox.start", s.Sandboxd.Start(r.Context(), id)
		if err == nil {
			s.markRunning(r.Context(), orgID, id)
		}
		release()
	case r.Method == http.MethodPost:
		action, err = "sandbox.stop", s.Sandboxd.Stop(r.Context(), id)
	default:
		action, err = "sandbox.delete", s.Sandboxd.Delete(r.Context(), id)
	}
	if err != nil {
		s.writeSandboxdErr(w, err)
		return
	}
	if r.Method == http.MethodDelete {
		s.dropSandboxRow(r.Context(), orgID, id)
	} else {
		// Best-effort refresh so the next render sees the new state.
		if sb, gerr := s.Sandboxd.Get(r.Context(), id); gerr == nil {
			s.syncSandboxRow(r.Context(), orgID, "", sb)
		}
	}
	s.auditSandbox(r.Context(), orgID, "user", userID, action, id, nil)
	w.WriteHeader(http.StatusNoContent)
}

// keep the errors import honest if helpers move
var _ = errors.Is
