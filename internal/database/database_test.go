package database

import (
	"context"
	"os"
	"testing"
)

func TestSplitStatements(t *testing.T) {
	ddl := "-- comment\nCREATE TABLE a (id text);\n\nCREATE INDEX i ON a(id);\n"
	stmts := splitStatements(ddl)
	if len(stmts) != 2 {
		t.Fatalf("want 2 statements, got %d: %q", len(stmts), stmts)
	}
}

func TestMigrateLive(t *testing.T) {
	url := os.Getenv("REPLX_EDGE_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("REPLX_EDGE_TEST_POSTGRES_URL not set; CI migrations job covers SQL apply")
	}
	ctx := context.Background()
	pool, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := pool.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if !pool.MigrationsComplete() {
		t.Fatal("expected completed")
	}
	// Idempotent second run.
	if err := pool.Migrate(ctx); err != nil {
		t.Fatalf("Migrate again: %v", err)
	}
	if !pool.Ping(ctx) {
		t.Fatal("expected ping true")
	}
}
