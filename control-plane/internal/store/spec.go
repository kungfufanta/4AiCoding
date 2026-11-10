package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Spec is the per-sandbox runtime spec (migrations/0028_sandbox_spec.sql).
// Zero values mean "instance default": Image "" follows SANDBOXD_IMAGE,
// Command nil keeps the image's CMD, CPUs/MemoryMB 0 use the defaults.
type Spec struct {
	Image    string
	Command  []string
	Env      map[string]string
	CPUs     float64
	MemoryMB int
	// IdleTimeoutSecs overrides the instance idle threshold; 0 = default.
	IdleTimeoutSecs int
	// Egress is the hostnames the sandbox may reach through the egress
	// proxy; empty = none (deny-by-default).
	Egress []string
}

// IsZero reports whether the spec carries nothing beyond the defaults.
func (sp Spec) IsZero() bool {
	return sp.Image == "" && len(sp.Command) == 0 && len(sp.Env) == 0 && sp.CPUs == 0 && sp.MemoryMB == 0 &&
		sp.IdleTimeoutSecs == 0 && len(sp.Egress) == 0
}

func (s *Store) specFor(ctx context.Context, id string) (Spec, error) {
	var sp Spec
	var image, cmdJSON, envJSON, egressJSON sql.NullString
	var cpus sql.NullFloat64
	var mem, idle sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT image, command_json, env_json, cpus, memory_mb, idle_timeout_secs, egress_json FROM sandbox_spec WHERE sandbox_id = ?`, id).
		Scan(&image, &cmdJSON, &envJSON, &cpus, &mem, &idle, &egressJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return sp, nil
	}
	if err != nil {
		return sp, err
	}
	sp.Image = image.String
	sp.CPUs = cpus.Float64
	sp.MemoryMB = int(mem.Int64)
	sp.IdleTimeoutSecs = int(idle.Int64)
	if egressJSON.Valid && egressJSON.String != "" {
		if err := json.Unmarshal([]byte(egressJSON.String), &sp.Egress); err != nil {
			return sp, err
		}
	}
	if cmdJSON.Valid && cmdJSON.String != "" {
		if err := json.Unmarshal([]byte(cmdJSON.String), &sp.Command); err != nil {
			return sp, err
		}
	}
	if envJSON.Valid && envJSON.String != "" {
		if err := json.Unmarshal([]byte(envJSON.String), &sp.Env); err != nil {
			return sp, err
		}
	}
	return sp, nil
}

// fill loads the child rows (ports + spec) of a scanned sandbox.
func (s *Store) fill(ctx context.Context, sb *Sandbox) error {
	ports, err := s.portsFor(ctx, sb.ID)
	if err != nil {
		return err
	}
	sb.Ports = ports
	sp, err := s.specFor(ctx, sb.ID)
	if err != nil {
		return err
	}
	sb.Spec = sp
	return nil
}

func putSpec(ctx context.Context, tx *sql.Tx, id string, sp Spec) error {
	if sp.IsZero() {
		_, err := tx.ExecContext(ctx, `DELETE FROM sandbox_spec WHERE sandbox_id = ?`, id)
		return err
	}
	var cmdJSON, envJSON sql.NullString
	if len(sp.Command) > 0 {
		b, err := json.Marshal(sp.Command)
		if err != nil {
			return err
		}
		cmdJSON = sql.NullString{String: string(b), Valid: true}
	}
	if len(sp.Env) > 0 {
		b, err := json.Marshal(sp.Env)
		if err != nil {
			return err
		}
		envJSON = sql.NullString{String: string(b), Valid: true}
	}
	var egressJSON sql.NullString
	if len(sp.Egress) > 0 {
		b, err := json.Marshal(sp.Egress)
		if err != nil {
			return err
		}
		egressJSON = sql.NullString{String: string(b), Valid: true}
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO sandbox_spec (sandbox_id, image, command_json, env_json, cpus, memory_mb, idle_timeout_secs, egress_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(sandbox_id) DO UPDATE SET
		    image = excluded.image, command_json = excluded.command_json,
		    env_json = excluded.env_json, cpus = excluded.cpus, memory_mb = excluded.memory_mb,
		    idle_timeout_secs = excluded.idle_timeout_secs, egress_json = excluded.egress_json`,
		id, sql.NullString{String: sp.Image, Valid: sp.Image != ""}, cmdJSON, envJSON,
		sql.NullFloat64{Float64: sp.CPUs, Valid: sp.CPUs > 0},
		sql.NullInt64{Int64: int64(sp.MemoryMB), Valid: sp.MemoryMB > 0},
		sql.NullInt64{Int64: int64(sp.IdleTimeoutSecs), Valid: sp.IdleTimeoutSecs > 0},
		egressJSON)
	return err
}

// UpdateSpec replaces the stored spec of a sandbox.
func (s *Store) UpdateSpec(ctx context.Context, id string, sp Spec) error {
	return s.submit(ctx, func(db *sql.DB) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(1) FROM sandbox WHERE id = ?`, id).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		if err := putSpec(ctx, tx, id, sp); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sandbox SET updated_at = ? WHERE id = ?`, time.Now().UTC().Unix(), id); err != nil {
			return err
		}
		return tx.Commit()
	})
}

// GetConfig returns the JSON value stored under key, or ErrNotFound.
func (s *Store) GetConfig(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM instance_config WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

// SetConfig upserts a JSON value under key.
func (s *Store) SetConfig(ctx context.Context, key, value string) error {
	return s.submit(ctx, func(db *sql.DB) error {
		_, err := db.ExecContext(ctx, `
			INSERT INTO instance_config (key, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			key, value, time.Now().UTC().Unix())
		return err
	})
}

// SetPorts replaces the preview ports of a sandbox.
func (s *Store) SetPorts(ctx context.Context, id string, ports []int) error {
	return s.submit(ctx, func(db *sql.DB) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(1) FROM sandbox WHERE id = ?`, id).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM sandbox_port WHERE sandbox_id = ?`, id); err != nil {
			return err
		}
		for _, p := range ports {
			if _, err := tx.ExecContext(ctx, `INSERT INTO sandbox_port (sandbox_id, port) VALUES (?, ?)`, id, p); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sandbox SET updated_at = ? WHERE id = ?`, time.Now().UTC().Unix(), id); err != nil {
			return err
		}
		return tx.Commit()
	})
}

// SetIdlePolicy sets idle_policy ("sleep" | "always_on").
func (s *Store) SetIdlePolicy(ctx context.Context, id, idlePolicy string) error {
	return s.submit(ctx, func(db *sql.DB) error {
		res, err := db.ExecContext(ctx, `UPDATE sandbox SET idle_policy = ?, updated_at = ? WHERE id = ?`,
			idlePolicy, time.Now().UTC().Unix(), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// RunningByIP returns the running sandbox whose recorded container IP is ip
// (the egress proxy's source identity), or ErrNotFound.
func (s *Store) RunningByIP(ctx context.Context, ip string) (*Sandbox, error) {
	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM sandbox WHERE container_ip = ? AND status = 'running' LIMIT 1`, ip).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}
