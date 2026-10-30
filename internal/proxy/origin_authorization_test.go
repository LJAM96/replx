package proxy

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/LJAM96/replx/internal/artwork"
	"github.com/LJAM96/replx/internal/cache"
	"github.com/LJAM96/replx/internal/warmer"
)

func TestOriginRejectionRevokesLocalAuthorization(t *testing.T) {
	const metadata = "/library/metadata/1"
	const image = "/photo/:/transcode?url=%2Flibrary%2Fmetadata%2F1%2Fthumb&width=480"
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		for _, rejectedPath := range []string{
			"/library/metadata/denied",
			"/photo/:/transcode?url=%2Flibrary%2Fmetadata%2Fdenied%2Fthumb&width=480",
		} {
			t.Run(http.StatusText(status)+rejectedPath, func(t *testing.T) {
				var rejected atomic.Bool
				origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if rejected.Load() && r.Header.Get("X-Plex-Token") == "rejected-caller" {
						w.WriteHeader(status)
						_, _ = w.Write([]byte("origin rejection"))
						return
					}
					if artwork.Match(r.URL.Path) {
						w.Header().Set("Content-Type", "image/jpeg")
					}
					_, _ = w.Write([]byte("private resource"))
				}))
				defer origin.Close()
				art, err := artwork.New(t.TempDir(), 1)
				if err != nil {
					t.Fatal(err)
				}
				store := cache.NewMemory()
				warm := warmer.New(store, origin.URL, "test-secret", nil, nil, nil)
				h, err := New(Options{OriginBase: origin.URL, IngressMode: "direct", Secret: "test-secret", Cache: store, Artwork: art, Warmer: warm})
				if err != nil {
					t.Fatal(err)
				}
				for _, token := range []string{"rejected-caller", "other-caller"} {
					for _, path := range []string{metadata, image} {
						if got := cacheRequest(h, "GET", path, token); got.Code != http.StatusOK {
							t.Fatalf("initial response: %d %s", got.Code, got.Body.String())
						}
						if got := cacheRequest(h, "GET", path, token); got.Header().Get(CacheHeader) != "hit" {
							t.Fatalf("initial resource did not become reusable: %s", path)
						}
					}
				}
				if st := warm.Stats(); st.ActiveContexts != 2 || st.Tracked != 2 {
					t.Fatalf("missing initial warming contexts/jobs: %+v", st)
				}
				rejected.Store(true)
				if got := cacheRequest(h, "GET", rejectedPath, "rejected-caller"); got.Code != status || got.Body.String() != "origin rejection" {
					t.Fatalf("origin rejection changed: %d %q", got.Code, got.Body.String())
				}
				if st := warm.Stats(); st.ActiveContexts != 1 || st.Tracked != 1 {
					t.Fatalf("rejected scope retained warming contexts/jobs: %+v", st)
				}
				for _, path := range []string{metadata, image} {
					if got := cacheRequest(h, "GET", path, "rejected-caller"); got.Code != status || got.Header().Get(CacheHeader) == "hit" || got.Body.String() != "origin rejection" {
						t.Fatalf("rejected caller reused %s: status=%d cache=%s body=%q", path, got.Code, got.Header().Get(CacheHeader), got.Body.String())
					}
					if got := cacheRequest(h, "GET", path, "other-caller"); got.Code != http.StatusOK || got.Header().Get(CacheHeader) != "hit" {
						t.Fatalf("unrelated caller lost reusable resource %s: status=%d cache=%s", path, got.Code, got.Header().Get(CacheHeader))
					}
				}
			})
		}
	}
}
