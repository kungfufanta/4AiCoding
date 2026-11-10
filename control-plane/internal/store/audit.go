package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
)

// AuditRow is one audit_log row.
type AuditRow struct {
	ID             int64  `json:"id"`
	At             int64  `json:"at"`
	ActorKind      string `json:"actor_kind"`
	ActorName      string `json:"actor_name,omitempty"`
	ActorIP        string `json:"actor_ip,omitempty"`
	ExternalUserID string `json:"external_user_id,omitempty"`
	Action         string `json:"action"`
	Target         string `json:"target,omitempty"`
	Detail         string `json:"detail,omitempty"`
	PrevHash       string `json:"prev_hash,omitempty"`
	Hash           string `json:"hash,omitempty"`
}

// ComputeHash returns sha256(prev_hash + "\n" + canonical JSON of the row's
// content). The id is not hashed (it is assigned by SQLite after hashing);
// ordering is protected by prev_hash instead.
func (r AuditRow) ComputeHash() string {
	content, _ := json.Marshal([]any{r.At, r.ActorKind, r.ActorName, r.ActorIP,
		r.ExternalUserID, r.Action, r.Target, r.Detail})
	sum := sha256.Sum256(append([]byte(r.PrevHash+"\n"), content...))
	return hex.EncodeToString(sum[:])
}

const auditCols = `id, at, actor_kind, COALESCE(actor_name,''), COALESCE(actor_ip,''),
	COALESCE(external_user_id,''), action, COALESCE(target,''), COALESCE(detail,''),
	COALESCE(prev_hash,''), COALESCE(hash,'')`

func scanAudit(sc scanner) (AuditRow, error) {
	var a AuditRow
	err := sc.Scan(&a.ID, &a.At, &a.ActorKind, &a.ActorName, &a.ActorIP,
		&a.ExternalUserID, &a.Action, &a.Target, &a.Detail, &a.PrevHash, &a.Hash)
	return a, err
}

// ListAudit returns up to limit rows with id < before (0 = newest), newest
// first, optionally filtered by action and/or target.
func (s *Store) ListAudit(ctx context.Context, before int64, limit int, action, target string) ([]AuditRow, error) {
	q := `SELECT ` + auditCols + ` FROM audit_log WHERE 1=1`
	var args []any
	if before > 0 {
		q += ` AND id < ?`
		args = append(args, before)
	}
	if action != "" {
		q += ` AND action = ?`
		args = append(args, action)
	}
	if target != "" {
		q += ` AND target = ?`
		args = append(args, target)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditRow
	for rows.Next() {
		a, err := scanAudit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AuditVerification is the result of walking the chain.
type AuditVerification struct {
	OK        bool   `json:"ok"`
	Checked   int    `json:"checked"`
	Unchained int    `json:"unchained"` // rows written before the chain existed
	BrokenAt  int64  `json:"broken_at,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// VerifyAudit recomputes every chained row's hash in id order and checks
// that each prev_hash equals the previous row's hash. Any edit, deletion or
// reordering of chained rows breaks it.
func (s *Store) VerifyAudit(ctx context.Context) (AuditVerification, error) {
	var v AuditVerification
	rows, err := s.db.QueryContext(ctx, `SELECT `+auditCols+` FROM audit_log ORDER BY id`)
	if err != nil {
		return v, err
	}
	defer rows.Close()
	prev := ""
	started := false
	for rows.Next() {
		a, err := scanAudit(rows)
		if err != nil {
			return v, err
		}
		if a.Hash == "" {
			if started {
				v.BrokenAt, v.Reason = a.ID, "row without a hash inside the chain"
				return v, nil
			}
			v.Unchained++
			continue
		}
		started = true
		if a.PrevHash != prev {
			v.BrokenAt, v.Reason = a.ID, "prev_hash does not match the previous row (row deleted or reordered)"
			return v, nil
		}
		if a.ComputeHash() != a.Hash {
			v.BrokenAt, v.Reason = a.ID, "hash mismatch (row edited)"
			return v, nil
		}
		prev = a.Hash
		v.Checked++
	}
	if err := rows.Err(); err != nil && err != sql.ErrNoRows {
		return v, err
	}
	v.OK = true
	return v, nil
}
