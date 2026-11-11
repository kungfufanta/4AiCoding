// v1_workspace_test.go — Portal S3a (PORTAL_PLAN §4): the workspace_id tag
// on the v1 surface, the ?external_workspace_id= list filter, the
// require_external API key flag, and the lifecycle webhook wiring.
package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/loopback"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/webhook"
)

// workspaceServer returns a file-backed server with three rows:
// A and C tagged org-1, B tagged org-2.
func workspaceServer(t *testing.T) (*Server, string, string, string) {
	t.Helper()
	st := fileStore(t)
	lm := loopback.New()
	lm.Root = t.TempDir()
	s := &Server{Store: st, Loopback: lm, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	mk := func(status, workspace, user, project string) string {
		id := newULID()
		sb := &store.Sandbox{ID: id, Status: status,
			ExternalUserID:      nullStr(user),
			ExternalProjectID:   nullStr(project),
			ExternalWorkspaceID: nullStr(workspace)}
		if err := st.Create(context.Background(), sb); err != nil {
			t.Fatal(err)
		}
		return id
	}
	a := mk("running", "org-1", "u1", "")
	b := mk("running", "org-2", "u2", "")
	c := mk("stopped", "org-1", "u3", "p1")
	return s, a, b, c
}

// TestV1WorkspaceTagRoundTrip: GET /v1/sandboxes/{id} returns the stored
// workspace tag under external.workspace_id.
func TestV1WorkspaceTagRoundTrip(t *testing.T) {
	s, a, _, _ := workspaceServer(t)
	w := v1Call(s.v1GetSandbox, "GET", "/v1/sandboxes/"+a, a, "")
	if w.Code != http.StatusOK {
		t.Fatalf("get: %d %s", w.Code, w.Body)
	}
	var sb struct {
		External *struct {
			WorkspaceID string `json:"workspace_id"`
			UserID      string `json:"user_id"`
		} `json:"external"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &sb); err != nil {
		t.Fatal(err)
	}
	if sb.External == nil || sb.External.WorkspaceID != "org-1" || sb.External.UserID != "u1" {
		t.Fatalf("external = %+v", sb.External)
	}
}

// TestV1ListFilterByWorkspace: ?external_workspace_id= narrows the list and
// composes with the other external filters and ?status=.
func TestV1ListFilterByWorkspace(t *testing.T) {
	s, a, b, c := workspaceServer(t)
	cases := []struct {
		query string
		want  []string
	}{
		{"?external_workspace_id=org-1", []string{a, c}},
		{"?external_workspace_id=org-2", []string{b}},
		{"?external_workspace_id=org-3", []string{}},
		{"?external_workspace_id=org-1&status=stopped", []string{c}},
		{"?external_workspace_id=org-1&external_user_id=u1", []string{a}},
		{"", []string{a, b, c}}, // unfiltered
	}
	for _, tc := range cases {
		w := v1Call(s.v1ListSandboxes, "GET", "/v1/sandboxes"+tc.query, "", "")
		if w.Code != http.StatusOK {
			t.Fatalf("list %s: %d %s", tc.query, w.Code, w.Body)
		}
		var out struct {
			Sandboxes []struct {
				ID string `json:"id"`
			} `json:"sandboxes"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if len(out.Sandboxes) != len(tc.want) {
			t.Fatalf("list %s: got %d rows (%v), want %d", tc.query, len(out.Sandboxes), out.Sandboxes, len(tc.want))
		}
		got := map[string]bool{}
		for _, sb := range out.Sandboxes {
			got[sb.ID] = true
		}
		for _, id := range tc.want {
			if !got[id] {
				t.Fatalf("list %s: missing %s", tc.query, id)
			}
		}
	}
}

// workspaceKeyFlow mints a require_external (or plain) key through the real
// console flow and returns its plaintext.
func workspaceKeyFlow(t *testing.T, h http.Handler, name string, requireExternal bool) string {
	t.Helper()
	w := doAuth(t, h, "POST", "/v1/auth/setup", `{"password":"correcthorse"}`, "", "")
	if w.Code != 204 {
		t.Fatalf("setup: %d", w.Code)
	}
	cookie := sessionFrom(t, w)
	body := `{"name":"` + name + `"`
	if requireExternal {
		body += `,"require_external":true`
	}
	body += `}`
	w = doAuth(t, h, "POST", "/v1/api-keys", body, cookie, "")
	if w.Code != 201 {
		t.Fatalf("key create %s: %d %s", body, w.Code, w.Body)
	}
	var created struct {
		Key             string `json:"key"`
		RequireExternal bool   `json:"require_external"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.RequireExternal != requireExternal {
		t.Fatalf("require_external echo = %v, want %v", created.RequireExternal, requireExternal)
	}
	return created.Key
}

// taggedKeyServer builds a server whose image policy rejects everything not
// allowlisted, so a create that passes the workspace gate fails at the image
// policy instead of needing Docker.
func taggedKeyServer(t *testing.T) (http.Handler, *Server, string) {
	t.Helper()
	h, s := authTestServer(t)
	s.Image = "sandboxd-base:0.3.0"
	s.Limits = Limits{ImageAllowlist: []string{"alpine:3.20"}}
	s.Log = slog.New(slog.NewTextHandler(io.Discard, nil)) // the legacy create path logs
	return h, s, "sandboxd-base:0.3.0"
}

// TestV1RequireExternalKeyWithoutTagIs400: the Portal key must tag the org,
// on BOTH the v1 and the legacy surface (no bypass).
func TestV1RequireExternalKeyWithoutTagIs400(t *testing.T) {
	h, _, _ := taggedKeyServer(t)
	key := workspaceKeyFlow(t, h, "portal-key", true)

	// v1: no tag → the workspace gate fires first.
	w := doAuth(t, h, "POST", "/v1/sandboxes", `{"image":"evil:latest"}`, "", key)
	if w.Code != http.StatusBadRequest || v1ErrCodeOf(t, w) != "workspace_required" {
		t.Fatalf("untagged v1 create = %d %s, want 400 workspace_required", w.Code, w.Body)
	}

	// legacy surface: same duty.
	w = doAuth(t, h, "POST", "/sandbox", `{}`, "", key)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "workspace_id") {
		t.Fatalf("untagged legacy create = %d %s", w.Code, w.Body)
	}
}

// TestV1RequireExternalKeyWithTagProceeds: a tagged create passes the gate
// and reaches the (deliberately failing) image policy — proving the gate is
// satisfied, not that the create succeeded.
func TestV1RequireExternalKeyWithTagProceeds(t *testing.T) {
	h, _, _ := taggedKeyServer(t)
	key := workspaceKeyFlow(t, h, "portal-key-tagged", true)

	w := doAuth(t, h, "POST", "/v1/sandboxes",
		`{"external":{"workspace_id":"org-1","user_id":"u1"},"image":"evil:latest"}`, "", key)
	if w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), "workspace_required") {
		t.Fatalf("tagged create = %d %s — must reach image policy, not the workspace gate", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "not allowed") {
		t.Fatalf("tagged create = %d %s — want image policy rejection", w.Code, w.Body)
	}

	// legacy surface, tagged: also past the gate.
	w = doAuth(t, h, "POST", "/sandbox",
		`{"external":{"workspace_id":"org-1"},"image":"evil:latest"}`, "", key)
	if w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), "workspace_id") {
		t.Fatalf("tagged legacy create = %d %s — must reach image policy", w.Code, w.Body)
	}
}

// TestPlainKeyUnaffected: a key without the flag may still create untagged.
func TestPlainKeyUnaffected(t *testing.T) {
	h, _, _ := taggedKeyServer(t)
	key := workspaceKeyFlow(t, h, "plain-key", false)

	w := doAuth(t, h, "POST", "/v1/sandboxes", `{"image":"evil:latest"}`, "", key)
	if w.Code != http.StatusBadRequest || strings.Contains(w.Body.String(), "workspace_required") {
		t.Fatalf("plain key untagged = %d %s — flag must not apply", w.Code, w.Body)
	}
}

// TestAPIKeyListShowsFlag: GET /v1/api-keys reports require_external.
func TestAPIKeyListShowsFlag(t *testing.T) {
	h, _ := authTestServer(t)
	w := doAuth(t, h, "POST", "/v1/auth/setup", `{"password":"correcthorse"}`, "", "")
	cookie := sessionFrom(t, w)
	if w := doAuth(t, h, "POST", "/v1/api-keys", `{"name":"flagged","require_external":true}`, cookie, ""); w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	w = doAuth(t, h, "GET", "/v1/api-keys", "", cookie, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"require_external":true`) {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
}

// captureWebhook is a webhook receiver for the emitLifecycle test.
type captureWebhook struct {
	mu   sync.Mutex
	sigs []string
	ids  []string
	body [][]byte
}

func (c *captureWebhook) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.sigs = append(c.sigs, r.Header.Get("X-Sandboxd-Signature"))
		c.ids = append(c.ids, r.Header.Get("X-Sandboxd-Event-Id"))
		c.body = append(c.body, b)
		c.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}
}

