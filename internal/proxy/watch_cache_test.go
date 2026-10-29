package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LJAM96/replx/internal/cache"
)

func watchCacheHandler(t *testing.T, origin http.Handler) *Handler {
	t.Helper()
	server := httptest.NewServer(origin)
	t.Cleanup(server.Close)
	h, err := New(Options{OriginBase: server.URL, IngressMode: "direct", Secret: "watch-cache-test-secret", Cache: cache.NewMemory()})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func cacheRequest(h *Handler, method, path, user string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	r.Header.Set("X-Plex-Token", user)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestHubAndResumeFeedsAlwaysReachOrigin(t *testing.T) {
	var hits atomic.Int32
	h := watchCacheHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, hits.Add(1))
	}))
	for _, path := range []string{"/hubs", "/hubs/home", "/hubs/promoted", "/hubs/sections/1", "/hubs/home/continueWatching", "/library/onDeck", "/library/sections/1/onDeck"} {
		first := cacheRequest(h, "GET", path, "a")
		second := cacheRequest(h, "GET", path, "a")
		if first.Header().Get(CacheHeader) != "bypass" || second.Header().Get(CacheHeader) != "bypass" || first.Body.String() == second.Body.String() {
			t.Errorf("%s must fetch live each time", path)
		}
	}
}

func TestSuccessfulWatchWritesInvalidateAllWriterResponses(t *testing.T) {
	for _, path := range []string{"/:/timeline?state=stopped", "/:/progress", "/:/scrobble", "/:/unscrobble", "/video/:/transcode/universal/stop"} {
		t.Run(path, func(t *testing.T) {
			var state atomic.Int32
			h := watchCacheHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if isStateWrite(r.URL.Path) {
					state.Add(1)
					w.WriteHeader(204)
					return
				}
				fmt.Fprint(w, state.Load())
			}))
			for _, resource := range []string{"/library/metadata/1", "/library/sections/1/all", "/library/collections/1/children"} {
				cacheRequest(h, "GET", resource, "a")
				cacheRequest(h, "GET", resource, "b")
			}
			if got := cacheRequest(h, "GET", path, "a"); got.Code != 204 {
				t.Fatalf("write status %d", got.Code)
			}
			for _, resource := range []string{"/library/metadata/1", "/library/sections/1/all", "/library/collections/1/children"} {
				if got := cacheRequest(h, "GET", resource, "a"); got.Header().Get(CacheHeader) != "miss" || got.Body.String() != "1" {
					t.Errorf("writer %s stale: %s %s", resource, got.Header().Get(CacheHeader), got.Body.String())
				}
				if got := cacheRequest(h, "GET", resource, "b"); got.Header().Get(CacheHeader) != "hit" {
					t.Errorf("other user's %s unnecessarily retired", resource)
				}
			}
		})
	}
}

func TestWatchInvalidationWaitsForSuccessfulAcknowledgement(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusInternalServerError} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			h := watchCacheHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if isStateWrite(r.URL.Path) {
					close(entered)
					<-release
					w.WriteHeader(status)
					return
				}
				fmt.Fprint(w, "details")
			}))
			cacheRequest(h, "GET", "/library/metadata/1", "a")
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- cacheRequest(h, "POST", "/:/timeline", "a") }()
			<-entered
			during := cacheRequest(h, "GET", "/library/metadata/1", "a")
			close(release)
			<-done
			if during.Header().Get(CacheHeader) != "hit" {
				t.Fatal("pending write must not invalidate before acknowledgement")
			}
			want := "hit"
			if status == http.StatusNoContent {
				want = "miss"
			}
			if got := cacheRequest(h, "GET", "/library/metadata/1", "a"); got.Header().Get(CacheHeader) != want {
				t.Fatalf("status %d: want %s, got %s", status, want, got.Header().Get(CacheHeader))
			}
		})
	}
}

type recordingCache struct {
	*cache.Memory
	sets atomic.Int32
}

func (s *recordingCache) Set(ctx context.Context, key string, e cache.Entry, ttl time.Duration) error {
	s.sets.Add(1)
	return s.Memory.Set(ctx, key, e, ttl)
}

func TestWatchAcknowledgementRejectsOlderInFlightFill(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var state atomic.Int32
	h := watchCacheHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isStateWrite(r.URL.Path) {
			state.Store(1)
			w.WriteHeader(204)
			return
		}
		value := state.Load()
		if value == 0 {
			close(entered)
			<-release
		}
		fmt.Fprint(w, value)
	}))
	store := &recordingCache{Memory: cache.NewMemory()}
	h.cache = store
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- cacheRequest(h, "GET", "/library/metadata/1", "a") }()
	<-entered
	cacheRequest(h, "POST", "/:/timeline", "a")
	close(release)
	<-done
	if store.sets.Load() != 0 {
		t.Fatal("retired in-flight body must not be admitted")
	}
	got := cacheRequest(h, "GET", "/library/metadata/1", "a")
	if got.Body.String() != "1" || got.Header().Get(CacheHeader) != "miss" {
		t.Fatalf("next read must fetch current origin: %s %s", got.Header().Get(CacheHeader), got.Body.String())
	}
}

func TestPaginationAndClientHeadersVaryCachedResponses(t *testing.T) {
	h := watchCacheHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Plex-Container-Start", r.Header.Get("X-Plex-Container-Start"))
		fmt.Fprintf(w, "%s:%s:%s", r.Header.Get("X-Plex-Container-Start"), r.Header.Get("Accept-Language"), r.Header.Get("X-Plex-Product"))
	}))
	for _, values := range [][3]string{{"0", "en", "Web"}, {"50", "en", "Web"}, {"50", "fr", "Web"}, {"50", "fr", "TV"}} {
		for i := 0; i < 2; i++ {
			r := httptest.NewRequest("GET", "/library/sections/1/all", nil)
			r.Header.Set("X-Plex-Token", "a")
			r.Header.Set("X-Plex-Container-Start", values[0])
			r.Header.Set("Accept-Language", values[1])
			r.Header.Set("X-Plex-Product", values[2])
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			want := "miss"
			if i == 1 {
				want = "hit"
			}
			if w.Header().Get(CacheHeader) != want || w.Body.String() != fmt.Sprintf("%s:%s:%s", values[0], values[1], values[2]) || w.Header().Get("X-Plex-Container-Start") != values[0] {
				t.Errorf("variation %v read %d: headers=%v body=%s", values, i, w.Header(), w.Body.String())
			}
		}
	}
}
