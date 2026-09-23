package warmer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/LJAM96/replx/internal/artwork"
	"github.com/LJAM96/replx/internal/cache"
)

func TestPreloadFillsOwnerPagesAndMatchingArtwork(t *testing.T) {
	pages, images := 0, 0
	seen := map[string]bool{}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != "owner-token" {
			t.Errorf("preload used the wrong credential")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/photo/:/transcode" {
			inner, err := url.Parse(r.URL.Query().Get("url"))
			if err != nil || inner.Query().Get("X-Plex-Token") != "owner-token" {
				t.Errorf("artwork URL did not use the current owner credential")
			}
			images++
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte("poster"))
			return
		}
		pages++
		seen[r.URL.Path] = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"MediaContainer":{}}`))
	}))
	defer origin.Close()
	art, err := artwork.New(t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	store := cache.NewMemory()
	w := withOwnerAccount(New(store, origin.URL, testSecret, ownerProvider("owner-token"), nil, nil), 7)
	w.Artwork = art
	w.KeyFunc = func(s Snapshot) string {
		q, _ := url.ParseQuery(s.RawQuery)
		return cache.ResponseKey(s.Scope, s.Method, s.Path, q, s.Accept)
	}
	w.PreloadSections = func(context.Context) ([]string, error) { return []string{"23", "../bad"}, nil }
	w.PreloadArtworkPaths = func(context.Context) ([]string, error) {
		return []string{"/library/metadata/1/thumb/2?X-Plex-Token=stale", "https://evil.example/poster"}, nil
	}
	result := w.PreloadOnce(context.Background())
	if !result.Ready || result.Pages != 6 || result.Artwork != 1 || result.Errors != 0 {
		t.Fatalf("preload: %+v", result)
	}
	if pages != 6 || images != 1 {
		t.Fatalf("unexpected origin work: pages=%d images=%d", pages, images)
	}
	if !seen["/hubs/continueWatching"] || !seen["/library/sections/23/collections"] {
		t.Fatal("common owner pages were not fetched")
	}
	if stats := w.Stats(); stats.PreloadPages != 6 || stats.PreloadArtwork != 1 || stats.LastPreloadUnix == 0 {
		t.Fatalf("preload evidence missing: %+v", stats)
	}
	key := w.KeyFunc(Snapshot{Method: http.MethodGet, Path: "/hubs/sections/23",
		Accept: preloadAccept, Scope: "acct:7"})
	if _, ok, _ := store.Get(context.Background(), key); !ok {
		t.Fatal("owner section hub was not preloaded")
	}
	q := url.Values{"width": {"480"}, "height": {"720"}, "minSize": {"1"},
		"upscale": {"1"}, "url": {"/library/metadata/1/thumb/2?X-Plex-Token=rotated"}}
	if _, body, ok := art.Get(artwork.Key("acct:7", "/photo/:/transcode", q)); !ok || string(body) != "poster" {
		t.Fatal("browser request with a rotated token did not hit preloaded artwork")
	}
	if _, _, ok := art.Get(artwork.Key("acct:8", "/photo/:/transcode", q)); ok {
		t.Fatal("another user must not receive owner artwork")
	}
	w.PreloadOnce(context.Background())
	if pages != 6 || images != 1 {
		t.Fatal("a second preload pass must reuse fresh cached entries")
	}
}

func TestPreloadWithoutOwnerDoesNotFetch(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("preload must not fetch without an owner credential")
	}))
	defer origin.Close()
	w := New(cache.NewMemory(), origin.URL, testSecret, ownerProvider(""), nil, nil)
	w.OwnerAccount = ownerAccount(7)
	w.KeyFunc = func(s Snapshot) string { return strings.Join([]string{s.Scope, s.Path}, ":") }
	if got := w.PreloadOnce(context.Background()); got.Ready || got.Pages != 0 {
		t.Fatalf("preload without owner: %+v", got)
	}
}

func TestPreloadUsesConfiguredHubQueryForBrowserKey(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != "owner-token" {
			t.Error("preload did not use owner credential")
		}
		if r.URL.Query().Has("X-Plex-Token") {
			t.Error("preload replayed a token from the query profile")
		}
		_, _ = w.Write([]byte(`{"MediaContainer":{}}`))
	}))
	defer origin.Close()
	store := cache.NewMemory()
	w := withOwnerAccount(New(store, origin.URL, testSecret, ownerProvider("owner-token"), nil, nil), 7)
	w.PreloadHubQuery = "count=12&includeMeta=1&X-Plex-Client-Identifier=browser-1&X-Plex-Token=must-not-replay"
	w.PreloadSections = func(context.Context) ([]string, error) { return []string{"23"}, nil }
	w.KeyFunc = func(s Snapshot) string {
		q, _ := url.ParseQuery(s.RawQuery)
		return cache.ResponseKey(s.Scope, s.Method, s.Path, q, s.Accept)
	}
	w.PreloadOnce(context.Background())
	browserQuery, _ := url.ParseQuery("includeMeta=1&count=12&X-Plex-Client-Identifier=browser-1&X-Plex-Token=rotated")
	key := cache.ResponseKey("acct:7", http.MethodGet, "/hubs/sections/23", browserQuery, preloadAccept)
	if _, ok, _ := store.Get(context.Background(), key); !ok {
		t.Fatal("browser's exact collection query was not preloaded")
	}
	cwKey := cache.ResponseKey("acct:7", http.MethodGet, "/hubs/continueWatching", browserQuery, preloadAccept)
	if _, ok, _ := store.Get(context.Background(), cwKey); !ok {
		t.Fatal("browser's exact Continue Watching query was not preloaded")
	}
}
