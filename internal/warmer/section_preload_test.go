package warmer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/LJAM96/replx/internal/cache"
	"github.com/LJAM96/replx/internal/crypto"
	"github.com/LJAM96/replx/internal/identity"
	"github.com/LJAM96/replx/internal/testdb"
)

func TestManagedSectionHubsWarmWithOwnToken(t *testing.T) {
	ctx, db := testdb.Begin(t)
	var serverID, ownerIdentity string
	if err := db.QueryRow(ctx, `INSERT INTO plex_servers(name,internal_origin_url,machine_identifier,enabled)
		VALUES('Section Box','http://test.invalid:32400','test-section-box',true) RETURNING id`).Scan(&serverID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `INSERT INTO plex_identities(server_id,plex_account_id,identity_type)
		VALUES($1,7,'owner') RETURNING id`, serverID).Scan(&ownerIdentity); err != nil {
		t.Fatal(err)
	}
	const managedToken = "managed-section-token"
	fingerprint := crypto.Fingerprint(testSecret, managedToken)
	ciphertext, err := crypto.Encrypt(testSecret, identity.PurposeUserToken, []byte(managedToken))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO plex_token_identities(server_id,token_fingerprint,token_ciphertext,token_status,last_validated_at)
		VALUES($1,$2,$3,'pms_valid',now())`, serverID, fingerprint, ciphertext); err != nil {
		t.Fatal(err)
	}
	calls := 0
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != managedToken || !sectionHubPath(r.URL.Path) ||
			r.URL.Query().Get("count") != "12" || r.URL.Query().Has("X-Plex-Token") {
			t.Error("section refresh used wrong credential or request")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"MediaContainer":{"Hub":[]}}`))
	}))
	defer origin.Close()
	store := cache.NewMemory()
	w := New(store, origin.URL, testSecret, ownerProvider("owner-token"), nil, nil)
	w.DB = db
	w.OwnerAccount = ownerAccount(7)
	w.PreloadSections = func(context.Context) ([]string, error) { return []string{"22", "23"}, nil }
	w.KeyFunc = func(s Snapshot) string {
		q, _ := url.ParseQuery(s.RawQuery)
		return cache.ResponseKey(s.Scope, s.Method, s.Path, q, s.Accept)
	}
	scope := "tok:" + fingerprint
	w.Track("observed", Snapshot{Method: http.MethodGet, Path: "/hubs/sections/22",
		RawQuery: "count=12&includeMeta=1&X-Plex-Token=private", Scope: scope,
		Class: "hubs", Accept: preloadAccept, TTL: 10 * time.Second})
	entry, hit, err := store.Get(ctx, sectionHubProfileKey(scope))
	if err != nil || !hit || strings.Contains(string(entry.Body), "private") {
		t.Fatal("sanitized section profile was not saved")
	}
	w.sectionScopeCursor = 1 // owner first, managed user second
	pages, failures := w.PreloadUserSectionsOnce(ctx)
	if pages != 2 || failures != 0 || calls != 2 {
		t.Fatalf("section pages=%d failures=%d calls=%d", pages, failures, calls)
	}
	if stats := w.Stats(); stats.UserSectionPages != 2 || stats.UserSectionErrors != 0 || stats.LastSectionUnix == 0 {
		t.Fatalf("section warm stats: %+v", stats)
	}
	for _, id := range []string{"22", "23"} {
		s := Snapshot{Method: http.MethodGet, Path: "/hubs/sections/" + id, Scope: scope,
			Accept: preloadAccept, RawQuery: "count=12&includeMeta=1"}
		if _, ok, _ := store.Get(ctx, cache.StaleKey(w.KeyFunc(s))); !ok {
			t.Fatalf("section %s not warmed", id)
		}
		s.Scope = cache.UserScope(ownerIdentity)
		if _, ok, _ := store.Get(ctx, cache.StaleKey(w.KeyFunc(s))); ok {
			t.Fatal("managed response leaked into owner cache")
		}
	}
	w.sectionScopeCursor = 1
	pages, failures = w.PreloadUserSectionsOnce(ctx)
	if pages != 0 || failures != 0 || calls != 2 {
		t.Fatal("warm section hubs were fetched again")
	}
}

func TestSectionHubFallbackMatchesCurrentBrowserQuery(t *testing.T) {
	w := New(cache.NewMemory(), "http://example.invalid", testSecret, nil, nil, nil)
	w.PreloadHubQuery = "contentDirectoryID=22&pinnedContentDirectoryID=22%2C23&excludeContinueWatching=1&count=12&includeMeta=1&includeLibraryPlaylists=1&X-Plex-Model=bundled&X-Plex-Device-Screen-Resolution=800x600&X-Plex-Client-Identifier=old"
	profile, ok := w.loadSectionHubProfile(context.Background(), "user:example")
	if !ok {
		t.Fatal("section hub fallback missing")
	}
	fallback, _ := url.ParseQuery(profile.Query)
	browser, _ := url.ParseQuery("count=12&includeMeta=1&includeLibraryPlaylists=1&includeExternalMetadata=1&X-Plex-Model=standalone&X-Plex-Device-Screen-Resolution=1800x1100&X-Plex-Client-Identifier=new")
	a := cache.ResponseKey("user:example", "GET", "/hubs/sections/22", fallback, profile.Accept)
	b := cache.ResponseKey("user:example", "GET", "/hubs/sections/22", browser, "application/json")
	if a != b {
		t.Fatal("fallback did not match current Plex Web section query")
	}
}
