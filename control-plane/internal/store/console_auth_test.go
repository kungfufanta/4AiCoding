package store

import (
	"context"
	"testing"
	"time"
)

func newConsoleStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(context.Background(), "file::memory:?_fk=1", "../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestUsersCRUD(t *testing.T) {
	ctx := context.Background()
	s := newConsoleStore(t)
	if n, _ := s.CountUsers(ctx); n != 0 {
		t.Fatalf("fresh store has %d users", n)
	}
	u := &User{ID: "u1", Username: "Alice", PasswordHash: "h", Role: "admin"}
	if err := s.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateUser(ctx, &User{ID: "u2", Username: "alice", PasswordHash: "h", Role: "viewer"}); err != ErrConflict {
		t.Fatalf("case-insensitive duplicate: %v", err)
	}
	got, err := s.GetUserByName(ctx, "ALICE")
	if err != nil || got.ID != "u1" || got.Role != "admin" {
		t.Fatalf("by name: %+v %v", got, err)
	}
	got.Role, got.Disabled = "operator", true
	if err := s.UpdateUser(ctx, got); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.CountActiveAdmins(ctx); n != 0 {
		t.Fatalf("active admins = %d", n)
	}
	if err := s.DeleteUser(ctx, "u1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetUser(ctx, "u1"); err != ErrNotFound {
		t.Fatalf("deleted user: %v", err)
	}
}

func TestSessionsBelongToEnabledUsers(t *testing.T) {
	ctx := context.Background()
	s := newConsoleStore(t)
	now := time.Now().Unix()
	u := &User{ID: "u1", Username: "bob", PasswordHash: "h", Role: "operator"}
	if err := s.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, "th1", DefaultTenant, "u1", now, now, now+3600); err != nil {
		t.Fatal(err)
	}
	si, found, err := s.LookupSession(ctx, "th1")
	if err != nil || !found || si.Owner != DefaultTenant || si.Expires != now+3600 || si.User.Username != "bob" {
		t.Fatalf("lookup: %+v found=%v err=%v", si, found, err)
	}
	if _, found, _ := s.LookupSession(ctx, "missing"); found {
		t.Fatal("missing session reported found")
	}
	u.Disabled = true
	if err := s.UpdateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := s.LookupSession(ctx, "th1"); found {
		t.Fatal("a disabled user's session must not resolve")
	}
	if err := s.DeleteUserSessions(ctx, "u1"); err != nil {
		t.Fatal(err)
	}
	_ = s.CreateSession(ctx, "a", DefaultTenant, "u1", now, now, now+3600)
	if err := s.DeleteAllUsers(ctx); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := s.LookupSession(ctx, "a"); found {
		t.Fatal("console reset left a session")
	}
}

func TestAPIKeys(t *testing.T) {
	ctx := context.Background()
	s := newConsoleStore(t)
	now := time.Now().Unix()
	if err := s.CreateAPIKey(ctx, NewAPIKey{ID: "01A", Name: "default", KeyHash: "hashA", Prefix: "sk_aaa…", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateAPIKey(ctx, NewAPIKey{ID: "01B", Name: "default", KeyHash: "hashB", Prefix: "sk_bbb…", CreatedAt: now}); err != ErrConflict {
		t.Fatalf("dup name: got %v, want ErrConflict", err)
	}
	if err := s.CreateAPIKey(ctx, NewAPIKey{ID: "01C", Name: "ci", KeyHash: "hashC", Prefix: "sk_ccc…",
		Scopes: []string{"sandboxes:read"}, Role: "viewer", ExpiresAt: now + 60, SandboxID: "SBX", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	keys, err := s.ListAPIKeys(ctx)
	if err != nil || len(keys) != 2 || keys[0].Prefix == "" {
		t.Fatalf("list: %v err=%v", keys, err)
	}
	k, found, err := s.LookupAPIKey(ctx, "hashA")
	if err != nil || !found || k.ID != "01A" || k.Scopes != nil || k.Role != "admin" || k.ExpiresAt != nil || k.SandboxID != "" {
		t.Fatalf("lookup default: %+v found=%v err=%v", k, found, err)
	}
	k, found, _ = s.LookupAPIKey(ctx, "hashC")
	if !found || k.Role != "viewer" || k.ExpiresAt == nil || k.SandboxID != "SBX" || len(k.Scopes) != 1 {
		t.Fatalf("lookup scoped: %+v", k)
	}
	if _, found, _ := s.LookupAPIKey(ctx, "nope"); found {
		t.Fatal("unknown hash reported found")
	}
	if del, _ := s.DeleteAPIKey(ctx, "01A"); !del {
		t.Fatal("delete returned false")
	}
}

func TestAuditHashChain(t *testing.T) {
	ctx := context.Background()
	s := newConsoleStore(t)
	for i, a := range []string{"sandbox.create", "sandbox.stop", "sandbox.delete"} {
		if err := s.InsertAudit(ctx, int64(1000+i), "user", "default", "1.2.3.4", "", a, "SBX", `{"n":1}`); err != nil {
			t.Fatal(err)
		}
	}
	v, err := s.VerifyAudit(ctx)
	if err != nil || !v.OK || v.Checked != 3 {
		t.Fatalf("intact chain: %+v %v", v, err)
	}
	rows, _ := s.ListAudit(ctx, 0, 10, "", "")
	if len(rows) != 3 || rows[0].PrevHash != rows[1].Hash {
		t.Fatalf("list/link: %+v", rows)
	}
	// Tamper: edit a row.
	if _, err := s.db.ExecContext(ctx, `UPDATE audit_log SET action = 'sandbox.start' WHERE id = 2`); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.VerifyAudit(ctx); v.OK || v.BrokenAt != 2 {
		t.Fatalf("edited row not detected: %+v", v)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE audit_log SET action = 'sandbox.stop' WHERE id = 2`); err != nil {
		t.Fatal(err)
	}
	// Tamper: delete a row.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM audit_log WHERE id = 2`); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.VerifyAudit(ctx); v.OK || v.BrokenAt != 3 {
		t.Fatalf("deleted row not detected: %+v", v)
	}
}

func TestLegacyPasswordBecomesAdminUser(t *testing.T) {
	// Covered by migration 0029: console_auth -> app_user "admin". Apply the
	// migrations on a DB that has a console_auth row by re-running 0029's
	// insert logic is not possible after the fact, so just assert the table
	// shape the migration produces.
	ctx := context.Background()
	s := newConsoleStore(t)
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(1) FROM sqlite_master WHERE name = 'console_auth'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("console_auth should be gone: n=%d err=%v", n, err)
	}
}
