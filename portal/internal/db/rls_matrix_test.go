// rls_matrix_test.go — R0 acceptance criteria (PLATFORM_RLS_PLAN §7):
// the four RLS holes L1–L4 from §1.2 must now be rejected (42501 or 0
// rows) when run with SET ROLE portal_app, and the legitimate
// bootstrap-admin path (L4 success) still works.
//
// Hole recap (plan §1.2):
//
//	L1  UPDATE orgs SET plan_id='enterprise' WHERE id=A   (member self-upgrade)
//	L2  UPDATE memberships SET role='owner' WHERE user_id=me  (self-promote)
//	L3  INSERT INTO memberships VALUES (B, me, 'owner')  (join other org as owner)
//	L4  INSERT INTO model_connections (org_id NULL, …)    (anyone writes platform conn)
//
// The first three are blocked by the new per-command policies and the
// orgs_guard trigger; L4 is blocked by the new
// `(SELECT app_platform_role()) IS NOT NULL` arm (R0: password_logins
// = bootstrap admin).
package db

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tastyeffectco/sandboxd/portal/internal/tenants"
)

// isInsufficientPrivilege reports whether err is a 42501
// (insufficient_privilege) — the SQLSTATE RLS and the orgs_guard
// trigger raise when a portal_app row violates a policy or guard.
func isInsufficientPrivilege(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "42501"
	}
	return false
}

// ensureBootstrapLogin inserts a password_logins row for userID so the
// R0 app_platform_role() (which reads password_logins) returns
// 'admin' for that user. Runs as portal_app (0003 grants
// SELECT/INSERT/UPDATE/DELETE on password_logins). The hash is a
// placeholder — these tests only exercise RLS, not password verify.
func ensureBootstrapLogin(t *testing.T, ctx context.Context, pool *pgxpool.Pool, userID string) {
	t.Helper()
	if err := WithTx(ctx, pool, func(tx Tx) error {
		_, err := tx.Exec(ctx, `
                        INSERT INTO password_logins (user_id, username, password_hash)
                        VALUES ($1::uuid, $2, 'placeholder-hash')
                        ON CONFLICT (username) DO UPDATE
                                SET user_id = EXCLUDED.user_id,
                                    password_hash = EXCLUDED.password_hash`,
			userID, "rls-boot-"+userID)
		return err
	}); err != nil {
		t.Fatalf("ensure bootstrap login: %v", err)
	}
}

// TestRLSHardeningL1ToL4 exercises the four RLS holes after migration
// 0012. Each offending statement must fail with 42501 (or affect 0
// rows for UPDATE — Postgres raises 42501 in both cases when WITH
// CHECK fails for a USING-matched row).
func TestRLSHardeningL1ToL4(t *testing.T) {
	ctx := context.Background()
	pool := appPool(t)
	orgA, orgB, userA, _ := setupTwoOrgs(t, pool)

	// --- L1: member updates org's plan_id -----------------------------
	// User A is a member of org A. With app.org_id empty (WithTx) the
	// USING arm of orgs_update matches via EXISTS membership, so the
	// statement reaches the trigger. orgs_guard raises 42501 because
	// plan_id is platform-managed and app_platform_role() is NULL
	// (userA is not a password login).
	ctxA := WithUserID(ctx, userA)
	err := WithTx(ctxA, pool, func(tx Tx) error {
		_, err := tx.Exec(ctx, `
                        UPDATE orgs SET plan_id = 'enterprise' WHERE id = $1::uuid`, orgA)
		return err
	})
	if !isInsufficientPrivilege(err) {
		t.Errorf("L1: UPDATE orgs.plan_id by member = %v, want 42501", err)
	}

	// --- L2: member updates own role to owner -------------------------
	// With app.org_id = orgA the memberships_update USING matches the
	// caller's own row, but WITH CHECK requires user_id <> ME, so the
	// UPDATE fails with 42501.
	err = WithOrg(WithOrgID(ctxA, orgA), pool, orgA, func(tx Tx) error {
		_, err := tx.Exec(ctx, `
                        UPDATE memberships SET role = 'owner'
                        WHERE user_id = NULLIF(current_setting('app.user_id', true), '')::uuid`)
		return err
	})
	if !isInsufficientPrivilege(err) {
		t.Errorf("L2: UPDATE memberships role='owner' WHERE user_id=me = %v, want 42501", err)
	}

	// --- L3: member inserts themselves as owner of another org -------
	// With app.org_id empty the memberships_insert WITH CHECK requires
	// org_id = NULLIF(app.org_id, '') = NULL, which orgB does not match,
	// so the INSERT fails with 42501.
	err = WithTx(ctxA, pool, func(tx Tx) error {
		_, err := tx.Exec(ctx, `
                        INSERT INTO memberships (org_id, user_id, role)
                        VALUES ($1::uuid, $2::uuid, 'owner')`, orgB, userA)
		return err
	})
	if !isInsufficientPrivilege(err) {
		t.Errorf("L3: INSERT INTO memberships (B, me, 'owner') = %v, want 42501", err)
	}

	// --- L4: non-platform-admin inserts a platform model connection ---
	// With app.org_id empty and app.user_id = userA (no password_logins
	// row), the (SELECT app_platform_role()) IS NOT NULL arm is false,
	// so the INSERT fails with 42501.
	err = WithTx(ctxA, pool, func(tx Tx) error {
		_, err := tx.Exec(ctx, `
                        INSERT INTO model_connections
                            (org_id, name, protocol, flavor, base_url, default_model,
                             key_mode, timeout_s, max_tokens, enabled, is_default)
                        VALUES (NULL, $1, 'openai', 'generic', 'http://x.example.test', 'm',
                                'shared', 60, 4096, true, false)`,
			"rls-evil-"+orgA[:8])
		return err
	})
	if !isInsufficientPrivilege(err) {
		t.Errorf("L4: INSERT platform model_connection by non-admin = %v, want 42501", err)
	}
}

