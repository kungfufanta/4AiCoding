// rls_matrix_test.go — R0 acceptance criteria (PLATFORM_RLS_PLAN §7):
// the four RLS holes L1–L4 from §1.2 must now be rejected (42501 or 0
// rows) when run with SET ROLE portal_app, the legitimate bootstrap-
// admin path (L4 success) still works, and the role-based guards added
// in the review fix (member cannot demote/promote/remove others; only
// owner/admin can) hold.
//
// Hole recap (plan §1.2):
//
//	L1  UPDATE orgs SET plan_id='enterprise' WHERE id=A   (member self-upgrade)
//	L2  UPDATE memberships SET role='owner' WHERE user_id=me  (self-promote)
//	L3  INSERT INTO memberships VALUES (B, me, 'owner')  (join other org as owner)
//	L4  INSERT INTO model_connections (org_id NULL, …)    (anyone writes platform conn)
//
// Review fix guards (PR #95 review):
//
//	M1  member demoting owner → 0 rows
//	M2  member promoting someone else to admin → 0 rows
//	M3  member deleting owner → 0 rows
//	M4  member updating org name → 0 rows
//	M5  admin changing member's role → succeeds (handleSetMemberRole path)
//	M6  member self-leaving (own row, role <> owner) → succeeds
//	M7  owner cannot self-leave (role = 'owner') → 0 rows
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

// addMember creates a user and adds them to orgID with the given role
// via the same invite+accept path the BFF uses (CreateInvite +
// accept_invite DEFINER). The owner of the org (ownerID) issues the
// invite; the new member accepts with their own email. Returns the new
// user id.
func addMember(t *testing.T, ctx context.Context, pool *pgxpool.Pool, orgID, ownerID, email, role string) string {
	t.Helper()
	// Create the user row first (no org context — users is global).
	var memberID string
	if err := WithTx(ctx, pool, func(tx Tx) error {
		u, _, err := tenants.UpsertUser(ctx, tx, "rls-"+email, email, "Member "+email)
		memberID = u.ID
		return err
	}); err != nil {
		t.Fatalf("upsert member %s: %v", email, err)
	}
	// Owner creates the invite inside the org's RLS context.
	ownerCtx := WithOrgID(WithUserID(ctx, ownerID), orgID)
	var token string
	if err := WithOrg(ownerCtx, pool, orgID, func(tx Tx) error {
		_, t2, err := tenants.CreateInvite(ctx, tx, orgID, email, role, ownerID, 3600e9)
		token = t2
		return err
	}); err != nil {
		t.Fatalf("create invite %s: %v", email, err)
	}
	// The new member accepts (accept_invite DEFINER bypasses RLS on
	// the membership INSERT).
	memberCtx := WithOrgID(WithUserID(ctx, memberID), orgID)
	if err := WithOrg(memberCtx, pool, orgID, func(tx Tx) error {
		_, err := tenants.AcceptInvite(ctx, tx, orgID, token, memberID, email)
		return err
	}); err != nil {
		t.Fatalf("accept invite %s: %v", email, err)
	}
	return memberID
}

