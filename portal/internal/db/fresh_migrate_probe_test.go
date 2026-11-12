package db

// Temporary A3.1 probe: migration 0010 on a brand-new database must
// leave both SECURITY DEFINER functions callable (the plpgsql OUT
// ambiguity bit exactly this once).
import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestFreshDatabaseMigratesWithAgentFunctions(t *testing.T) {
	url := os.Getenv("PORTAL_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("PORTAL_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	base := url[:len(url)-len("portal_test")]
	fresh := base + "portal_fresh_probe"
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `DROP DATABASE IF EXISTS portal_fresh_probe`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `CREATE DATABASE portal_fresh_probe`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(), `DROP DATABASE IF EXISTS portal_fresh_probe`)
		_ = conn.Close(context.Background())
	})
	pool, err := Open(ctx, testLogger(), fresh, "")
	if err != nil {
		t.Fatalf("open fresh: %v", err)
	}
	if err := Migrate(ctx, pool, testLogger()); err != nil {
		t.Fatalf("migrate fresh: %v", err)
	}
	pool.Close()
	probe, err := pgx.Connect(ctx, fresh)
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close(ctx)
	var n int
	if err := probe.QueryRow(ctx, `SELECT COUNT(*) FROM agent_claim_next('probe', 60)`).Scan(&n); err != nil {
		t.Fatalf("claim probe: %v", err)
	}
	if err := probe.QueryRow(ctx, `SELECT COUNT(*) FROM agent_sweep_lost(60)`).Scan(&n); err != nil {
		t.Fatalf("sweep probe: %v", err)
	}
}
