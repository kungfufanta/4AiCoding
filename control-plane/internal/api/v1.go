// v1.go — the public /v1 sandbox lifecycle API.
// It is a thin translation layer over the internal machinery: create,
// delete, exec and wake delegate to the internal /sandbox handlers, so
// both surfaces share one implementation. The legacy /sandbox routes
// stay available until they are deprecated.
package api

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/audit"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/events"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

// errPerAppImage is returned when a request names an image. Per-sandbox images
// arrive in M3a; until then the image is instance-wide (SANDBOXD_IMAGE).
const errPerAppImage = "per-sandbox image selection is not supported yet; set SANDBOXD_IMAGE instance-wide"

// --- v1 response shapes ---------------------------------------------

type v1Preview struct {
	URL    string `json:"url"`
	Port   int    `json:"port,omitempty"` // the resolved web port the URL routes to
	Status string `json:"status"`         // running | down
}

type v1External struct {
	UserID    string `json:"user_id,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
}

type v1Sandbox struct {
	ID             string      `json:"id"`
	Status         string      `json:"status"`
	Image          string      `json:"image"`
	Ports          []int       `json:"ports"`
	Preview        *v1Preview  `json:"preview,omitempty"` // only when the sandbox exposes ports
	Visibility     string      `json:"visibility"`
	IdlePolicy     string      `json:"idle_policy"`
	External       *v1External `json:"external,omitempty"`
	ErrorMessage   string      `json:"error_message,omitempty"`
	LastActiveAt   string      `json:"last_active_at,omitempty"`
	KeepaliveUntil string      `json:"keepalive_until,omitempty"`
	CreatedAt      string      `json:"created_at"`
	UpdatedAt      string      `json:"updated_at,omitempty"`
}

// --- helpers --------------------------------------------------------

// writeV1Err emits the v1 error envelope.
func writeV1Err(w http.ResponseWriter, code int, errCode, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]any{
		"code": errCode, "message": msg, "retryable": code == 502 || code == 503,
	}})
}

func v1ErrCode(code int) string {
	switch code {
	case http.StatusBadRequest:
		return "invalid_request"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusConflict:
		return "conflict"
	case http.StatusServiceUnavailable:
		return "sandbox_capacity"
	default:
		return "internal"
	}
}

// relayV1Error reshapes an internal-handler error body into the v1
// envelope.
func relayV1Error(w http.ResponseWriter, code int, body []byte) {
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	msg := e.Error
	if msg == "" {
		msg = http.StatusText(code)
	}
	writeV1Err(w, code, v1ErrCode(code), msg)
}

// delegate invokes an internal handler with a synthesized request and
// returns the captured status + body. The original request context is
// carried so the auth actor and request id propagate.
func (s *Server) delegate(r *http.Request, h http.HandlerFunc, method, path string, pathValues map[string]string, body []byte) (int, []byte) {
	var inner *http.Request
	if body != nil {
		inner = httptest.NewRequest(method, path, bytes.NewReader(body))
	} else {
		inner = httptest.NewRequest(method, path, nil)
	}
	inner = inner.WithContext(r.Context())
	for k, v := range pathValues {
		inner.SetPathValue(k, v)
	}
	rec := httptest.NewRecorder()
	h(rec, inner)
	return rec.Code, rec.Body.Bytes()
}

// previewScheme is the scheme (and its default port) generated preview URLs
// use: PreviewURLScheme when set, else derived from PreviewTLS.
func (s *Server) previewScheme() (scheme, defaultPort string) {
	switch s.PreviewURLScheme {
	case "https":
		return "https", "443"
	case "http":
		return "http", "80"
	}
	if s.PreviewTLS {
		return "https", "443"
	}
	return "http", "80"
}

func (s *Server) previewURL(id string, webPort int) string {
	// Reflect the scheme the browser must use: plain HTTP by default (so a
	// local deploy returns a reachable http:// URL the console can iframe),
	// https when we terminate TLS OR when something in front of us does.
	scheme, defaultPort := s.previewScheme()
	// An explicit PREVIEW_URL_SCHEME means something in front of us owns the
	// public port (a TLS terminator on 443, a tunnel gateway): our own host
	// port is not what the browser dials, so it must not appear in the URL.
	if s.PreviewURLScheme != "" {
		defaultPort = s.PublicHTTPPort
	}
	if webPort <= 0 {
		webPort = 3000 // backward-compatible default
	}
	// The preview hostname's port is the sandbox's RESOLVED web port (manifest /
	// preset / 3000) — the same port the Traefik router serves — so a non-3000
	// app (e.g. Astro on 4321) gets a reachable URL.
	host := s.hosts().Preview(id, webPort)
	// Append the host-facing port unless it's the scheme default. On a
	// shared host published on e.g. :18080, the bare URL would hit whatever
	// owns :80 (a front proxy), so the port must be in the URL the browser,
	// console iframe, and open-in-tab link all use.
	if p := s.PublicHTTPPort; p != "" && p != defaultPort {
		host += ":" + p
	}
	return scheme + "://" + host
}

// v1SandboxFromRow reshapes a stored sandbox to the v1 object.
func (s *Server) v1SandboxFromRow(_ *http.Request, sb *store.Sandbox) v1Sandbox {
	out := v1Sandbox{
		ID:         sb.ID,
		Status:     sb.Status,
		Image:      sb.Image,
		Ports:      sb.Ports,
		Visibility: sb.Visibility,
		IdlePolicy: sb.IdlePolicy,
		CreatedAt:  sb.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:  sb.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if out.Ports == nil {
		out.Ports = []int{}
	}
	if len(sb.Ports) > 0 {
		p := s.v1PreviewView(sb.ID, sb.Status, webPortOf(sb))
		out.Preview = &p
	}
	if sb.ExternalUserID.Valid || sb.ExternalProjectID.Valid {
		out.External = &v1External{UserID: sb.ExternalUserID.String, ProjectID: sb.ExternalProjectID.String}
	}
	if sb.ErrorMessage.Valid {
		out.ErrorMessage = sb.ErrorMessage.String
	}
	if !sb.LastActiveAt.IsZero() {
		out.LastActiveAt = sb.LastActiveAt.UTC().Format(time.RFC3339)
	}
	if sb.KeepaliveUntil.Valid && sb.KeepaliveUntil.Int64 > 0 {
		out.KeepaliveUntil = time.Unix(sb.KeepaliveUntil.Int64, 0).UTC().Format(time.RFC3339)
	}
	return out
}

// v1PreviewView derives the public preview shape from the row status alone:
// the control plane does not look inside the sandbox. Pure given the Server
// config — unit-tested.
func (s *Server) v1PreviewView(id, rowStatus string, webPort int) v1Preview {
	port := webPort
	if port <= 0 {
		port = 3000
	}
	prev := v1Preview{URL: s.previewURL(id, webPort), Port: port}
	if rowStatus == "running" {
		prev.Status = "running"
	} else {
		prev.Status = "down"
	}
	return prev
}

// v1Lookup loads a sandbox row and writes the v1 404/500 on failure.
func (s *Server) v1Lookup(w http.ResponseWriter, r *http.Request, id string) (*store.Sandbox, bool) {
	sb, err := s.Store.Get(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeV1Err(w, http.StatusNotFound, "not_found", "no such sandbox")
		return nil, false
	}
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return nil, false
	}
	return sb, true
}

// --- GET /v1/sandboxes ----------------------------------------------

// v1ListSandboxes lists sandboxes, newest first, optionally filtered by
// external identity (?external_user_id= / ?external_project_id=) and by
// ?status=.
func (s *Server) v1ListSandboxes(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	euid, epid, status := q.Get("external_user_id"), q.Get("external_project_id"), q.Get("status")
	var rows []*store.Sandbox
	var err error
	if euid != "" || epid != "" {
		rows, err = s.Store.ListFiltered(r.Context(), euid, epid)
	} else {
		rows, err = s.Store.List(r.Context())
	}
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	out := make([]v1Sandbox, 0, len(rows))
	for _, sb := range rows {
		if status != "" && sb.Status != status {
			continue
		}
		out = append(out, s.v1SandboxFromRow(r, sb))
	}
	writeJSON(w, http.StatusOK, map[string]any{"sandboxes": out})
}

// --- POST /v1/sandboxes ---------------------------------------------

type v1CreateReq struct {
	// Ports the sandbox serves; each gets a preview route. Empty = no preview.
	Ports      []int             `json:"ports,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	Visibility string            `json:"visibility,omitempty"`  // public | private (default private)
	IdlePolicy string            `json:"idle_policy,omitempty"` // sleep | always_on
	External   v1External        `json:"external,omitempty"`
	// FromSnapshot clones the workspace from a snapshot the caller owns.
	FromSnapshot string `json:"from_snapshot,omitempty"`
	// Workspace clones a public git repository into the workspace.
	Workspace *struct {
		RepoURL string `json:"repo_url"`
		Branch  string `json:"branch,omitempty"`
	} `json:"workspace,omitempty"`
	// Image is rejected until per-sandbox images land (M3a). Declared so we
	// 400 it instead of silently dropping it.
	Image *string `json:"image,omitempty"`
}

