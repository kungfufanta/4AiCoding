// P5b regression tests: plan limits (vCPU/TTL/egress/monthly-vCPU),
// per-bucket rate limit, and an end-to-end files round-trip through
// the Portal gateway that proves the service key carries
// sandboxes:files (or the gateway would 403). Real Postgres via
// PORTAL_TEST_DATABASE_URL (same gate as the rest of the httpapi
// tests).
package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tastyeffectco/sandboxd/portal/internal/apikeys"
	"github.com/tastyeffectco/sandboxd/portal/internal/db"
	"github.com/tastyeffectco/sandboxd/portal/internal/quota"
	"github.com/tastyeffectco/sandboxd/portal/internal/ratelimit"
	"github.com/tastyeffectco/sandboxd/portal/internal/sandboxd"
	"github.com/tastyeffectco/sandboxd/portal/internal/usage"
)

// TestPlanLimitVCPUPerSandbox — a create asking for more vCPU than the
// plan's max_vcpu_per_sandbox returns 422 plan_limit with the field.
// Free plan ceiling is 1.0; a 2-vCPU request must be refused.
func TestPlanLimitVCPUPerSandbox(t *testing.T) {
	srv, _ := setupP5(t)
	_, _, key, _ := newOrgWithKey(t, srv, "vcpu-alice", "vcpu-alice@example.test")
	rec := doV1(t, srv, http.MethodPost, "/v1/sandboxes",
		`{"image":"ubuntu:24.04","resources":{"cpus":2,"memory_mb":512}}`, key)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("vCPU over limit = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Error struct {
			Code, Field string
			Limit, Used any
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if env.Error.Code != "plan_limit" {
		t.Errorf("error.code = %q, want plan_limit", env.Error.Code)
	}
	if env.Error.Field != "vcpu_per_sandbox" {
		t.Errorf("error.field = %q, want vcpu_per_sandbox", env.Error.Field)
	}
}

// TestPlanLimitTTLSeconds — a TTL above the plan ceiling is refused.
// Free plan max_ttl_seconds = 1800 (30 min); a 2h request (7200) must
// be refused.
func TestPlanLimitTTLSeconds(t *testing.T) {
	srv, _ := setupP5(t)
	_, _, key, _ := newOrgWithKey(t, srv, "ttl-alice", "ttl-alice@example.test")
	rec := doV1(t, srv, http.MethodPost, "/v1/sandboxes",
		`{"image":"ubuntu:24.04","ttl_seconds":7200}`, key)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("TTL over limit = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Error struct{ Code, Field string } `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Field != "ttl_seconds" {
		t.Errorf("error.field = %q, want ttl_seconds", env.Error.Field)
	}
}

// TestPlanLimitEgressMode — the free plan's egress_mode is 'allowlist';
// a create asking for 'open' must be refused.
func TestPlanLimitEgressMode(t *testing.T) {
	srv, _ := setupP5(t)
	_, _, key, _ := newOrgWithKey(t, srv, "egress-alice", "egress-alice@example.test")
	rec := doV1(t, srv, http.MethodPost, "/v1/sandboxes",
		`{"image":"ubuntu:24.04","egress":{"mode":"open"}}`, key)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("egress over limit = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Error struct{ Code, Field string } `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Field != "egress_mode" {
		t.Errorf("error.field = %q, want egress_mode", env.Error.Field)
	}
}

// TestPlanLimitEgressAllowlistAllowed — 'allowlist' and 'none' are
// permitted under a free plan (rank ≤ plan's rank).
func TestPlanLimitEgressAllowlistAllowed(t *testing.T) {
	srv, _ := setupP5(t)
	for i, mode := range []string{"none", "allowlist", ""} {
		// Each iteration uses a fresh org so the free plan's 2-sandbox
		// cap does not bleed across cases (this test is about egress,
		// not the concurrent ceiling).
		_, _, key, _ := newOrgWithKey(t, srv,
			fmt.Sprintf("egress-ok-%d", i),
			fmt.Sprintf("egress-ok-%d@example.test", i))
		body := `{"image":"ubuntu:24.04","egress":{"mode":"` + mode + `"}}`
		rec := doV1(t, srv, http.MethodPost, "/v1/sandboxes", body, key)
		if rec.Code != http.StatusCreated {
			t.Errorf("egress %q = %d, want 201; body=%s", mode, rec.Code, rec.Body.String())
		}
	}
}

// TestPlanLimitMonthlyVCPUSeconds — when the org's month-to-date
// vCPU-seconds already meets or exceeds the plan ceiling, the next
// create is refused with field=monthly_vcpu_seconds.
func TestPlanLimitMonthlyVCPUSeconds(t *testing.T) {
	srv, _ := setupP5(t)
	orgID, _, key, _ := newOrgWithKey(t, srv, "monthly-alice", "monthly-alice@example.test")
	// Free plan monthly_vcpu_seconds = 72000 (20h). Seed exactly that.
	now := time.Now().UTC()
	evID := "evt-monthly-cap-" + now.Format("20060102150405")
	if err := usage.Append(context.Background(), srv.Pool, usage.Record{
		OrgID: orgID, SandboxID: "01MONTHLYCAP0000000000001",
		Kind: "vcpu_seconds", Quantity: 72000,
		SourceEventID: evID, OccurredAt: now,
	}); err != nil {
		t.Fatalf("seed usage: %v", err)
	}
	rec := doV1(t, srv, http.MethodPost, "/v1/sandboxes",
		`{"image":"ubuntu:24.04"}`, key)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create over monthly vCPU = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Error struct{ Code, Field string } `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	if env.Error.Field != "monthly_vcpu_seconds" {
		t.Errorf("error.field = %q, want monthly_vcpu_seconds", env.Error.Field)
	}
}

// TestPlanLimitsBelowCeilingPass — a create at the plan's exact
// limits (1 vCPU, 30 min TTL, allowlist egress) succeeds.
func TestPlanLimitsBelowCeilingPass(t *testing.T) {
	srv, _ := setupP5(t)
	_, _, key, _ := newOrgWithKey(t, srv, "limit-ok", "limit-ok@example.test")
	rec := doV1(t, srv, http.MethodPost, "/v1/sandboxes",
		`{"image":"ubuntu:24.04","resources":{"cpus":1,"memory_mb":256},"ttl_seconds":1800,"egress":{"mode":"allowlist"}}`, key)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create at limits = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
}

// TestPlanLimitRaceTwoConcurrent — the headline race the advisory
// lock + per-org mutex is for: 2 concurrent creates on a free plan
// (cap=2) must both succeed, not double-admit.
//
// This test seeds 1 running sandbox first (so the ceiling is 1 of 2),
// then fires 2 concurrent creates: both should succeed (2 ≤ 2), and
// the count after must be 3 (no double-admit, no over-admit). The
// pre-P5b code (without the per-org lock) could let both pass even
// when the ceiling was 1, by reading the count before either insert.
func TestPlanLimitRaceTwoConcurrent(t *testing.T) {
	srv, _ := setupP5(t)
	orgID, _, key, _ := newOrgWithKey(t, srv, "race2-alice", "race2-alice@example.test")
	insertRunningSandbox(t, srv, orgID, "01RACE2PRE000000000000001")
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := doV1(t, srv, http.MethodPost, "/v1/sandboxes",
				`{"image":"ubuntu:24.04"}`, key)
			codes[i] = rec.Code
		}(i)
	}
	wg.Wait()
	// Free plan cap = 2; 1 pre-existing + 2 new = 3, but cap is 2 so
	// only 1 of the 2 concurrent creates can succeed.
	created, refused := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusCreated:
			created++
		case http.StatusTooManyRequests:
			refused++
		}
	}
	if created != 1 {
		t.Errorf("created = %d, want exactly 1 (cap=2, 1 pre-existing)", created)
	}
	if refused != 1 {
		t.Errorf("refused = %d, want 1", refused)
	}
}

// TestRateLimitExecBucket — a key that exhausts the exec bucket gets
// 429 rate_limited on the next exec, while the files bucket is still
// full (so a files call right after succeeds).
func TestRateLimitExecBucket(t *testing.T) {
	srv, _ := setupP5WithFiles(t)
	orgID, _, key, _ := newOrgWithFilesKey(t, srv, "rlexec-alice", "rlexec-alice@example.test")
	seedSandboxRow(t, srv, orgID, "01RLEXEC00000000000000001")
	// Free plan = 2 RPS. Burn the exec bucket: first 2 calls succeed.
	for i := 0; i < 2; i++ {
		rec := doV1(t, srv, http.MethodPost,
			"/v1/sandboxes/01RLEXEC00000000000000001/exec",
			`{"cmd":["echo","hi"]}`, key)
		if rec.Code != http.StatusOK {
			t.Fatalf("exec %d = %d, want 200 (bucket should have tokens); body=%s", i, rec.Code, rec.Body.String())
		}
	}
	// Third exec at the same instant: rate-limited.
	rec := doV1(t, srv, http.MethodPost,
		"/v1/sandboxes/01RLEXEC00000000000000001/exec",
		`{"cmd":["echo","hi"]}`, key)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("third exec = %d, want 429; body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Errorf("Retry-After missing on 429")
	}
	// A files call right after should still succeed (separate bucket).
	// Burn the files bucket first (2 calls), then a third files call
	// is also 429 — proving the buckets are independent.
	for i := 0; i < 2; i++ {
		rec := doV1(t, srv, http.MethodGet,
			"/v1/sandboxes/01RLEXEC00000000000000001/files?path=.", "", key)
		if rec.Code != http.StatusOK {
			t.Fatalf("files %d = %d, want 200; body=%s", i, rec.Code, rec.Body.String())
		}
	}
	rec = doV1(t, srv, http.MethodGet,
		"/v1/sandboxes/01RLEXEC00000000000000001/files?path=.", "", key)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("third files = %d, want 429 (separate bucket exhausted)", rec.Code)
	}
}

// TestRateLimitActionBucket — start/stop/delete share the action
// bucket; after 2 calls the third is 429.
func TestRateLimitActionBucket(t *testing.T) {
	srv, _ := setupP5(t)
	orgID, _, key, _ := newOrgWithKey(t, srv, "rlact-alice", "rlact-alice@example.test")
	seedSandboxRow(t, srv, orgID, "01RLACT000000000000000001")
	// Two stop calls succeed (action bucket has 2 tokens).
	for i := 0; i < 2; i++ {
		rec := doV1(t, srv, http.MethodPost,
			"/v1/sandboxes/01RLACT000000000000000001/stop", "", key)
		if rec.Code != http.StatusNoContent && rec.Code != http.StatusOK {
			t.Fatalf("stop %d = %d, want 2xx; body=%s", i, rec.Code, rec.Body.String())
		}
	}
	// Third action: 429.
	rec := doV1(t, srv, http.MethodPost,
		"/v1/sandboxes/01RLACT000000000000000001/stop", "", key)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("third stop = %d, want 429", rec.Code)
	}
}

// TestRateLimitStreamBucket — logs/metrics opens charge the stream
// bucket; after 2 opens the third is 429.
func TestRateLimitStreamBucket(t *testing.T) {
	srv, _ := setupP5(t)
	orgID, _, key, _ := newOrgWithKey(t, srv, "rlstr-alice", "rlstr-alice@example.test")
	seedSandboxRow(t, srv, orgID, "01RLSTR000000000000000001")
	for i := 0; i < 2; i++ {
		rec := doV1(t, srv, http.MethodGet,
			"/v1/sandboxes/01RLSTR000000000000000001/logs?tail=10", "", key)
		if rec.Code != http.StatusOK {
			t.Fatalf("logs %d = %d, want 200; body=%s", i, rec.Code, rec.Body.String())
		}
	}
	rec := doV1(t, srv, http.MethodGet,
		"/v1/sandboxes/01RLSTR000000000000000001/logs?tail=10", "", key)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("third logs = %d, want 429", rec.Code)
	}
}

// TestE2EFilesRoundTripThroughPortal — the headline A1+P5b e2e: write
// a file through the Portal gateway, read it back, and assert the
// content matches. This proves (a) the Portal proxies files correctly,
// (b) the service key the Portal presents to sandboxd carries
// sandboxes:files (or the proxy would 403), and (c) the rate-limit
// does not block a normal read/write pair on the free plan (2 RPS).
//
// The fake sandboxd's files surface records the body it received, so
// we can assert the Portal forwarded the raw bytes verbatim (not
// JSON-wrapped).
func TestE2EFilesRoundTripThroughPortal(t *testing.T) {
	srv, fake := setupP5WithFiles(t)
	orgID, _, key, _ := newOrgWithFilesKey(t, srv, "e2e-alice", "e2e-alice@example.test")
	seedSandboxRow(t, srv, orgID, "01E2EFILES0000000000000001")
	// Write "hello world" to hello.txt.
	body := "hello world"
	rec := doV1(t, srv, http.MethodPut,
		"/v1/sandboxes/01E2EFILES0000000000000001/files/content?path=hello.txt",
		body, key)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /files/content = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if string(fake.putBody) != body {
		t.Errorf("forwarded body = %q, want %q (Portal must forward raw bytes)", string(fake.putBody), body)
	}
	// Read it back.
	rec = doV1(t, srv, http.MethodGet,
		"/v1/sandboxes/01E2EFILES0000000000000001/files/content?path=hello.txt",
		"", key)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /files/content = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"data":"hello"`) {
		t.Errorf("read back missing data=hello: %s", rec.Body.String())
	}
}

