package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveWritePath pins the lexical rules of the write-path guard.
// Ported from upstream v1_files_write_test.go, with our reserved set.
func TestResolveWritePath(t *testing.T) {
	const mnt = "/var/lib/sandboxd/workspaces/01XYZ.mnt"

	cases := []struct {
		name     string
		raw      string
		wantErr  string // substring; "" means must succeed
		wantFull string // ignored if wantErr != ""
		wantRel  string
	}{
		// happy paths
		{"basic file", "AGENTS.md", "", mnt + "/AGENTS.md", "AGENTS.md"},
		{"nested file", "workspace/app/AGENTS.md", "", mnt + "/workspace/app/AGENTS.md", "workspace/app/AGENTS.md"},
		{"dotfile under home", ".config/opencode/opencode.json", "", mnt + "/.config/opencode/opencode.json", ".config/opencode/opencode.json"},
		{"trims a leading ./", "./AGENTS.md", "", mnt + "/AGENTS.md", "AGENTS.md"},
		{"clean-able redundancy", "workspace/./app/AGENTS.md", "", mnt + "/workspace/app/AGENTS.md", "workspace/app/AGENTS.md"},

		// rejections — caller errors
		{"empty", "", "required", "", ""},
		{"NUL byte", "AGENTS\x00.md", "NUL", "", ""},
		{"absolute path", "/etc/passwd", "relative", "", ""},
		{"absolute under mount", mnt + "/AGENTS.md", "relative", "", ""},
		{"traversal segment", "../etc/passwd", "traversal", "", ""},
		{"traversal mid-path", "workspace/../../etc/passwd", "traversal", "", ""},
		{"root", ".", "must name a file", "", ""},
		{"slash only", "/", "relative", "", ""},
		{"trailing slash (dir)", "workspace/", "must not end with /", "", ""},

		// reserved subtrees — platform-owned
		{"reserved .ssh", ".ssh/id_ed25519", "reserved", "", ""},
		{"reserved .ssh exact", ".ssh", "reserved", "", ""},
		{"reserved lost+found", "lost+found/something", "reserved", "", ""},

		// the .config dir itself is NOT reserved — agents need it
		{"under .config OK", ".config/claude/config.json", "", mnt + "/.config/claude/config.json", ".config/claude/config.json"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			full, rel, err := resolveWritePath(mnt, tc.raw)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil (full=%q)", tc.wantErr, full)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if full != tc.wantFull {
				t.Errorf("full = %q, want %q", full, tc.wantFull)
			}
			if rel != tc.wantRel {
				t.Errorf("rel = %q, want %q", rel, tc.wantRel)
			}
			// Invariant: the resolved path MUST be under the mount.
			if full != mnt && !strings.HasPrefix(full, mnt+"/") {
				t.Errorf("invariant violated: %q escapes %q", full, mnt)
			}
		})
	}
}

// Property check: no string passed as `raw` can resolve to a path that
// escapes the mount, no matter how mean.
func TestResolveWritePath_NeverEscapes(t *testing.T) {
	const mnt = "/var/lib/sandboxd/workspaces/01XYZ.mnt"
	probes := []string{
		"..", "../", "../..", "../../../etc/passwd",
		"workspace/../..",
		"workspace/../../../etc/passwd",
		"./..",
		"/..",
		"/etc/passwd",
		"workspace/AGENTS.md/..",
		"a/b/c/../../../..",
	}
	for _, p := range probes {
		full, _, err := resolveWritePath(mnt, p)
		if err == nil {
			if full != mnt && !strings.HasPrefix(full, mnt+"/") {
				t.Fatalf("escape via %q resolved to %q", p, full)
			}
			// resolved inside is fine; rejection is also fine
		}
	}
}

// filePutServer is a harness for the mutation handlers.
func filePutServer(t *testing.T) (*Server, string, string) { return fileRowServer(t) }

func filePut(t *testing.T, s *Server, id, query, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("PUT", "/v1/sandboxes/"+id+"/files/content"+query, strings.NewReader(body))
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	s.v1PutFile(w, r)
	return w
}

func fileDelete(t *testing.T, s *Server, id, query string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("DELETE", "/v1/sandboxes/"+id+"/files"+query, nil)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	s.v1DeleteFile(w, r)
	return w
}

func fileMove(t *testing.T, s *Server, id, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "/v1/sandboxes/"+id+"/files/move", strings.NewReader(body))
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	s.v1MoveFile(w, r)
	return w
}

