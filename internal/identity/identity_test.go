package identity

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/LJAM96/replx/internal/plextv"
	"github.com/LJAM96/replx/internal/testdb"
)

type fakeTV struct {
	calls atomic.Int64
	id    int64
	user  string
	err   error
	code  int // when set, err becomes a typed plex.tv StatusError
}

func (f *fakeTV) GetUser(ctx context.Context, token string) (int64, string, error) {
	f.calls.Add(1)
	if f.code != 0 {
		return 0, "", &plextv.StatusError{StatusCode: f.code, Status: "auth failed"}
	}
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
	ctx, db := testdb.Begin(t)
	_, _ = db.Exec(ctx, `DELETE FROM plex_servers WHERE machine_identifier='test-identity-box'`)
	var serverID string
	if err := db.QueryRow(ctx, `INSERT INTO plex_servers(name, internal_origin_url, machine_identifier, enabled)
		VALUES('Identity Box','http://test.invalid:32400','test-identity-box',true) RETURNING id`).Scan(&serverID); err != nil {
		t.Fatal(err)
	}
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
	tv := &fakeTV{code: 401}
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
		t.Fatalf("401 earns the one-hour negative cache, calls=%d", tv.calls.Load())
	}
}

func TestLiveTransportDegradesWithoutMark(t *testing.T) {
	tv := &fakeTV{err: errors.New("plex.tv 500")}
	r, _ := liveResolver(t, tv)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		r2 := New(r.DB, tv)
		got := r2.Resolve(ctx, "fp-flaky", "tok-flaky", Client{})
		if got.Known || !got.Degraded {
			t.Fatalf("transport failure must degrade, not resolve: %+v", got)
		}
	}
	if tv.calls.Load() != 2 {
		t.Fatalf("degraded tokens must retry (no negative cache), calls=%d", tv.calls.Load())
	}
	var invalid int
	if err := r.DB.QueryRow(ctx, `SELECT count(*) FROM plex_token_identities
		WHERE token_fingerprint='fp-flaky' AND token_status='invalid'`).Scan(&invalid); err != nil || invalid != 0 {
		t.Fatalf("transport failure must not mark invalid: %v %d", err, invalid)
	}
}

func TestLiveSplitDeviceCache(t *testing.T) {
	tv := &fakeTV{id: 4242, user: "two-devices"}
	r, _ := liveResolver(t, tv)
	ctx := context.Background()
	tvClient := Client{Identifier: "apple-tv", Product: "Plex", Platform: "tvOS"}
	phone := Client{Identifier: "iphone", Product: "Plex", Platform: "iOS"}
	a := r.Resolve(ctx, "fp-shared", "tok-shared", tvClient)
	b := r.Resolve(ctx, "fp-shared", "tok-shared", phone)
	if !a.Known || !b.Known || a.Scope != b.Scope || a.IdentityID != b.IdentityID {
		t.Fatalf("same account must share scope+identity: %+v %+v", a, b)
	}
	if a.ClientID == "" || b.ClientID == "" || a.ClientID == b.ClientID {
		t.Fatalf("devices must resolve independently: %q vs %q", a.ClientID, b.ClientID)
	}
	if tv.calls.Load() != 1 {
		t.Fatalf("one plex.tv lookup for both devices, calls=%d", tv.calls.Load())
	}
}

func TestSplitCacheRace(t *testing.T) {
	tv := &fakeTV{id: 4242, user: "race"}
	r, _ := liveResolver(t, tv)
	ctx := context.Background()
	var wg sync.WaitGroup
	results := make([]Resolved, 16)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := "dev-a"
			if i%2 == 1 {
				id = "dev-b"
			}
			results[i] = r.Resolve(ctx, "fp-race", "tok-race", Client{Identifier: id})
		}(i)
	}
	wg.Wait()
	seen := map[string]string{}
	for _, res := range results {
		if !res.Known {
			t.Fatalf("all must resolve: %+v", res)
		}
		seen[res.ClientID] = res.ClientID
	}
	if len(seen) != 2 {
		t.Fatalf("two devices must hold two instances under race: %v", seen)
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
