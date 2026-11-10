// db_test.go — migration tests run against a real Postgres when
// PORTAL_TEST_DATABASE_URL is set (CI wires a postgres:16 service
// container; locally point it at any throwaway database). Without it
// the tests skip, so `go test ./...` stays hermetic.
package db

import (
	"context"
	"log/slog"
	"os"
	"testing"
)

func testURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("PORTAL_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("PORTAL_TEST_DATABASE_URL not set")
	}
	return url
}

func TestMigrateAppliesAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	log := slog.Default()
	// Migration runs as the (owner) login user, before SET ROLE.
	pool, err := Open(ctx, log, testURL(t), "")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pool.Close()

	if err := Migrate(ctx, pool, log); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	if err := Migrate(ctx, pool, log); err != nil {
		t.Fatalf("second Migrate (idempotent): %v", err)
	}

	var name string
	if err := pool.QueryRow(ctx,
		"SELECT name FROM portal_migrations WHERE name = '0001_init.sql'").Scan(&name); err != nil {
		t.Fatalf("migration not recorded: %v", err)
	}

	for _, ext := range []string{"pgcrypto", "citext"} {
		var got string
		if err := pool.QueryRow(ctx,
			"SELECT extname FROM pg_extension WHERE extname = $1", ext).Scan(&got); err != nil {
			t.Fatalf("%s extension missing after migrations: %v", ext, err)
		}
	}
}

func TestMigrateCreatesTenantTables(t *testing.T) {
	ctx := context.Background()
	pool, err := Open(ctx, slog.Default(), testURL(t), "")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer pool.Close()
	if err := Migrate(ctx, pool, slog.Default()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	for _, table := range []string{"users", "orgs", "memberships", "invites", "projects", "audit_events", "web_sessions"} {
		var exists bool
		if err := pool.QueryRow(ctx,
			"SELECT EXISTS (SELECT 1 FROM pg_tables WHERE tablename = $1)", table).Scan(&exists); err != nil || !exists {
			t.Fatalf("table %s missing (err=%v)", table, err)
		}
	}
}