func (s *Server) v1CreateSandbox(w http.ResponseWriter, r *http.Request) {
	var req v1CreateReq
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "invalid json: "+err.Error())
		return
	}
	if req.Image != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", errPerAppImage)
		return
	}
	if req.FromSnapshot != "" && req.Workspace != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "from_snapshot and workspace are mutually exclusive")
		return
	}
	vis := req.Visibility
	if vis == "" {
		vis = "private"
	}
	createBody := map[string]any{
		"ports":      req.Ports,
		"visibility": vis,
		"env":        req.Env,
		"external":   map[string]string{"user_id": req.External.UserID, "project_id": req.External.ProjectID},
	}
	if req.IdlePolicy != "" {
		createBody["idle_policy"] = req.IdlePolicy
	}
	if req.Workspace != nil {
		if req.Workspace.RepoURL == "" {
			writeV1Err(w, http.StatusBadRequest, "invalid_request", "workspace.repo_url is required")
			return
		}
		createBody["git"] = map[string]string{"repo_url": req.Workspace.RepoURL, "branch": req.Workspace.Branch}
	}
	if req.FromSnapshot != "" {
		// Resolve + authorize the snapshot under the caller's tenant,
		// then hand handleCreate the pre-resolved image path. A
		// cross-tenant or missing snapshot is a 404 (don't leak existence).
		snap, err := s.snapshotForTenant(r, req.FromSnapshot)
		if errors.Is(err, store.ErrNotFound) {
			writeV1Err(w, http.StatusNotFound, "not_found", "no such snapshot")
			return
		}
		if err != nil {
			writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		if snap.Status != "ready" {
			writeV1Err(w, http.StatusBadRequest, "invalid_request", "snapshot is not ready")
			return
		}
		createBody["template_path"] = snap.ImagePath
	}
	internal, _ := json.Marshal(createBody)
	code, body := s.delegate(r, s.handleCreate, http.MethodPost, "/sandbox", nil, internal)
	if code != http.StatusCreated {
		// Capacity pushback (503) carries a Retry-After on the internal
		// handler; the delegate's recorder drops it, so re-assert it here.
		if code == http.StatusServiceUnavailable {
			w.Header().Set("Retry-After", "30")
		}
		relayV1Error(w, code, body)
		return
	}
	var sr sandboxResp
	if err := json.Unmarshal(body, &sr); err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", "decode create result: "+err.Error())
		return
	}
	sb, err := s.Store.Get(r.Context(), sr.ID)
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", "post-create get: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, s.v1SandboxFromRow(r, sb))
}

