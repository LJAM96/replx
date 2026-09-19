// Package testdb opens the CI Postgres, applies migrations once, and hands
// each test its own rolled-back transaction: full isolation between
// packages and tests without weakening the single-enabled-server
// constraint or any other production invariant.
package testdb

import (
	"context"
	"os"
	"testing"

	"github.com/LJAM96/replx/internal/database"
	"github.com/jackc/pgx/v5"
)

// Begin migrates the shared CI database and returns a transaction the
// caller uses as database.DBTX. Rollback at cleanup erases every row the
// test wrote, including enabled servers and FK-linked playback state.
func Begin(t *testing.T) (context.Context, pgx.Tx) {
	t.Helper()
	url := os.Getenv("REPLX_EDGE_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("REPLX_EDGE_TEST_POSTGRES_URL not set; CI go job covers live SQL")
	}
	ctx := context.Background()
	pool, err := database.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.Migrate(ctx); err != nil {
		pool.Close()
		t.Fatal(err)
	}
	tx, err := pool.Raw().Begin(ctx)
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = tx.Rollback(context.Background())
		pool.Close()
	})
	return ctx, tx
}
