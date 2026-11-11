package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/tastyeffectco/sandboxd/portal/internal/db"
	"github.com/tastyeffectco/sandboxd/portal/internal/tenants"
)

// Two users whose email local parts match both get a personal org on
// first sign-in (the second with a suffixed slug), and a returning
// user with no org gets one back.
func TestBootstrapLoginSameLocalPart(t *testing.T) {
	srv := setupBFF(t)
	ctx := context.Background()
	local := "admin" + uuid.NewString()[:6]
	for _, domain := range []string{"a.test", "b.test"} {
		uid, err := srv.bootstrapLogin(ctx, uuid.NewString(), local+"@"+domain, "")
		if err != nil {
			t.Fatalf("bootstrap %s@%s: %v", local, domain, err)
		}
		if n := countOrgs(t, srv, uid); n != 1 {
			t.Fatalf("%s@%s has %d orgs, want 1", local, domain, n)
		}
	}

	// A user row with no membership (as an older failed bootstrap
	// left behind) gets a personal org on the next sign-in.
	sub, email := uuid.NewString(), uuid.NewString()+"@c.test"
	_, _, _ = login(t, srv, sub, email, "Orphan")
	uid, err := srv.bootstrapLogin(ctx, sub, email, "Orphan")
	if err != nil {
		t.Fatal(err)
	}
	if n := countOrgs(t, srv, uid); n != 1 {
		t.Fatalf("orphan has %d orgs after sign-in, want 1", n)
	}
	// Signing in again does not create a second org.
	if _, err := srv.bootstrapLogin(ctx, sub, email, "Orphan"); err != nil {
		t.Fatal(err)
	}
	if n := countOrgs(t, srv, uid); n != 1 {
		t.Fatalf("returning user has %d orgs, want 1", n)
	}
}

func countOrgs(t *testing.T, srv *Server, userID string) int {
	t.Helper()
	ctx := db.WithUserID(context.Background(), userID)
	var n int
	if err := db.WithTx(ctx, srv.Pool, func(tx db.Tx) error {
		var err error
		n, err = tenants.CountMyMemberships(ctx, tx, userID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

// An admin cannot promote themselves (or anyone) to owner, and
// cannot change their own role; an owner can grant owner.
func TestRoleChangeRules(t *testing.T) {
	srv := setupBFF(t)
	tag := uuid.NewString()[:8]
	_, owner, ownerCSRF := login(t, srv, "own-"+tag, "own-"+tag+"@example.test", "Owner")
	adminID, admin, adminCSRF := login(t, srv, "adm-"+tag, "adm-"+tag+"@example.test", "Admin")
	memberID, member, memberCSRF := login(t, srv, "mem-"+tag, "mem-"+tag+"@example.test", "Member")

	rec := do(t, srv, http.MethodPost, "/api/orgs", `{"name":"Roles `+tag+`"}`, owner, ownerCSRF)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create org = %d: %s", rec.Code, rec.Body.String())
	}
	orgID := extractString(t, rec.Body.String(), "id")
	join := func(email, role string, c, csrf *http.Cookie) {
		rec := do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/invites", `{"email":"`+email+`","role":"`+role+`"}`, owner, ownerCSRF)
		if rec.Code != http.StatusCreated {
			t.Fatalf("invite %s = %d: %s", email, rec.Code, rec.Body.String())
		}
		u := extractString(t, rec.Body.String(), "url")
		token := u[strings.Index(u, "token=")+len("token="):]
		if rec := do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/invites/accept", `{"token":"`+token+`"}`, c, csrf); rec.Code != http.StatusOK {
			t.Fatalf("accept %s = %d: %s", email, rec.Code, rec.Body.String())
		}
	}
	join("adm-"+tag+"@example.test", "admin", admin, adminCSRF)
	join("mem-"+tag+"@example.test", "member", member, memberCSRF)

	patch := func(target, role string, c, csrf *http.Cookie) int {
		return do(t, srv, http.MethodPatch, "/api/orgs/"+orgID+"/members/"+target, `{"role":"`+role+`"}`, c, csrf).Code
	}
	if code := patch(adminID, "owner", admin, adminCSRF); code != http.StatusForbidden {
		t.Errorf("admin self-promote to owner = %d, want 403", code)
	}
	if code := patch(adminID, "member", admin, adminCSRF); code != http.StatusForbidden {
		t.Errorf("admin changes own role = %d, want 403", code)
	}
	if code := patch(memberID, "owner", admin, adminCSRF); code != http.StatusForbidden {
		t.Errorf("admin grants owner = %d, want 403", code)
	}
	if code := patch(memberID, "admin", admin, adminCSRF); code != http.StatusOK {
		t.Errorf("admin promotes member to admin = %d, want 200", code)
	}
	if code := patch(memberID, "owner", owner, ownerCSRF); code != http.StatusOK {
		t.Errorf("owner grants owner = %d, want 200", code)
	}
}

// An invite link used by a different signed-in user is refused.
func TestInviteAcceptWrongEmail(t *testing.T) {
	srv := setupBFF(t)
	tag := uuid.NewString()[:8]
	_, owner, ownerCSRF := login(t, srv, "o2-"+tag, "o2-"+tag+"@example.test", "Owner")
	_, eve, eveCSRF := login(t, srv, "eve-"+tag, "eve-"+tag+"@example.test", "Eve")
	rec := do(t, srv, http.MethodPost, "/api/orgs", `{"name":"Inv `+tag+`"}`, owner, ownerCSRF)
	orgID := extractString(t, rec.Body.String(), "id")
	rec = do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/invites", `{"email":"bob-`+tag+`@example.test"}`, owner, ownerCSRF)
	u := extractString(t, rec.Body.String(), "url")
	token := u[strings.Index(u, "token=")+len("token="):]
	rec = do(t, srv, http.MethodPost, "/api/orgs/"+orgID+"/invites/accept", `{"token":"`+token+`"}`, eve, eveCSRF)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "invite_email_mismatch") {
		t.Fatalf("eve accepts bob's invite = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := do(t, srv, http.MethodGet, "/api/orgs/"+orgID+"/members", "", eve, eveCSRF); rec.Code != http.StatusNotFound {
		t.Fatalf("eve sees members after refused accept = %d", rec.Code)
	}
}
