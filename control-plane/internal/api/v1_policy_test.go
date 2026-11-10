package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/auth"
)

// callAs runs h with an actor of the given kind and optional path values.
func callAs(h http.HandlerFunc, kind, method, target, body string, hdr map[string]string, pv map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r = r.WithContext(auth.WithActor(r.Context(), auth.Actor{Kind: kind, Name: "default", User: kind + "-1", Role: auth.RoleAdmin}))
	for k, v := range pv {
		r.SetPathValue(k, v)
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h(w, r)
	return w
}

func TestPolicyGetPutAndSections(t *testing.T) {
	s, _ := newSandboxRowServer(t)
	w := callAs(s.v1GetPolicy, "user", "GET", "/v1/policy", "", nil, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"mode":"strict"`) {
		t.Fatalf("get: %d %s", w.Code, w.Body)
	}
	w = callAs(s.v1PutPolicySection, "user", "PUT", "/", `{"mode":"strict","allow":["github.com","*.npmjs.org"]}`, nil, map[string]string{"section": "egress"})
	if w.Code != 200 || !strings.Contains(w.Body.String(), "github.com") {
		t.Fatalf("put section: %d %s", w.Code, w.Body)
	}
	w = callAs(s.v1PutPolicySection, "user", "PUT", "/", `{"mode":"bogus"}`, nil, map[string]string{"section": "egress"})
	if w.Code != 400 {
		t.Fatalf("invalid mode: %d %s", w.Code, w.Body)
	}
	w = callAs(s.v1GetPolicySection, "user", "GET", "/", "", nil, map[string]string{"section": "nope"})
	if w.Code != 404 {
		t.Fatalf("unknown section: %d", w.Code)
	}
}

func TestLifecycleKeepAliveNeedsPolicy(t *testing.T) {
	s, id := newSandboxRowServer(t)
	pv := map[string]string{"id": id}
	if w := callAs(s.v1KeepAliveOn, "user", "POST", "/", "", nil, pv); w.Code != http.StatusForbidden {
		t.Fatalf("keep-alive must be denied by the default policy: %d %s", w.Code, w.Body)
	}
	if w := callAs(s.v1PutPolicySection, "user", "PUT", "/", `{"allow_keep_alive":true,"max_idle_timeout_secs":3600}`, nil, map[string]string{"section": "lifecycle"}); w.Code != 200 {
		t.Fatalf("policy: %d %s", w.Code, w.Body)
	}
	if w := callAs(s.v1KeepAliveOn, "user", "POST", "/", "", nil, pv); w.Code != 200 || !strings.Contains(w.Body.String(), `"keep_alive":true`) {
		t.Fatalf("keep-alive on: %d %s", w.Code, w.Body)
	}
	w := callAs(s.v1PatchLifecycle, "user", "PATCH", "/", `{"idle_timeout_secs":999999}`, nil, pv)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"idle_timeout_secs":3600`) {
		t.Fatalf("idle clamp: %d %s", w.Code, w.Body)
	}
	if w := callAs(s.v1KeepAliveOff, "user", "DELETE", "/", "", nil, pv); w.Code != 200 || !strings.Contains(w.Body.String(), `"keep_alive":false`) {
		t.Fatalf("keep-alive off: %d %s", w.Code, w.Body)
	}
}

func TestEgressPolicyAndApproval(t *testing.T) {
	s, id := newSandboxRowServer(t)
	pv := map[string]string{"id": id}
	callAs(s.v1PutPolicySection, "user", "PUT", "/", `{"mode":"strict","allow":["github.com"]}`, nil, map[string]string{"section": "egress"})

	if w := callAs(s.v1PutEgress, "service", "PUT", "/", `{"allow":["api.github.com"]}`, nil, pv); w.Code != 200 {
		t.Fatalf("subdomain of an allowed domain: %d %s", w.Code, w.Body)
	}
	if w := callAs(s.v1PutEgress, "service", "PUT", "/", `{"allow":["evil.example"]}`, nil, pv); w.Code != http.StatusForbidden {
		t.Fatalf("outside strict policy: %d %s", w.Code, w.Body)
	}

	// Gate it behind an approval instead.
	if w := callAs(s.v1PutPolicySection, "user", "PUT", "/", `["sandbox.egress_outside_policy"]`, nil, map[string]string{"section": "approvals"}); w.Code != 200 {
		t.Fatalf("approvals section: %d %s", w.Code, w.Body)
	}
	body := `{"allow":["evil.example"]}`
	w := callAs(s.v1PutEgress, "service", "PUT", "/", body, nil, pv)
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202 pending: %d %s", w.Code, w.Body)
	}
	var pend struct {
		ApprovalID string `json:"approval_id"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &pend)
	hdr := map[string]string{ApprovalHeader: pend.ApprovalID}

	// Not approved yet.
	if w := callAs(s.v1PutEgress, "service", "PUT", "/", body, hdr, pv); w.Code != http.StatusForbidden {
		t.Fatalf("pending approval used: %d %s", w.Code, w.Body)
	}
	// API keys can never approve.
	apv := map[string]string{"id": pend.ApprovalID}
	if w := callAs(s.v1ApproveApproval, "service", "POST", "/", "", nil, apv); w.Code != http.StatusForbidden {
		t.Fatalf("service approved: %d %s", w.Code, w.Body)
	}
	if w := callAs(s.v1ApproveApproval, "user", "POST", "/", `{"reason":"ok"}`, nil, apv); w.Code != 200 {
		t.Fatalf("approve: %d %s", w.Code, w.Body)
	}
	// A different payload cannot ride on it.
	if w := callAs(s.v1PutEgress, "service", "PUT", "/", `{"allow":["other.example"]}`, hdr, pv); w.Code != http.StatusForbidden {
		t.Fatalf("approval reused for another payload: %d %s", w.Code, w.Body)
	}
	if w := callAs(s.v1PutEgress, "service", "PUT", "/", body, hdr, pv); w.Code != 200 || !strings.Contains(w.Body.String(), "evil.example") {
		t.Fatalf("approved request: %d %s", w.Code, w.Body)
	}
	// Single use.
	if w := callAs(s.v1PutEgress, "service", "PUT", "/", body, hdr, pv); w.Code != http.StatusForbidden {
		t.Fatalf("approval reused: %d %s", w.Code, w.Body)
	}
	w = callAs(s.v1ListApprovals, "user", "GET", "/v1/approvals?status=used", "", nil, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), pend.ApprovalID) {
		t.Fatalf("list used: %d %s", w.Code, w.Body)
	}
}

func TestDeleteKeepAliveNeedsApproval(t *testing.T) {
	s, id := newSandboxRowServer(t)
	callAs(s.v1PutPolicySection, "user", "PUT", "/", `{"allow_keep_alive":true}`, nil, map[string]string{"section": "lifecycle"})
	callAs(s.v1PutPolicySection, "user", "PUT", "/", `["sandbox.delete_keep_alive"]`, nil, map[string]string{"section": "approvals"})
	if w := callAs(s.v1KeepAliveOn, "user", "POST", "/", "", nil, map[string]string{"id": id}); w.Code != 200 {
		t.Fatalf("keep-alive: %d %s", w.Code, w.Body)
	}
	if w := callAs(s.v1DeleteSandbox, "service", "DELETE", "/", "", nil, map[string]string{"id": id}); w.Code != http.StatusAccepted {
		t.Fatalf("delete of keep-alive sandbox: %d %s", w.Code, w.Body)
	}
}
