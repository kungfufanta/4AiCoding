// Package sandboxtest is a scripted sandboxd fake for tests, the same
// role llmtest plays for the LLM client (A3.2). It serves the subset
// of the sandboxd v1 surface the agent loop and the tool registry
// touch — lifecycle read/start/keep-alive, exec v2 and the files API —
// against a plain directory on the test host:
//
//	/v1/sandboxes/{id}            GET (running, ports [3000]), POST /start
//	/v1/sandboxes/{id}/keep-alive POST/DELETE
//	/v1/sandboxes/{id}/exec       POST (commands run for real, cwd mapped
//	                               into the fake's root)
//	/v1/sandboxes/{id}/files*     GET list / GET+PUT content
//
// exec runs the commands with os/exec against the mapped cwd — the
// commands come from the loop code itself (probe, checkpoint, build),
// so tests exercise the real scripts end to end. Paths that escape
// the root are refused, mirroring the real files API.
package sandboxtest

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/tastyeffectco/sandboxd/portal/internal/sandboxd"
)

// Sandbox ids the fake knows by default.
const DefaultID = "SBXTEST"

// Server is the fake sandboxd. Requests are recorded so tests can
// assert what the loop really sent.
type Server struct {
	*httptest.Server
	mu    sync.Mutex
	roots map[string]string // sandbox id → filesystem root
	Execs []ExecRecord
	// Hook, when set, may answer an exec call itself (return handled
	// = true): tests use it to simulate a missing tool or to inject
	// environment the loop must never leak. Set BEFORE the worker
	// starts; read without a lock by design.
	Hook func(rec ExecRecord) (response map[string]any, handled bool)
}

// ExecRecord is one exec call the fake received.
type ExecRecord struct {
	ID  string
	Cmd []string
	Cwd string
	Env map[string]string
}

// New starts a fake with one sandbox (id SBXTEST) whose filesystem is
// the given directory. Extra sandboxes: Add.
func New(root string) *Server {
	s := &Server{roots: map[string]string{DefaultID: root}}
	s.Server = httptest.NewServer(s.handler())
	return s
}

// Add registers another sandbox id with its own root. The sandbox
// always answers "running" — a stopped-sandbox test simply starts
// from a stopped state by seeding the DB row differently.
func (s *Server) Add(id, root string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.roots[id] = root
}

// Root returns the filesystem root of a sandbox id.
func (s *Server) Root(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.roots[id]
}

// Client returns a sandboxd client pointed at the fake.
func (s *Server) Client() *sandboxd.Client {
	return sandboxd.New(s.URL, "", 30*time.Second)
}

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/sandboxes/", s.sandbox)
	return mux
}

func (s *Server) sandbox(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/sandboxes/")
	id, action, _ := strings.Cut(rest, "/")
	s.mu.Lock()
	root, ok := s.roots[id]
	if !ok {
		// One shared root for unknown ids: tests seed random sandbox
		// ids; they all map onto the fake's default filesystem.
		root = s.roots[DefaultID]
		ok = root != ""
	}
	s.mu.Unlock()
	if !ok {
		writeErr(w, 404, "not_found", "no such sandbox")
		return
	}
	switch {
	case action == "" && r.Method == http.MethodGet:
		writeJSON(w, map[string]any{
			"id": id, "status": "running", "image": "ubuntu:24.04",
			"resources": map[string]any{"cpus": 2, "memory_mb": 2048},
			"lifecycle": map[string]any{"idle_timeout_secs": 3600, "keep_alive": false},
			"egress":    []string{}, "ports": []int{3000},
			"visibility": "private", "idle_policy": "normal",
			"created_at": "2026-01-01T00:00:00Z",
		})
	case action == "start" && r.Method == http.MethodPost:
		writeJSON(w, map[string]any{"id": id, "status": "running"})
	case action == "keep-alive" && (r.Method == http.MethodPost || r.Method == http.MethodDelete):
		writeJSON(w, map[string]any{"id": id, "keep_alive": r.Method == http.MethodPost})
	case action == "exec" && r.Method == http.MethodPost:
		s.exec(w, r, id, root)
	case action == "files" && r.Method == http.MethodGet:
		s.listFiles(w, r, root)
	case action == "files/content" && r.Method == http.MethodGet:
		s.readFile(w, r, root)
	case action == "files/content" && r.Method == http.MethodPut:
		s.writeFile(w, r, root)
	default:
		writeErr(w, 400, "invalid_request", "unsupported fake route "+r.Method+" "+r.URL.Path)
	}
}