func TestPutFileRoundTripSha256(t *testing.T) {
	s, id, mnt := filePutServer(t)
	content := strings.Repeat("portal-a0 \x00\x01 binary-ish ", 1000)
	w := filePut(t, s, id, "?path=sub%2Fdir%2Ffile.bin", content)
	if w.Code != 200 {
		t.Fatalf("put: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Path   string `json:"path"`
		Size   int64  `json:"size"`
		Sha256 string `json:"sha256"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Path != "sub/dir/file.bin" || resp.Size != int64(len(content)) {
		t.Errorf("resp = %+v", resp)
	}
	sum := sha256.Sum256([]byte(content))
	if resp.Sha256 != hex.EncodeToString(sum[:]) {
		t.Errorf("sha256 mismatch: %s", resp.Sha256)
	}
	// the file landed inside the workspace, readable, 0644
	got, err := os.ReadFile(filepath.Join(mnt, "sub", "dir", "file.bin"))
	if err != nil || string(got) != content {
		t.Fatalf("roundtrip read: %v", err)
	}
	fi, err := os.Stat(filepath.Join(mnt, "sub", "dir", "file.bin"))
	if err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("perm = %v, want 0644 (no host-side exec bits)", fi.Mode().Perm())
	}
}

func TestPutFileIfMatch(t *testing.T) {
	s, id, mnt := filePutServer(t)
	if err := os.WriteFile(filepath.Join(mnt, "f.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("v1"))
	good := hex.EncodeToString(sum[:])
	// wrong hash → 409
	if w := filePut(t, s, id, "?path=f.txt&if_match="+strings.Repeat("0", 64), "v2"); w.Code != http.StatusConflict {
		t.Errorf("stale if_match: got %d, want 409", w.Code)
	}
	// missing file + if_match → 409
	if w := filePut(t, s, id, "?path=none.txt&if_match="+good, "v2"); w.Code != http.StatusConflict {
		t.Errorf("if_match on missing file: got %d, want 409", w.Code)
	}
	// matching hash → 200, content replaced
	if w := filePut(t, s, id, "?path=f.txt&if_match="+good, "v2"); w.Code != 200 {
		t.Fatalf("matching if_match: %d %s", w.Code, w.Body.String())
	}
	got, _ := os.ReadFile(filepath.Join(mnt, "f.txt"))
	if string(got) != "v2" {
		t.Errorf("content = %q, want v2", got)
	}
}

func TestPutFileRefusesSymlinkEscapes(t *testing.T) {
	s, id, mnt := filePutServer(t)
	outside := t.TempDir()
	secret := filepath.Join(outside, "environ")
	if err := os.WriteFile(secret, []byte(secretMarker), 0o600); err != nil {
		t.Fatal(err)
	}
	// leaf symlink
	if err := os.Symlink(secret, filepath.Join(mnt, "leak")); err != nil {
		t.Fatal(err)
	}
	// intermediate symlink
	if err := os.Symlink(outside, filepath.Join(mnt, "escape")); err != nil {
		t.Fatal(err)
	}
	// leaf symlink overwrite → 400, target untouched
	if w := filePut(t, s, id, "?path=leak", "PWNED"); w.Code != 400 {
		t.Errorf("leaf symlink put: got %d, want 400", w.Code)
	}
	if got, _ := os.ReadFile(secret); string(got) != secretMarker {
		t.Fatalf("SECRET CLOBBERED via leaf symlink put")
	}
	// write THROUGH the intermediate symlink → 400, target dir untouched
	if w := filePut(t, s, id, "?path=escape%2Fenviron", "PWNED"); w.Code != 400 {
		t.Errorf("intermediate symlink put: got %d, want 400", w.Code)
	}
	if got, _ := os.ReadFile(secret); string(got) != secretMarker {
		t.Fatalf("SECRET CLOBBERED via intermediate symlink put")
	}
	// traversal → 400 (lexical guard)
	if w := filePut(t, s, id, "?path=..%2Fetc%2Fpasswd", "PWNED"); w.Code != 400 {
		t.Errorf("traversal put: got %d, want 400", w.Code)
	}
	// reserved → 400
	if w := filePut(t, s, id, "?path=.ssh%2Fauthorized_keys", "PWNED"); w.Code != 400 {
		t.Errorf("reserved put: got %d, want 400", w.Code)
	}
}

func TestDeleteFileRules(t *testing.T) {
	s, id, mnt := filePutServer(t)
	if err := os.WriteFile(filepath.Join(mnt, "gone.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(mnt, "d", "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mnt, "d", "inner", "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../outside-target", filepath.Join(mnt, "badlink")); err != nil {
		t.Fatal(err)
	}
	if w := fileDelete(t, s, id, "?path=gone.txt"); w.Code != http.StatusNoContent {
		t.Errorf("delete file: got %d, want 204", w.Code)
	}
	if _, err := os.Stat(filepath.Join(mnt, "gone.txt")); !os.IsNotExist(err) {
		t.Errorf("file still there")
	}
	// non-empty dir → 409
	if w := fileDelete(t, s, id, "?path=d"); w.Code != http.StatusConflict {
		t.Errorf("delete non-empty dir: got %d, want 409", w.Code)
	}
	// nested-then-empty: inner first (still has f.txt → 409), then f.txt, then inner
	if w := fileDelete(t, s, id, "?path=d%2Finner%2Ff.txt"); w.Code != 204 {
		t.Errorf("delete nested file: got %d", w.Code)
	}
	if w := fileDelete(t, s, id, "?path=d%2Finner"); w.Code != 204 {
		t.Errorf("delete empty dir: got %d", w.Code)
	}
	if w := fileDelete(t, s, id, "?path=d"); w.Code != 204 {
		t.Errorf("delete empty parent: got %d", w.Code)
	}
	// symlink leaf → 400, link untouched
	if w := fileDelete(t, s, id, "?path=badlink"); w.Code != 400 {
		t.Errorf("delete symlink: got %d, want 400", w.Code)
	}
	if _, err := os.Lstat(filepath.Join(mnt, "badlink")); err != nil {
		t.Errorf("symlink was removed")
	}
	// missing → 404
	if w := fileDelete(t, s, id, "?path=nope.txt"); w.Code != http.StatusNotFound {
		t.Errorf("delete missing: got %d, want 404", w.Code)
	}
}

func TestMoveFileRules(t *testing.T) {
	s, id, mnt := filePutServer(t)
	if err := os.WriteFile(filepath.Join(mnt, "a.txt"), []byte("A"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mnt, "b.txt"), []byte("B"), 0o644); err != nil {
		t.Fatal(err)
	}
	// happy move
	w := fileMove(t, s, id, `{"from":"a.txt","to":"sub/dir/x.txt"}`)
	if w.Code != 200 {
		t.Fatalf("move: %d %s", w.Code, w.Body.String())
	}
	if got, err := os.ReadFile(filepath.Join(mnt, "sub", "dir", "x.txt")); err != nil || string(got) != "A" {
		t.Fatalf("moved content: %v %q", err, got)
	}
	if _, err := os.Stat(filepath.Join(mnt, "a.txt")); !os.IsNotExist(err) {
		t.Errorf("source still there")
	}
	// target exists → 409
	if w := fileMove(t, s, id, `{"from":"sub/dir/x.txt","to":"b.txt"}`); w.Code != http.StatusConflict {
		t.Errorf("move onto existing: got %d, want 409", w.Code)
	}
	// missing source → 404
	if w := fileMove(t, s, id, `{"from":"nope","to":"y"}`); w.Code != http.StatusNotFound {
		t.Errorf("move missing: got %d, want 404", w.Code)
	}
	// from == to → 400
	if w := fileMove(t, s, id, `{"from":"b.txt","to":"b.txt"}`); w.Code != 400 {
		t.Errorf("move onto itself: got %d, want 400", w.Code)
	}
	// traversal → 400
	if w := fileMove(t, s, id, `{"from":"b.txt","to":"../escaped"}`); w.Code != 400 {
		t.Errorf("move traversal: got %d, want 400", w.Code)
	}
	// symlink source → 400
	if err := os.Symlink("/etc/hostname", filepath.Join(mnt, "lnk")); err != nil {
		t.Fatal(err)
	}
	if w := fileMove(t, s, id, `{"from":"lnk","to":"renamed"}`); w.Code != 400 {
		t.Errorf("move symlink: got %d, want 400", w.Code)
	}
}

// TestFileMutationsNeverCreateOutsideViaSymlink: a write or move through a
// planted `escape -> <outside>` symlink with a NEW subdirectory must not
// create that subdirectory (or temp files) outside the workspace before the
// symlink check refuses it (MkdirAll/CreateTemp follow symlinks).
func TestFileMutationsNeverCreateOutsideViaSymlink(t *testing.T) {
	s, id, mnt := filePutServer(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(mnt, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mnt, "a.txt"), []byte("A"), 0o644); err != nil {
		t.Fatal(err)
	}
	if w := filePut(t, s, id, "?path=escape%2Fnewdir%2Ff", "PWNED"); w.Code != 400 {
		t.Errorf("put through symlink: got %d, want 400", w.Code)
	}
	if w := fileMove(t, s, id, `{"from":"a.txt","to":"escape/movedir/x"}`); w.Code != 400 {
		t.Errorf("move through symlink: got %d, want 400", w.Code)
	}
	ents, _ := os.ReadDir(outside)
	if len(ents) != 0 {
		names := []string{}
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Fatalf("files created outside the workspace: %v", names)
	}
}
