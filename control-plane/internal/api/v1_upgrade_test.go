package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/upgrade"
)

// Docker is nil on the Manager here: Start validates, persists state, and
// skips the launch — enough to drive every HTTP status the handler maps.
func upgradeServer(t *testing.T, srcDir string) (*Server, *upgrade.Manager) {
	t.Helper()
	m := &upgrade.Manager{
		DataDir: t.TempDir(), SrcDir: srcDir, Version: "v0.3.10",
		UpgraderImage: "sandboxd-upgrader:v0.3.10",
		ReleaseExists: func(ctx context.Context, tag string) bool { return tag == "v0.3.11" },
		FreeBytes:     func(string) uint64 { return 10 << 30 },
	}
	return &Server{Upgrade: m}, m
}

func post(s *Server, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/v1/upgrade", strings.NewReader(body))
	w := httptest.NewRecorder()
	s.v1UpgradeStart(w, req)
	return w
}

func TestUpgrade_StartAccepted(t *testing.T) {
	s, _ := upgradeServer(t, "/opt/sandboxd")
	if w := post(s, `{"target":"v0.3.11"}`); w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), `"phase":"running"`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	req := httptest.NewRequest("GET", "/v1/upgrade", nil)
	w := httptest.NewRecorder()
	s.v1UpgradeStatus(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"target":"v0.3.11"`) {
		t.Fatalf("status: %d %s", w.Code, w.Body)
	}
}

func TestUpgrade_StatusCodes(t *testing.T) {
	s, m := upgradeServer(t, "/opt/sandboxd")
	if w := post(s, `{"target":"main"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("bad target: %d", w.Code)
	}
	if w := post(s, `{}`); w.Code != http.StatusBadRequest {
		t.Fatalf("missing target: %d", w.Code)
	}
	m.FreeBytes = func(string) uint64 { return 1 << 30 }
	if w := post(s, `{"target":"v0.3.11"}`); w.Code != http.StatusInsufficientStorage {
		t.Fatalf("low disk: %d", w.Code)
	}
	m.FreeBytes = func(string) uint64 { return 10 << 30 }
	// simulate a running upgrade recorded on disk
	os.MkdirAll(filepath.Join(m.DataDir, "state"), 0o755)
	os.WriteFile(filepath.Join(m.DataDir, "state", "upgrade.json"), []byte(`{"phase":"running","target":"v0.3.11"}`), 0o644)
	if w := post(s, `{"target":"v0.3.11"}`); w.Code != http.StatusConflict {
		t.Fatalf("running: %d", w.Code)
	}
	noSrc, _ := upgradeServer(t, "")
	if w := post(noSrc, `{"target":"v0.3.11"}`); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "upgrade.sh") {
		t.Fatalf("no src dir must 409 with CLI hint: %d %s", w.Code, w.Body)
	}
	unsupported := &Server{}
	if w := post(unsupported, `{"target":"v0.3.11"}`); w.Code != http.StatusConflict {
		t.Fatalf("nil manager: %d", w.Code)
	}
	w := httptest.NewRecorder()
	unsupported.v1UpgradeStatus(w, httptest.NewRequest("GET", "/v1/upgrade", nil))
	if !strings.Contains(w.Body.String(), `"phase":"unavailable"`) {
		t.Fatalf("nil manager status: %s", w.Body)
	}
}