// --- GET /v1/sandboxes/{id} -----------------------------------------

func (s *Server) v1GetSandbox(w http.ResponseWriter, r *http.Request) {
	sb, ok := s.v1Lookup(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, s.v1SandboxFromRow(r, sb))
}

// --- POST /v1/sandboxes/{id}/stop -----------------------------------

func (s *Server) v1StopSandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sb, ok := s.v1Lookup(w, r, id)
	if !ok {
		return
	}
	if sb.Status == "stopped" {
		writeJSON(w, http.StatusOK, s.v1SandboxFromRow(r, sb)) // idempotent
		return
	}
	if sb.Status != "running" {
		writeV1Err(w, http.StatusConflict, "conflict", "sandbox is "+sb.Status+" — cannot stop")
		return
	}
	if !s.stopSandbox(w, r, id) {
		return
	}
	sb, _ = s.Store.Get(r.Context(), id)
	writeJSON(w, http.StatusOK, s.v1SandboxFromRow(r, sb))
}

// stopSandbox stops a running sandbox's container and records it. Writes the
// v1 error and returns false on failure.
func (s *Server) stopSandbox(w http.ResponseWriter, r *http.Request, id string) bool {
	if err := s.Docker.Stop(r.Context(), "s-"+id, 10); err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", "docker stop: "+err.Error())
		return false
	}
	if err := s.Store.MarkStoppedAt(r.Context(), id, time.Now().UTC()); err != nil {
		s.loggerFor(r, id).Warn("v1 stop: MarkStoppedAt failed", "err", err.Error())
	}
	s.auditAction(r, audit.Entry{Action: "sandbox.stop", Target: id})
	s.recordEvent(r, events.Event{Type: events.SandboxStopped, Severity: events.SeverityInfo,
		Message: "Sandbox stopped", SandboxID: id})
	return true
}

// --- POST /v1/sandboxes/{id}/start ----------------------------------

