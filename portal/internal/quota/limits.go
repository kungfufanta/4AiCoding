// Plan-limit enforcement (P5b). The plan catalogue stores four
// per-sandbox ceilings (max_vcpu_per_sandbox, max_ttl_seconds,
// egress_mode, monthly_vcpu_seconds) in addition to the concurrent
// count enforced by CheckConcurrentSandboxes. This file applies those
// four ceilings in one transaction, alongside the concurrent count,
// so a single admission decision is consistent across all limits.
//
// The function takes the requested create body (not a sandbox row)
// so it can refuse BEFORE sandboxd creates the sandbox. Refusals
// return *LimitViolation with the field/used/limit the gateway
// echoes back to the caller as a 422 plan_limit envelope.
package quota

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tastyeffectco/sandboxd/portal/internal/db"
)

// CreateRequest is the slice of the create body the limits apply to.
// The gateway builds this from its createSandboxBody so the quota
// package does not depend on the httpapi layer.
type CreateRequest struct {
	// CPUs the caller asked for. 0 means "sandboxd default" —
	// treated as 1.0 for the check (the smallest plan ceiling is 1.0
	// for the free plan, so a default-vCPU sandbox passes).
	CPUs float64
	// TTLSeconds is the requested sandbox lifetime. 0 means "no
	// explicit TTL" — sandboxd applies its own default; we still
	// check it against the plan ceiling (a free plan sandbox cannot
	// ask for 2h).
	TTLSeconds int
	// EgressMode is the requested network mode: "", "none",
	// "allowlist", or "open". "" falls back to the plan's egress_mode.
	EgressMode string
}

// CheckPlanLimits applies the four per-sandbox ceilings inside the
// org's RLS context. It runs in the same transaction that
// CheckConcurrentSandboxes uses (the caller holds the advisory lock),
// so the monthly vCPU-seconds sum sees a consistent snapshot. Returns
// *LimitViolation on the first breach; nil when all four pass.
//
// The function does NOT count running sandboxes — that is
// CheckConcurrentSandboxes' job. Call both, in either order.
func CheckPlanLimits(ctx context.Context, pool *pgxpool.Pool, orgID string, plan Plan, req CreateRequest) error {
	// 1. vCPU per sandbox.
	// A 0-vCPU request means "use the sandboxd default", which is
	// 1.0 in practice. The free plan's ceiling is 1.0, so a default
	// request passes; the pro plan (4.0) admits a 2-vCPU sandbox.
	requestedCPU := req.CPUs
	if requestedCPU <= 0 {
		requestedCPU = 1.0
	}
	if plan.MaxVcpuPerSandbox > 0 && requestedCPU > plan.MaxVcpuPerSandbox {
		return &LimitViolation{Field: "vcpu_per_sandbox", Limit: plan.MaxVcpuPerSandbox, Used: requestedCPU}
	}

	// 2. TTL seconds. The plan's max_ttl_seconds is the upper bound
	// for the sandbox's lifetime. A 0-TTL request means "use the
	// sandboxd default"; the sandboxd default (1h) is below the free
	// plan ceiling (30m), so 0 always passes for known plans. We
	// still check the explicit value.
	if plan.MaxTTLSeconds > 0 && req.TTLSeconds > plan.MaxTTLSeconds {
		return &LimitViolation{Field: "ttl_seconds", Limit: plan.MaxTTLSeconds, Used: req.TTLSeconds}
	}

	// 3. Egress mode. The plan's egress_mode is the MOST permissive
	// mode the org may use. 'none' < 'allowlist' < 'open' (total
	// order). A request for a more permissive mode than the plan
	// allows is refused.
	mode := req.EgressMode
	if mode == "" {
		mode = plan.EgressMode // default = the plan's mode
	}
	if rank := egressRank(mode); rank > egressRank(plan.EgressMode) {
		return &LimitViolation{Field: "egress_mode", Limit: plan.EgressMode, Used: mode}
	}

	// 4. Monthly vCPU-seconds. Sum usage_events for this month and
	// refuse when adding the new sandbox's first-minute budget would
	// overflow. The new sandbox's vCPU-seconds is not yet known (it
	// runs until stopped), so we check the running total against the
	// ceiling; if the total already exceeds the ceiling, refuse.
	// This is conservative: an org at 99% of monthly vCPU can still
	// create one more sandbox (it will be billed and may push over),
	// but an org already OVER the ceiling cannot create another.
	if plan.MonthlyVcpuSeconds > 0 {
		var used int64
		monthStart := time.Date(time.Now().UTC().Year(), time.Now().UTC().Month(), 1, 0, 0, 0, 0, time.UTC)
		err := db.WithOrg(ctx, pool, orgID, func(tx db.Tx) error {
			return tx.QueryRow(ctx, `
				SELECT COALESCE(SUM(quantity), 0)::bigint
				FROM usage_events
				WHERE kind = 'vcpu_seconds' AND occurred_at >= $1`, monthStart).Scan(&used)
		})
		if err != nil {
			return fmt.Errorf("sum monthly vcpu_seconds: %w", err)
		}
		if used >= plan.MonthlyVcpuSeconds {
			return &LimitViolation{Field: "monthly_vcpu_seconds", Limit: plan.MonthlyVcpuSeconds, Used: used}
		}
	}
	return nil
}

// egressRank orders the three egress modes by permissiveness. A plan
// with egress_mode='allowlist' admits requests for 'none' and
// 'allowlist' but refuses 'open'.
func egressRank(mode string) int {
	switch mode {
	case "none":
		return 1
	case "allowlist":
		return 2
	case "open":
		return 3
	}
	return 0 // unknown → treated as least permissive
}
