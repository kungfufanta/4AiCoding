package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

// planted secret bytes we assert never leak through the file API.
const secretMarker = "SANDBOXD_API_TOKENS=default=sk_TOPSECRET"

// fileRowServer builds a Server backed by a file store row whose
// WorkspaceMnt is a fresh temp dir — the host-side stand-in for
// /home/sandbox inside the container.
func fileRowServer(t *testing.T) (*Server, string, string) {
	t.Helper()
	st := fileStore(t)
	t.Cleanup(func() { st.Close() })
	mnt := t.TempDir()
	s := &Server{Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	id := newULID()
	if err := st.Create(context.Background(), &store.Sandbox{
		ID: id, Status: "running", WorkspaceMnt: mnt, ExternalUserID: nullStr("local"),
	}); err != nil {
		t.Fatal(err)
	}
	return s, id, mnt
}

// filesGet performs a GET against a files handler with a query string.
func filesGet(t *testing.T, h func(http.ResponseWriter, *http.Request), s *Server, id, query string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("GET", "/v1/sandboxes/"+id+"/files"+query, nil)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

// fileSymlinkServer plants a leaf symlink and an intermediate-directory
// symlink that escape the workspace, plus a normal file that must still be
// readable. Ported from upstream v1_files_symlink_test.go.
func fileSymlinkServer(t *testing.T) (*Server, string, string) {
	t.Helper()
	s, id, mnt := fileRowServer(t)
	// A secret OUTSIDE the workspace (stands in for /proc/self/environ etc.).
	outside := t.TempDir()
	secret := filepath.Join(outside, "environ")
	if err := os.WriteFile(secret, []byte(secretMarker), 0o600); err != nil {
		t.Fatal(err)
	}
	// Leaf symlink: leak -> outside/environ
	if err := os.Symlink(secret, filepath.Join(mnt, "leak")); err != nil {
		t.Fatal(err)
	}
	// Intermediate symlink: escape -> outside/
	if err := os.Symlink(outside, filepath.Join(mnt, "escape")); err != nil {
		t.Fatal(err)
	}
	// A normal file that MUST still read fine.
	if err := os.WriteFile(filepath.Join(mnt, "ok.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	return s, id, mnt
}

// fileReadResponse decodes the /files/content envelope.
type fileReadResponse struct {
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	Offset    int64  `json:"offset"`
	Encoding  string `json:"encoding"`
	Data      string `json:"data"`
	Truncated bool   `json:"truncated"`
}

func decodeFileRead(t *testing.T, body string) fileReadResponse {
	t.Helper()
	var out fileReadResponse
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode read response: %v — body %q", err, body)
	}
	return out
}

func TestFileContentDoesNotFollowSymlinks(t *testing.T) {
	s, id, _ := fileSymlinkServer(t)

	// leaf symlink escape must NOT leak the secret
	w := filesGet(t, s.v1FileContent, s, id, "?path=leak")
	if w.Code == 200 && strings.Contains(w.Body.String(), secretMarker) {
		t.Fatalf("LEAK via leaf symlink: code=%d body=%q", w.Code, w.Body.String())
	}
	if w.Code != http.StatusNotFound {
		t.Errorf("leaf symlink read: got %d, want 404 not_found", w.Code)
	}
	// intermediate-component symlink escape must NOT leak either
	w = filesGet(t, s.v1FileContent, s, id, "?path=escape%2Fenviron")
	if w.Code == 200 && strings.Contains(w.Body.String(), secretMarker) {
		t.Fatalf("LEAK via intermediate symlink: code=%d body=%q", w.Code, w.Body.String())
	}
	if w.Code != http.StatusNotFound {
		t.Errorf("intermediate symlink read: got %d, want 404", w.Code)
	}
	// a normal in-workspace file must still read (JSON envelope, utf-8)
	w = filesGet(t, s.v1FileContent, s, id, "?path=ok.txt")
	if w.Code != 200 {
		t.Fatalf("normal read broken: code=%d body=%q", w.Code, w.Body.String())
	}
	out := decodeFileRead(t, w.Body.String())
	if out.Encoding != "utf-8" || out.Data != "hello" || out.Size != 5 || out.Truncated {
		t.Errorf("read = %+v, want utf-8 hello size 5", out)
	}
}

func TestListDoesNotFollowSymlinkedDir(t *testing.T) {
	s, id, _ := fileSymlinkServer(t)
	// Both recursive and non-recursive listing INTO the symlinked dir must
	// not expose the outside file.
	for _, rec := range []string{"true", "false"} {
		w := filesGet(t, s.v1ListFiles, s, id, "?path=escape&recursive="+rec)
		if w.Code == 200 && strings.Contains(w.Body.String(), "environ") {
			t.Fatalf("LEAK: list (recursive=%s) followed the symlinked dir: %s", rec, w.Body.String())
		}
	}
	// Listing never exposes the symlink entry itself.
	w := filesGet(t, s.v1ListFiles, s, id, "?recursive=true")
	if strings.Contains(w.Body.String(), `"leak"`) || strings.Contains(w.Body.String(), `"escape"`) {
		t.Errorf("list exposed symlink entries: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"ok.txt"`) {
		t.Errorf("list dropped the normal file: %s", w.Body.String())
	}
}

func TestListExcludesAndTruncates(t *testing.T) {
	s, id, mnt := fileRowServer(t)
	for _, d := range []string{"node_modules", ".git", ".venv", "dist", ".vite"} {
		if err := os.MkdirAll(filepath.Join(mnt, d), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(mnt, d, "x.js"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"app.ts", "f1.txt", "f2.txt", "f3.txt"} {
		if err := os.WriteFile(filepath.Join(mnt, f), []byte("a"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	w := filesGet(t, s.v1ListFiles, s, id, "?recursive=true")
	if strings.Contains(w.Body.String(), "node_modules") || strings.Contains(w.Body.String(), "x.js") {
		t.Errorf("recursive listing included excluded dirs: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"app.ts"`) {
		t.Errorf("listing missing app.ts: %s", w.Body.String())
	}
	// max_entries bounds the listing and sets truncated (4 visible entries).
	w = filesGet(t, s.v1ListFiles, s, id, "?recursive=true&max_entries=2")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"truncated":true`) {
		t.Errorf("max_entries=1: code=%d body=%s", w.Code, w.Body.String())
	}
	// bad max_entries → 400
	if w := filesGet(t, s.v1ListFiles, s, id, "?max_entries=nope"); w.Code != 400 {
		t.Errorf("max_entries=nope: got %d, want 400", w.Code)
	}
}

func TestFileReadWindowAndBinary(t *testing.T) {
	s, id, mnt := fileRowServer(t)
	big := strings.Repeat("abcdefgh", 4000) // 32000 bytes
	if err := os.WriteFile(filepath.Join(mnt, "big.txt"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := []byte{0x00, 0x01, 0x02, 0xff, 0xfe}
	if err := os.WriteFile(filepath.Join(mnt, "blob.bin"), bin, 0o644); err != nil {
		t.Fatal(err)
	}

	// paging: offset=8 limit=10
	w := filesGet(t, s.v1FileContent, s, id, "?path=big.txt&offset=8&limit=10")
	if w.Code != 200 {
		t.Fatalf("window read: %d %s", w.Code, w.Body.String())
	}
	out := decodeFileRead(t, w.Body.String())
	if out.Data != big[8:18] || !out.Truncated || out.Offset != 8 || out.Size != int64(len(big)) {
		t.Errorf("window = %+v, want data %q truncated", out, big[8:18])
	}
	// limit above the 2 MiB cap → 400
	if w := filesGet(t, s.v1FileContent, s, id, "?path=big.txt&limit=3000000"); w.Code != 400 {
		t.Errorf("limit>2MiB: got %d, want 400", w.Code)
	}
	// offset past EOF → 400
	if w := filesGet(t, s.v1FileContent, s, id, "?path=big.txt&offset=999999"); w.Code != 400 {
		t.Errorf("offset past EOF: got %d, want 400", w.Code)
	}
	// binary → base64
	w = filesGet(t, s.v1FileContent, s, id, "?path=blob.bin")
	if w.Code != 200 {
		t.Fatalf("binary read: %d", w.Code)
	}
	out = decodeFileRead(t, w.Body.String())
	if out.Encoding != "base64" || base64.StdEncoding.EncodeToString(bin) != out.Data {
		t.Errorf("binary read = %+v", out)
	}
	// traversal attempts: never a readable 200 — the lexical clean absorbs
	// the ../ against the virtual root, so the resolved path simply does not
	// exist inside the workspace (404) or is rejected (400).
	for _, p := range []string{"..%2Fetc%2Fpasswd", "a%2F..%2F..%2Fetc%2Fpasswd"} {
		w := filesGet(t, s.v1FileContent, s, id, "?path="+p)
		if w.Code == 200 || strings.Contains(w.Body.String(), secretMarker) {
			t.Errorf("traversal %s readable: %d %s", p, w.Code, w.Body.String())
		}
	}
}

func TestReservedSubtreeBlockedEverywhere(t *testing.T) {
	s, id, mnt := fileRowServer(t)
	if err := os.MkdirAll(filepath.Join(mnt, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mnt, ".ssh", "id_ed25519"), []byte("KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	// list into .ssh → 400
	if w := filesGet(t, s.v1ListFiles, s, id, "?path=.ssh"); w.Code != 400 {
		t.Errorf("list .ssh: got %d, want 400", w.Code)
	}
	// read .ssh key → 400
	if w := filesGet(t, s.v1FileContent, s, id, "?path=.ssh%2Fid_ed25519"); w.Code != 400 {
		t.Errorf("read .ssh: got %d, want 400", w.Code)
	}
	// recursive listing must not expose the key either
	if w := filesGet(t, s.v1ListFiles, s, id, "?recursive=true"); strings.Contains(w.Body.String(), "id_ed25519") {
		t.Errorf("recursive listing exposed .ssh contents")
	}
}