// execInput mirrors sandboxd's exec v2 body.
type execInput struct {
	Cmd            []string          `json:"cmd"`
	Cwd            string            `json:"cwd"`
	Env            map[string]string `json:"env"`
	Stdin          *string           `json:"stdin"`
	TimeoutS       int               `json:"timeout_s"`
	MaxOutputBytes int64             `json:"max_output_bytes"`
}

func (s *Server) exec(w http.ResponseWriter, r *http.Request, id, root string) {
	var in execInput
	body, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(body, &in); err != nil || len(in.Cmd) == 0 {
		writeErr(w, 400, "invalid_request", "bad exec body")
		return
	}
	cwd := in.Cwd
	if cwd == "" {
		cwd = "/home/sandbox"
	}
	local, ok := mapIn(root, cwd)
	if !ok {
		writeErr(w, 400, "invalid_request", "cwd escapes the sandbox")
		return
	}
	rec := ExecRecord{ID: id, Cmd: in.Cmd, Cwd: in.Cwd, Env: in.Env}
	s.mu.Lock()
	s.Execs = append(s.Execs, rec)
	s.mu.Unlock()
	if s.Hook != nil {
		if resp, handled := s.Hook(rec); handled {
			writeJSON(w, resp)
			return
		}
	}

	ctx := context.Background()
	cancel := func() {}
	if in.TimeoutS > 0 {
		ctx, cancel = context.WithTimeout(ctx, time.Duration(in.TimeoutS)*time.Second)
	}
	defer cancel()

	cmd := exec.CommandContext(ctx, in.Cmd[0], in.Cmd[1:]...)
	cmd.Dir = local
	if len(in.Env) > 0 {
		env := os.Environ()
		for k, v := range in.Env {
			env = append(env, k+"="+v)
		}
		cmd.Env = env
	}
	if in.Stdin != nil {
		cmd.Stdin = strings.NewReader(*in.Stdin)
	}
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	timedOut := false
	if runErr != nil && ctx.Err() == context.DeadlineExceeded {
		timedOut = true
	}
	exit := 0
	if runErr != nil {
		if ee, is := runErr.(*exec.ExitError); is {
			exit = ee.ExitCode()
		} else {
			exit = 127
			stderr.WriteString(runErr.Error())
		}
	}
	stdoutS := stdout.String()
	stderrS := stderr.String()
	cap := in.MaxOutputBytes
	if cap <= 0 {
		cap = 256 << 10
	}
	truncated := false
	if int64(len(stdoutS)+len(stderrS)) > cap {
		keep := int(cap)
		if len(stdoutS) > keep {
			stdoutS = stdoutS[:keep]
		} else {
			stderrS = stderrS[:max(0, keep-len(stdoutS))]
		}
		truncated = true
	}
	writeJSON(w, map[string]any{
		"stdout": stdoutS, "stderr": stderrS, "exit_code": exit,
		"timed_out": timedOut, "truncated": truncated, "duration_ms": 1,
	})
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// mapIn translates an in-container /home/sandbox/... path into the
// fake root, refusing escapes.
func mapIn(root, p string) (string, bool) {
	rel := strings.TrimPrefix(path.Clean(p), "/home/sandbox")
	rel = strings.TrimPrefix(rel, "/")
	if rel == "" {
		return root, true
	}
	full := filepath.Join(root, filepath.FromSlash(rel))
	if !strings.HasPrefix(full, filepath.Clean(root)+string(filepath.Separator)) &&
		full != filepath.Clean(root) {
		return "", false
	}
	return full, true
}

func (s *Server) listFiles(w http.ResponseWriter, r *http.Request, root string) {
	q := r.URL.Query()
	local, ok := mapIn(root, q.Get("path"))
	if !ok {
		writeErr(w, 400, "invalid_request", "invalid path")
		return
	}
	info, err := os.Stat(local)
	if err != nil || !info.IsDir() {
		writeErr(w, 404, "not_found", "no such directory")
		return
	}
	maxEntries := 500
	if v := q.Get("max_entries"); v != "" {
		fmt.Sscanf(v, "%d", &maxEntries)
	}
	excluded := map[string]bool{"node_modules": true, ".git": true, ".venv": true, "dist": true, ".vite": true}
	var entries []map[string]any
	truncated := false
	base := strings.TrimPrefix(q.Get("path"), "/home/sandbox/")
	base = strings.TrimPrefix(base, "/")
	var walk func(dir, rel string) bool
	walk = func(dir, rel string) bool {
		dents, err := os.ReadDir(dir)
		if err != nil {
			return true
		}
		sort.Slice(dents, func(i, j int) bool { return dents[i].Name() < dents[j].Name() })
		for _, d := range dents {
			if excluded[d.Name()] {
				continue
			}
			if len(entries) >= maxEntries {
				truncated = true
				return false
			}
			rp := d.Name()
			if rel != "" {
				rp = rel + "/" + d.Name()
			}
			if d.IsDir() {
				entries = append(entries, map[string]any{"path": rp, "type": "dir"})
				if q.Get("recursive") == "true" && !walk(filepath.Join(dir, d.Name()), rp) {
					return false
				}
			} else {
				size := int64(0)
				if fi, err := d.Info(); err == nil {
					size = fi.Size()
				}
				entries = append(entries, map[string]any{"path": rp, "type": "file", "size": size})
			}
		}
		return true
	}
	walk(local, base)
	if entries == nil {
		entries = []map[string]any{}
	}
	writeJSON(w, map[string]any{"path": q.Get("path"), "recursive": q.Get("recursive") == "true",
		"entries": entries, "truncated": truncated})
}

func (s *Server) readFile(w http.ResponseWriter, r *http.Request, root string) {
	local, ok := mapIn(root, r.URL.Query().Get("path"))
	if !ok {
		writeErr(w, 400, "invalid_request", "invalid path")
		return
	}
	data, err := os.ReadFile(local)
	if err != nil {
		writeErr(w, 404, "not_found", "no such file")
		return
	}
	enc := "utf-8"
	out := string(data)
	if strings.ContainsRune(out, 0) || !utf8.Valid(data) {
		enc = "base64"
		out = base64.StdEncoding.EncodeToString(data)
	}
	writeJSON(w, map[string]any{"path": r.URL.Query().Get("path"), "size": len(data),
		"offset": 0, "encoding": enc, "data": out, "truncated": false})
}

func (s *Server) writeFile(w http.ResponseWriter, r *http.Request, root string) {
	local, ok := mapIn(root, r.URL.Query().Get("path"))
	if !ok {
		writeErr(w, 400, "invalid_request", "invalid path")
		return
	}
	if want := r.URL.Query().Get("if_match"); want != "" {
		if old, err := os.ReadFile(local); err == nil && sha256Hex(old) != want {
			writeErr(w, 409, "if_match_mismatch", "if_match does not match the current file content")
			return
		}
	}
	body, _ := io.ReadAll(r.Body)
	if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
		writeErr(w, 500, "internal", err.Error())
		return
	}
	if err := os.WriteFile(local, body, 0o644); err != nil {
		writeErr(w, 500, "internal", err.Error())
		return
	}
	writeJSON(w, map[string]any{"path": r.URL.Query().Get("path"), "size": len(body),
		"sha256": sha256Hex(body)})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	b, _ := json.Marshal(v)
	_, _ = w.Write(b)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"code": code, "message": msg}})
	_, _ = w.Write(b)
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
