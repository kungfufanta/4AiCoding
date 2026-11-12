// Regression tests for the review fixes on P5 (#50) and A1 (#51).
package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tastyeffectco/sandboxd/portal/internal/db"
)

// TestBFFUsageRequiresMembership: a signed-in user who is not a member
// of the org must not read its usage/plan (the handler sets app.org_id
// from the path, so RLS alone lets every row through).
func TestBFFUsageRequiresMembership(t *testing.T) {
	srv, _ := setupP5(t)
	orgA, _, _, cookiesA := newOrgWithKey(t, srv, "usage-owner", "usage-owner@example.test")
	_, _, _, cookiesB := newOrgWithKey(t, srv, "usage-stranger", "usage-stranger@example.test")

	if rec := do(t, srv, http.MethodGet, "/api/orgs/"+orgA+"/usage", "", cookiesA...); rec.Code != http.StatusOK {
		t.Fatalf("member usage = %d: %s", rec.Code, rec.Body.String())
	}
	rec := do(t, srv, http.MethodGet, "/api/orgs/"+orgA+"/usage", "", cookiesB...)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-member usage = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestQuotaCoversDashboardCreateAndStart: the ceiling holds for the
// dashboard create and for start, not only for POST /v1/sandboxes.
func TestQuotaCoversDashboardCreateAndStart(t *testing.T) {
	srv, _ := setupP5(t)
	orgID, _, key, cookies := newOrgWithKey(t, srv, "quota-bff", "quota-bff@example.test")
	insertRunningSandbox(t, srv, orgID, "01QBFF0000000000000000001")
	insertRunningSandbox(t, srv, orgID, "01QBFF0000000000000000002")

	rec := do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/sandboxes", `{"image":"ubuntu:24.04"}`, cookies...)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("dashboard create at ceiling = %d, want 429; body=%s", rec.Code, rec.Body.String())
	}

	// A stopped third sandbox may not be started while two run.
	ctx := context.Background()
	if err := db.WithOrg(ctx, srv.Pool, orgID, func(tx db.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO sandboxes (id, org_id, status, image, created_at, updated_at)
			VALUES ('01QBFF0000000000000000003', $1::uuid, 'stopped', 'ubuntu:24.04', now(), now())`, orgID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if rec := doV1(t, srv, http.MethodPost, "/v1/sandboxes/01QBFF0000000000000000003/start", "", key); rec.Code != http.StatusTooManyRequests {
		t.Errorf("v1 start at ceiling = %d, want 429; body=%s", rec.Code, rec.Body.String())
	}
	if rec := do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/sandboxes/01QBFF0000000000000000003/start", "", cookies...); rec.Code != http.StatusTooManyRequests {
		t.Errorf("dashboard start at ceiling = %d, want 429; body=%s", rec.Code, rec.Body.String())
	}
}

// TestWebhookBillsEachRunOnce: create → stop → delete bills the run
// exactly once, as both sandbox_seconds and vcpu_seconds; a later
// start → delete bills the second run.
func TestWebhookBillsEachRunOnce(t *testing.T) {
	srv, _ := setupP5(t)
	orgID, _, key, _ := newOrgWithKey(t, srv, "bill-user", "bill-user@example.test")
	rec := doV1(t, srv, http.MethodPost, "/v1/sandboxes", `{"image":"ubuntu:24.04"}`, key)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	sbxID := extractString(t, rec.Body.String(), "id")

	// Pretend the run started 100s ago (the fake reports cpus=1).
	ctx := context.Background()
	t0 := time.Now().UTC().Add(-100 * time.Second).Truncate(time.Second)
	if err := db.WithOrg(ctx, srv.Pool, orgID, func(tx db.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE sandboxes SET started_at = $2, vcpu = 2 WHERE id = $1`, sbxID, t0)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	run := time.Now().UnixNano()
	n := 0
	send := func(event string, at time.Time) {
		t.Helper()
		n++
		payload := fmt.Sprintf(`{"id":"bill-%d-%d","event":%q,"sandbox_id":%q,"external":{"workspace_id":%q},"occurred_at":%q}`,
			run, n, event, sbxID, orgID, at.Format(time.RFC3339))
		req := httptest.NewRequest(http.MethodPost, "/api/webhooks/sandboxd", strings.NewReader(payload))
		mac := hmac.New(sha256.New, []byte("test-webhook-secret"))
		mac.Write([]byte(payload))
		req.Header.Set("X-Sandboxd-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s webhook = %d: %s", event, rr.Code, rr.Body.String())
		}
	}
	totals := func() (sec, vcpu int64) {
		t.Helper()
		if err := db.WithOrg(ctx, srv.Pool, orgID, func(tx db.Tx) error {
			return tx.QueryRow(ctx, `SELECT
				COALESCE(SUM(quantity) FILTER (WHERE kind='sandbox_seconds'),0),
				COALESCE(SUM(quantity) FILTER (WHERE kind='vcpu_seconds'),0)
				FROM usage_events WHERE sandbox_id = $1`, sbxID).Scan(&sec, &vcpu)
		}); err != nil {
			t.Fatal(err)
		}
		return
	}

	send("stopped", t0.Add(100*time.Second))
	send("deleted", t0.Add(150*time.Second)) // already stopped: no second bill
	sec, vcpu := totals()
	if sec != 100 || vcpu != 200 {
		t.Fatalf("after stop+delete: sandbox_seconds=%d vcpu_seconds=%d, want 100 and 200", sec, vcpu)
	}
}

// TestStreamedExecReplayDoesNotRerun: reusing an Idempotency-Key on a
// ?stream=ndjson exec must not run the command again.
func TestStreamedExecReplayDoesNotRerun(t *testing.T) {
	srv, fake := setupA1(t)
	orgID, _, key, _ := newOrgWithFilesKey(t, srv, "stream-idem", "stream-idem@example.test")
	seedSandboxRow(t, srv, orgID, "01STREAMIDEM00000000000001")
	idemKey := fmt.Sprintf("stream-%d", time.Now().UnixNano())
	send := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/sandboxes/01STREAMIDEM00000000000001/exec?stream=ndjson",
			strings.NewReader(`{"cmd":["echo","hi"]}`))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Idempotency-Key", idemKey)
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		return rr
	}
	if rr := send(); rr.Code != http.StatusOK {
		t.Fatalf("first stream = %d: %s", rr.Code, rr.Body.String())
	}
	rr := send()
	if rr.Code != http.StatusConflict {
		t.Errorf("replayed stream = %d, want 409; body=%s", rr.Code, rr.Body.String())
	}
	if fake.execCalls != 1 {
		t.Errorf("sandboxd exec calls = %d, want 1 (replay re-ran the command)", fake.execCalls)
	}
}
