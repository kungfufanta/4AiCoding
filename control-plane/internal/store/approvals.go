package store

import (
	"context"
	"database/sql"
	"time"
)

// Approval is one human-in-the-loop request (migrations/0031).
type Approval struct {
	ID          string `json:"id"`
	Action      string `json:"action"`
	Target      string `json:"target"`
	Payload     string `json:"payload"`
	PayloadHash string `json:"payload_hash"`
	Status      string `json:"status"`
	RequestedBy string `json:"requested_by"`
	DecidedBy   string `json:"decided_by,omitempty"`
	Reason      string `json:"reason,omitempty"`
	CreatedAt   int64  `json:"created_at"`
	DecidedAt   int64  `json:"decided_at,omitempty"`
	ExpiresAt   int64  `json:"expires_at"`
}

const approvalCols = `id, action, target, payload, payload_hash, status, requested_by,
	COALESCE(decided_by,''), COALESCE(reason,''), created_at, COALESCE(decided_at,0), expires_at`

func scanApproval(sc scanner) (*Approval, error) {
	a := &Approval{}
	err := sc.Scan(&a.ID, &a.Action, &a.Target, &a.Payload, &a.PayloadHash, &a.Status, &a.RequestedBy,
		&a.DecidedBy, &a.Reason, &a.CreatedAt, &a.DecidedAt, &a.ExpiresAt)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return a, err
}

// CreateApproval inserts a pending approval.
func (s *Store) CreateApproval(ctx context.Context, a *Approval) error {
	return s.submit(ctx, func(db *sql.DB) error {
		_, err := db.ExecContext(ctx, `
			INSERT INTO approval (id, action, target, payload, payload_hash, status, requested_by, created_at, expires_at)
			VALUES (?,?,?,?,?,?,?,?,?)`,
			a.ID, a.Action, a.Target, a.Payload, a.PayloadHash, a.Status, a.RequestedBy, a.CreatedAt, a.ExpiresAt)
		return err
	})
}

// GetApproval returns one approval.
func (s *Store) GetApproval(ctx context.Context, id string) (*Approval, error) {
	return scanApproval(s.db.QueryRowContext(ctx, `SELECT `+approvalCols+` FROM approval WHERE id = ?`, id))
}

// ListApprovals returns approvals, newest first, optionally by status.
func (s *Store) ListApprovals(ctx context.Context, status string, limit int) ([]*Approval, error) {
	q := `SELECT ` + approvalCols + ` FROM approval`
	var args []any
	if status != "" {
		q += ` WHERE status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Approval
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// TransitionApproval moves an approval from one status to another (with
// decider/reason/expiry when given). It is a compare-and-set: it returns
// ErrConflict when the approval is not in `from` (already decided or used),
// which is what makes an approval single-use.
func (s *Store) TransitionApproval(ctx context.Context, id, from, to, decidedBy, reason string, expiresAt int64) error {
	return s.submit(ctx, func(db *sql.DB) error {
		q := `UPDATE approval SET status = ?`
		args := []any{to}
		if decidedBy != "" {
			q += `, decided_by = ?, decided_at = ?`
			args = append(args, decidedBy, time.Now().Unix())
		}
		if reason != "" {
			q += `, reason = ?`
			args = append(args, reason)
		}
		if expiresAt > 0 {
			q += `, expires_at = ?`
			args = append(args, expiresAt)
		}
		q += ` WHERE id = ? AND status = ?`
		args = append(args, id, from)
		res, err := db.ExecContext(ctx, q, args...)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrConflict
		}
		return nil
	})
}

// ExpireApprovals marks pending/approved approvals past their expiry.
func (s *Store) ExpireApprovals(ctx context.Context, now int64) error {
	return s.submit(ctx, func(db *sql.DB) error {
		_, err := db.ExecContext(ctx,
			`UPDATE approval SET status = 'expired' WHERE status IN ('pending','approved') AND expires_at < ?`, now)
		return err
	})
}
