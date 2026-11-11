package db

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/tastyeffectco/sandboxd/portal/internal/tenants"
)

func newUser(t *testing.T, ctx context.Context, sub, email string) string {
	t.Helper()
	pool := appPool(t)
	var id string
	if err := WithTx(ctx, pool, func(tx Tx) error {
		u, _, err := tenants.UpsertUser(ctx, tx, sub, email, "u")
		id = u.ID
		return err
	}); err != nil {
		t.Fatalf("upsert %s: %v", email, err)
	}
	return id
}

// Two users who cannot see each other's orgs both create an org with
// the same name: the second gets a suffixed slug instead of a
// unique-violation error (RLS hides the first org's slug).
func TestCreateOrgSlugCollisionAcrossTenants(t *testing.T) {
	ctx := context.Background()
	pool := appPool(t)
	base := "acme-" + uuid.NewString()[:8]
	var slugs []string
	for i := 0; i < 2; i++ {
		uid := newUser(t, ctx, uuid.NewString(), uuid.NewString()+"@example.test")
		orgID, _ := NewOrgID()
		uctx := WithUserID(ctx, uid)
		if err := BootstrapTx(uctx, pool, orgID, func(tx Tx) error {
			o, err := tenants.CreateOrg(uctx, tx, orgID, base, "Acme", uid)
			slugs = append(slugs, o.Slug)
			return err
		}); err != nil {
			t.Fatalf("user %d create org: %v", i, err)
		}
	}
	if slugs[0] != base || slugs[1] == base {
		t.Fatalf("slugs = %v, want %q then a suffixed variant", slugs, base)
	}
}

// A second OIDC subject with an existing email gets ErrEmailInUse,
// not a raw unique-violation.
func TestUpsertUserEmailInUse(t *testing.T) {
	ctx := context.Background()
	pool := appPool(t)
	email := uuid.NewString() + "@example.test"
	newUser(t, ctx, uuid.NewString(), email)
	err := WithTx(ctx, pool, func(tx Tx) error {
		_, _, err := tenants.UpsertUser(ctx, tx, uuid.NewString(), email, "other")
		return err
	})
	if !errors.Is(err, tenants.ErrEmailInUse) {
		t.Fatalf("err = %v, want ErrEmailInUse", err)
	}
}

// An invite can only be accepted by the user it was sent to, and a
// rejected attempt leaves it usable by the right person.
func TestAcceptInviteBoundToEmail(t *testing.T) {
	ctx := context.Background()
	pool := appPool(t)
	owner := newUser(t, ctx, uuid.NewString(), uuid.NewString()+"@example.test")
	inviteeEmail := uuid.NewString() + "@example.test"
	invitee := newUser(t, ctx, uuid.NewString(), inviteeEmail)
	outsider := newUser(t, ctx, uuid.NewString(), uuid.NewString()+"@example.test")

	orgID, _ := NewOrgID()
	octx := WithUserID(ctx, owner)
	var token string
	if err := BootstrapTx(octx, pool, orgID, func(tx Tx) error {
		if _, err := tenants.CreateOrg(octx, tx, orgID, "inv-"+uuid.NewString()[:8], "Inv", owner); err != nil {
			return err
		}
		var err error
		_, token, err = tenants.CreateInvite(octx, tx, orgID, inviteeEmail, tenants.RoleMember, owner, 3600e9)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	err := WithOrg(WithUserID(ctx, outsider), pool, orgID, func(tx Tx) error {
		_, err := tenants.AcceptInvite(ctx, tx, orgID, token, outsider, "someone-else@example.test")
		return err
	})
	if !errors.Is(err, tenants.ErrInviteEmailMismatch) {
		t.Fatalf("outsider accept err = %v, want ErrInviteEmailMismatch", err)
	}
	// Upper-case email still matches (citext semantics).
	if err := WithOrg(WithUserID(ctx, invitee), pool, orgID, func(tx Tx) error {
		_, err := tenants.AcceptInvite(ctx, tx, orgID, token, invitee, toUpper(inviteeEmail))
		return err
	}); err != nil {
		t.Fatalf("invitee accept: %v", err)
	}
}

func toUpper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - 32
		}
	}
	return string(b)
}
