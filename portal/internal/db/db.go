// Package db opens the Portal's Postgres connection pool and applies
// embedded SQL migrations at boot (PORTAL_PLAN 2: "migration SQL
// thuần", 3.4: Postgres là database duy nhất của Portal).
package db

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tastyeffectco/sandboxd/portal/migrations"
)

// PoolDefaults are the pool knobs used by Open; small on purpose —
// the Portal is not the data-plane hot path.
const (
	maxConns         = 10
	connectTimeout   = 10 * time.Second
	pingReadyTimeout = 2 * time.Second
)

// Open connects to databaseURL and returns a pgx pool. dbRole names
// the non-owner role every pooled connection assumes (SET ROLE) so
// Row Level Security applies even when the login user is a superuser
// or the table owner; empty disables the switch.
func Open(ctx context.Context, log *slog.Logger, databaseURL, dbRole string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	cfg.MaxConns = maxConns
	cfg.ConnConfig.ConnectTimeout = connectTimeout
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 15 * time.Minute
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		return SetRoleConn(ctx, conn, dbRole)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	// Fail fast on a bad DSN rather than on the first request.
	pingCtx, cancel := context.WithTimeout(ctx, pingReadyTimeout)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	log.Info("database connected", "max_conns", maxConns, "db_role", dbRole)
	return pool, nil
}

// Migrate applies every embedded migration that the portal_migrations
// table doesn't know yet. Each file runs in its own transaction; an
// advisory lock keeps concurrent replicas from racing (Kubernetes
// rolling updates start the new pod before the old one exits).
func Migrate(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) error {
	files, err := migrations.FS.ReadDir(".")
	if err != nil {
		return fmt.Errorf("read embedded migrations: %w", err)
	}
	names := make([]string, 0, len(files))
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".sql") {
			continue
		}
		names = append(names, f.Name())
	}
	sort.Strings(names)
	if err := ensureTrackingTable(ctx, pool); err != nil {
		return err
	}
	for _, name := range names {
		if err := applyOne(ctx, pool, name, log); err != nil {
			return err
		}
	}
	return nil
}

// ensureTrackingTable creates portal_migrations in its own
// transaction, guarded by the same advisory lock used by applyOne.
func ensureTrackingTable(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin ensure portal_migrations: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(0x706f7274)"); err != nil {
		return fmt.Errorf("lock ensure portal_migrations: %w", err)
	}
	if _, err := tx.Exec(ctx, `
                CREATE TABLE IF NOT EXISTS portal_migrations (
                        name       TEXT PRIMARY KEY,
                        applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
                )`); err != nil {
		return fmt.Errorf("ensure portal_migrations: %w", err)
	}
	return tx.Commit(ctx)
}

func applyOne(ctx context.Context, pool *pgxpool.Pool, name string, log *slog.Logger) error {
	body, err := migrations.FS.ReadFile(name)
	if err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin %s: %w", name, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// 0x706f7274 = "port". Any fixed bigserial keeps pods from racing.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(0x706f7274)"); err != nil {
		return fmt.Errorf("lock %s: %w", name, err)
	}
	var applied bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM portal_migrations WHERE name = $1)", name).Scan(&applied); err != nil {
		return fmt.Errorf("check %s: %w", name, err)
	}
	if applied {
		log.Debug("migration already applied", "name", name)
		return nil
	}
	if _, err := tx.Exec(ctx, string(body)); err != nil {
		return fmt.Errorf("apply %s: %w", name, err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO portal_migrations (name) VALUES ($1)", name); err != nil {
		return fmt.Errorf("record %s: %w", name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit %s: %w", name, err)
	}
	log.Info("migration applied", "name", name)
	return nil
}
