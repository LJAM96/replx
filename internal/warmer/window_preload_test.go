package warmer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/LJAM96/replx/internal/cache"
	"github.com/LJAM96/replx/internal/crypto"
	"github.com/LJAM96/replx/internal/identity"
	"github.com/LJAM96/replx/internal/testdb"
)

func TestFullCollectionRefreshStoresSlicableUserWindow(t *testing.T) {
	const token = "user-a-token"
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != token || r.URL.Query().Get("X-Plex-Container-Start") != "0" || r.URL.Query().Get("X-Plex-Container-Size") != "350" {
			t.Error("full window used wrong token or range")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"MediaContainer":{"offset":0,"size":3,"totalSize":3,"Metadata":[{"title":"a"},{"title":"b"},{"title":"c"}]}}`))
	}))
	defer origin.Close()
	store := cache.NewMemory()
	w := New(store, origin.URL, testSecret, ownerProvider(""), nil, nil)
	w.KeyFunc = func(s Snapshot) string {
		q, _ := url.ParseQuery(s.RawQuery)
		return cache.ResponseKey(s.Scope, s.Method, s.Path, q, s.Accept)
	}
	q := "includeMeta=1&X-Plex-Container-Start=0&X-Plex-Container-Size=350"
	s := Snapshot{Method: http.MethodGet, Path: "/library/collections/101/children", RawQuery: q,
		Scope: "tok:user-a", Class: "collections", Accept: "application/json", TTL: time.Minute}
	if err := w.refresh(context.Background(), w.KeyFunc(s), s, token); err != nil {
		t.Fatal(err)
	}
	browserQ, _ := url.ParseQuery("includeMeta=1&X-Plex-Container-Start=1&X-Plex-Container-Size=2")
	windowKey := cache.CollectionWindowKeyGen(s.Scope, s.Class, s.Method, s.Path, browserQ, s.Accept, 0, 0)
	entry, ok, err := store.Get(context.Background(), windowKey)
	if err != nil || !ok {
		t.Fatalf("user collection window missing: %v %v", ok, err)
	}
	if _, ok := cache.CollectionWindowPage(entry.Body, 1, 2); !ok {
		t.Fatal("browser range could not be sliced from the stored window")
	}
	if _, ok, _ := store.Get(context.Background(), cache.CollectionWindowKeyGen("tok:user-b", s.Class, s.Method, s.Path, browserQ, s.Accept, 0, 0)); ok {
		t.Fatal("user-b could read user-a's window")
	}
}

func TestCollectionWindowProfilesReplaceOnlyPagination(t *testing.T) {
	w := New(cache.NewMemory(), "http://test.invalid", testSecret, nil, nil, nil)
	w.PreloadCollectionQuery = "includeMeta=1&X-Plex-Container-Start=12&X-Plex-Container-Size=24&X-Plex-Token=secret"
	profiles := w.collectionWindowProfiles()
	if len(profiles) != 1 {
		t.Fatalf("profiles: %d", len(profiles))
	}
	q, err := url.ParseQuery(profiles[0])
	if err != nil || q.Get("X-Plex-Container-Start") != "0" || q.Get("X-Plex-Container-Size") != "350" || q.Get("includeMeta") != "1" || q.Has("X-Plex-Token") {
		t.Fatalf("unsafe or incorrect profile: %v", err)
	}
}

func TestLiveManagedUserWindowPreloadUsesOnlyItsToken(t *testing.T) {
	ctx, db := testdb.Begin(t)
	var serverID, ownerIdentity string
	if err := db.QueryRow(ctx, `INSERT INTO plex_servers(name,internal_origin_url,machine_identifier,enabled)
		VALUES('Window Box','http://test.invalid:32400','test-window-box',true) RETURNING id`).Scan(&serverID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `INSERT INTO plex_identities(server_id,plex_account_id,identity_type)
		VALUES($1,7,'owner') RETURNING id`, serverID).Scan(&ownerIdentity); err != nil {
		t.Fatal(err)
	}
	const managedToken = "managed-window-token"
	fingerprint := crypto.Fingerprint(testSecret, managedToken)
	ciphertext, err := crypto.Encrypt(testSecret, identity.PurposeUserToken, []byte(managedToken))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO plex_token_identities(server_id,token_fingerprint,token_ciphertext,token_status,last_validated_at)
		VALUES($1,$2,$3,'pms_valid',now())`, serverID, fingerprint, ciphertext); err != nil {
		t.Fatal(err)
	}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != managedToken {
			t.Error("owner credential used for managed user's collection")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"MediaContainer":{"offset":0,"size":2,"totalSize":2,"Metadata":[{"title":"one"},{"title":"two"}]}}`))
	}))
	defer origin.Close()
	store := cache.NewMemory()
	w := New(store, origin.URL, testSecret, ownerProvider("owner-token"), nil, nil)
	w.DB = db
	w.OwnerAccount = ownerAccount(7)
	w.PreloadSections = func(context.Context) ([]string, error) { return []string{"23"}, nil }
	w.PreloadCollectionQuery = "includeMeta=1&X-Plex-Container-Start=12&X-Plex-Container-Size=24"
	w.KeyFunc = func(s Snapshot) string {
		q, _ := url.ParseQuery(s.RawQuery)
		return cache.ResponseKey(s.Scope, s.Method, s.Path, q, s.Accept)
	}
	list := Snapshot{Method: http.MethodGet, Path: "/library/sections/23/collections", Scope: cache.UserScope(ownerIdentity), Accept: preloadAccept}
	if err := store.Set(ctx, w.KeyFunc(list), cache.Entry{Status: 200, ContentType: "application/json",
		Body: []byte(`{"MediaContainer":{"Metadata":[{"ratingKey":"101"}]}}`)}, time.Minute); err != nil {
		t.Fatal(err)
	}
	w.windowScopeCursor = 1 // owner first, then managed user
	pages, failures := w.PreloadCollectionWindowsOnce(ctx)
	if pages != 1 || failures != 0 {
		t.Fatalf("managed user preload: pages=%d failures=%d", pages, failures)
	}
	q, _ := url.ParseQuery("includeMeta=1&X-Plex-Container-Start=1&X-Plex-Container-Size=1")
	key := cache.CollectionWindowKeyGen("tok:"+fingerprint, "collections", http.MethodGet,
		"/library/collections/101/children", q, preloadAccept, 0, 0)
	if _, ok, _ := store.Get(ctx, key); !ok {
		t.Fatal("managed user's full window not cached")
	}
	ownerKey := cache.CollectionWindowKeyGen(cache.UserScope(ownerIdentity), "collections", http.MethodGet,
		"/library/collections/101/children", q, preloadAccept, 0, 0)
	if _, ok, _ := store.Get(ctx, ownerKey); ok {
		t.Fatal("managed response placed in owner scope")
	}
}
