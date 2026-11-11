package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Tx helpers (PORTAL_PLAN 3.4, 7): every query that touches tenant
// data runs inside a transaction whose GUCs scope it under Row Level
// Security:
//
//      app.org_id  — org the request acts on; set only after a
//                    membership check, never from client input alone
//      app.user_id — authenticated user
//
// Empty values stay empty strings; the RLS policies NULLIF them into
// NULL so no arm matches.

type ctxKey int

const (
	orgIDKey ctxKey = iota + 1
	userIDKey
)

// WithOrgID stores the acting org id in ctx (used by handlers to
// carry the resolved org into nested calls).
func WithOrgID(ctx context.Context, orgID string) context.Context {
	return context.WithValue(ctx, orgIDKey, orgID)
}

// OrgID returns the acting org id from ctx, or "".
func OrgID(ctx context.Context) string {
	v, _ := ctx.Value(orgIDKey).(string)
	return v
}

// WithUserID stores the authenticated user id in ctx.
func WithUserID(ctx context.Context, userID string) context.Context {
	return context.WithValue(ctx, userIDKey, userID)
}

// UserID returns the authenticated user id from ctx, or "".
func UserID(ctx context.Context) string {
	v, _ := ctx.Value(userIDKey).(string)
	return v
}

// Tx is the transaction handle repositories operate on.
type Tx = pgx.Tx

// NewOrgID mints the UUID of a brand-new org client-side so the RLS
// context can be pinned to it before the row exists (BootstrapTx).
func NewOrgID() (string, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return "", fmt.Errorf("generate org id: %w", err)
	}
	return id.String(), nil
}

// setGUCs pins the RLS context onto a tx.
func setGUCs(ctx context.Context, tx pgx.Tx, orgID, userID string) error {
	_, err := tx.Exec(ctx,
		`SELECT set_config('app.org_id',  $1, true),
                        set_config('app.user_id', $2, true)`,
		orgID, userID)
	if err != nil {
		return fmt.Errorf("set tenant context: %w", err)
	}
	return nil
}

// WithTx runs fn inside one transaction with the RLS context from
// ctx (OrgID/UserID helpers). Read paths and list paths use it.
func WithTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := setGUCs(ctx, tx, OrgID(ctx), UserID(ctx)); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// WithOrg runs fn in a transaction scoped to exactly one org (and,
// when userID is non-empty, the acting user). Writes into other orgs
// are rejected by RLS even if fn's SQL forgets `WHERE org_id`.
func WithOrg(ctx context.Context, pool *pgxpool.Pool, orgID string, fn func(pgx.Tx) error) error {
	if orgID == "" {
		return errors.New("WithOrg: empty org id")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := setGUCs(ctx, tx, orgID, UserID(ctx)); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// BootstrapTx runs fn in a transaction scoped to a brand-new org:
// used by first login, where the org row does not exist yet and the
// caller supplies the freshly generated org id (RLS passes because
// app.org_id equals the id being inserted).
func BootstrapTx(ctx context.Context, pool *pgxpool.Pool, orgID string, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := setGUCs(ctx, tx, orgID, UserID(ctx)); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SetRole pins a pooled connection to the non-owner app role so RLS
// applies even when the login user is a superuser or table owner.
// Empty role disables it (explicit opt-out for local psql debugging).
func SetRole(ctx context.Context, conn *pgxpool.Conn, role string) error {
	if role == "" {
		return nil
	}
	if _, err := conn.Exec(ctx, "SET ROLE "+pgx.Identifier{role}.Sanitize()); err != nil {
		return fmt.Errorf("set role %s: %w", role, err)
	}
	return nil
}

// SetRoleConn is SetRole for the raw pgx connection handed to the
// pool's AfterConnect hook.
func SetRoleConn(ctx context.Context, conn *pgx.Conn, role string) error {
	if role == "" {
		return nil
	}
	if _, err := conn.Exec(ctx, "SET ROLE "+pgx.Identifier{role}.Sanitize()); err != nil {
		return fmt.Errorf("set role %s: %w", role, err)
	}
	return nil
}

// PinUser sets app.user_id inside an open transaction, for flows that
// learn the user id mid-transaction (first login upserts the user,
// then reads that user's memberships).
func PinUser(ctx context.Context, tx pgx.Tx, userID string) error {
	if _, err := tx.Exec(ctx, `SELECT set_config('app.user_id', $1, true)`, userID); err != nil {
		return fmt.Errorf("pin user: %w", err)
	}
	return nil
}
