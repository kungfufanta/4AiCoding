package api

import (
	"encoding/base64"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Workspace files API (AGENT_PLAN A0). Served by the control plane directly
// from the host-side workspace mount (container path /home/sandbox), so they
// work whether or not the sandbox is running. For sandboxes homed on a fleet
// executor worker, remoteRoute forwards these routes to the owning worker
// before the local handlers below run.
//
// Security model: the in-sandbox tenant owns the workspace and can plant
// symlinks pointing anywhere in the HOST namespace (a bind mount resolves
// symlink targets on the host, e.g. `ln -s ../../../etc/passwd x`). Every
// handler therefore (1) cleans + prefixes-checks the caller path lexically
// (safeJoin), (2) resolves every symlink component and re-checks containment
// (realpathWithin), and (3) never exposes or follows a symlink entry when
// listing. Writes additionally refuse symlink leaves and verify that no
// intermediate component is a symlink (see v1_files_write.go).

const (
	// maxFileReadBytes caps one /files/content request window (plan: ≤2 MiB
	// per read; larger files page via offset/limit).
	maxFileReadBytes = 2 << 20
	// filesDefaultEntries / filesMaxEntries bound one listing.
	filesDefaultEntries = 500
	filesMaxEntries     = 10000
)

// excludedFromFiles are never listed (generated dirs that drown recursive
// listings and inflate responses).
var excludedFromFiles = map[string]bool{
	"node_modules": true, ".git": true, ".venv": true, "dist": true, ".vite": true,
}

// reservedEntry reports whether a directory ENTRY named `name` is a
// reserved subtree that listings must never descend into or expose.
func reservedEntry(name string) bool {
	for _, p := range reservedFromFiles {
		if name == p {
			return true
		}
	}
	return false
}

// reservedFromFiles are subtrees the platform owns: never listed, read,
// written, deleted or moved. `.ssh` may hold the tenant's private key
// material — file access to it stays exec-only, inside the sandbox.
var reservedFromFiles = []string{".ssh", "lost+found"}

// workspaceDirFor returns the host-side workspace root for the sandbox in
// the path value (the row must exist — v1Lookup).
func (s *Server) workspaceDirFor(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	sb, ok := s.v1Lookup(w, r, id)
	if !ok {
		return "", false
	}
	mnt := sb.WorkspaceMnt
	if mnt == "" {
		writeV1Err(w, http.StatusNotFound, "not_found", "no workspace for that sandbox")
		return "", false
	}
	if info, err := os.Stat(mnt); err != nil || !info.IsDir() {
		writeV1Err(w, http.StatusNotFound, "not_found", "no workspace for that sandbox")
		return "", false
	}
	return mnt, true
}

// safeJoin resolves a caller-supplied path under root, rejecting any escape
// (`..`, absolute paths) LEXICALLY. Callers that then open the path must
// also pass it through realpathWithin — a lexical check alone follows
// symlinks planted in the workspace (CWE-59).
func safeJoin(root, p string) (string, bool) {
	full := filepath.Join(root, filepath.Clean("/"+p))
	if full != root && !strings.HasPrefix(full, root+string(os.PathSeparator)) {
		return "", false
	}
	return full, true
}

// realpathWithin canonicalizes full (resolving every symlink component —
// leaf AND intermediate) and confirms the result is still inside root. It
// closes the symlink-following read hole: the in-sandbox tenant owns the
// workspace and can plant `ln -s /proc/self/environ leak`; a lexical guard
// passes it and os.Stat/ReadFile then follow the link into the root-owned
// control-plane filesystem. ok=false on any escape, nonexistent path, or
// broken link. The returned path is symlink-free and provably under root,
// so a subsequent os.Open/Stat cannot be redirected out of the workspace.
func realpathWithin(full, root string) (string, bool) {
	real, err := filepath.EvalSymlinks(full)
	if err != nil {
		return "", false
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", false
	}
	if real != realRoot && !strings.HasPrefix(real, realRoot+string(os.PathSeparator)) {
		return "", false
	}
	return real, true
}

// reservedPath reports whether the cleaned (slash-separated, relative)
// workspace path targets a reserved subtree.
func reservedPath(clean string) bool {
	for _, p := range reservedFromFiles {
		if clean == p || strings.HasPrefix(clean, p+"/") {
			return true
		}
	}
	return false
}

// fileEntry is one listing row; Path is relative to the workspace root.
type fileEntry struct {
	Path string `json:"path"` // relative to /home/sandbox
	Type string `json:"type"` // "file" | "dir"
	Size int64  `json:"size,omitempty"`
}

// --- GET /v1/sandboxes/{id}/files ------------------------------------

func (s *Server) v1ListFiles(w http.ResponseWriter, r *http.Request) {
	root, ok := s.workspaceDirFor(w, r)
	if !ok {
		return
	}
	p := r.URL.Query().Get("path")
	recursive := r.URL.Query().Get("recursive") == "true"
	maxEntries := filesDefaultEntries
	if v := r.URL.Query().Get("max_entries"); v != "" {
		n, ok := atoiNonNeg(v)
		if !ok {
			writeV1Err(w, http.StatusBadRequest, "invalid_request", "max_entries must be a positive integer")
			return
		}
		maxEntries = min(n, filesMaxEntries)
	}
	clean := filepath.Clean("/" + p) // "/a/b" form for the reserved check
	if reservedPath(strings.TrimPrefix(clean, "/")) {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "path is a reserved subtree")
		return
	}
	dir, ok := safeJoin(root, p)
	if !ok {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "invalid path")
		return
	}
	// Resolve symlinks and re-check containment so a symlinked `path` dir
	// can't redirect the listing outside the workspace (CWE-59).
	dir, ok = realpathWithin(dir, root)
	if !ok {
		writeV1Err(w, http.StatusNotFound, "not_found", "no such directory")
		return
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		writeV1Err(w, http.StatusNotFound, "not_found", "no such directory")
		return
	}

	var entries []fileEntry
	truncated := false
	add := func(path string, d fs.DirEntry) {
		if len(entries) >= maxEntries {
			truncated = true
			return
		}
		rel, _ := filepath.Rel(root, path)
		e := fileEntry{Path: rel, Type: "file"}
		if d.IsDir() {
			e.Type = "dir"
		} else if fi, err := d.Info(); err == nil {
			e.Size = fi.Size()
		}
		entries = append(entries, e)
	}
	if recursive {
		_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil || path == dir {
				return nil
			}
			if d.Type()&fs.ModeSymlink != 0 {
				return nil // never expose or follow a symlink
			}
			if excludedFromFiles[d.Name()] || reservedEntry(d.Name()) {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			add(path, d)
			if truncated {
				return fs.SkipAll
			}
			return nil
		})
	} else {
		ents, _ := os.ReadDir(dir)
		for _, d := range ents {
			if d.Type()&fs.ModeSymlink != 0 || excludedFromFiles[d.Name()] || reservedEntry(d.Name()) {
				continue
			}
			add(filepath.Join(dir, d.Name()), d)
			if truncated {
				break
			}
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	writeJSON(w, http.StatusOK, map[string]any{
		"path": p, "recursive": recursive, "entries": entries, "truncated": truncated,
	})
}

// --- GET /v1/sandboxes/{id}/files/content ----------------------------

// atoiNonNeg parses a decimal non-negative integer from a query param.
func atoiNonNeg(v string) (int, bool) {
	n, err := strconv.ParseUint(v, 10, 31)
	if err != nil {
		return 0, false
	}
	return int(n), true
}

// looksBinary applies the classic heuristic: a NUL byte anywhere, or
// invalid UTF-8, makes the window binary (returned as base64).
func looksBinary(b []byte) bool {
	if !utf8.Valid(b) {
		return true
	}
	return strings.ContainsRune(string(b), 0)
}

func (s *Server) v1FileContent(w http.ResponseWriter, r *http.Request) {
	root, ok := s.workspaceDirFor(w, r)
	if !ok {
		return
	}
	p := r.URL.Query().Get("path")
	if p == "" {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "path is required")
		return
	}
	if reservedPath(filepath.Clean(strings.TrimPrefix(filepath.Clean("/"+p), "/"))) {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "path is a reserved subtree")
		return
	}
	full, ok := safeJoin(root, p)
	if !ok || full == root {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "invalid path")
		return
	}
	// Resolve symlinks and re-check containment BEFORE stat/read, so a
	// symlink (leaf or intermediate) can't redirect the read out of the
	// workspace into root-owned control-plane files (CWE-59).
	full, ok = realpathWithin(full, root)
	if !ok {
		writeV1Err(w, http.StatusNotFound, "not_found", "no such file")
		return
	}
	info, err := os.Stat(full)
	if err != nil || info.IsDir() {
		writeV1Err(w, http.StatusNotFound, "not_found", "no such file")
		return
	}

	offset := int64(0)
	if v := r.URL.Query().Get("offset"); v != "" {
		n, ok := atoiNonNeg(v)
		if !ok {
			writeV1Err(w, http.StatusBadRequest, "invalid_request", "offset must be a non-negative integer")
			return
		}
		offset = int64(n)
	}
	limit := int64(0) // 0 = the whole file (up to the 2 MiB request cap)
	if v := r.URL.Query().Get("limit"); v != "" {
		n, ok := atoiNonNeg(v)
		if !ok {
			writeV1Err(w, http.StatusBadRequest, "invalid_request", "limit must be a non-negative integer")
			return
		}
		limit = int64(n)
		if limit > maxFileReadBytes {
			writeV1Err(w, http.StatusBadRequest, "invalid_request",
				"limit exceeds the 2 MiB per-request read cap")
			return
		}
	}
	if limit == 0 && info.Size() > maxFileReadBytes {
		writeV1Err(w, http.StatusBadRequest, "invalid_request",
			"file exceeds the 2 MiB read cap — page through it with offset/limit")
		return
	}
	if offset > info.Size() {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "offset is past the end of the file")
		return
	}
	window := info.Size() - offset
	if limit > 0 && limit < window {
		window = limit
	}
	data := make([]byte, window)
	if window > 0 {
		f, err := os.Open(full)
		if err != nil {
			writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
		defer f.Close()
		if _, err := f.ReadAt(data, offset); err != nil && !errors.Is(err, io.EOF) {
			writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
			return
		}
	}
	enc := "utf-8"
	out := string(data)
	if looksBinary(data) {
		enc = "base64"
		out = base64.StdEncoding.EncodeToString(data)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path":      p,
		"size":      info.Size(),
		"offset":    offset,
		"encoding":  enc,
		"data":      out,
		"truncated": offset+window < info.Size(),
	})
}
