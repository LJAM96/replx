package identity

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"

	"github.com/LJAM96/replx/internal/database"
)

type fakeTV struct {
	calls atomic.Int64
	id    int64
	user  string
	err   error
}

func (f *fakeTV) GetUser(ctx context.Context, token string) (int64, string, error) {
	f.calls.Add(1)
	return f.id, f.user, f.err
}

func TestNilResolverFallsBack(t *testing.T) {
	var r *Resolver
	got := r.Resolve(context.Background(), "fp", "tok", Client{})
	if got.Scope != "tok:fp" || got.Known {
		t.Fatalf("%+v", got)
	}
}

func TestMemCacheSingleLookup(t *testing.T) {
	tv := &fakeTV{err: errors.New("plex.tv down")}
	r := New(nil, tv) // nil DB: always fallback, but mem caches it
	for i := 0; i < 3; i++ {
		if got := r.Resolve(context.Background(), "fp", "tok", Client{}); got.Scope != "tok:fp" {
			t.Fatal("nil DB must fall back")
		}
	}
	_ = tv
}

func liveResolver(t *testing.T, tv Account) (*Resolver, string) {
	t.Helper()
	url := os.Getenv("REPLX_EDGE_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("REPLX_EDGE_TEST_POSTGRES_URL not set; CI go job covers live identity SQL")
	}
	ctx := context.Background()
	pool, err := database.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	db := pool.Raw()
	_, _ = db.Exec(ctx, `DELETE FROM plex_servers WHERE machine_identifier='test-identity-box'`)
	var serverID string
	if err := db.QueryRow(ctx, `INSERT INTO plex_servers(name, internal_origin_url, machine_identifier, enabled)
		VALUES('Identity Box','http://test.invalid:32400','test-identity-box',true) RETURNING id`).Scan(&serverID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(context.Background(), `DELETE FROM plex_servers WHERE machine_identifier='test-identity-box'`)
	})
	return New(db, tv), serverID
}

func TestLiveResolveLinksAndCaches(t *testing.T) {
	tv := &fakeTV{id: 4242, user: "warmer-owner"}
	r, _ := liveResolver(t, tv)
	ctx := context.Background()
	client := Client{Identifier: "client-9", Product: "Plex Web", Platform: "Web"}
	got := r.Resolve(ctx, "fp-cold", "tok-cold", client)
	if !got.Known || got.Scope != "acct:4242" || got.IdentityID == "" || got.ClientID == "" {
		t.Fatalf("cold resolve: %+v", got)
	}
	if tv.calls.Load() != 1 {
		t.Fatalf("one plex.tv lookup, got %d", tv.calls.Load())
	}
	// Second sighting serves from memory: no new lookup.
	got2 := r.Resolve(ctx, "fp-cold", "tok-cold", client)
	if got2.Scope != got.Scope || tv.calls.Load() != 1 {
		t.Fatalf("mem cache: %+v calls=%d", got2, tv.calls.Load())
	}
	// Token ciphertext must never be stored.
	var n int
	db := r.DB
	if err := db.QueryRow(ctx, `SELECT count(*) FROM plex_token_identities
		WHERE token_fingerprint='fp-cold' AND token_ciphertext IS NOT NULL`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("user token vaulted: %v %d", err, n)
	}
}

func TestLiveInvalidBackoff(t *testing.T) {
	tv := &fakeTV{err: errors.New("401")}
	r, _ := liveResolver(t, tv)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		// Fresh resolver each pass to bypass the memory cache and prove
		// the DATABASE backoff, not just the mem layer.
		r2 := New(r.DB, tv)
		if got := r2.Resolve(ctx, "fp-bad", "tok-bad", Client{}); got.Known {
			t.Fatal("bad token must stay unknown")
		}
	}
	if tv.calls.Load() != 1 {
		t.Fatalf("invalid tokens back off for an hour, calls=%d", tv.calls.Load())
	}
}

func TestLiveKnownLinkFastPath(t *testing.T) {
	tv := &fakeTV{id: 777, user: "fast"}
	r, _ := liveResolver(t, tv)
	ctx := context.Background()
	_ = r.Resolve(ctx, "fp-fast", "tok-fast", Client{Identifier: "c-fast"})
	before := tv.calls.Load()
	// New resolver: memory empty, link table hit, no plex.tv.
	r2 := New(r.DB, tv)
	got := r2.Resolve(ctx, "fp-fast", "tok-fast", Client{Identifier: "c-fast"})
	if !got.Known || got.Scope != "acct:777" || tv.calls.Load() != before {
		t.Fatalf("link fast path: %+v calls=%d", got, tv.calls.Load())
	}
}
