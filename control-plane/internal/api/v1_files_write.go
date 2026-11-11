package api

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"encoding/json"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/audit"
)

// Workspace file mutations (AGENT_PLAN A0): PUT /files/content,
// DELETE /files, POST /files/move. Paths are relative to the workspace
// root (container path /home/sandbox) and the body is opaque bytes.
//
// Security model (the caller is an external API key, not a trusted
// backend — a stricter model than upstream sandboxd's):
//   - Path is Clean-normalised; absolute paths, `..`, NUL bytes and
//     trailing slashes are rejected; the resolved path MUST stay under
//     the mount root via prefix check (resolveWritePath).
//   - Reserved subtrees (.ssh, lost+found) are refused for every mutation.
//   - The atomic write uses a temp file + rename: O_NOFOLLOW semantics
//     come from CreateTemp, and a symlink LEAF is refused before the
//     rename (rename would only replace the link itself, but refusal
//     keeps the API honest).
//   - Every INTERMEDIATE component is Lstat-verified as a real directory
//     right before the rename (noSymlinkComponents): the in-sandbox tenant
//     can plant `mkdir a; ln -s /etc a/b`-style escapes that a purely
//     lexical check would happily traverse. Residual TOCTOU between the
//     check and the rename is accepted (mirrors upstream's EvalSymlinks
//     approach for reads).
//   - Written files are chmod 0644 — no host-side exec bits; the tenant
//     can chmod inside the sandbox via exec. Owner is chown'd to the
//     workspace owner (the sandbox user), so the agent sees its own files.

const (
	// maxPutFileBytes — per-request write body cap.
	maxPutFileBytes = 25 << 20
)

// resolveWritePath validates a caller-supplied relative path against the
// workspace mount root and returns the absolute on-disk path plus the
// cleaned relative form (for audit + responses).
//
// Rules (in order):
//  1. path is non-empty and contains no NUL byte.
//  2. path is not absolute.
//  3. After Clean, no segment is `..` (would escape).
//  4. The cleaned path does not target a reserved subtree.
//  5. The resolved on-disk path stays under <mnt>/.
func resolveWritePath(mnt, raw string) (string, string, error) {
	if raw == "" {
		return "", "", errors.New("path is required")
	}
	if strings.ContainsRune(raw, 0) {
		return "", "", errors.New("invalid path: NUL byte")
	}
	if filepath.IsAbs(raw) {
		return "", "", errors.New("path must be relative to the workspace root")
	}
	// Trailing slash signals directory intent — reject before Clean,
	// which would strip it.
	if strings.HasSuffix(raw, "/") {
		return "", "", errors.New("path must not end with /")
	}
	clean := filepath.Clean(raw)
	// filepath.Clean reduces "a/../b" to "b" but leaves a leading ".." in place.
	for _, seg := range strings.Split(clean, string(os.PathSeparator)) {
		if seg == ".." {
			return "", "", errors.New("path traversal (..) not allowed")
		}
	}
	if clean == "." || clean == "/" {
		return "", "", errors.New("path must name a file, not the root")
	}
	if reservedPath(clean) {
		return "", "", errors.New("path is in a reserved subtree")
	}
	full := filepath.Join(mnt, clean)
	// Defence-in-depth: re-check the final prefix after Join.
	if full != mnt && !strings.HasPrefix(full, mnt+string(os.PathSeparator)) {
		return "", "", errors.New("resolved path escapes the workspace mount")
	}
	return full, clean, nil
}

// noSymlinkComponents verifies every EXISTING path component from root down
// to full is a real entry, not a symlink; missing components pass (they are
// created by MkdirAll or reported by the caller's own existence check). On
// the host side of the bind mount a tenant-planted symlink can point
// anywhere in the host namespace, so a mutation must never traverse one
// (CWE-59).
func noSymlinkComponents(root, full string) bool {
	rel, err := filepath.Rel(root, full)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return false
	}
	cur := root
	for _, seg := range strings.Split(rel, string(os.PathSeparator)) {
		cur = filepath.Join(cur, seg)
		fi, err := os.Lstat(cur)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return true
			}
			return false
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return false
		}
	}
	return true
}

// mountOwner returns the uid/gid that owns the workspace mount root — the
// sandbox user as seen on the host. Falls back to -1 so callers can skip
// chown gracefully.
func mountOwner(mnt string) (uid, gid int) {
	fi, err := os.Stat(mnt)
	if err != nil {
		return -1, -1
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return -1, -1
	}
	return int(st.Uid), int(st.Gid)
}