// TestRLSHardeningL1ToL4 exercises the four RLS holes after migration
// 0012. Each offending statement must fail with 42501 or affect 0
// rows (plan §7 R0 acceptance criterion accepts either).
func TestRLSHardeningL1ToL4(t *testing.T) {
	ctx := context.Background()
	pool := appPool(t)
	orgA, orgB, userA, _ := setupTwoOrgs(t, pool)

	// --- L1: member updates org's plan_id -----------------------------
	// Two layers block this now:
	// (a) when app.org_id is empty, orgs_update USING requires
	//     id = ORG (= NULL), so the row is invisible — 0 rows, no
	//     trigger.
	// (b) when app.org_id = orgA, USING requires R IN (owner, admin).
	//     userA is the owner of orgA, so USING matches; the trigger
	//     then raises 42501 because plan_id is platform-managed and
	//     app_platform_role() is NULL (userA has no password_logins
	//     row).
	// We exercise (b) because that is the actual L1 scenario
	// (a member/owner with org context tries to bump the plan).
	ctxA := WithOrgID(WithUserID(ctx, userA), orgA)
	err := WithOrg(ctxA, pool, orgA, func(tx Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE orgs SET plan_id = 'enterprise' WHERE id = $1::uuid`, orgA)
		return err
	})
	if !isInsufficientPrivilege(err) {
		t.Errorf("L1: UPDATE orgs.plan_id by owner (no platform role) = %v, want 42501 (orgs_guard)", err)
	}

	// --- L2: member updates own role to owner -------------------------
	// memberships_update USING requires user_id <> ME, so the caller's
	// own row is invisible to UPDATE. 0 rows affected.
	var rows int64
	err = WithOrg(ctxA, pool, orgA, func(tx Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE memberships SET role = 'owner'
			WHERE user_id = NULLIF(current_setting('app.user_id', true), '')::uuid`)
		if err != nil {
			return err
		}
		rows = tag.RowsAffected()
		return nil
	})
	if err != nil {
		t.Errorf("L2: UPDATE own role to owner = %v, want nil", err)
	}
	if rows != 0 {
		t.Errorf("L2: UPDATE own role to owner affected %d rows, want 0 (user_id <> ME)", rows)
	}

	// --- L3: member inserts themselves as owner of another org -------
	// memberships_insert WITH CHECK requires org_id = NULLIF(app.org_id,
	// '') AND role='owner' AND org has no members yet. With app.org_id
	// empty, org_id = NULL which orgB (a uuid) does not match.
	err = WithTx(WithUserID(ctx, userA), pool, func(tx Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO memberships (org_id, user_id, role)
			VALUES ($1::uuid, $2::uuid, 'owner')`, orgB, userA)
		return err
	})
	if !isInsufficientPrivilege(err) {
		t.Errorf("L3: INSERT INTO memberships (B, me, 'owner') = %v, want 42501", err)
	}

	// --- L4: non-platform-admin inserts a platform model connection ---
	// model_connections INSERT WITH CHECK for org_id NULL requires
	// (SELECT app_platform_role()) IS NOT NULL. userA has no
	// password_logins row, so app_platform_role() is NULL → 42501.
	err = WithTx(WithUserID(ctx, userA), pool, func(tx Tx) error {
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

// TestRLSHardeningRoleGuards exercises the role-based guards added in
// the review fix (PR #95). A regular member of org A must not be able
// to demote the owner, promote someone else, delete the owner, or
// rename the org — all of these now hit 0 rows or 42501. An admin can
// still change a member's role (the handleSetMemberRole path must
// keep working). A member can self-leave; an owner cannot.
func TestRLSHardeningRoleGuards(t *testing.T) {
	ctx := context.Background()
	pool := appPool(t)
	// Build one org with an owner, an admin, and a regular member.
	orgA, _, ownerA, _ := setupTwoOrgs(t, pool)
	memberEmail := "rls-member-" + orgA[:8] + "@example.test"
	memberA := addMember(t, ctx, pool, orgA, ownerA, memberEmail, tenants.RoleMember)
	otherEmail := "rls-other-" + orgA[:8] + "@example.test"
	otherUser := addMember(t, ctx, pool, orgA, ownerA, otherEmail, tenants.RoleMember)
	// Promote otherUser to admin via the owner (the legitimate path).
	ownerCtx := WithOrgID(WithUserID(ctx, ownerA), orgA)
	if err := WithOrg(ownerCtx, pool, orgA, func(tx Tx) error {
		return tenants.SetMemberRole(ctx, tx, orgA, otherUser, tenants.RoleAdmin)
	}); err != nil {
		t.Fatalf("setup: promote other to admin: %v", err)
	}

	memberCtx := WithOrgID(WithUserID(ctx, memberA), orgA)

	// M1: member demoting owner → 0 rows.
	var rows int64
	err := WithOrg(memberCtx, pool, orgA, func(tx Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE memberships SET role = 'member'
			WHERE user_id = $1::uuid AND role = 'owner'`, ownerA)
		if err != nil {
			return err
		}
		rows = tag.RowsAffected()
		return nil
	})
	if err != nil {
		t.Errorf("M1: member demote owner err = %v, want nil", err)
	}
	if rows != 0 {
		t.Errorf("M1: member demote owner affected %d rows, want 0 (R not in owner/admin)", rows)
	}

	// M2: member promoting someone else to admin → 0 rows.
	err = WithOrg(memberCtx, pool, orgA, func(tx Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE memberships SET role = 'admin'
			WHERE user_id = $1::uuid`, otherUser)
		if err != nil {
			return err
		}
		rows = tag.RowsAffected()
		return nil
	})
	if err != nil {
		t.Errorf("M2: member promote other err = %v, want nil", err)
	}
	// otherUser is currently admin; demoting themselves to admin is a
	// no-op (role already admin), but the policy still blocks because
	// the caller is a member, not owner/admin.
	if rows != 0 {
		t.Errorf("M2: member promote other affected %d rows, want 0 (R not in owner/admin)", rows)
	}

	// M3: member deleting owner → 0 rows.
	err = WithOrg(memberCtx, pool, orgA, func(tx Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM memberships WHERE user_id = $1::uuid`, ownerA)
		if err != nil {
			return err
		}
		rows = tag.RowsAffected()
		return nil
	})
	if err != nil {
		t.Errorf("M3: member delete owner err = %v, want nil", err)
	}
	if rows != 0 {
		t.Errorf("M3: member delete owner affected %d rows, want 0 (owner row protected)", rows)
	}

	// M4: member updating org name → 0 rows (orgs_update requires
	// R IN owner/admin).
	err = WithOrg(memberCtx, pool, orgA, func(tx Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE orgs SET name = 'hacked'`)
		if err != nil {
			return err
		}
		rows = tag.RowsAffected()
		return nil
	})
	if err != nil {
		t.Errorf("M4: member UPDATE orgs name err = %v, want nil", err)
	}
	if rows != 0 {
		t.Errorf("M4: member UPDATE orgs name affected %d rows, want 0 (R not in owner/admin)", rows)
	}

	// M5: admin changing a member's role succeeds (the
	// handleSetMemberRole path must keep working).
	adminCtx := WithOrgID(WithUserID(ctx, otherUser), orgA)
	err = WithOrg(adminCtx, pool, orgA, func(tx Tx) error {
		return tenants.SetMemberRole(ctx, tx, orgA, memberA, tenants.RoleAdmin)
	})
	if err != nil {
		t.Errorf("M5: admin promote member to admin = %v, want nil", err)
	}

	// M6: member self-leaving (own row, role <> owner) succeeds. Use
	// memberA again — but they are now admin (M5 promoted them). Demote
	// them back to member first so the self-leave path applies.
	if err := WithOrg(ownerCtx, pool, orgA, func(tx Tx) error {
		return tenants.SetMemberRole(ctx, tx, orgA, memberA, tenants.RoleMember)
	}); err != nil {
		t.Fatalf("M6 setup: demote memberA back to member: %v", err)
	}
	err = WithOrg(memberCtx, pool, orgA, func(tx Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM memberships WHERE user_id = $1::uuid`, memberA)
		if err != nil {
			return err
		}
		rows = tag.RowsAffected()
		return nil
	})
	if err != nil {
		t.Errorf("M6: member self-leave err = %v, want nil", err)
	}
	if rows != 1 {
		t.Errorf("M6: member self-leave affected %d rows, want 1", rows)
	}

	// M7: owner cannot self-leave (role = 'owner' protected).
	err = WithOrg(ownerCtx, pool, orgA, func(tx Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM memberships WHERE user_id = $1::uuid`, ownerA)
		if err != nil {
			return err
		}
		rows = tag.RowsAffected()
		return nil
	})
	if err != nil {
		t.Errorf("M7: owner self-leave err = %v, want nil", err)
	}
	if rows != 0 {
		t.Errorf("M7: owner self-leave affected %d rows, want 0 (role = 'owner' protected)", rows)
	}
}
