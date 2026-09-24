package warmer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LJAM96/replx/internal/cache"
	"github.com/LJAM96/replx/internal/crypto"
	"github.com/LJAM96/replx/internal/identity"
	"github.com/LJAM96/replx/internal/testdb"
)

const testSecret = "warmer-test-secret-0123456789abcdef"

type fixture struct {
	hits   int
	token  string
	body   string
	status int
}

func (f *fixture) handler(w http.ResponseWriter, r *http.Request) {
	f.hits++
	if r.Header.Get("X-Plex-Token") != f.token {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if f.status != 0 {
		w.WriteHeader(f.status)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write([]byte(f.body))
}

func ownerProvider(token string) func(ctx context.Context) (string, bool) {
	return func(ctx context.Context) (string, bool) { return token, token != "" }
}

func ownerAccount(id int64) func(ctx context.Context) (int64, bool) {
	return func(ctx context.Context) (int64, bool) { return id, id != 0 }
}

func withOwnerAccount(w *Warmer, id int64) *Warmer {
	w.OwnerAccount = ownerAccount(id)
	return w
}

func TestRefreshesOwnerEntry(t *testing.T) {
	fx := &fixture{token: "owner-tok", body: "<v>1</v>"}
	origin := httptest.NewServer(http.HandlerFunc(fx.handler))
	defer origin.Close()
	store := cache.NewMemory()
	w := withOwnerAccount(New(store, origin.URL, testSecret, ownerProvider("owner-tok"), nil, nil), 7)
	w.now = time.Now

	ctx := context.Background()
	key := "k-owner"
	w.Track(key, Snapshot{Method: "GET", Path: "/library/sections", Scope: "acct:7", TTL: time.Minute})
	// Not due yet: no fetch.
	w.RefreshOnce(ctx)
	if fx.hits != 0 {
		t.Fatalf("must not refresh before TTL/2, hits=%d", fx.hits)
	}
	// Age past due by moving the clock.
	w.now = func() time.Time { return time.Now().Add(40 * time.Second) }
	w.RefreshOnce(ctx)
	if fx.hits != 1 {
		t.Fatalf("want 1 refresh fetch, hits=%d", fx.hits)
	}
	e, ok, err := store.Get(ctx, key)
	if err != nil || !ok || string(e.Body) != "<v>1</v>" {
		t.Fatalf("refreshed entry: ok=%v err=%v %+v", ok, err, e)
	}
	st := w.Stats()
	if st.Refreshed != 1 || !st.OwnerWarming {
		t.Fatalf("stats: %+v", st)
	}
}

func TestDropsNonOwnerEntry(t *testing.T) {
	fx := &fixture{token: "owner-tok", body: "<v>1</v>"}
	origin := httptest.NewServer(http.HandlerFunc(fx.handler))
	defer origin.Close()
	store := cache.NewMemory()
	w := withOwnerAccount(New(store, origin.URL, testSecret, ownerProvider("owner-tok"), nil, nil), 7)
	w.Track("k-user", Snapshot{Method: "GET", Path: "/hubs/x", Scope: "acct:9", TTL: time.Minute})
	// Age the clock so the entry is immediately due.
	w.now = func() time.Time { return time.Now().Add(time.Hour) }
	w.RefreshOnce(context.Background())
	if fx.hits != 0 {
		t.Fatal("non-owner entry must never trigger an origin fetch")
	}
	if st := w.Stats(); st.Tracked != 0 {
		t.Fatalf("non-owner entry must be dropped, tracked=%d", st.Tracked)
	}
}

func TestLiveRefreshesOnlyMatchingValidatedUserToken(t *testing.T) {
	ctx, db := testdb.Begin(t)
	const token = "managed-a-token"
	fingerprint := crypto.Fingerprint(testSecret, token)
	ciphertext, err := crypto.Encrypt(testSecret, identity.PurposeUserToken, []byte(token))
	if err != nil {
		t.Fatal(err)
	}
	var serverID string
	if err := db.QueryRow(ctx, `INSERT INTO plex_servers(name,internal_origin_url,machine_identifier,enabled)
		VALUES('Warmer Box','http://test.invalid:32400','test-warmer-box',true) RETURNING id`).Scan(&serverID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO plex_token_identities(server_id,token_fingerprint,token_ciphertext,token_status,last_validated_at)
		VALUES($1,$2,$3,'pms_valid',now())`, serverID, fingerprint, ciphertext); err != nil {
		t.Fatal(err)
	}
	hits := 0
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.Header.Get("X-Plex-Token") != token {
			t.Error("wrong user's credential used")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"user":"a"}`))
	}))
	defer origin.Close()
	store := cache.NewMemory()
	w := New(store, origin.URL, testSecret, ownerProvider(""), nil, nil)
	w.DB = db
	w.Track("user-a-page", Snapshot{Method: "GET", Path: "/library/collections/101/children", Scope: "tok:" + fingerprint, TTL: time.Minute})
	w.now = func() time.Time { return time.Now().Add(40 * time.Second) }
	w.RefreshOnce(ctx)
	entry, ok, err := store.Get(ctx, "user-a-page")
	if err != nil || !ok || string(entry.Body) != `{"user":"a"}` || hits != 1 {
		t.Fatalf("per-user refresh: entry=%q ok=%v err=%v hits=%d", entry.Body, ok, err, hits)
	}
	if got := w.userToken(ctx, "tok:"+crypto.Fingerprint(testSecret, "other-user")); got != "" {
		t.Fatal("another scope obtained a credential")
	}
}

func TestNoCredentialsSkips(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no fetch without credentials")
	}))
	defer origin.Close()
	w := New(cache.NewMemory(), origin.URL, testSecret, ownerProvider(""), nil, nil)
	w.now = func() time.Time { return time.Now().Add(time.Hour) }
	w.Track("k", Snapshot{Method: "GET", Path: "/x", Scope: "tok:fp", TTL: time.Minute})
	w.RefreshOnce(context.Background())
	if st := w.Stats(); st.OwnerWarming {
		t.Fatal("warming must be false without credentials")
	}
}

func TestErrorStatusNotStored(t *testing.T) {
	fx := &fixture{token: "owner-tok", status: http.StatusInternalServerError}
	origin := httptest.NewServer(http.HandlerFunc(fx.handler))
	defer origin.Close()
	store := cache.NewMemory()
	w := withOwnerAccount(New(store, origin.URL, testSecret, ownerProvider("owner-tok"), nil, nil), 7)
	w.Track("k-err", Snapshot{Method: "GET", Path: "/x", Scope: "acct:7", TTL: time.Minute})
	w.now = func() time.Time { return time.Now().Add(time.Hour) }
	w.RefreshOnce(context.Background())
	if _, ok, _ := store.Get(context.Background(), "k-err"); ok {
		t.Fatal("error responses must never populate hot keys")
	}
	if st := w.Stats(); st.Errors != 1 {
		t.Fatalf("errors: %+v", st)
	}
	w.RefreshOnce(context.Background())
	if fx.hits != 1 {
		t.Fatalf("failed refresh retried immediately: hits=%d", fx.hits)
	}
}

func TestStripSecrets(t *testing.T) {
	got := stripSecrets("type=2&X-Plex-Token=user-secret&b=1&authToken=z")
	if strings.Contains(got, "user-secret") || strings.Contains(got, "X-Plex-Token") || strings.Contains(got, "authToken") {
		t.Fatalf("secrets retained: %s", got)
	}
	if !strings.Contains(got, "type=2") || !strings.Contains(got, "b=1") {
		t.Fatalf("params lost: %s", got)
	}
}
