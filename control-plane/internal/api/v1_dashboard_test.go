package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/auth"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/netlog"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

func TestV1OverviewCountsAndAdminOnlyParts(t *testing.T) {
	s, _ := newSandboxRowServer(t, 3000, 8080) // one running sandbox, 2 ports
	ctx := context.Background()
	if err := s.Store.Create(ctx, &store.Sandbox{ID: newULID(), Status: "stopped", WorkspaceMnt: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if _, err := createUser(ctx, s.Store, "alice", "password123", auth.RoleOperator); err != nil {
		t.Fatal(err)
	}
	s.NetLog = netlog.New()
	s.NetLog.Record("sb1", netlog.Event{Host: "a.example", Allowed: true})
	s.NetLog.Record("sb1", netlog.Event{Host: "b.example", Allowed: false})
	s.NetLog.Record("sb1", netlog.Event{Host: "old.example", Allowed: false, Time: time.Now().Add(-48 * time.Hour)})

	call := func(role string) overviewResp {
		r := httptest.NewRequest("GET", "/v1/overview", nil)
		r = r.WithContext(auth.WithActor(r.Context(), auth.Actor{Kind: "user", Role: role}))
		w := httptest.NewRecorder()
		s.v1Overview(w, r)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", role, w.Code, w.Body)
		}
		var o overviewResp
		if err := json.Unmarshal(w.Body.Bytes(), &o); err != nil {
			t.Fatal(err)
		}
		return o
	}

	o := call(auth.RoleAdmin)
	if o.Sandboxes.Total != 2 || o.Sandboxes.Running != 1 || o.Sandboxes.Stopped != 1 {
		t.Fatalf("counts: %+v", o.Sandboxes)
	}
	if o.PortsExposed != 2 || len(o.Recent) != 2 {
		t.Fatalf("ports=%d recent=%d", o.PortsExposed, len(o.Recent))
	}
	if o.NetEvents24h.Total != 2 || o.NetEvents24h.Blocked != 1 {
		t.Fatalf("net: %+v", o.NetEvents24h)
	}
	if o.Users == nil || o.Users.Total != 1 || o.Users.Operators != 1 {
		t.Fatalf("users: %+v", o.Users)
	}

	o = call(auth.RoleViewer)
	if o.Users != nil || len(o.Audit) != 0 {
		t.Fatalf("viewer must not see users/audit: %+v %+v", o.Users, o.Audit)
	}
	if o.Sandboxes.Total != 2 {
		t.Fatalf("viewer counts: %+v", o.Sandboxes)
	}
}

func TestV1MonitoringWithoutDocker(t *testing.T) {
	s, _ := newSandboxRowServer(t)
	w := v1Call(s.v1Monitoring, "GET", "/v1/monitoring", "", "")
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var m monResp
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m.Fleet.Total != 1 || m.Fleet.Running != 1 || m.Sandboxes == nil || len(m.History) == 0 {
		t.Fatalf("monitoring: %+v", m)
	}
}

func TestMonHistoryStepAndWindow(t *testing.T) {
	var h monHistory
	t0 := time.Now()
	h.add(t0, monPoint{T: t0.UTC().Format(time.RFC3339)})
	if got := h.add(t0.Add(2*time.Second), monPoint{T: t0.UTC().Format(time.RFC3339)}); len(got) != 1 {
		t.Fatalf("sampled faster than monStep: %d", len(got))
	}
	t1 := t0.Add(monWindow + time.Minute)
	if got := h.add(t1, monPoint{T: t1.UTC().Format(time.RFC3339)}); len(got) != 1 {
		t.Fatalf("window not trimmed: %d", len(got))
	}
}
