// fleet.go — the store slice for the worker fleet (M6, PLAN §5): the
// `nodes` table (fleet membership, capacity, drain state, heartbeat) and
// `sandbox_placements` (which node a sandbox is assigned to). Reads are
// direct; writes go through the single writer goroutine like every other
// store method.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Node roles. 'local' is the control plane itself (the all-in-one
// executor); 'worker' is a remote fleet member registered over /internal.
const (
	RoleLocal  = "local"
	RoleWorker = "worker"
)

// Node drain states. 'drain' refuses NEW sandboxes; existing ones are
// untouched until an operator moves them (a later milestone).
const (
	DrainActive = "active"
	DrainDrain  = "drain"
)

// Capacity is what a node advertises (worker register/heartbeat, local
// bootstrap). Zero values mean "unreported" and are rendered as 0.
type Capacity struct {
	CPUs     float64 `json:"cpus,omitempty"`
	MemoryMB int     `json:"memory_mb,omitempty"`
	DiskMB   int     `json:"disk_mb,omitempty"`
}

// Usage is what a node currently consumes: the number of live sandboxes
// plus their explicit resource sums. A sandbox without an explicit limit
// runs at the instance default, which is not attributable per-node at the
// store layer, so it is not summed here (documented in /v1/capacity).
type Usage struct {
	Sandboxes int     `json:"sandboxes"`
	CPUs      float64 `json:"cpus,omitempty"`
	MemoryMB  int     `json:"memory_mb,omitempty"`
	DiskMB    int     `json:"disk_mb,omitempty"`
}

// Node is the in-memory mirror of a `nodes` row.
type Node struct {
	NodeID       string
	Role         string
	Address      string
	Version      string
	Capacity     Capacity
	Used         Capacity // worker-reported; zero value = derive from placements
	DrainState   string
	RegisteredAt time.Time
	HeartbeatAt  time.Time
}

// Placement is the in-memory mirror of a `sandbox_placements` row.
type Placement struct {
	SandboxID  string
	NodeID     string
	State      string
	ReservedAt time.Time
}

// ValidNodeID reports whether id is an acceptable node id: 1-63 chars of
// [a-z0-9][a-z0-9.-]. Workers self-report ids, so the format is pinned.
func ValidNodeID(id string) bool {
	if len(id) < 1 || len(id) > 63 {
		return false
	}
	for i, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case (r == '-' || r == '.') && i > 0:
		default:
			return false
		}
	}
	return true
}

func scanNode(sc scanner) (*Node, error) {
	n := &Node{}
	var capacityJSON, usedJSON string
	var registered, heartbeat int64
	err := sc.Scan(&n.NodeID, &n.Role, &n.Address, &n.Version,
		&capacityJSON, &usedJSON, &n.DrainState, &registered, &heartbeat)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(capacityJSON), &n.Capacity)
	_ = json.Unmarshal([]byte(usedJSON), &n.Used)
	if n.DrainState == "" {
		n.DrainState = DrainActive
	}
	n.RegisteredAt = time.Unix(registered, 0).UTC()
	n.HeartbeatAt = time.Unix(heartbeat, 0).UTC()
	return n, nil
}

const nodeSelectCols = `node_id, role, address, version, capacity_json, used_json, drain_state, registered_at, heartbeat_at`

// GetNode returns one fleet member or ErrNotFound.
func (s *Store) GetNode(ctx context.Context, nodeID string) (*Node, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+nodeSelectCols+` FROM nodes WHERE node_id = ?`, nodeID)
	return scanNode(row)
}

