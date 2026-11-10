// fleet_exec.go — the store slice for sandboxes executed on workers (M7b):
// each worker's API credential and the remote_sandboxes routing table.
package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// NodeAPIToken returns a node's stored (encrypted) API credential, "" when
// the node never sent one. ErrNotFound for an unknown node.
func (s *Store) NodeAPIToken(ctx context.Context, nodeID string) (string, error) {
	var enc string
	err := s.db.QueryRowContext(ctx, `SELECT api_token_enc FROM nodes WHERE node_id = ?`, nodeID).Scan(&enc)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return enc, err
}

// SetNodeAPIToken stores a node's (encrypted) API credential.
func (s *Store) SetNodeAPIToken(ctx context.Context, nodeID, enc string) error {
	return s.submit(ctx, func(db *sql.DB) error {
		res, err := db.ExecContext(ctx, `UPDATE nodes SET api_token_enc = ? WHERE node_id = ?`, enc, nodeID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// RemoteSandbox is one remote_sandboxes row.
type RemoteSandbox struct {
	SandboxID string
	NodeID    string
	CreatedBy string
	CreatedAt time.Time
}

// AddRemoteSandbox records that sandboxID runs on nodeID.
func (s *Store) AddRemoteSandbox(ctx context.Context, sandboxID, nodeID, createdBy string) error {
	return s.submit(ctx, func(db *sql.DB) error {
		_, err := db.ExecContext(ctx, `
			INSERT INTO remote_sandboxes (sandbox_id, node_id, created_by, created_at) VALUES (?, ?, ?, ?)
			ON CONFLICT(sandbox_id) DO UPDATE SET node_id = excluded.node_id`,
			sandboxID, nodeID, createdBy, time.Now().Unix())
		return err
	})
}

// RemoteSandboxFor returns where a remote sandbox runs, or ErrNotFound when
// the sandbox is not remote (or unknown).
func (s *Store) RemoteSandboxFor(ctx context.Context, sandboxID string) (*RemoteSandbox, error) {
	rs := &RemoteSandbox{}
	var created int64
	err := s.db.QueryRowContext(ctx,
		`SELECT sandbox_id, node_id, created_by, created_at FROM remote_sandboxes WHERE sandbox_id = ?`, sandboxID).
		Scan(&rs.SandboxID, &rs.NodeID, &rs.CreatedBy, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	rs.CreatedAt = time.Unix(created, 0).UTC()
	return rs, nil
}

// RemoteSandboxCounts returns the number of remote sandboxes per node.
func (s *Store) RemoteSandboxCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT node_id, COUNT(*) FROM remote_sandboxes GROUP BY node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// DeleteRemoteSandbox forgets a remote sandbox (after it was deleted on its
// worker). Deleting an unknown id is not an error.
func (s *Store) DeleteRemoteSandbox(ctx context.Context, sandboxID string) error {
	return s.submit(ctx, func(db *sql.DB) error {
		_, err := db.ExecContext(ctx, `DELETE FROM remote_sandboxes WHERE sandbox_id = ?`, sandboxID)
		return err
	})
}
