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

	"github.com/jackc/pgx/v5/pgxpool"
)

func testPool(t *testing.T) (*pgxpool.Pool, func()) {
	t.Helper()
	url := os.Getenv("PORTAL_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("PORTAL_TEST_DATABASE_URL not set")
	}
	pool, err := Open(context.Background(), slog.Default(), url)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return pool, pool.Close
}

func TestMigrateAppliesAndIsIdempotent(t *testing.T) {
	pool, closePool := testPool(t)
	defer closePool()
	ctx := context.Background()
	log := slog.Default()

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

	var ext string
	if err := pool.QueryRow(ctx,
		"SELECT extname FROM pg_extension WHERE extname = 'pgcrypto'").Scan(&ext); err != nil {
		t.Fatalf("pgcrypto extension missing after migrations: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"SELECT extname FROM pg_extension WHERE extname = 'citext'").Scan(&ext); err != nil {
		t.Fatalf("citext extension missing after migrations: %v", err)
	}
}