func (c *captureWebhook) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.body)
}

// TestEmitLifecycleWebhook: the created/deleted webhooks carry the row's
// external tags (including workspace_id) and resources, signed with the
// secret, with the event id header matching the body.
func TestEmitLifecycleWebhook(t *testing.T) {
	cap := &captureWebhook{}
	srv := httptest.NewServer(cap.handler())
	defer srv.Close()

	s, a, _, _ := workspaceServer(t)
	s.Webhook = &webhook.Dispatcher{URL: srv.URL, Secret: "whsec-test",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.Webhook.SetTestBackoff(time.Millisecond, time.Second)

	sb, err := s.Store.Get(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	s.emitLifecycle(httptest.NewRequest("POST", "/sandbox", nil), webhook.EventCreated, a, sb)
	s.emitLifecycle(httptest.NewRequest("POST", "/sandbox", nil), webhook.EventDeleted, a, sb)

	deadline := time.Now().Add(5 * time.Second)
	for cap.count() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("only %d deliveries", cap.count())
		}
		time.Sleep(2 * time.Millisecond)
	}

	want := map[string]bool{webhook.EventCreated: true, webhook.EventDeleted: true}
	for i := range cap.body {
		var ev webhook.Event
		if err := json.Unmarshal(cap.body[i], &ev); err != nil {
			t.Fatalf("delivery %d not JSON: %v", i, err)
		}
		if !want[ev.Event] {
			t.Fatalf("delivery %d unexpected event %q (%+v)", i, ev.Event, ev)
		}
		want[ev.Event] = false
		if ev.SandboxID != a {
			t.Fatalf("delivery %d = %+v", i, ev)
		}
		if ev.External == nil || ev.External["workspace_id"] != "org-1" || ev.External["user_id"] != "u1" {
			t.Fatalf("delivery %d external = %+v", i, ev.External)
		}
		if ev.Resources == nil || ev.Resources.MemoryMB <= 0 {
			t.Fatalf("delivery %d resources = %+v", i, ev.Resources)
		}
		if ev.ID == "" || cap.ids[i] != ev.ID {
			t.Fatalf("delivery %d id = %q, header %q", i, ev.ID, cap.ids[i])
		}
		// The signature authenticates the exact body delivered.
		wantSig := (&webhook.Dispatcher{Secret: "whsec-test"}).Sign(cap.body[i])
		if cap.sigs[i] != wantSig {
			t.Fatalf("delivery %d signature = %q, want %q", i, cap.sigs[i], wantSig)
		}
	}

	// No dispatcher → no crash, no delivery.
	s2, a2, _, _ := workspaceServer(t)
	s2.emitLifecycle(httptest.NewRequest("POST", "/sandbox", nil), webhook.EventCreated, a2, nil)
}
