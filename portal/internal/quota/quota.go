// Package quota enforces the plan ceilings (PORTAL_PLAN 6/S4) before
// the gateway creates a sandbox. The check is one short locked
// transaction: count running sandboxes in the org, compare to the
// plan's max_concurrent_sandboxes, refuse on overflow.
//
// The plan row is read from the same transaction so a mid-request plan
// change (an admin switching orgs.plan_id) cannot race the count: the
// check is consistent with the moment of insert. The sandbox row
// itself is inserted by the gateway after this returns ok, so the
// count strictly sees sandboxes that already exist (committed or
// pending in an earlier tx).
package quota

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tastyeffectco/sandboxd/portal/internal/db"
)

// Plan is the slice of plans.* the gateway reads to clamp a create.
type Plan struct {
	ID                     string
	Name                   string
	MaxConcurrentSandboxes int
	MaxVcpuPerSandbox      float64
	MaxTTLSeconds          int
	MonthlyVcpuSeconds     int64
	EgressMode             string
	RateLimitRPS           int
}

// ErrExceeded is returned when the org is at its concurrent sandbox
// ceiling. The gateway maps it to 429 quota_exceeded.
var ErrExceeded = errors.New("quota_exceeded")

// ErrUnknownPlan is returned when orgs.plan_id does not match a row
// in plans (e.g. an org row predates the plans seed). The gateway
// treats this as a server error rather than a quota verdict.
var ErrUnknownPlan = errors.New("unknown_plan")

// LoadPlan reads the plan row for the org's plan_id inside the org's
// RLS context. Returns ErrUnknownPlan when the row is missing.
func LoadPlan(ctx context.Context, pool *pgxpool.Pool, orgID string) (Plan, error) {
	var p Plan
	var planID string
	err := db.WithOrg(ctx, pool, orgID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `SELECT plan_id FROM orgs WHERE id = $1::uuid`, orgID)
		if err := row.Scan(&planID); err != nil {
			return fmt.Errorf("load org plan_id: %w", err)
		}
		return tx.QueryRow(ctx, `
                        SELECT id, name, max_concurrent_sandboxes, max_vcpu_per_sandbox,
                               max_ttl_seconds, monthly_vcpu_seconds, egress_mode, rate_limit_rps
                        FROM plans WHERE id = $1`, planID).Scan(
			&p.ID, &p.Name, &p.MaxConcurrentSandboxes, &p.MaxVcpuPerSandbox,
			&p.MaxTTLSeconds, &p.MonthlyVcpuSeconds, &p.EgressMode, &p.RateLimitRPS)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Plan{}, ErrUnknownPlan
	}
	if err != nil {
		return Plan{}, err
	}
	return p, nil
}

// CheckConcurrentSandboxes counts running sandboxes for orgID and
// refuses when adding one more would exceed the plan ceiling. The
// count is taken inside a transaction that holds an advisory lock
// keyed by org_id, so two concurrent creates in the same org cannot
// both pass: the second waits for the first to commit, then sees the
// new row. Returns ErrExceeded when the next create would overflow.
//
// The advisory lock is scoped to the transaction (pg_advisory_xact_lock,
// released on commit/rollback), keyed by a stable hash of org_id so
// concurrent creates of the SAME org serialise while other orgs are
// unaffected. The sandbox row itself is inserted by the gateway AFTER
// this returns ok; this function only counts and locks.
func CheckConcurrentSandboxes(ctx context.Context, pool *pgxpool.Pool, orgID string, plan Plan) error {
	if plan.MaxConcurrentSandboxes <= 0 {
		// A 0 ceiling means "unlimited" in the plan catalogue (only
		// used by hypothetical custom plans); never refuse.
		return nil
	}
	return db.WithOrg(ctx, pool, orgID, func(tx pgx.Tx) error {
		var lockKey int64
		if err := tx.QueryRow(ctx,
			`SELECT hashtext($1)::bigint`, orgID).Scan(&lockKey); err != nil {
			return fmt.Errorf("compute advisory lock key: %w", err)
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, lockKey); err != nil {
			return fmt.Errorf("advisory lock: %w", err)
		}
		var running int
		if err := tx.QueryRow(ctx, `
                        SELECT COUNT(*) FROM sandboxes
                        WHERE status IN ('pending','running','created','starting')`).Scan(&running); err != nil {
			return fmt.Errorf("count running sandboxes: %w", err)
		}
		if running >= plan.MaxConcurrentSandboxes {
			return ErrExceeded
		}
		return nil
	})
}
