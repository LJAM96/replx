package warmer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/LJAM96/replx/internal/cache"
	"github.com/LJAM96/replx/internal/crypto"
	"github.com/LJAM96/replx/internal/identity"
	"github.com/LJAM96/replx/internal/testdb"
)

func TestManagedUserHomeWarmsPinnedSectionsWithOwnToken(t *testing.T) {
	ctx, db := testdb.Begin(t)
	var serverID, ownerIdentity string
	if err := db.QueryRow(ctx, `INSERT INTO plex_servers(name,internal_origin_url,machine_identifier,enabled)
		VALUES('Home Box','http://test.invalid:32400','test-home-box',true) RETURNING id`).Scan(&serverID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `INSERT INTO plex_identities(server_id,plex_account_id,identity_type)
		VALUES($1,7,'owner') RETURNING id`, serverID).Scan(&ownerIdentity); err != nil {
		t.Fatal(err)
	}
	const managedToken = "managed-home-token"
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
		if r.Header.Get("X-Plex-Token") != managedToken || r.URL.Path != "/hubs/promoted" ||
			!strings.Contains(",22,23,", ","+r.URL.Query().Get("contentDirectoryID")+",") ||
			r.URL.Query().Has("X-Plex-Token") {
			t.Error("Home refresh used wrong credential, path, or section")
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
	w.Track("observed", Snapshot{Method: http.MethodGet, Path: "/hubs/promoted",
		RawQuery: "contentDirectoryID=22&pinnedContentDirectoryID=22%2C23&X-Plex-Token=private",
		Scope:    scope, Class: "hubs", Accept: preloadAccept, TTL: 10})
	entry, ok, err := store.Get(ctx, homeProfileKey(scope))
	if err != nil || !ok || strings.Contains(string(entry.Body), "private") {
		t.Fatal("sanitized Home profile not saved")
	}
	w.homeScopeCursor = 1 // owner first, managed user second
	pages, failures := w.PreloadUserHomeOnce(ctx)
	if pages != 2 || failures != 0 || calls != 2 {
		t.Fatalf("Home warm pages=%d failures=%d origin calls=%d", pages, failures, calls)
	}
	for _, section := range []string{"22", "23"} {
		s := Snapshot{Method: http.MethodGet, Path: "/hubs/promoted", Scope: scope, Accept: preloadAccept,
			RawQuery: "contentDirectoryID=" + section + "&pinnedContentDirectoryID=22%2C23"}
		if _, ok, _ := store.Get(ctx, cache.StaleKey(w.KeyFunc(s))); !ok {
			t.Fatalf("managed Home section %s not cached", section)
		}
		s.Scope = cache.UserScope(ownerIdentity)
		if _, ok, _ := store.Get(ctx, cache.StaleKey(w.KeyFunc(s))); ok {
			t.Fatal("managed Home response leaked to owner scope")
		}
	}
	w.homeScopeCursor = 1
	pages, failures = w.PreloadUserHomeOnce(ctx)
	if pages != 0 || failures != 0 || calls != 2 {
		t.Fatal("already warm Home sections were fetched again")
	}
}
