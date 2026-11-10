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

func TestBuildSpecImagePolicy(t *testing.T) {
	s := &Server{Image: "sandboxd-base:0.3.0", Limits: Limits{ImageAllowlist: []string{"alpine:3.20"}}}
	if sp, err := s.buildSpec(specInput{}); err != nil || sp.Image != "" {
		t.Fatalf("default image: %+v %v", sp, err)
	}
	if sp, err := s.buildSpec(specInput{Image: "sandboxd-base:0.3.0"}); err != nil || sp.Image != "" {
		t.Fatalf("naming the instance image must follow it, got %+v %v", sp, err)
	}
	if sp, err := s.buildSpec(specInput{Image: "alpine:3.20"}); err != nil || sp.Image != "alpine:3.20" {
		t.Fatalf("allowlisted image: %+v %v", sp, err)
	}
	for _, bad := range []string{"ubuntu:24.04", "--privileged", "Alpine", "a b"} {
		if _, err := s.buildSpec(specInput{Image: bad}); err == nil {
			t.Errorf("image %q must be rejected", bad)
		}
	}
}

func TestBuildSpecClampsResourcesAndValidatesEnv(t *testing.T) {
	s := &Server{Image: "img", Limits: Limits{MaxCPUs: 2, MaxMemoryMB: 4096}}
	sp, err := s.buildSpec(specInput{CPUs: 8, MemoryMB: 99999})
	if err != nil || sp.CPUs != 2 || sp.MemoryMB != 4096 {
		t.Fatalf("clamp: %+v %v", sp, err)
	}
	if sp, _ := s.buildSpec(specInput{MemoryMB: 1}); sp.MemoryMB != 64 {
		t.Fatalf("memory floor: %d", sp.MemoryMB)
	}
	if _, err := s.buildSpec(specInput{CPUs: -1}); err == nil {
		t.Fatal("negative cpus must be rejected")
	}
	for _, k := range []string{"", "1A", "A-B", "A=B"} {
		if _, err := s.buildSpec(specInput{Env: map[string]string{k: "v"}}); err == nil {
			t.Errorf("env key %q must be rejected", k)
		}
	}
	if _, err := s.buildSpec(specInput{Env: map[string]string{"OK": "a\nb"}}); err == nil {
		t.Error("newline in env value must be rejected")
	}
	if _, err := s.buildSpec(specInput{Command: []string{"a\x00b"}}); err == nil {
		t.Error("NUL in command must be rejected")
	}
}

func TestCheckPorts(t *testing.T) {
	s := &Server{Limits: Limits{MaxPorts: 2}}
	if err := s.checkPorts([]int{3000, 8080}); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]int{{0}, {70000}, {3000, 3000}, {1, 2, 3}} {
		if err := s.checkPorts(bad); err == nil {
			t.Errorf("ports %v must be rejected", bad)
		}
	}
}

func TestV1CreateRejectsDisallowedImageBeforeDocker(t *testing.T) {
	s, _ := newSandboxRowServer(t)
	s.Image = "sandboxd-base:0.3.0"
	w := v1Call(s.v1CreateSandbox, "POST", "/v1/sandboxes", "", `{"image":"evil:latest"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "not allowed") {
		t.Fatalf("got %d %s", w.Code, w.Body)
	}
	w = v1Call(s.v1CreateSandbox, "POST", "/v1/sandboxes", "", `{"ports":[3000],"network":{"ports":[{"container":8080}]}}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("ports + network.ports: got %d %s", w.Code, w.Body)
	}
}

func TestStoreSpecRoundTrip(t *testing.T) {
	s, _ := newSandboxRowServer(t)
	id := newULID()
	want := store.Spec{Image: "alpine:3.20", Command: []string{"sleep", "1"}, Env: map[string]string{"A": "1"}, CPUs: 0.5, MemoryMB: 256}
	if err := s.Store.Create(context.Background(), &store.Sandbox{ID: id, Status: "stopped", Spec: want}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Store.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	gb, _ := json.Marshal(got.Spec)
	wb, _ := json.Marshal(want)
	if string(gb) != string(wb) {
		t.Fatalf("spec = %s, want %s", gb, wb)
	}
	v := s.v1SandboxFromRow(nil, got)
	if v.Image != "alpine:3.20" || v.Resources.MemoryMB != 256 || v.Resources.CPUs != 0.5 || v.Env["A"] != "1" {
		t.Fatalf("v1 view = %+v", v)
	}
}

func TestV1PortsEndpoints(t *testing.T) {
	s, id := newSandboxRowServer(t, 3000)
	call := func(h http.HandlerFunc, method, target, body string, extra map[string]string) (int, string) {
		r := reqAs(method, target, body, "local")
		r.SetPathValue("id", id)
		for k, v := range extra {
			r.SetPathValue(k, v)
		}
		w := httptest.NewRecorder()
		h(w, r)
		return w.Code, w.Body.String()
	}
	code, body := call(s.v1GetPorts, "GET", "/", "", nil)
	if code != 200 || !strings.Contains(body, `"container":3000`) || !strings.Contains(body, "preview_url") {
		t.Fatalf("get: %d %s", code, body)
	}
	// No-op change needs no container rebuild.
	if code, body := call(s.v1AddPort, "POST", "/", `{"container":3000}`, nil); code != 200 {
		t.Fatalf("idempotent add: %d %s", code, body)
	}
	if code, _ := call(s.v1AddPort, "POST", "/", `{"container":0}`, nil); code != 400 {
		t.Fatalf("bad port: %d", code)
	}
	if code, _ := call(s.v1PutPorts, "PUT", "/", `{"ports":[{"container":1},{"container":1}]}`, nil); code != 400 {
		t.Fatalf("duplicate: %d", code)
	}
	if code, _ := call(s.v1DeletePort, "DELETE", "/", "", map[string]string{"port": "abc"}); code != 400 {
		t.Fatalf("non-numeric port: %d", code)
	}
	creating := newULID()
	if err := s.Store.Create(context.Background(), &store.Sandbox{ID: creating, Status: "creating"}); err != nil {
		t.Fatal(err)
	}
	r := reqAs("POST", "/", `{"container":8080}`, "local")
	r.SetPathValue("id", creating)
	w := httptest.NewRecorder()
	s.v1AddPort(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("creating sandbox: %d %s", w.Code, w.Body)
	}
}

func TestV1MetricsAndVolumesWithoutContainer(t *testing.T) {
	s, _ := newSandboxRowServer(t)
	id := newULID()
	if err := s.Store.Create(context.Background(), &store.Sandbox{ID: id, Status: "stopped", WorkspaceMnt: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	w := v1Call(s.v1SandboxMetrics, "GET", "/", id, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"stopped"`) {
		t.Fatalf("metrics: %d %s", w.Code, w.Body)
	}
	w = v1Call(s.v1SandboxVolumes, "GET", "/", id, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"destination":"/home/sandbox"`) {
		t.Fatalf("volumes: %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "source") {
		t.Fatalf("volumes must not expose host paths: %s", w.Body)
	}
	w = v1Call(s.v1SandboxMetricsStream, "GET", "/?interval=0", id, "")
	if w.Code != 400 {
		t.Fatalf("stream interval: %d", w.Code)
	}
}