// TestRLSHardeningL4BootstrapAdmin verifies the L4 fix doesn't break
// the legitimate bootstrap-admin path: a user with a password_logins
// row (the R0 bootstrap admin) can still insert a platform connection
// when app.org_id is empty. The plan calls this out as the "bản tạm"
// (temporary) — R1 replaces it with membership in the platform org.
func TestRLSHardeningL4BootstrapAdmin(t *testing.T) {
	ctx := context.Background()
	pool := appPool(t)
	var adminID string
	if err := WithTx(ctx, pool, func(tx Tx) error {
		u, _, err := tenants.UpsertUser(ctx, tx,
			"rls-boot-sub", "rls-boot@example.test", "Boot Admin")
		adminID = u.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ensureBootstrapLogin(t, ctx, pool, adminID)

	ctxAdmin := WithUserID(ctx, adminID)
	connName := "rls-platform-" + adminID[:8]
	err := WithTx(ctxAdmin, pool, func(tx Tx) error {
		_, err := tx.Exec(ctx, `
                        INSERT INTO model_connections
                            (org_id, name, protocol, flavor, base_url, default_model,
                             key_mode, timeout_s, max_tokens, enabled, is_default)
                        VALUES (NULL, $1, 'openai', 'generic', 'http://x.example.test', 'm',
                                'shared', 60, 4096, true, false)`, connName)
		return err
	})
	if err != nil {
		t.Fatalf("L4 bootstrap admin: INSERT platform connection = %v, want success", err)
	}

	// Cleanup: the DELETE arm also requires the platform role, so this
	// doubles as a check that the bootstrap admin can remove a platform
	// row.
	if err := WithTx(ctxAdmin, pool, func(tx Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM model_connections WHERE name = $1`, connName)
		return err
	}); err != nil {
		t.Errorf("L4 bootstrap admin: DELETE platform connection = %v, want success", err)
	}
}

// TestRLSHardeningOrgDeleteBlocked ensures portal_app cannot DROP an
// org row (R0: only the table owner / login migration can). This is
// the `orgs_delete USING (false)` policy: no rows are visible to
// DELETE, so the statement affects 0 rows rather than raising.
func TestRLSHardeningOrgDeleteBlocked(t *testing.T) {
	ctx := context.Background()
	pool := appPool(t)
	orgA, _, userA, _ := setupTwoOrgs(t, pool)
	ctxA := WithUserID(ctx, userA)
	var rows int64
	err := WithTx(ctxA, pool, func(tx Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM orgs WHERE id = $1::uuid`, orgA)
		if err != nil {
			return err
		}
		rows = tag.RowsAffected()
		return nil
	})
	if err != nil {
		t.Fatalf("DELETE orgs by portal_app = %v, want nil (0 rows affected)", err)
	}
	if rows != 0 {
		t.Errorf("DELETE orgs affected %d rows, want 0 (USING (false))", rows)
	}
}
