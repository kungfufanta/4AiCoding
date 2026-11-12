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
	"strconv"
	"sync"
	"time"

	"github.com/tastyeffectco/sandboxd/portal/internal/apikeys"
	"github.com/tastyeffectco/sandboxd/portal/internal/db"
	"github.com/tastyeffectco/sandboxd/portal/internal/quota"
	"github.com/tastyeffectco/sandboxd/portal/internal/ratelimit"
)

var (
	// errQuotaExceeded: the org is at its plan's concurrent ceiling.
	errQuotaExceeded = errors.New("quota exceeded")
	// errQuotaCheck wraps a failure to load the plan or count rows.
	errQuotaCheck = errors.New("quota check failed")
)

// orgRunLocks serialises admissions per org (see package comment).
var orgRunLocks sync.Map // org id → *sync.Mutex

// admitRun checks the org's concurrent-sandbox ceiling AND the four
// plan limits (vCPU/TTL/egress/monthly-vCPU), and when the run is
// admitted returns a release func the caller MUST call once the
// sandbox is running and indexed (or the attempt failed). With quota
// disabled, or an org whose plan is missing from the catalogue, every
// run is admitted.
//
// req carries the create request fields the plan limits apply to. It
// is built by the caller from its createSandboxBody so this package
// does not depend on httpapi types.
func (s *Server) admitRun(ctx context.Context, orgID string, req quota.CreateRequest) (func(), error) {
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
	// P5b: per-sandbox limits. CheckConcurrentSandboxes ran its own
	// tx with the advisory lock; CheckPlanLimits opens a second tx
	// (the monthly vCPU sum is read-only and does not need the lock,
	// but the lock above still gates the create that follows).
	if err := quota.CheckPlanLimits(ctx, s.Pool, orgID, plan, req); err != nil {
		mu.Unlock()
		if lv, ok := quota.IsLimitViolation(err); ok {
			return nil, lv
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
		if lv, ok := quota.IsLimitViolation(err); ok {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"error": map[string]any{
					"code":    "plan_limit",
					"message": "the requested sandbox exceeds the org's plan limit",
					"limit":   lv.Limit,
					"used":    lv.Used,
					"field":   lv.Field,
				},
			})
			return true
		}
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

// rateLimit applies the per-key token bucket for one route family.
// Returns true when the caller may proceed; false (after writing the
// 429 response) when the bucket is empty. The refill rate comes from
// the org's plan (plan.rate_limit_rps); a missing plan falls back to
// the free default (2 RPS). The kind picks one of the per-family
// buckets so a files loop cannot starve exec.
//
// The check is best-effort: if LoadPlan fails (e.g. the DB is briefly
// unreachable), the call proceeds rather than failing open on a 500 —
// a misbehaving limiter must not lock the org out of its sandboxes.
func (s *Server) rateLimit(w http.ResponseWriter, key apikeys.Key, kind ratelimit.BucketKind) bool {
	if s.Limiter == nil {
		return true
	}
	plan, _ := quota.LoadPlan(context.Background(), s.Pool, key.OrgID)
	rps := plan.RateLimitRPS
	if rps <= 0 {
		rps = 2 // free plan default; LoadPlan never returns 0 for known plans
	}
	ok, retry := s.Limiter.TakeKind(key.KeyID, kind, rps, 1)
	if ok {
		return true
	}
	secs := int(retry.Round(time.Second).Seconds())
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	writeErr(w, http.StatusTooManyRequests, "rate_limited",
		"too many requests on the "+string(kind)+" bucket; retry after the backoff")
	return false
}