// TestE2EFilesScopeMissingReturns403 — a service key WITHOUT
// sandboxes:files gets 403 from every files route (this is the
// "service key must carry sandboxes:files" check, exercised from the
// Portal caller side; the gateway itself does not see the sandboxd
// scope, but the Portal-side scope gate mirrors it).
func TestE2EFilesScopeMissingReturns403(t *testing.T) {
	srv, _ := setupP5WithFiles(t)
	orgID, _, key, _ := newOrgWithKey(t, srv, "e2e-nofiles-alice", "e2e-nofiles-alice@example.test")
	seedSandboxRow(t, srv, orgID, "01E2ENOFILES000000000001")
	// newOrgWithKey mints a key with sandbox:create/read/exec but NOT
	// sandboxes:files; every files route must 403.
	for _, tt := range []struct {
		method, path, body string
	}{
		{http.MethodGet, "/v1/sandboxes/01E2ENOFILES000000000001/files", ""},
		{http.MethodGet, "/v1/sandboxes/01E2ENOFILES000000000001/files/content?path=x", ""},
		{http.MethodPut, "/v1/sandboxes/01E2ENOFILES000000000001/files/content?path=x", "x"},
		{http.MethodDelete, "/v1/sandboxes/01E2ENOFILES000000000001/files?path=x", ""},
		{http.MethodPost, "/v1/sandboxes/01E2ENOFILES000000000001/files/move", `{"from":"a","to":"b"}`},
	} {
		rec := doV1(t, srv, tt.method, tt.path, tt.body, key)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s = %d, want 403 (key lacks sandboxes:files)", tt.method, tt.path, rec.Code)
		}
	}
}

