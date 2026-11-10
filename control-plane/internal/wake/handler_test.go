package wake

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/docker"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/previewhost"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

// fakeDocker writes a stand-in docker CLI whose `inspect` answers with
// the given state ("running", "exited", "missing" or "broken") and whose
// `start` always fails, so a wake that gets past the "already running"
// check stops there instead of needing a real daemon. Every invocation
// is appended to the returned calls file.
func fakeDocker(t *testing.T, state string) (bin, calls string) {
	t.Helper()
	dir := t.TempDir()
	calls = filepath.Join(dir, "calls")
	inspect := map[string]string{
		"running": `echo '{"Id":"c1","State":{"Status":"running","Running":true}}'`,
		"exited":  `echo '{"Id":"c1","State":{"Status":"exited","Running":false}}'`,
		"missing": `echo "Error: No such object: $4" >&2; exit 1`,
		"broken":  `echo "Cannot connect to the Docker daemon" >&2; exit 1`,
	}[state]
	script := "#!/bin/sh\n" +
		`echo "$*" >> "` + calls + `"` + "\n" +
		"case \"$1\" in\n" +
		"inspect) " + inspect + " ;;\n" +
		"*) echo \"fake docker: $1 not supported\" >&2; exit 1 ;;\n" +
		"esac\n"
	bin = filepath.Join(dir, "docker")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, calls
}

func newDriftHandler(t *testing.T, state string) (*Handler, *store.Store, string) {
	t.Helper()
	st, err := store.Open(context.Background(), "file::memory:?_fk=1", "../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	const id = "01WAKEDRIFT0000000000001"
	if err := st.Create(context.Background(), &store.Sandbox{ID: id, Status: "running"}); err != nil {
		t.Fatal(err)
	}
	bin, calls := fakeDocker(t, state)
	hosts := previewhost.Scheme{Domain: "localhost"}
	h, err := New(st, &docker.Client{Bin: bin}, hosts, Config{}, AdmitConfig{}, nil, nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return h, st, calls
}

func wakeJSON(h *Handler, id string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/wake/"+id, nil)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	h.ServeJSON(w, r)
	return w
}

// A row that says running while its container is gone or exited must not
// be answered with "already running" (the browser would refresh into the
// wake page forever): it is marked stopped and goes down the start path.
func TestWakeRunningRowWithoutContainer(t *testing.T) {
	for _, state := range []string{"missing", "exited"} {
		t.Run(state, func(t *testing.T) {
			h, st, calls := newDriftHandler(t, state)
			w := wakeJSON(h, "01WAKEDRIFT0000000000001")

			var body struct{ Status, Error string }
			_ = json.Unmarshal(w.Body.Bytes(), &body)
			if w.Code == http.StatusOK || body.Status == "running" {
				t.Fatalf("wake answered %d %s; want it to try starting the sandbox", w.Code, w.Body)
			}
			sb, err := st.Get(context.Background(), "01WAKEDRIFT0000000000001")
			if err != nil {
				t.Fatal(err)
			}
			if sb.Status != "stopped" {
				t.Fatalf("row status = %q, want stopped", sb.Status)
			}
			log, _ := os.ReadFile(calls)
			if !strings.Contains(string(log), "inspect") {
				t.Fatalf("expected an inspect of the container, docker calls:\n%s", log)
			}
		})
	}
}

// A healthy running container, or an inspect that fails for some other
// reason, keeps today's behaviour: trust the row and report running.
func TestWakeRunningRowTrustedWhenContainerUpOrInspectFails(t *testing.T) {
	for _, state := range []string{"running", "broken"} {
		t.Run(state, func(t *testing.T) {
			h, st, calls := newDriftHandler(t, state)
			w := wakeJSON(h, "01WAKEDRIFT0000000000001")

			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"status":"running"`) {
				t.Fatalf("wake answered %d %s; want 200 running", w.Code, w.Body)
			}
			sb, err := st.Get(context.Background(), "01WAKEDRIFT0000000000001")
			if err != nil {
				t.Fatal(err)
			}
			if sb.Status != "running" {
				t.Fatalf("row status = %q, want running", sb.Status)
			}
			log, _ := os.ReadFile(calls)
			if strings.Contains(string(log), "start") {
				t.Fatalf("healthy sandbox must not be restarted, docker calls:\n%s", log)
			}
		})
	}
}
