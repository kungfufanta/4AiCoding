// rls_test.go — the S1 "Xong khi" integration proof
// (PORTAL_PLAN 6/S1): a user in org A cannot read or write org B's
// data *even when the repository SQL forgets WHERE org_id*, because
// Row Level Security has the final say. The test connects as the
// non-owner portal_app role (SET ROLE) so RLS actually applies even
// when the login user is a superuser or the table owner.
package db

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tastyeffectco/sandboxd/portal/internal/tenants"
)

var (
	migrateOnce sync.Once
	migrateErr  error
)

// appPool returns a pool whose connections run as portal_app, after
// making sure migrations are applied (as the owner login).
func appPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := testURL(t)
	ctx := context.Background()
	migrateOnce.Do(func() {
		admin, err := Open(ctx, slog.Default(), url, "")
		if err != nil {
			migrateErr = err
			return
		}
		defer admin.Close()
		migrateErr = Migrate(ctx, admin, slog.Default())
	})
	if migrateErr != nil {
		t.Fatalf("migrate: %v", migrateErr)
	}
	pool, err := Open(ctx, slog.Default(), url, "portal_app")
	if err != nil {
		t.Fatalf("Open as portal_app: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func setupTwoOrgs(t *testing.T, pool *pgxpool.Pool) (orgA, orgB, userA, userB string) {
	t.Helper()
	ctx := context.Background()
	// Slugs are unique per test run: the suite shares one database.
	suffix := strings.ToLower(uuid.NewString()[:8])

	for _, who := range []string{"A", "B"} {
		// Step 1: create the user (commit first — the org bootstrap
		// below inserts a FK reference to it).
		var uID string
		if err := WithTx(ctx, pool, func(tx Tx) error {
			u, _, err := tenants.UpsertUser(ctx, tx, "rls-sub-"+who, "rls-"+who+"@example.test", "User "+who)
			uID = u.ID
			return err
		}); err != nil {
			t.Fatal(err)
		}
		// Step 2: org + owner membership + project in the org's RLS tx.
		id, err := NewOrgID()
		if err != nil {
			t.Fatal(err)
		}
		if err := BootstrapTx(ctx, pool, id, func(btx Tx) error {
			if _, err := tenants.CreateOrg(ctx, btx, id, "rls-org-"+who+"-"+suffix, "Org "+who, uID); err != nil {
				return err
			}
			if _, err := tenants.CreateProject(ctx, btx, "proj-"+strings.ToLower(who)+"-"+suffix, "Project "+who); err != nil {
				return err
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if who == "A" {
			orgA, userA = id, uID
		} else {
			orgB, userB = id, uID
		}
	}
	if orgA == "" || orgB == "" {
		t.Fatal("setup: orgs missing")
	}
	return orgA, orgB, userA, userB
}

// TestRLSCrossOrgIsolation asserts the full matrix: reads scoped,
// writes rejected, ownership checks survive forgotten WHERE clauses.
func TestRLSCrossOrgIsolation(t *testing.T) {
	ctx := context.Background()
	pool := appPool(t)
	orgA, orgB, userA, userB := setupTwoOrgs(t, pool)

	// --- reads: user A's session, org A context ------------------------
	ctxA := WithOrgID(WithUserID(ctx, userA), orgA)
	err := WithOrg(ctxA, pool, orgA, func(tx Tx) error {
		// Forget WHERE org_id on purpose: RLS must scope it.
		ps, err := tenants.ListProjects(ctx, tx)
		if err != nil {
			return err
		}
		if len(ps) != 1 || !strings.HasPrefix(ps[0].Slug, "proj-a-") {
			t.Errorf("org A sees %d projects (%v), want exactly its own project", len(ps), ps)
		}
		// ListMyOrgs without any WHERE: only org A is visible.
		orgs, err := tenants.ListMyOrgs(ctx, tx)
		if err != nil {
			return err
		}
		for _, o := range orgs {
			if o.ID == orgB {
				t.Errorf("user A can see org B in a WHERE-less org list")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read as org A: %v", err)
	}

	// --- writes: user A's session is pinned to org A (the server sets
	// app.org_id only after its membership check; RLS is the second
	// layer). Queries that forget WHERE org_id must stay inside it.
	err = WithOrg(ctxA, pool, orgA, func(tx Tx) error {
		// UPDATE without WHERE: RLS scopes it to org A's own row.
		ct, err := tx.Exec(ctx,
			`UPDATE projects SET name = 'renamed'`)
		if err != nil {
			return err
		}
		if ct.RowsAffected() != 1 {
			t.Errorf("WHERE-less UPDATE touched %d rows, want exactly org A's 1", ct.RowsAffected())
		}
		// INSERT into org B must be rejected outright (WITH CHECK).
		// The expected failure aborts the tx, so run it inside a
		// savepoint and roll back to keep asserting.
		if _, err := tx.Exec(ctx, `SAVEPOINT rls_probe`); err != nil {
			return err
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO projects (org_id, slug, name)
                         VALUES ($1, 'evil', 'evil')`, orgB)
		if err == nil {
			t.Errorf("INSERT into org B succeeded for org A user, want RLS rejection")
		}
		if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT rls_probe`); err != nil {
			return err
		}
		// DELETE without WHERE must not touch org B memberships; it
		// may only remove rows the caller can see (their own).
		ct, err = tx.Exec(ctx, `DELETE FROM memberships`)
		if err != nil {
			return err
		}
		if ct.RowsAffected() == 0 {
			t.Errorf("DELETE FROM memberships removed 0 rows, want at least the caller's own")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("write as org A: %v", err)
	}

	// --- org B is intact -----------------------------------------------
	ctxB := WithOrgID(WithUserID(ctx, userB), orgB)
	if err := WithOrg(ctxB, pool, orgB, func(tx Tx) error {
		ps, err := tenants.ListProjects(ctx, tx)
		if err != nil {
			return err
		}
		if len(ps) != 1 || ps[0].Name == "hacked" || ps[0].Slug == "evil" {
			t.Errorf("org B projects corrupted: %+v", ps)
		}
		if len(ps) != 1 || ps[0].Name != "Project B" {
			t.Errorf("org B projects = %+v, want its own untouched project", ps)
		}
		ms, err := tenants.ListMembers(ctx, tx)
		if err != nil {
			return err
		}
		// User B owns one org per run (same oidc_sub), so their own
		// rows are all visible through the user_id arm. The invariant
		// is about org B itself: exactly one member row points at it.
		orgBMembers := 0
		for _, m := range ms {
			if m.OrgID == orgB {
				orgBMembers++
			}
		}
		if orgBMembers != 1 {
			t.Errorf("org B has %d member rows after cross-org DELETE, want 1", orgBMembers)
		}
		return nil
	}); err != nil {
		t.Fatalf("verify org B intact: %v", err)
	}

	// --- no context at all: nothing is visible --------------------------
	err = WithTx(ctx, pool, func(tx Tx) error {
		ps, err := tenants.ListProjects(ctx, tx)
		if err != nil {
			return err
		}
		if len(ps) != 0 {
			t.Errorf("context-less query sees %d projects, want 0", len(ps))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("context-less read: %v", err)
	}

	// --- membership check: org A user is not a member of org B ----------
	if err := WithTx(ctxA, pool, func(tx Tx) error {
		_, err := tenants.MembershipRole(ctx, tx, orgB, userA)
		if !errors.Is(err, tenants.ErrNotFound) {
			t.Errorf("MembershipRole(org B) for user A = %v, want ErrNotFound", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("membership check: %v", err)
	}
}

// TestRLSInviteAcceptCrossOrg covers the one flow that must pierce
// the wall: an invite link (org id in URL + secret token).
func TestRLSInviteAcceptCrossOrg(t *testing.T) {
	ctx := context.Background()
	pool := appPool(t)
	orgA, _, userA, _ := setupTwoOrgs(t, pool)

	var userC string
	if err := WithTx(ctx, pool, func(tx Tx) error {
		u, _, err := tenants.UpsertUser(ctx, tx, "rls-sub-C", "rls-c@example.test", "User C")
		userC = u.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// User A (owner of org A) invites user C's email.
	var inviteToken string
	ctxA := WithOrgID(WithUserID(ctx, userA), orgA)
	if err := WithOrg(ctxA, pool, orgA, func(tx Tx) error {
		_, token, err := tenants.CreateInvite(ctx, tx, orgA, "rls-c@example.test", tenants.RoleMember, userA, 3600e9)
		inviteToken = token
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// User C has no membership anywhere: with only their user context
	// set, the invite is invisible; the org id from the URL pins the
	// context and the token does the rest.
	ctxC := WithUserID(ctx, userC)
	if err := WithOrg(ctxC, pool, orgA, func(tx Tx) error {
		role, err := tenants.AcceptInvite(ctx, tx, orgA, inviteToken, userC, "rls-c@example.test")
		if err != nil {
			return err
		}
		if role != tenants.RoleMember {
			t.Errorf("accepted role = %s, want member", role)
		}
		return nil
	}); err != nil {
		t.Fatalf("accept invite: %v", err)
	}

	// Replay must fail (single-use token).
	if err := WithOrg(ctxC, pool, orgA, func(tx Tx) error {
		_, err := tenants.AcceptInvite(ctx, tx, orgA, inviteToken, userC, "rls-c@example.test")
		if !errors.Is(err, tenants.ErrNotFound) {
			t.Errorf("replayed invite accepted: %v, want ErrNotFound", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