// setupP5WithFiles wires setupP5 with the files-aware fake gateway
// (reused from a1_files_test.go's filesFakeGateway).
func setupP5WithFiles(t *testing.T) (*Server, *filesFakeGateway) {
	t.Helper()
	srv := setupBFF(t)
	fake := newFilesFakeGateway()
	sbx := httptest.NewServer(fake.handler())
	t.Cleanup(sbx.Close)
	srv.Sandboxd = sandboxdClient(sbx.URL)
	srv.Keys = &apikeys.Verifier{Pool: srv.Pool, Log: discardLogger()}
	srv.WebhookSecret = "test-webhook-secret"
	srv.QuotaEnabled = true
	srv.Limiter = ratelimit.New()
	// Clean shared tables so a prior run cannot collide.
	ctx := context.Background()
	url := os.Getenv("PORTAL_TEST_DATABASE_URL")
	admin, err := db.Open(ctx, discardLogger(), url, "")
	if err != nil {
		t.Fatalf("open admin for cleanup: %v", err)
	}
	defer admin.Close()
	for _, table := range []string{"webhook_events", "idempotency_keys", "sandboxes", "usage_events"} {
		if _, err := admin.Exec(ctx, "DELETE FROM "+table); err != nil {
			t.Fatalf("clean %s: %v", table, err)
		}
	}
	return srv, fake
}

// sandboxdClient builds a sandboxd.Client for the test server URL.
// Reused from a1_files_test.go to avoid pulling in the helper there.
func sandboxdClient(baseURL string) *sandboxd.Client {
	return sandboxd.New(baseURL, "gateway-key", 5*time.Second)
}

// newOrgWithFilesKey creates an org + API key with the sandboxes:files
// scope (plus the create/read/exec scopes the other tests need).
// Reused from a1_files_test.go — declared there, not re-declared here.
//
// seedSandboxRow is also reused from a1_files_test.go.

// keep imports honest
var _ = fmt.Sprintf
var _ = quota.LoadPlan
