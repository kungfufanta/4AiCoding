package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

func v1Call(h http.HandlerFunc, method, target, id, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	if id != "" {
		r.SetPathValue("id", id)
	}
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

func v1ErrCodeOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatalf("not a v1 error envelope: %s", w.Body)
	}
	return e.Error.Code
}

func TestV1ListSandboxes(t *testing.T) {
	s, running := newSandboxRowServer(t, 3000)
	stopped := newULID()
	if err := s.Store.Create(context.Background(), &store.Sandbox{ID: stopped, Status: "stopped"}); err != nil {
		t.Fatal(err)
	}

	list := func(target string) []string {
		t.Helper()
		w := v1Call(s.v1ListSandboxes, "GET", target, "", "")
		if w.Code != http.StatusOK {
			t.Fatalf("%s: got %d: %s", target, w.Code, w.Body)
		}
		var d struct {
			Sandboxes []v1Sandbox `json:"sandboxes"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, sb := range d.Sandboxes {
			ids = append(ids, sb.ID)
		}
		return ids
	}

	if ids := list("/v1/sandboxes"); len(ids) != 2 {
		t.Errorf("list all = %v, want 2 sandboxes", ids)
	}
	if ids := list("/v1/sandboxes?status=stopped"); len(ids) != 1 || ids[0] != stopped {
		t.Errorf("status=stopped = %v, want [%s]", ids, stopped)
	}
	if ids := list("/v1/sandboxes?external_user_id=local"); len(ids) != 1 || ids[0] != running {
		t.Errorf("external_user_id=local = %v, want [%s]", ids, running)
	}
}

func TestV1SandboxNotFound(t *testing.T) {
	s, _ := newSandboxRowServer(t)
	missing := newULID()
	for name, w := range map[string]*httptest.ResponseRecorder{
		"get":     v1Call(s.v1GetSandbox, "GET", "/x", missing, ""),
		"stop":    v1Call(s.v1StopSandbox, "POST", "/x", missing, ""),
		"start":   v1Call(s.v1StartSandbox, "POST", "/x", missing, ""),
		"restart": v1Call(s.v1RestartSandbox, "POST", "/x", missing, ""),
		"exec":    v1Call(s.v1ExecSandbox, "POST", "/x", missing, `{"cmd":["true"]}`),
		"logs":    v1Call(s.v1SandboxLogs, "GET", "/x", missing, ""),
	} {
		if w.Code != http.StatusNotFound || v1ErrCodeOf(t, w) != "not_found" {
			t.Errorf("%s: got %d %s, want 404 not_found", name, w.Code, w.Body)
		}
	}
}

func TestV1ExecValidation(t *testing.T) {
	s, id := newSandboxRowServer(t)
	for _, body := range []string{`{"cmd":[]}`, `{}`, `{"cmd":[""]}`, `{"cmd":["ls"],"tty":true}`, `not json`} {
		w := v1Call(s.v1ExecSandbox, "POST", "/x", id, body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("exec %s: got %d, want 400", body, w.Code)
		}
	}

	stopped := newULID()
	if err := s.Store.Create(context.Background(), &store.Sandbox{ID: stopped, Status: "stopped"}); err != nil {
		t.Fatal(err)
	}
	w := v1Call(s.v1ExecSandbox, "POST", "/x", stopped, `{"cmd":["true"]}`)
	if w.Code != http.StatusConflict {
		t.Errorf("exec on stopped sandbox: got %d, want 409", w.Code)
	}
}

func TestV1RestartRejectsTransientStates(t *testing.T) {
	s, _ := newSandboxRowServer(t)
	id := newULID()
	if err := s.Store.Create(context.Background(), &store.Sandbox{ID: id, Status: "creating"}); err != nil {
		t.Fatal(err)
	}
	if w := v1Call(s.v1RestartSandbox, "POST", "/x", id, ""); w.Code != http.StatusConflict {
		t.Errorf("restart while creating: got %d, want 409", w.Code)
	}
	if w := v1Call(s.v1StopSandbox, "POST", "/x", id, ""); w.Code != http.StatusConflict {
		t.Errorf("stop while creating: got %d, want 409", w.Code)
	}
}

func TestV1LogsTailValidation(t *testing.T) {
	s, id := newSandboxRowServer(t)
	for _, tail := range []string{"0", "-5", "abc"} {
		w := v1Call(s.v1SandboxLogs, "GET", "/x?tail="+tail, id, "")
		if w.Code != http.StatusBadRequest {
			t.Errorf("tail=%s: got %d, want 400", tail, w.Code)
		}
	}
}

func TestV1CreateRejectsUnknownAndConflictingFields(t *testing.T) {
	s, _ := newSandboxRowServer(t)
	for _, body := range []string{
		`{"template":"react-standard"}`,
		`{"runtime_preset":"nextjs"}`,
		`{"project":{"id":"p","user_id":"u"}}`,
		`{"from_snapshot":"x","workspace":{"repo_url":"https://example.com/r.git"}}`,
		`{"workspace":{}}`,
	} {
		w := v1Call(s.v1CreateSandbox, "POST", "/v1/sandboxes", "", body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("create %s: got %d, want 400 (%s)", body, w.Code, w.Body)
		}
	}
}

// The preview block appears only for sandboxes that expose ports.
func TestV1SandboxPreviewOnlyWithPorts(t *testing.T) {
	s, withPorts := newSandboxRowServer(t, 3000)
	s.PreviewDomain = "ex.localhost"
	noPorts := newULID()
	if err := s.Store.Create(context.Background(), &store.Sandbox{ID: noPorts, Status: "running"}); err != nil {
		t.Fatal(err)
	}
	get := func(id string) v1Sandbox {
		w := v1Call(s.v1GetSandbox, "GET", "/x", id, "")
		var sb v1Sandbox
		if err := json.Unmarshal(w.Body.Bytes(), &sb); err != nil {
			t.Fatal(err)
		}
		return sb
	}
	if sb := get(withPorts); sb.Preview == nil || sb.Preview.Status != "running" || !strings.Contains(sb.Preview.URL, "-3000.preview.ex.localhost") {
		t.Errorf("with ports: preview = %+v", sb.Preview)
	}
	if sb := get(noPorts); sb.Preview != nil {
		t.Errorf("without ports: preview should be omitted, got %+v", sb.Preview)
	}
}
