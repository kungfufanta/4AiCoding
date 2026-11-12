// Package usage reads and writes the usage_events ledger (P5). The
// webhook receiver calls Record on every stopped/deleted lifecycle
// event — it computes the sandbox_seconds / vcpu_seconds delta since
// the previous event for that sandbox and inserts an idempotent row.
// The dashboard reads totals via Daily.
package usage

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tastyeffectco/sandboxd/portal/internal/db"
)

// Record is one usage_events row the webhook receiver inserts. The
// source_event_id UNIQUE constraint dedupes replays.
type Record struct {
	OrgID         string
	ProjectID     string // "" → NULL
	SandboxID     string
	Kind          string // sandbox_seconds | vcpu_seconds
	Quantity      int64
	SourceEventID string
	OccurredAt    time.Time
}

// ErrDuplicateSource is returned by Append when the same source_event_id
// has already been applied. The webhook receiver treats this as a
// no-op success (the original delivery won).
var ErrDuplicateSource = fmt.Errorf("duplicate usage source event")

// Append inserts one usage_events row inside the org's RLS context.
// It returns ErrDuplicateSource on a UNIQUE(source_event_id) conflict;
// other errors wrap the underlying pg error.
func Append(ctx context.Context, pool *pgxpool.Pool, r Record) error {
	return db.WithOrg(ctx, pool, r.OrgID, func(tx pgx.Tx) error {
		var project any
		if r.ProjectID != "" {
			project = r.ProjectID
		}
		tag, err := tx.Exec(ctx, `
                        INSERT INTO usage_events
                                (org_id, project_id, sandbox_id, kind, quantity, unit, source_event_id, occurred_at)
                        VALUES ($1::uuid, $2::uuid, $3, $4, $5, 'seconds', $6, $7)
                        ON CONFLICT (source_event_id) DO NOTHING`,
			r.OrgID, project, r.SandboxID, r.Kind, r.Quantity,
			r.SourceEventID, r.OccurredAt)
		if err != nil {
			return fmt.Errorf("insert usage_event: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrDuplicateSource
		}
		return nil
	})
}

// Daily is one day's summed quantity for one kind.
type Daily struct {
	Day      time.Time `json:"day"`
	Kind     string    `json:"kind"`
	Quantity int64     `json:"quantity"`
}

// DailyTotals sums usage_events by day for the org in [from, to). kind
// filters to one of "sandbox_seconds" / "vcpu_seconds"; "" = both.
// Days without events are absent from the result (the UI fills gaps
// with zero).
func DailyTotals(ctx context.Context, pool *pgxpool.Pool, orgID string, from, to time.Time, kind string) ([]Daily, error) {
	var out []Daily
	err := db.WithOrg(ctx, pool, orgID, func(tx pgx.Tx) error {
		args := []any{from, to}
		q := `
                        SELECT date_trunc('day', occurred_at) AS day, kind, COALESCE(SUM(quantity), 0)
                        FROM usage_events
                        WHERE occurred_at >= $1 AND occurred_at < $2`
		if kind != "" {
			q += ` AND kind = $3`
			args = append(args, kind)
		}
		q += ` GROUP BY day, kind ORDER BY day, kind`
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return fmt.Errorf("query daily totals: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var d Daily
			if err := rows.Scan(&d.Day, &d.Kind, &d.Quantity); err != nil {
				return err
			}
			out = append(out, d)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// PlanUsage is the org's running usage against its plan for the
// current month: the sum of vcpu_seconds this month vs. the plan's
// monthly_vcpu_seconds ceiling.
type PlanUsage struct {
	PlanID              string `json:"plan_id"`
	PlanName            string `json:"plan_name"`
	MonthlyVcpuSeconds  int64  `json:"monthly_vcpu_seconds"`
	UsedVcpuSeconds     int64  `json:"used_vcpu_seconds"`
	MaxConcurrentSboxes int    `json:"max_concurrent_sandboxes"`
	RunningSandboxes    int    `json:"running_sandboxes"`
}

// MonthToDate returns the org's month-to-date vcpu_seconds usage and
// the plan ceiling. The cutoff is the first second of the current
// month in the server's timezone; UTC is fine because usage_events
// stores TIMESTAMPTZ.
func MonthToDate(ctx context.Context, pool *pgxpool.Pool, orgID string, now time.Time) (PlanUsage, error) {
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	var p PlanUsage
	err := db.WithOrg(ctx, pool, orgID, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`SELECT plan_id FROM orgs WHERE id = $1::uuid`, orgID).Scan(&p.PlanID); err != nil {
			return fmt.Errorf("load org plan_id: %w", err)
		}
		if err := tx.QueryRow(ctx, `
                        SELECT id, name, max_concurrent_sandboxes, monthly_vcpu_seconds
                        FROM plans WHERE id = $1`, p.PlanID).Scan(
			&p.PlanID, &p.PlanName, &p.MaxConcurrentSboxes, &p.MonthlyVcpuSeconds); err != nil {
			return fmt.Errorf("load plan: %w", err)
		}
		if err := tx.QueryRow(ctx, `
                        SELECT COALESCE(SUM(quantity), 0)::bigint
                        FROM usage_events
                        WHERE kind = 'vcpu_seconds' AND occurred_at >= $1`, monthStart).Scan(&p.UsedVcpuSeconds); err != nil {
			return fmt.Errorf("sum month usage: %w", err)
		}
		if err := tx.QueryRow(ctx, `
                        SELECT COUNT(*) FROM sandboxes
                        WHERE status IN ('pending','running','created','starting')`).Scan(&p.RunningSandboxes); err != nil {
			return fmt.Errorf("count running: %w", err)
		}
		return nil
	})
	if err != nil {
		return PlanUsage{}, err
	}
	return p, nil
}
