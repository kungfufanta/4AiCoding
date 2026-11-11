package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/policy"
)

// TestV1ExecV2Validation pins the exec v2 (AGENT_PLAN A0) request rules.
// Rejection paths run before Docker is touched, so a row-only server works.
func TestV1ExecV2Validation(t *testing.T) {
	s, id := newSandboxRowServer(t)

	// policy.exec.max_timeout_s lowers the ceiling. The stored policy must
	// be a COMPLETE valid policy, so start from Default() and override —
	// set BEFORE any call so the policy cache primes with it.
	pol := policy.Default()
	pol.Exec.MaxTimeoutSecs = 30
	b, _ := json.Marshal(pol)
	if err := s.Store.SetConfig(t.Context(), "policy", string(b)); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		body string
	}{
		{"cwd outside workspace", `{"cmd":["ls"],"cwd":"/etc"}`},
		{"cwd traversal", `{"cmd":["ls"],"cwd":"/home/sandbox/../.."} `},
		{"cwd sibling prefix", `{"cmd":["ls"],"cwd":"/home/sandboxx"}`},
		{"env PATH override", `{"cmd":["ls"],"env":{"PATH":"/x"}}`},
		{"env HOME override", `{"cmd":["ls"],"env":{"HOME":"/x"}}`},
		{"env LD_PRELOAD override", `{"cmd":["ls"],"env":{"LD_PRELOAD":"/x"}}`},
		{"env empty key", `{"cmd":["ls"],"env":{"":"v"}}`},
		{"env = in key", `{"cmd":["ls"],"env":{"A=B":"v"}}`},
		{"timeout zero", `{"cmd":["ls"],"timeout_s":0}`},
		{"timeout negative", `{"cmd":["ls"],"timeout_s":-5}`},
		{"timeout above default cap", `{"cmd":["ls"],"timeout_s":601}`},
		{"max_output zero", `{"cmd":["ls"],"max_output_bytes":0}`},
		{"max_output above cap", `{"cmd":["ls"],"max_output_bytes":5000000}`},
		{"stdin over 1 MiB", `{"cmd":["ls"],"stdin":"` + strings.Repeat("a", 1048577) + `"}`},
	}
	for _, tc := range cases {
		w := v1Call(s.v1ExecSandbox, "POST", "/x", id, tc.body)
		if w.Code != 400 {
			t.Errorf("%s: got %d, want 400 (%s)", tc.name, w.Code, w.Body.String())
		}
	}

	// 31 exceeds the stored ceiling of 30 (601 and friends exceed it too).
	if w := v1Call(s.v1ExecSandbox, "POST", "/x", id, `{"cmd":["ls"],"timeout_s":31}`); w.Code != 400 {
		t.Errorf("timeout 31 under max_timeout_s=30: got %d, want 400", w.Code)
	}
}

// TestV1ExecStreamParam pins ?stream handling.
func TestV1ExecStreamParam(t *testing.T) {
	s, id := newSandboxRowServer(t)
	// unknown stream value → 400 (checked before Docker)
	w := v1Call(s.v1ExecSandbox, "POST", "/x?stream=text", id, `{"cmd":["ls"],"timeout_s":5}`)
	if w.Code != 400 {
		t.Errorf("stream=text: got %d, want 400", w.Code)
	}
}

// TestV1ExecV2OptIn pins the opt-in detection: a bare {cmd} request must
// NOT get v2 defaults (the legacy e2e contract: {cmd} behaves exactly as
// before — no timeout wrapper, unbounded buffers).
func TestV1ExecV2OptIn(t *testing.T) {
	legacy := v1ExecReq{Cmd: []string{"ls"}}
	if legacy.v2Exec() {
		t.Error("bare {cmd} must stay legacy")
	}
	for _, req := range []v1ExecReq{
		{Cmd: []string{"ls"}, Cwd: "/home/sandbox"},
		{Cmd: []string{"ls"}, Env: map[string]string{"CI": "1"}},
		{Cmd: []string{"ls"}, Stdin: "x"},
		{Cmd: []string{"ls"}, TimeoutS: ptr(5)},
		{Cmd: []string{"ls"}, MaxOutputBytes: ptrI64(1024)},
	} {
		if !req.v2Exec() {
			t.Errorf("v2 field not detected: %+v", req)
		}
	}
}

func ptr(i int) *int        { return &i }
func ptrI64(i int64) *int64 { return &i }