// chownNewParents walks from parent up to mnt and fixes ownership of the
// directories this write created, so the agent can read its own tree.
func chownNewParents(mnt, parent string, uid, gid int) {
	if uid < 0 {
		return
	}
	for p := parent; p != mnt && strings.HasPrefix(p, mnt+string(os.PathSeparator)); p = filepath.Dir(p) {
		// Lstat + Lchown: never follow a symlink component — os.Chown on
		// a tenant-planted link would chown the HOST target.
		if fi, err := os.Lstat(p); err == nil {
			if fi.Mode()&os.ModeSymlink != 0 {
				continue
			}
			if st, ok := fi.Sys().(*syscall.Stat_t); ok && (int(st.Uid) != uid || int(st.Gid) != gid) {
				_ = os.Lchown(p, uid, gid)
			}
		}
	}
}

// fileAudit records a workspace mutation: path only, never content.
func (s *Server) fileAudit(r *http.Request, id, action string, detail map[string]any) {
	extUID := ""
	if sb, err := s.Store.Get(r.Context(), id); err == nil && sb.ExternalUserID.Valid {
		extUID = sb.ExternalUserID.String
	}
	detail["via"] = "files-api"
	s.auditAction(r, audit.Entry{
		Action: action, Target: id, ExternalUserID: extUID, Detail: detail,
	})
}

// --- PUT /v1/sandboxes/{id}/files/content ----------------------------

func (s *Server) v1PutFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	mnt, ok := s.workspaceDirFor(w, r)
	if !ok {
		return
	}
	full, rel, err := resolveWritePath(mnt, r.URL.Query().Get("path"))
	if err != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	// Hard size gate before reading the body.
	if r.ContentLength > maxPutFileBytes {
		writeV1Err(w, http.StatusRequestEntityTooLarge, "invalid_request",
			"file exceeds the 25 MiB limit")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxPutFileBytes)

	// Refuse a symlinked component BEFORE anything touches the path: the
	// if_match read, MkdirAll and CreateTemp below all follow symlinks, so
	// a tenant-planted `a -> /etc` would otherwise let `a/x/y` create host
	// directories and temp files outside the workspace (CWE-59).
	if !noSymlinkComponents(mnt, full) {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "path traverses a symlink")
		return
	}

	// Optional optimistic-concurrency gate: if_match carries the sha256 of
	// the content the caller last saw. A missing file or a different hash
	// refuses the write (409) instead of silently clobbering.
	if want := r.URL.Query().Get("if_match"); want != "" {
		cur, err := os.ReadFile(full)
		if err != nil {
			writeV1Err(w, http.StatusConflict, "conflict",
				"if_match set but the file does not exist")
			return
		}
		sum := sha256.Sum256(cur)
		if hex.EncodeToString(sum[:]) != strings.ToLower(want) {
			writeV1Err(w, http.StatusConflict, "conflict",
				"if_match does not match the current file content")
			return
		}
	}

	uid, gid := mountOwner(mnt)

	// Create parent dirs so the write can land in a fresh subtree; they are
	// chown'd back to the workspace owner below, after the rename.
	parent := filepath.Dir(full)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	// Atomic write: tmp in the same dir + rename, content hashed as it
	// streams (never re-read, never buffered twice).
	hasher := sha256.New()
	tmp, err := os.CreateTemp(parent, ".put-*.tmp")
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	tmpPath := tmp.Name()
	written, copyErr := io.Copy(tmp, io.TeeReader(r.Body, hasher))
	closeErr := tmp.Close()
	if copyErr != nil {
		_ = os.Remove(tmpPath)
		var mbe *http.MaxBytesError
		if errors.As(copyErr, &mbe) {
			writeV1Err(w, http.StatusRequestEntityTooLarge, "invalid_request",
				"file exceeds the 25 MiB limit")
			return
		}
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "read body: "+copyErr.Error())
		return
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		writeV1Err(w, http.StatusInternalServerError, "internal", "close tmp: "+closeErr.Error())
		return
	}

	// Intermediate-symlink check AFTER the parents exist and BEFORE the
	// rename — the tightest practical window.
	if !noSymlinkComponents(mnt, full) {
		_ = os.Remove(tmpPath)
		writeV1Err(w, http.StatusBadRequest, "invalid_request",
			"path traverses a symlink")
		return
	}
	// Refuse to overwrite if the existing leaf is a symlink — a symlinked
	// target could redirect the write off the mount.
	if fi, err := os.Lstat(full); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		_ = os.Remove(tmpPath)
		writeV1Err(w, http.StatusBadRequest, "invalid_request",
			"refusing to overwrite a symlink")
		return
	}

	// Set ownership BEFORE rename so the file is never visible at the
	// target path with wrong owner.
	if uid >= 0 {
		_ = os.Chown(tmpPath, uid, gid)
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		_ = os.Remove(tmpPath)
		writeV1Err(w, http.StatusInternalServerError, "internal", "chmod: "+err.Error())
		return
	}
	chownNewParents(mnt, parent, uid, gid)

	if err := os.Rename(tmpPath, full); err != nil {
		_ = os.Remove(tmpPath)
		writeV1Err(w, http.StatusInternalServerError, "internal", "rename: "+err.Error())
		return
	}

	s.fileAudit(r, id, "sandbox.file.write", map[string]any{
		"path": rel, "size": written,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"path": rel, "size": written, "sha256": hex.EncodeToString(hasher.Sum(nil)),
	})
}