// v1StartSandbox wakes a stopped sandbox. Idempotent when already running.
func (s *Server) v1StartSandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sb, ok := s.v1Lookup(w, r, id)
	if !ok {
		return
	}
	if sb.Status == "running" {
		writeJSON(w, http.StatusOK, s.v1SandboxFromRow(r, sb)) // idempotent
		return
	}
	if sb.Status != "stopped" {
		writeV1Err(w, http.StatusConflict, "conflict", "sandbox is "+sb.Status+" — cannot start")
		return
	}
	s.startAndRespond(w, r, id)
}

// startAndRespond wakes a stopped sandbox via the internal wake path and
// writes the v1 object.
func (s *Server) startAndRespond(w http.ResponseWriter, r *http.Request, id string) {
	code, body := s.delegate(r, s.handleWakeJSON, http.MethodPost, "/wake/"+id,
		map[string]string{"id": id}, nil)
	if code != http.StatusOK {
		relayV1Error(w, code, body) // 503 -> sandbox_capacity, etc.
		return
	}
	sb, err := s.Store.Get(r.Context(), id)
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.auditAction(r, audit.Entry{Action: "sandbox.start", Target: id})
	s.recordEvent(r, events.Event{Type: events.SandboxStarted, Severity: events.SeverityInfo,
		Message: "Sandbox started", SandboxID: id})
	writeJSON(w, http.StatusOK, s.v1SandboxFromRow(r, sb))
}

// --- POST /v1/sandboxes/{id}/restart --------------------------------

// v1RestartSandbox stops (if running) then starts the sandbox.
func (s *Server) v1RestartSandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sb, ok := s.v1Lookup(w, r, id)
	if !ok {
		return
	}
	switch sb.Status {
	case "running":
		if !s.stopSandbox(w, r, id) {
			return
		}
	case "stopped":
	default:
		writeV1Err(w, http.StatusConflict, "conflict", "sandbox is "+sb.Status+" — cannot restart")
		return
	}
	s.startAndRespond(w, r, id)
}

// --- POST /v1/sandboxes/{id}/exec -----------------------------------

type v1ExecReq struct {
	Cmd []string `json:"cmd"`
}

// v1ExecSandbox runs a one-shot command in a running sandbox and returns
// stdout, stderr and the exit code. A non-zero exit code is still a 200.
func (s *Server) v1ExecSandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sb, ok := s.v1Lookup(w, r, id)
	if !ok {
		return
	}
	if sb.Status != "running" {
		writeV1Err(w, http.StatusConflict, "conflict", "sandbox is "+sb.Status+" — start it first")
		return
	}
	var req v1ExecReq
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "invalid json: "+err.Error())
		return
	}
	if len(req.Cmd) == 0 || req.Cmd[0] == "" {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "cmd is required")
		return
	}
	body, _ := json.Marshal(execReq{Cmd: req.Cmd})
	code, out := s.delegate(r, s.handleExec, http.MethodPost, "/sandbox/"+id+"/exec",
		map[string]string{"id": id}, body)
	if code != http.StatusOK {
		relayV1Error(w, code, out)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// --- GET /v1/sandboxes/{id}/logs ------------------------------------

const (
	defaultLogTail = 200
	maxLogTail     = 10000
)

// v1SandboxLogs returns the container's stdout+stderr. ?tail=N limits the
// backlog (default 200, max 10000). ?follow=true streams new lines as
// Server-Sent Events (one `data:` line per log line) until the client
// disconnects; otherwise the response is text/plain.
func (s *Server) v1SandboxLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, ok := s.v1Lookup(w, r, id); !ok {
		return
	}
	tail := defaultLogTail
	if v := r.URL.Query().Get("tail"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeV1Err(w, http.StatusBadRequest, "invalid_request", "tail must be a positive integer")
			return
		}
		tail = min(n, maxLogTail)
	}
	follow := r.URL.Query().Get("follow") == "true"
	rc, err := s.Docker.Logs(r.Context(), "s-"+id, tail, follow)
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", "docker logs: "+err.Error())
		return
	}
	defer rc.Close()
	if !follow {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(rc)
		_, _ = w.Write(buf.Bytes())
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		if _, err := w.Write([]byte("data: " + sc.Text() + "\n\n")); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
	_, _ = w.Write([]byte("event: end\ndata: {}\n\n"))
	if flusher != nil {
		flusher.Flush()
	}
}

// --- DELETE /v1/sandboxes/{id} --------------------------------------

// v1DeleteSandbox is a full destroy. It delegates to the internal
// purge (container + workspace + row), not the internal soft DELETE —
// the soft DELETE preserves the workspace for id-reuse.
func (s *Server) v1DeleteSandbox(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	code, body := s.delegate(r, s.handlePurgeSandbox, http.MethodPost, "/sandbox/"+id+"/purge",
		map[string]string{"id": id}, nil)
	if code >= 200 && code < 300 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	relayV1Error(w, code, body)
}
