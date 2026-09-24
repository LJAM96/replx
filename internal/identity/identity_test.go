package identity

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/LJAM96/replx/internal/crypto"
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

type fakePMSValidator struct {
	valid bool
	err   error
	calls atomic.Int64
}

func (f *fakePMSValidator) ValidateToken(context.Context, string) (bool, error) {
	f.calls.Add(1)
	return f.valid, f.err
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

func TestLivePMSOnlyTokenUsesPrivateCacheScope(t *testing.T) {
	tv := &fakeTV{code: 401}
	r, _ := liveResolver(t, tv)
	pms := &fakePMSValidator{valid: true}
	r.PMS = pms
	ctx := context.Background()
	got := r.Resolve(ctx, "fp-managed", "managed-token", Client{})
	if !got.Fresh || got.Invalid || got.Known || got.IdentityID != "" || got.Scope != "tok:fp-managed" {
		t.Fatalf("PMS-only token must stay token scoped: %+v", got)
	}
	var status string
	if err := r.DB.QueryRow(ctx, `SELECT token_status FROM plex_token_identities WHERE token_fingerprint='fp-managed'`).Scan(&status); err != nil || status != "pms_valid" {
		t.Fatalf("PMS status: %q %v", status, err)
	}
	r2 := New(r.DB, tv)
	r2.PMS = pms
	got2 := r2.Resolve(ctx, "fp-managed", "managed-token", Client{})
	if !got2.Fresh || got2.Scope != got.Scope || tv.calls.Load() != 1 || pms.calls.Load() != 1 {
		t.Fatalf("reloaded PMS-only token: %+v tv=%d pms=%d", got2, tv.calls.Load(), pms.calls.Load())
	}
}

func TestLiveValidatedTokenEncryptedAndRevoked(t *testing.T) {
	const secret = "user-token-storage-test-secret-0123456789"
	const token = "managed-user-secret-token"
	r, serverID := liveResolver(t, &fakeTV{code: 401})
	r.Secret = secret
	pms := &fakePMSValidator{valid: true}
	r.PMS = pms
	fingerprint := crypto.Fingerprint(secret, token)
	got := r.Resolve(context.Background(), fingerprint, token, Client{})
	if !got.Fresh || got.Scope != "tok:"+fingerprint {
		t.Fatalf("managed identity: %+v", got)
	}
	var ciphertext []byte
	if err := r.DB.QueryRow(context.Background(), `SELECT token_ciphertext FROM plex_token_identities
		WHERE server_id=$1 AND token_fingerprint=$2`, serverID, fingerprint).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if string(ciphertext) == token || len(ciphertext) == 0 {
		t.Fatal("validated token was not encrypted")
	}
	plain, err := crypto.Decrypt(secret, PurposeUserToken, ciphertext)
	if err != nil || string(plain) != token {
		t.Fatalf("stored token cannot be recovered with the correct purpose: %v", err)
	}
	r2 := New(r.DB, &fakeTV{code: 401})
	r2.PMS = &fakePMSValidator{valid: false}
	r2.Secret = secret
	if _, err := r.DB.Exec(context.Background(), `UPDATE plex_token_identities SET last_validated_at=now()-make_interval(mins => 10)
		WHERE server_id=$1 AND token_fingerprint=$2`, serverID, fingerprint); err != nil {
		t.Fatal(err)
	}
	if result := r2.Resolve(context.Background(), fingerprint, token, Client{}); !result.Invalid {
		t.Fatalf("revoked token authorized: %+v", result)
	}
	var retained bool
	if err := r.DB.QueryRow(context.Background(), `SELECT token_ciphertext IS NOT NULL FROM plex_token_identities
		WHERE server_id=$1 AND token_fingerprint=$2`, serverID, fingerprint).Scan(&retained); err != nil || retained {
		t.Fatalf("revoked token retained: %v %v", retained, err)
	}
}

func TestLivePMSRejectKeepsCacheClosed(t *testing.T) {
	r, _ := liveResolver(t, &fakeTV{code: 401})
	r.PMS = &fakePMSValidator{valid: false}
	got := r.Resolve(context.Background(), "fp-revoked", "revoked-token", Client{})
	if !got.Invalid || got.Fresh || got.Scope != "tok:fp-revoked" {
		t.Fatalf("rejected token served locally: %+v", got)
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

func TestMemCacheRace(t *testing.T) {
	// Pure in-memory race: nil DB means every lookup falls back without
	// touching storage. Concurrent identical fingerprints must share one
	// scope deterministically, exercising the map locking for -race.
	// (pgx.Tx is a single connection and must never be shared across
	// goroutines; database concurrency is covered separately below.)
	r := New(nil, nil)
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
	for _, res := range results {
		if res.Scope != "tok:fp-race" || res.Known {
			t.Fatalf("fallback must be uniform: %+v", res)
		}
	}
}

func TestLiveConcurrentResolves(t *testing.T) {
	// Database concurrency over INDEPENDENT connections: each goroutine
	// owns its pool, transaction and connection, seeds its own server,
	// and rolls back before signalling done. The single-enabled-server
	// constraint serializes the inserts safely (no shared pgx.Tx, which
	// must never cross goroutines); rollback releases each waiter.
	// Pre-flight in the test goroutine so helper Skip/Fatal stays legal.
	preCtx, preTx := testdb.Begin(t)
	_ = preTx.Rollback(preCtx)
	tv := &fakeTV{id: 4242, user: "race"}
	ctx := context.Background()
	const n = 8
	results := make([]Resolved, n)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, tx := testdb.Begin(t)
			// Fresh transactions start empty: no DELETE needed, and
			// none is wanted (a DELETE would lock-wait on siblings'
			// uncommitted rows). Only the enabled-server inserts
			// serialize, single-statement, deadlock-free.
			machine := "test-identity-conc"
			var sid string
			if err := tx.QueryRow(ctx, `INSERT INTO plex_servers(name, internal_origin_url, machine_identifier, enabled)
				VALUES('Concurrent','http://test.invalid:32400',$1,true) RETURNING id`, machine).Scan(&sid); err != nil {
				return
			}
			_ = sid
			r := New(tx, tv)
			fp := "fp-conc-" + string(rune('a'+i))
			results[i] = r.Resolve(ctx, fp, "tok-conc", Client{Identifier: "dev-conc"})
			_ = tx.Rollback(ctx)
		}(i)
	}
	wg.Wait()
	for _, res := range results {
		if !res.Known || res.ClientID == "" {
			t.Fatalf("all must resolve with instances: %+v", res)
		}
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

func TestLiveRevokedBreaksAssociation(t *testing.T) {
	tv := &fakeTV{id: 4242, user: "revoked"}
	r, _ := liveResolver(t, tv)
	ctx := context.Background()
	first := r.Resolve(ctx, "fp-rev", "tok-rev", Client{})
	if !first.Known || !first.Fresh {
		t.Fatalf("fresh token must resolve known+fresh: %+v", first)
	}
	// Age the proof past the validity window, then revoke at plex.tv.
	if _, err := r.DB.Exec(ctx, `UPDATE plex_token_identities SET last_validated_at = now() - make_interval(days => 2)
		WHERE token_fingerprint='fp-rev'`); err != nil {
		t.Fatal(err)
	}
	tv.code = 401
	tv.id = 0
	got := New(r.DB, tv).Resolve(ctx, "fp-rev", "tok-rev", Client{})
	if got.Known || !got.Invalid {
		t.Fatalf("revoked credential must be unknown+invalid: %+v", got)
	}
	var assoc *string
	var status string
	if err := r.DB.QueryRow(ctx, `SELECT identity_id::text, token_status FROM plex_token_identities
		WHERE token_fingerprint='fp-rev'`).Scan(&assoc, &status); err != nil {
		t.Fatal(err)
	}
	if assoc != nil || status != "invalid" {
		t.Fatalf("revocation must break the association: %v %q", assoc, status)
	}
}
