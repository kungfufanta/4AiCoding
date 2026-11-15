package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tastyeffectco/sandboxd/portal/internal/sandboxd"
)

// A sandboxd key without the sandboxes:files scope answers 403 on the
// files API. BuildWorkspace must flag it so the run fails with a clear
// error instead of handing the model an "(unavailable)" tree.
func TestBuildWorkspaceFilesForbidden(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"this API key is limited to: sandboxes:exec"}}`))
	}))
	defer srv.Close()
	sbx := sandboxd.New(srv.URL, "k", 5*time.Second)
	ws := BuildWorkspace(context.Background(), sbx, "sb1", Env{ProjectDir: "/home/sandbox/workspace", RelPrefix: "workspace"})
	if ws.FilesForbidden == nil {
		t.Fatalf("FilesForbidden not set; notes=%v", ws.Notes)
	}
	if !strings.Contains(ws.FilesForbidden.Error(), "403") {
		t.Errorf("error %q does not carry the 403", ws.FilesForbidden)
	}
}
