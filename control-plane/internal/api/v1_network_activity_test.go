package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/netlog"
)

func TestEgressModesAndDenyList(t *testing.T) {
	s, id := newSandboxRowServer(t)
	pv := map[string]string{"id": id}

	// Default (strict) policy: open is refused with a hint.
	w := callAs(s.v1PutEgress, "user", "PUT", "/", `{"mode":"open"}`, nil, pv)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "permissive") {
		t.Fatalf("open under strict: %d %s", w.Code, w.Body)
	}
	// A deny list never needs policy.
	w = callAs(s.v1PutEgress, "user", "PUT", "/", `{"mode":"none","deny":["Evil.example","10.0.0.0/8"]}`, nil, pv)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"mode":"none"`) || !strings.Contains(w.Body.String(), `"10.0.0.0/8"`) {
		t.Fatalf("deny list: %d %s", w.Code, w.Body)
	}
	if w := callAs(s.v1PutEgress, "user", "PUT", "/", `{"mode":"sideways"}`, nil, pv); w.Code != http.StatusForbidden {
		t.Fatalf("bad mode: %d %s", w.Code, w.Body)
	}

	callAs(s.v1PutPolicySection, "user", "PUT", "/", `{"mode":"permissive"}`, nil, map[string]string{"section": "egress"})
	w = callAs(s.v1PutEgress, "user", "PUT", "/", `{"mode":"open"}`, nil, pv)
	var got struct {
		Mode  string   `json:"mode"`
		Allow []string `json:"allow"`
		Deny  []string `json:"deny"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &got) != nil {
		t.Fatalf("open under permissive: %d %s", w.Code, w.Body)
	}
	// Omitted fields keep their value.
	if got.Mode != "open" || len(got.Deny) != 2 || got.Deny[1] != "evil.example" {
		t.Fatalf("merge: %+v", got)
	}
	// The pre-mode body shape only edits the allow list.
	w = callAs(s.v1PutEgress, "user", "PUT", "/", `{"allow":["github.com"]}`, nil, pv)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"mode":"open"`) || !strings.Contains(w.Body.String(), `"github.com"`) {
		t.Fatalf("legacy body: %d %s", w.Code, w.Body)
	}
	// The sandbox view carries the rules.
	w = callAs(s.v1GetSandbox, "user", "GET", "/", "", nil, pv)
	if !strings.Contains(w.Body.String(), `"egress_mode":"open"`) || !strings.Contains(w.Body.String(), `"egress_deny":["10.0.0.0/8","evil.example"]`) {
		t.Fatalf("sandbox view: %s", w.Body)
	}
}

func TestNetworkActivity(t *testing.T) {
	s, id := newSandboxRowServer(t)
	s.NetLog = netlog.New()
	s.EgressProxyURL = "http://proxy:3128"
	pv := map[string]string{"id": id}
	s.NetLog.Record(id, netlog.Event{Method: "CONNECT", Host: "github.com", Port: "443", IP: "140.82.112.3", Allowed: true})
	s.NetLog.Record(id, netlog.Event{Method: "GET", Host: "evil.example", Port: "80", Reason: "blocked"})
	s.NetLog.Record("other", netlog.Event{Host: "x.example"})

	var out struct {
		Recording bool           `json:"recording"`
		Cursor    string         `json:"cursor"`
		Events    []netlog.Event `json:"events"`
		Hosts     []netlog.Host  `json:"hosts"`
		Rules     struct {
			Mode string `json:"mode"`
		} `json:"rules"`
	}
	w := callAs(s.v1NetworkActivity, "user", "GET", "/v1/sandboxes/"+id+"/network/activity", "", nil, pv)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("activity: %d %s", w.Code, w.Body)
	}
	if !out.Recording || len(out.Events) != 2 || len(out.Hosts) != 2 || out.Rules.Mode != "allowlist" || out.Cursor != "3" {
		t.Fatalf("activity = %+v", out)
	}
	w = callAs(s.v1NetworkActivity, "user", "GET", "/v1/sandboxes/"+id+"/network/activity?after=1", "", nil, pv)
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	if len(out.Events) != 1 || out.Events[0].Host != "evil.example" {
		t.Fatalf("after cursor = %+v", out.Events)
	}
	if w := callAs(s.v1NetworkActivity, "user", "GET", "/x?after=abc", "", nil, pv); w.Code != 400 {
		t.Fatalf("bad cursor: %d", w.Code)
	}
}
