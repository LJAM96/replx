package cache

import (
	"testing"

	"github.com/LJAM96/replx/internal/testdb"
)

func TestLivePostgresGenerations(t *testing.T) {
	ctx, db := testdb.Begin(t)
	g := NewPostgresGenerations(db, "generation-test")
	if err := g.Load(ctx); err != nil {
		t.Fatal(err)
	}
	testGenerationStore(t, g)

	// A new instance models a process restart while Valkey retains responses.
	restarted := NewPostgresGenerations(db, "generation-test")
	if err := restarted.Load(ctx); err != nil {
		t.Fatal(err)
	}
	sg, gg, err := restarted.Get(ctx, "user:one", "metadata")
	if err != nil || sg != 2 || gg != 1 {
		t.Fatalf("restart revived namespace: (%d, %d, %v)", sg, gg, err)
	}
	if err := restarted.BumpScope(ctx, "user:one"); err != nil {
		t.Fatal(err)
	}
	sg, gg, err = g.Get(ctx, "user:one", "metadata")
	if err != nil || sg != 3 || gg != 1 {
		t.Fatalf("scope bump after restart lost class: (%d, %d, %v)", sg, gg, err)
	}

	otherServer := NewPostgresGenerations(db, "other-server")
	sg, gg, err = otherServer.Get(ctx, "user:one", "metadata")
	if err != nil || sg != 0 || gg != 0 {
		t.Fatalf("server namespace leaked: (%d, %d, %v)", sg, gg, err)
	}
	var rawScopes int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM cache_generations
		WHERE server_id='generation-test' AND scope_hash NOT IN ('', $1)`, generationScopeHash("user:one")).Scan(&rawScopes); err != nil {
		t.Fatal(err)
	}
	if rawScopes != 0 {
		t.Fatal("stored scope identifiers must be digests")
	}
}
