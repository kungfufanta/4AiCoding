package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/docker"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

// v1Metrics is one resource sample of a sandbox.
type v1Metrics struct {
	SandboxID string       `json:"sandbox_id"`
	Status    string       `json:"status"`
	At        string       `json:"at"`
	Stats     docker.Stats `json:"stats"`
}

// --- GET /v1/sandboxes/{id}/metrics ---------------------------------

// v1SandboxMetrics returns one `docker stats` sample. A stopped sandbox
// returns zeros with status "stopped" (nothing is running to measure).
func (s *Server) v1SandboxMetrics(w http.ResponseWriter, r *http.Request) {
	sb, ok := s.v1Lookup(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	m, err := s.sampleMetrics(r, sb)
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) sampleMetrics(r *http.Request, sb *store.Sandbox) (v1Metrics, error) {
	m := v1Metrics{SandboxID: sb.ID, Status: sb.Status, At: time.Now().UTC().Format(time.RFC3339)}
	if sb.Status != "running" {
		return m, nil
	}
	st, err := s.Docker.Stats(r.Context(), "s-"+sb.ID)
	if errors.Is(err, docker.ErrNotFound) {
		m.Status = "stopped"
		return m, nil
	}
	if err != nil {
		return m, err
	}
	m.Stats = st
	return m, nil
}

// --- GET /v1/sandboxes/{id}/metrics/stream --------------------------

// v1SandboxMetricsStream sends a sample every ?interval= seconds (default 2,
// 1..60) as Server-Sent Events until the client disconnects.
func (s *Server) v1SandboxMetricsStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.v1Lookup(w, r, id); !ok {
		return
	}
	interval := 2
	if v := r.URL.Query().Get("interval"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 60 {
			writeV1Err(w, http.StatusBadRequest, "invalid_request", "interval must be 1..60 seconds")
			return
		}
		interval = n
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	t := time.NewTicker(time.Duration(interval) * time.Second)
	defer t.Stop()
	for {
		sb, err := s.Store.Get(r.Context(), id)
		if err != nil {
			_, _ = w.Write([]byte("event: end\ndata: {}\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
			return
		}
		m, err := s.sampleMetrics(r, sb)
		if err == nil {
			b, _ := json.Marshal(m)
			if _, err := w.Write([]byte("data: " + string(b) + "\n\n")); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		select {
		case <-r.Context().Done():
			return
		case <-t.C:
		}
	}
}

// --- GET /v1/sandboxes/{id}/volumes ---------------------------------

type v1Volume struct {
	Type        string `json:"type"`
	Destination string `json:"destination"`
	ReadOnly    bool   `json:"read_only"`
	// UsedBytes is the on-disk size, reported for the workspace only.
	UsedBytes *int64 `json:"used_bytes,omitempty"`
}

// v1SandboxVolumes lists what is mounted into the sandbox. Host paths are
// deliberately not returned: the API never hands out host layout.
func (s *Server) v1SandboxVolumes(w http.ResponseWriter, r *http.Request) {
	sb, ok := s.v1Lookup(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	used := diskBytes(sb.WorkspaceMnt)
	vols := []v1Volume{{Type: "workspace", Destination: "/home/sandbox", UsedBytes: &used}}
	if cj, err := s.inspectIfDocker(r, sb.ID); err == nil {
		for _, m := range cj.Mounts {
			if m.Destination == "/home/sandbox" {
				continue
			}
			vols = append(vols, v1Volume{Type: m.Type, Destination: m.Destination, ReadOnly: !m.RW})
		}
	}
	vols = append(vols,
		v1Volume{Type: "tmpfs", Destination: "/tmp"},
		v1Volume{Type: "tmpfs", Destination: "/var/tmp"})
	writeJSON(w, http.StatusOK, map[string]any{"volumes": vols})
}

// inspectIfDocker inspects the sandbox container; a server built without a
// docker client (unit tests) reports not-found.
func (s *Server) inspectIfDocker(r *http.Request, id string) (*docker.ContainerJSON, error) {
	if s.Docker == nil {
		return nil, docker.ErrNotFound
	}
	return s.Docker.Inspect(r.Context(), "s-"+id)
}
