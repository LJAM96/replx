package cache

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"

	"github.com/LJAM96/replx/internal/database"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func testGenerationStore(t *testing.T, g GenerationStore) {
	t.Helper()
	ctx := context.Background()
	check := func(scope, class string, wantScope, wantGlobal uint64) {
		t.Helper()
		scopeGeneration, globalGeneration, err := g.Get(ctx, scope, class)
		if err != nil || scopeGeneration != wantScope || globalGeneration != wantGlobal {
			t.Fatalf("Get(%q, %q) = (%d, %d, %v), want (%d, %d, nil)", scope, class,
				scopeGeneration, globalGeneration, err, wantScope, wantGlobal)
		}
	}
	check("user:one", "metadata", 0, 0)
	if err := g.Bump(ctx, "user:one", "metadata"); err != nil {
		t.Fatal(err)
	}
	check("user:one", "metadata", 1, 0)
	check("user:one", "sections", 0, 0)
	check("user:two", "metadata", 0, 0)
	if err := g.BumpScope(ctx, "user:one"); err != nil {
		t.Fatal(err)
	}
	check("user:one", "metadata", 2, 0)
	check("user:one", "sections", 1, 0)
	check("user:one", "never-read-before", 1, 0)
	check("user:two", "metadata", 0, 0)
	if err := g.Bump(ctx, "user:one", ""); err != nil {
		t.Fatal(err)
	}
	check("user:one", "browse", 2, 0)
	check("user:one", "", 2, 0)
	if err := g.BumpAll(ctx); err != nil {
		t.Fatal(err)
	}
	check("user:one", "metadata", 2, 1)
	check("user:two", "metadata", 0, 1)
}

func TestMemoryGenerationStore(t *testing.T) {
	testGenerationStore(t, NewMemoryGenerationStore())
}

func TestMemoryGenerationStoreConcurrentBumps(t *testing.T) {
	g := NewMemoryGenerationStore()
	ctx := context.Background()
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			if err := g.Bump(ctx, "user:one", "metadata"); err != nil {
				t.Error(err)
			}
			if err := g.BumpScope(ctx, "user:one"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	sg, gg, err := g.Get(ctx, "user:one", "metadata")
	if err != nil || sg != 200 || gg != 0 {
		t.Fatalf("concurrent increments lost: (%d, %d, %v)", sg, gg, err)
	}
}

func TestMemoryGenerationStoreCanceledContext(t *testing.T) {
	g := NewMemoryGenerationStore()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := g.Get(ctx, "scope", "class"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for _, bump := range []func() error{
		func() error { return g.Bump(ctx, "scope", "class") },
		func() error { return g.BumpScope(ctx, "scope") },
		func() error { return g.BumpAll(ctx) },
	} {
		if err := bump(); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
	sg, gg, err := g.Get(context.Background(), "scope", "class")
	if err != nil || sg != 0 || gg != 0 {
		t.Fatalf("canceled operations changed counters: (%d, %d, %v)", sg, gg, err)
	}
}

type generationTestDB struct {
	database.DBTX
	err  error
	args []any
	row  pgx.Row
}

func (d *generationTestDB) Exec(_ context.Context, _ string, args ...any) (pgconn.CommandTag, error) {
	d.args = args
	return pgconn.CommandTag{}, d.err
}

func (d *generationTestDB) QueryRow(_ context.Context, _ string, args ...any) pgx.Row {
	d.args = args
	return d.row
}

type generationTestRow struct {
	values [3]int64
	err    error
}

func (r generationTestRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i, v := range r.values {
		*dest[i].(*int64) = v
	}
	return nil
}

func TestPostgresGenerationsScopePrivacyAndCounterRange(t *testing.T) {
	db := &generationTestDB{row: generationTestRow{values: [3]int64{math.MaxInt64, math.MaxInt64, 3}}}
	g := NewPostgresGenerations(db, "")
	ctx := context.Background()
	scope := "tok:secret-must-not-be-stored"
	sg, gg, err := g.Get(ctx, scope, "")
	if err != nil || sg != math.MaxUint64-1 || gg != 3 {
		t.Fatalf("counter sum overflowed: (%d, %d, %v)", sg, gg, err)
	}
	if db.args[0] != "default" || db.args[1] != generationScopeHash(scope) || db.args[2] != "browse" {
		t.Fatalf("unexpected Get arguments: %v", db.args)
	}
	if err := g.BumpScope(ctx, scope); err != nil {
		t.Fatal(err)
	}
	if db.args[1] != generationScopeHash(scope) || db.args[2] != "" {
		t.Fatalf("scope bump must persist only digest: %v", db.args)
	}
}

func TestPostgresGenerationsErrorsPropagate(t *testing.T) {
	want := errors.New("database unavailable")
	db := &generationTestDB{err: want, row: generationTestRow{err: want}}
	g := NewPostgresGenerations(db, "default")
	ctx := context.Background()
	if _, _, err := g.Get(ctx, "scope", "metadata"); !errors.Is(err, want) {
		t.Fatal(err)
	}
	for _, operation := range []func() error{
		func() error { return g.Load(ctx) },
		func() error { return g.Bump(ctx, "scope", "metadata") },
		func() error { return g.BumpScope(ctx, "scope") },
		func() error { return g.BumpAll(ctx) },
	} {
		if err := operation(); !errors.Is(err, want) {
			t.Fatal(err)
		}
	}
	var unavailable *PostgresGenerations
	if _, _, err := unavailable.Get(ctx, "scope", "class"); err == nil {
		t.Fatal("missing storage must fail closed")
	}
}
