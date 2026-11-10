package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/audit"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

// v1PortView is one preview port of a sandbox.
type v1PortView struct {
	Container  int    `json:"container"`
	PreviewURL string `json:"preview_url"`
}

func (s *Server) v1PortsBody(sb *store.Sandbox) map[string]any {
	out := make([]v1PortView, 0, len(sb.Ports))
	for _, p := range sb.Ports {
		out = append(out, v1PortView{Container: p, PreviewURL: s.previewURL(sb.ID, p)})
	}
	return map[string]any{"ports": out}
}

// --- GET /v1/sandboxes/{id}/network/ports ---------------------------

func (s *Server) v1GetPorts(w http.ResponseWriter, r *http.Request) {
	sb, ok := s.v1Lookup(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.v1PortsBody(sb))
}

// --- PUT /v1/sandboxes/{id}/network/ports ---------------------------

// v1PutPorts replaces the preview port list: {"ports":[{"container":3000}]}.
func (s *Server) v1PutPorts(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ports []v1PortReq `json:"ports"`
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "invalid json: "+err.Error())
		return
	}
	ports := make([]int, 0, len(req.Ports))
	for _, p := range req.Ports {
		ports = append(ports, p.Container)
	}
	s.v1SetPorts(w, r, r.PathValue("id"), func([]int) []int { return ports })
}

// --- POST /v1/sandboxes/{id}/network/ports --------------------------

// v1AddPort adds one preview port: {"container":8080}. Idempotent.
func (s *Server) v1AddPort(w http.ResponseWriter, r *http.Request) {
	var req v1PortReq
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "invalid json: "+err.Error())
		return
	}
	s.v1SetPorts(w, r, r.PathValue("id"), func(cur []int) []int {
		for _, p := range cur {
			if p == req.Container {
				return cur
			}
		}
		return append(append([]int(nil), cur...), req.Container)
	})
}

// --- DELETE /v1/sandboxes/{id}/network/ports/{port} -----------------

func (s *Server) v1DeletePort(w http.ResponseWriter, r *http.Request) {
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "port must be a number")
		return
	}
	s.v1SetPorts(w, r, r.PathValue("id"), func(cur []int) []int {
		out := make([]int, 0, len(cur))
		for _, p := range cur {
			if p != port {
				out = append(out, p)
			}
		}
		return out
	})
}

// v1SetPorts applies a port-list change. Preview routes are container labels,
// so a change rebuilds the container from its row (lossless: all state is in
// the workspace). A running sandbox is restarted on the new container; a
// stopped one gets it on its next start.
func (s *Server) v1SetPorts(w http.ResponseWriter, r *http.Request, id string, change func([]int) []int) {
	sb, ok := s.v1Lookup(w, r, id)
	if !ok {
		return
	}
	if sb.Status != "running" && sb.Status != "stopped" {
		writeV1Err(w, http.StatusConflict, "conflict", "sandbox is "+sb.Status+" — cannot change ports")
		return
	}
	next := change(sb.Ports)
	sort.Ints(next)
	if err := s.checkPorts(r.Context(), next); err != nil {
		writeV1Err(w, http.StatusBadRequest, "policy_denied", err.Error())
		return
	}
	if equalInts(next, sb.Ports) {
		writeJSON(w, http.StatusOK, s.v1PortsBody(sb))
		return
	}
	if err := s.Store.SetPorts(r.Context(), id, next); err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if !s.rebuildContainer(w, r, sb) {
		return
	}
	s.auditAction(r, audit.Entry{Action: "sandbox.ports.update", Target: id,
		Detail: map[string]any{"from": sb.Ports, "to": next}})
	fresh, ok := s.v1Lookup(w, r, id)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.v1PortsBody(fresh))
}

// rebuildContainer replaces a sandbox's container so it picks up a changed
// row (ports, spec). It writes the v1 error and returns false on failure.
func (s *Server) rebuildContainer(w http.ResponseWriter, r *http.Request, sb *store.Sandbox) bool {
	wasRunning := sb.Status == "running"
	if wasRunning {
		if err := s.Docker.Stop(r.Context(), "s-"+sb.ID, 10); err != nil {
			writeV1Err(w, http.StatusInternalServerError, "internal", "docker stop: "+err.Error())
			return false
		}
		if err := s.Store.MarkStoppedAt(r.Context(), sb.ID, time.Now().UTC()); err != nil {
			s.loggerFor(r, sb.ID).Warn("rebuild: MarkStoppedAt failed", "err", err.Error())
		}
	}
	// The wake path recreates a missing container from the row.
	if err := s.Docker.Remove(r.Context(), "s-"+sb.ID); err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", "docker rm: "+err.Error())
		return false
	}
	if !wasRunning {
		return true
	}
	code, body := s.delegate(r, s.handleWakeJSON, http.MethodPost, "/wake/"+sb.ID,
		map[string]string{"id": sb.ID}, nil)
	if code != http.StatusOK {
		relayV1Error(w, code, body)
		return false
	}
	return true
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
