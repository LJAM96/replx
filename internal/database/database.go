// Package database owns the Postgres connection pool and the
// advisory-locked forward migration runner.
//
// Migrations in migrations/*.sql are embedded in the binary and applied
// under a transaction-scoped advisory lock at startup. Migration failure
// prevents readiness but never crashes the admin plane: serve keeps
// running with ready=not-ready and retries in the background.
package database

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/LJAM96/replx/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DBTX is the query surface production code depends on. *pgxpool.Pool and
// pgx.Tx both satisfy it, so live tests run inside rolled-back
// transactions: full isolation between packages without weakening the
// single-enabled-server constraint or any other production invariant.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
}

// lockKey scopes the advisory lock to replx-edge migrations.
const lockKey = "replx_edge_migrations"

// Pool wraps pgxpool with migration state. Completion is an atomic bool:
// Migrate runs on a background goroutine while readiness probes read it.
type Pool struct {
	inner     *pgxpool.Pool
	completed atomic.Bool
}

// Open creates a pool. It does not connect until first use.
func Open(ctx context.Context, databaseURL string) (*Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("database: parse url: %w", err)
	}
	cfg.MaxConns = 10
	cfg.MinConns = 1
	cfg.MaxConnLifetime = 30 * time.Minute
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("database: new pool: %w", err)
	}
	return &Pool{inner: pool}, nil
}

// Close releases the pool.
func (p *Pool) Close() {
	if p != nil && p.inner != nil {
		p.inner.Close()
	}
}

// Raw exposes the underlying pool for store queries in other packages.
// Callers must respect the single-active-origin and audit invariants.
func (p *Pool) Raw() *pgxpool.Pool {
	if p == nil {
		return nil
	}
	return p.inner
}

// Ping reports Postgres reachability with a short timeout.
func (p *Pool) Ping(ctx context.Context) bool {
	if p == nil || p.inner == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return p.inner.Ping(ctx) == nil
}

// MigrationsComplete reports whether the startup migration succeeded.
func (p *Pool) MigrationsComplete() bool {
	return p != nil && p.completed.Load()
}

// Migrate applies pending embedded migrations under an advisory lock.
// It is idempotent: applied versions recorded in schema_migrations are
// skipped. The lock serializes concurrent replicas and manual runs.
func (p *Pool) Migrate(ctx context.Context) error {
	files, err := migrations.FS.ReadDir(".")
	if err != nil {
		return fmt.Errorf("database: list migrations: %w", err)
	}
	names := make([]string, 0, len(files))
	for _, f := range files {
		if !f.IsDir() && strings.HasSuffix(f.Name(), ".sql") {
			names = append(names, f.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return fmt.Errorf("database: no migrations embedded")
	}

	tx, err := p.inner.Begin(ctx)
	if err != nil {
		return fmt.Errorf("database: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", lockKey); err != nil {
		return fmt.Errorf("database: advisory lock: %w", err)
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version text PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return fmt.Errorf("database: migrations table: %w", err)
	}

	for _, name := range names {
		var exists bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)", name).Scan(&exists); err != nil {
			return fmt.Errorf("database: check %s: %w", name, err)
		}
		if exists {
			continue
		}
		body, err := readMigration(name)
		if err != nil {
			return err
		}
		for i, stmt := range splitStatements(body) {
			if _, err := tx.Exec(ctx, stmt); err != nil {
				return fmt.Errorf("database: %s statement %d: %w", name, i+1, err)
			}
		}
		if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations(version) VALUES($1)", name); err != nil {
			return fmt.Errorf("database: record %s: %w", name, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("database: commit: %w", err)
	}
	p.completed.Store(true)
	return nil
}

func readMigration(name string) (string, error) {
	b, err := migrations.FS.ReadFile(name)
	if err != nil {
		return "", fmt.Errorf("database: read %s: %w", name, err)
	}
	return string(b), nil
}

// splitStatements splits ddl on semicolons at line ends. The bundled
// migrations contain plain DDL only (no plpgsql dollar bodies), so a full
// SQL parser is intentionally avoided; keep it that way or replace this.
func splitStatements(ddl string) []string {
	var out []string
	var cur strings.Builder
	for _, line := range strings.Split(ddl, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		cur.WriteString(line)
		cur.WriteString("\n")
		if strings.HasSuffix(trimmed, ";") {
			if s := strings.TrimSpace(cur.String()); s != "" {
				out = append(out, s)
			}
			cur.Reset()
		}
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		out = append(out, s)
	}
	return out
}

// PingURL is a one-shot reachability check without opening a pool.
func PingURL(ctx context.Context, databaseURL string) bool {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close(ctx) }()
	return conn.Ping(ctx) == nil
}
