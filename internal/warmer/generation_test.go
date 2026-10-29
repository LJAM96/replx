package warmer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LJAM96/replx/internal/cache"
)

func TestRefreshRejectsGenerationChangedDuringFetch(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		_, _ = w.Write([]byte("old watch state"))
	}))
	defer origin.Close()
	store := cache.NewMemory()
	w := New(store, origin.URL, testSecret, ownerProvider("owner"), nil, nil)
	var generation atomic.Int32
	w.KeyFunc = func(Snapshot) string {
		if generation.Load() == 0 {
			return "before"
		}
		return "after"
	}
	done := make(chan error, 1)
	go func() {
		done <- w.refresh(context.Background(), "before", Snapshot{Method: "GET", Path: "/library/metadata/1", TTL: time.Minute}, "owner")
	}()
	<-entered
	generation.Store(1)
	close(release)
	if err := <-done; err == nil {
		t.Fatal("superseded refresh must be rejected")
	}
	for _, key := range []string{"before", "after"} {
		if _, ok, _ := store.Get(context.Background(), key); ok {
			t.Fatalf("old body stored at %s", key)
		}
	}
}

func TestRefreshSharesForegroundAdmission(t *testing.T) {
	for _, vary := range []string{"*", "Accept-Language", "X-Unknown-Feature"} {
		t.Run(vary, func(t *testing.T) {
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Add("Vary", "Accept")
				w.Header().Add("Vary", vary)
				_, _ = w.Write([]byte("unsafe response"))
			}))
			defer origin.Close()
			store := cache.NewMemory()
			w := New(store, origin.URL, testSecret, ownerProvider("owner"), nil, nil)
			if err := w.refresh(context.Background(), "key", Snapshot{Method: "GET", Path: "/library/metadata/1", TTL: time.Minute}, "owner"); err == nil {
				t.Fatal("unsupported Vary must be rejected")
			}
			if _, ok, _ := store.Get(context.Background(), "key"); ok {
				t.Fatal("unsafe refresh was stored")
			}
		})
	}
}
