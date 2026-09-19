package warmer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LJAM96/replx/internal/cache"
	"github.com/LJAM96/replx/internal/trace"
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

func TestRefreshesOwnerEntry(t *testing.T) {
	fx := &fixture{token: "owner-tok", body: "<v>1</v>"}
	origin := httptest.NewServer(http.HandlerFunc(fx.handler))
	defer origin.Close()
	store := cache.NewMemory()
	w := New(store, origin.URL, testSecret, ownerProvider("owner-tok"), nil, nil)
	w.now = time.Now

	ctx := context.Background()
	key := "k-owner"
	ownerFP := trace.Fingerprint(testSecret, "owner-tok")
	w.Track(key, Snapshot{Method: "GET", Path: "/library/sections", Fingerprint: ownerFP, TTL: time.Minute})
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
	w := New(store, origin.URL, testSecret, ownerProvider("owner-tok"), nil, nil)
	w.Track("k-user", Snapshot{Method: "GET", Path: "/hubs/x", Fingerprint: "fp-someone-else", TTL: time.Minute})
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

func TestNoCredentialsSkips(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no fetch without credentials")
	}))
	defer origin.Close()
	w := New(cache.NewMemory(), origin.URL, testSecret, ownerProvider(""), nil, nil)
	w.now = func() time.Time { return time.Now().Add(time.Hour) }
	w.Track("k", Snapshot{Method: "GET", Path: "/x", Fingerprint: "fp", TTL: time.Minute})
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
	w := New(store, origin.URL, testSecret, ownerProvider("owner-tok"), nil, nil)
	ownerFP := trace.Fingerprint(testSecret, "owner-tok")
	w.Track("k-err", Snapshot{Method: "GET", Path: "/x", Fingerprint: ownerFP, TTL: time.Minute})
	w.now = func() time.Time { return time.Now().Add(time.Hour) }
	w.RefreshOnce(context.Background())
	if _, ok, _ := store.Get(context.Background(), "k-err"); ok {
		t.Fatal("error responses must never populate hot keys")
	}
	if st := w.Stats(); st.Errors != 1 {
		t.Fatalf("errors: %+v", st)
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