// --- DELETE /v1/sandboxes/{id}/files ---------------------------------

func (s *Server) v1DeleteFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	mnt, ok := s.workspaceDirFor(w, r)
	if !ok {
		return
	}
	full, rel, err := resolveWritePath(mnt, r.URL.Query().Get("path"))
	if err != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if !noSymlinkComponents(mnt, full) {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "path traverses a symlink")
		return
	}
	fi, err := os.Lstat(full)
	if err != nil {
		writeV1Err(w, http.StatusNotFound, "not_found", "no such file")
		return
	}
	if fi.IsDir() {
		// os.Remove only succeeds on an EMPTY directory — a non-empty one
		// fails with ENOTEMPTY and is reported as a conflict.
		if err := os.Remove(full); err != nil {
			writeV1Err(w, http.StatusConflict, "conflict",
				"directory not empty — delete its contents first")
			return
		}
	} else if err := os.Remove(full); err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.fileAudit(r, id, "sandbox.file.delete", map[string]any{"path": rel})
	w.WriteHeader(http.StatusNoContent)
}

// --- POST /v1/sandboxes/{id}/files/move ------------------------------

type v1FileMoveReq struct {
	From string `json:"from"`
	To   string `json:"to"`
}

func (s *Server) v1MoveFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	mnt, ok := s.workspaceDirFor(w, r)
	if !ok {
		return
	}
	var req v1FileMoveReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "invalid json: "+err.Error())
		return
	}
	src, fromRel, err := resolveWritePath(mnt, req.From)
	if err != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "from: "+err.Error())
		return
	}
	dst, toRel, err := resolveWritePath(mnt, req.To)
	if err != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "to: "+err.Error())
		return
	}
	if fromRel == toRel {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "from and to are the same path")
		return
	}
	fi, err := os.Lstat(src)
	if err != nil {
		writeV1Err(w, http.StatusNotFound, "not_found", "no such file")
		return
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "refusing to move a symlink")
		return
	}
	// Verify both paths BEFORE MkdirAll/chown: both follow symlinks, so a
	// planted `a -> /etc` with to="a/x" would create host directories and
	// chown them (and /etc itself) to the sandbox user (CWE-59).
	if !noSymlinkComponents(mnt, src) || !noSymlinkComponents(mnt, dst) {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "path traverses a symlink")
		return
	}
	// The destination's parent is created, then the full walk re-runs to
	// narrow the race with a concurrent in-sandbox symlink swap.
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if uid, gid := mountOwner(mnt); uid >= 0 {
		chownNewParents(mnt, filepath.Dir(dst), uid, gid)
	}
	if !noSymlinkComponents(mnt, src) || !noSymlinkComponents(mnt, dst) {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "path traverses a symlink")
		return
	}
	if _, err := os.Lstat(dst); err == nil {
		writeV1Err(w, http.StatusConflict, "conflict", "target already exists")
		return
	}
	if err := os.Rename(src, dst); err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", "rename: "+err.Error())
		return
	}
	s.fileAudit(r, id, "sandbox.file.move", map[string]any{"from": fromRel, "to": toRel})
	writeJSON(w, http.StatusOK, map[string]any{"from": fromRel, "to": toRel})
}
