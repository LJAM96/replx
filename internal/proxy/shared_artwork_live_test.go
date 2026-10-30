package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/LJAM96/replx/internal/artwork"
	"github.com/LJAM96/replx/internal/testdb"
)

func TestSharedArtworkAuthorizesEachUserAndDeduplicatesBlob(t *testing.T) {
	ctx, db := testdb.Begin(t)
	store, err := artwork.NewShared(db, t.TempDir(), artwork.SharedOptions{
		Namespace: t.Name(), MaxBytes: 64 << 20,
		FreeBytes: func(string) (uint64, error) { return 1 << 50, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	hits := 0
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("same-artwork-bytes"))
	}))
	defer origin.Close()
	h, err := New(Options{OriginBase: origin.URL, IngressMode: "direct", Secret: "test-secret-key", Artwork: store})
	if err != nil {
		t.Fatal(err)
	}
	get := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/photo/:/transcode?width=480&url=%2Flibrary%2Fmetadata%2F1%2Fthumb", nil)
		if token != "" {
			req.Header.Set("X-Plex-Token", token)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	for i, token := range []string{"authorized-a", "authorized-b", "authorized-c"} {
		first := get(token)
		if first.Code != http.StatusOK || first.Header().Get(CacheHeader) != "miss" || hits != i+1 {
			t.Fatalf("user %d must authorize through PMS first: status=%d cache=%s originHits=%d", i, first.Code, first.Header().Get(CacheHeader), hits)
		}
		repeat := get(token)
		if repeat.Header().Get(CacheHeader) != "hit" || hits != i+1 {
			t.Fatalf("user %d should hit only its scoped reference: cache=%s originHits=%d", i, repeat.Header().Get(CacheHeader), hits)
		}
	}
	stats, err := store.Stats(ctx)
	if err != nil || stats.Blobs != 1 || stats.Variants != 3 {
		t.Fatalf("three authorized users should share one blob through scoped refs: %+v %v", stats, err)
	}
	if denied := get(""); denied.Header().Get(CacheHeader) == "hit" || hits != 4 {
		t.Fatal("anonymous user must not reuse an authorized artwork reference")
	}
}
