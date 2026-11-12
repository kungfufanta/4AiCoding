// Concurrent-sandbox admission (P5). Every path that makes a sandbox
// run — /v1 create, dashboard create, /v1 start, dashboard start —
// goes through admitRun, so the plan ceiling cannot be bypassed from
// the dashboard or by stopping and starting.
//
// The check and the run it admits must be atomic: counting under a
// lock that is released before the sandbox exists lets two concurrent
// creates both see "1 of 2" and both pass. admitRun therefore holds a
// per-org lock until the caller releases it, after the sandbox is
// created/started and its index row written. The lock is in-process,
// which matches the current single-replica deployment (the rate
// limiter has the same scope); a multi-replica Portal needs a
// reservation row instead.
package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/tastyeffectco/sandboxd/portal/internal/db"
	"github.com/tastyeffectco/sandboxd/portal/internal/quota"
)

var (
	// errQuotaExceeded: the org is at its plan's concurrent ceiling.
	errQuotaExceeded = errors.New("quota exceeded")
	// errQuotaCheck wraps a failure to load the plan or count rows.
	errQuotaCheck = errors.New("quota check failed")
)

// orgRunLocks serialises admissions per org (see package comment).
var orgRunLocks sync.Map // org id → *sync.Mutex

// admitRun checks the org's concurrent-sandbox ceiling and, when the
// run is admitted, returns a release func the caller MUST call once
// the sandbox is running and indexed (or the attempt failed). With
// quota disabled, or an org whose plan is missing from the catalogue,
// every run is admitted.
func (s *Server) admitRun(ctx context.Context, orgID string) (func(), error) {
	if !s.QuotaEnabled {
		return func() {}, nil
	}
	v, _ := orgRunLocks.LoadOrStore(orgID, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	plan, err := quota.LoadPlan(ctx, s.Pool, orgID)
	if errors.Is(err, quota.ErrUnknownPlan) {
		return mu.Unlock, nil
	}
	if err != nil {
		mu.Unlock()
		return nil, fmt.Errorf("%w: %v", errQuotaCheck, err)
	}
	if err := quota.CheckConcurrentSandboxes(ctx, s.Pool, orgID, plan); err != nil {
		mu.Unlock()
		if errors.Is(err, quota.ErrExceeded) {
			return nil, errQuotaExceeded
		}
		return nil, fmt.Errorf("%w: %v", errQuotaCheck, err)
	}
	return mu.Unlock, nil
}

// writeAdmitErr answers an admitRun failure; false when err is not one.
func (s *Server) writeAdmitErr(w http.ResponseWriter, orgID string, err error) bool {
	switch {
	case errors.Is(err, errQuotaExceeded):
		writeErr(w, http.StatusTooManyRequests, "quota_exceeded",
			"org has reached its concurrent sandbox quota; stop a sandbox or upgrade the plan")
	case errors.Is(err, errQuotaCheck):
		s.Log.Error("quota check failed", "org_id", orgID, "error", err.Error())
		writeErr(w, http.StatusInternalServerError, "internal", "could not check quota")
	default:
		return false
	}
	return true
}

// markRunning flips the index row to running right after a successful
// start, still under the admission lock, so the next admission counts
// it without waiting for the lifecycle webhook.
func (s *Server) markRunning(ctx context.Context, orgID, id string) {
	if err := db.WithOrg(ctx, s.Pool, orgID, func(tx db.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE sandboxes SET status = 'running', updated_at = now() WHERE id = $1`, id)
		return err
	}); err != nil {
		s.Log.Warn("sandbox index mark running failed", "sandbox_id", id, "error", err.Error())
	}
}