// ListNodes returns every fleet member, local node first then by id.
func (s *Store) ListNodes(ctx context.Context) ([]*Node, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+nodeSelectCols+` FROM nodes ORDER BY (role='local') DESC, node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

// UpsertNode registers a fleet member or refreshes its address / version /
// capacity. drain_state is NOT touched here — draining must survive a
// worker re-registering (a worker that restarts does not un-drain itself).
// The heartbeat is stamped, because a successful register IS evidence of
// life. Returns ErrConflict for a bad role.
func (s *Store) UpsertNode(ctx context.Context, n *Node) error {
	if n.Role != RoleLocal && n.Role != RoleWorker {
		return fmt.Errorf("%w: node role %q", ErrConflict, n.Role)
	}
	return s.submit(ctx, func(db *sql.DB) error {
		capJSON, err := json.Marshal(n.Capacity)
		if err != nil {
			return err
		}
		now := time.Now().Unix()
		_, err = db.ExecContext(ctx, `
                        INSERT INTO nodes (node_id, role, address, version, capacity_json, used_json,
                                           drain_state, registered_at, heartbeat_at)
                        VALUES (?, ?, ?, ?, ?, '{}', ?, ?, ?)
                        ON CONFLICT(node_id) DO UPDATE SET
                            role          = excluded.role,
                            address       = excluded.address,
                            version       = excluded.version,
                            capacity_json = excluded.capacity_json,
                            heartbeat_at  = excluded.heartbeat_at`,
			n.NodeID, n.Role, n.Address, n.Version, string(capJSON),
			DrainActive, now, now)
		return err
	})
}

// NodeHeartbeat bumps a node's heartbeat and optionally refreshes its
// advertised capacity and reported usage. Returns the node AFTER the
// update so the worker learns its current drain_state (the control plane
// may have drained it in between). ErrNotFound when the node never
// registered — the worker must re-register.
func (s *Store) NodeHeartbeat(ctx context.Context, nodeID string, capacity *Capacity, used *Capacity) (*Node, error) {
	var out *Node
	err := s.submit(ctx, func(db *sql.DB) error {
		now := time.Now().Unix()
		var err error
		switch {
		case capacity != nil && used != nil:
			capJSON, mErr := json.Marshal(capacity)
			if mErr != nil {
				return mErr
			}
			usedJSON, mErr := json.Marshal(used)
			if mErr != nil {
				return mErr
			}
			_, err = db.ExecContext(ctx,
				`UPDATE nodes SET heartbeat_at = ?, capacity_json = ?, used_json = ? WHERE node_id = ?`,
				now, string(capJSON), string(usedJSON), nodeID)
		case capacity != nil:
			capJSON, mErr := json.Marshal(capacity)
			if mErr != nil {
				return mErr
			}
			_, err = db.ExecContext(ctx,
				`UPDATE nodes SET heartbeat_at = ?, capacity_json = ? WHERE node_id = ?`,
				now, string(capJSON), nodeID)
		default:
			_, err = db.ExecContext(ctx,
				`UPDATE nodes SET heartbeat_at = ? WHERE node_id = ?`, now, nodeID)
		}
		if err != nil {
			return err
		}
		row := db.QueryRowContext(ctx,
			`SELECT `+nodeSelectCols+` FROM nodes WHERE node_id = ?`, nodeID)
		out, err = scanNode(row)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SetNodeDrain flips a node's drain state. ErrNotFound for an unknown id.
func (s *Store) SetNodeDrain(ctx context.Context, nodeID, drainState string) error {
	if drainState != DrainActive && drainState != DrainDrain {
		return fmt.Errorf("%w: drain state %q", ErrConflict, drainState)
	}
	return s.submit(ctx, func(db *sql.DB) error {
		res, err := db.ExecContext(ctx,
			`UPDATE nodes SET drain_state = ?, heartbeat_at = CASE WHEN role='local' THEN ? ELSE heartbeat_at END WHERE node_id = ?`,
			drainState, time.Now().Unix(), nodeID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// DeleteNode removes a fleet member. Placements pointing at it stay (they
// record where sandboxes WERE assigned); the local node cannot be deleted.
func (s *Store) DeleteNode(ctx context.Context, nodeID string) error {
	return s.submit(ctx, func(db *sql.DB) error {
		res, err := db.ExecContext(ctx,
			`DELETE FROM nodes WHERE node_id = ? AND role != 'local'`, nodeID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// --- placements -------------------------------------------------------

// PlaceSandbox records the node assignment of a sandbox. Called inside the
// Create transaction when the sandbox carries a NodeID; also used by tests.
func (s *Store) PlaceSandbox(ctx context.Context, sandboxID, nodeID string) error {
	return s.submit(ctx, func(db *sql.DB) error {
		return placeSandboxTx(ctx, db, sandboxID, nodeID)
	})
}

// dbtx is the shared subset of *sql.DB / *sql.Tx the placement helper
// needs, so it works inside the Create transaction and standalone.
type dbtx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func placeSandboxTx(ctx context.Context, tx dbtx, sandboxID, nodeID string) error {
	_, err := tx.ExecContext(ctx, `
                INSERT INTO sandbox_placements (sandbox_id, node_id, state, reserved_at)
                VALUES (?, ?, 'placed', ?)
                ON CONFLICT(sandbox_id) DO UPDATE SET node_id = excluded.node_id, reserved_at = excluded.reserved_at`,
		sandboxID, nodeID, time.Now().Unix())
	return err
}

// PlacementFor returns the node a sandbox is assigned to, or ErrNotFound
// (including "no placement recorded" — legacy rows and unplaced creates).
func (s *Store) PlacementFor(ctx context.Context, sandboxID string) (*Placement, error) {
	row := s.db.QueryRowContext(ctx, `
                SELECT sandbox_id, node_id, state, reserved_at FROM sandbox_placements WHERE sandbox_id = ?`,
		sandboxID)
	p := &Placement{}
	var reserved int64
	err := row.Scan(&p.SandboxID, &p.NodeID, &p.State, &reserved)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.ReservedAt = time.Unix(reserved, 0).UTC()
	return p, nil
}

// ListPlacements returns every placement, oldest first.
func (s *Store) ListPlacements(ctx context.Context) ([]*Placement, error) {
	rows, err := s.db.QueryContext(ctx, `
                SELECT sandbox_id, node_id, state, reserved_at FROM sandbox_placements ORDER BY reserved_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Placement
	for rows.Next() {
		p := &Placement{}
		var reserved int64
		if err := rows.Scan(&p.SandboxID, &p.NodeID, &p.State, &reserved); err != nil {
			return nil, err
		}
		p.ReservedAt = time.Unix(reserved, 0).UTC()
		out = append(out, p)
	}
	return out, nil
}

// NodeUsage counts the live (creating|running) sandboxes per node from
// placements joined to their spec, so /v1/capacity reflects reality without
// trusting worker reports.
func (s *Store) NodeUsage(ctx context.Context) (map[string]Usage, error) {
	rows, err := s.db.QueryContext(ctx, `
                SELECT p.node_id, COUNT(*),
                       COALESCE(SUM(COALESCE(sp.cpus, 0)), 0),
                       COALESCE(SUM(COALESCE(sp.memory_mb, 0)), 0)
                  FROM sandbox_placements p
                  JOIN sandbox s   ON s.id = p.sandbox_id
                  LEFT JOIN sandbox_spec sp ON sp.sandbox_id = s.id
                 WHERE s.status IN ('creating', 'running')
                 GROUP BY p.node_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Usage{}
	for rows.Next() {
		var nodeID string
		var u Usage
		if err := rows.Scan(&nodeID, &u.Sandboxes, &u.CPUs, &u.MemoryMB); err != nil {
			return nil, err
		}
		out[nodeID] = u
	}
	return out, nil
}
