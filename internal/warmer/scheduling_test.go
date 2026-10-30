package warmer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LJAM96/replx/internal/cache"
)

func TestAuthorizedJobsFairnessPriorityAndSecretSeparation(t *testing.T) {
	var requests []string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Header.Get("X-Plex-Token")+r.URL.Path)
		_, _ = w.Write([]byte("ok"))
	}))
	defer origin.Close()
	w := New(cache.NewMemory(), origin.URL, testSecret, nil, nil, nil)
	now := time.Now()
	w.now = func() time.Time { return now }
	cfg := DefaultConfig()
	cfg.RequestsPerPass = 3
	cfg.MaxPerScopePerPass = 1
	if err := w.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"one", "two", "three"} {
		w.TrackAuthorized(scope+"-normal", Snapshot{Method: "GET", Path: "/library/metadata/1", Scope: scope, RawQuery: "X-Plex-Token=secret-" + scope, TTL: time.Minute}, "secret-"+scope)
		w.TrackAuthorized(scope+"-collection", Snapshot{Method: "GET", Path: "/library/collections/8/children", Scope: scope, TTL: time.Minute}, "secret-"+scope)
	}
	now = now.Add(40 * time.Second)
	w.RefreshOnce(context.Background())
	if len(requests) != 3 {
		t.Fatalf("requests=%v", requests)
	}
	seen := map[string]bool{}
	for _, request := range requests {
		if !strings.Contains(request, "/library/collections/") {
			t.Fatalf("lower-priority job ran first: %v", requests)
		}
		seen[strings.Split(request, "/")[0]] = true
	}
	if len(seen) != 3 {
		t.Fatalf("scope fairness lost: %v", requests)
	}
	for _, job := range w.tracked {
		raw, _ := json.Marshal(job.snap)
		if strings.Contains(string(raw), "secret-") {
			t.Fatalf("credential entered job: %s", raw)
		}
	}
	raw, _ := json.Marshal(w.Stats())
	if strings.Contains(string(raw), "secret-") || strings.Contains(string(raw), "one") {
		t.Fatalf("credential or user scope leaked to stats: %s", raw)
	}
}

func TestUserContextExpiryRevocationAndRestart(t *testing.T) {
	hits, status := 0, http.StatusOK
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(status)
		_, _ = w.Write([]byte("ok"))
	}))
	defer origin.Close()
	w := New(cache.NewMemory(), origin.URL, testSecret, nil, nil, nil)
	now := time.Now()
	w.now = func() time.Time { return now }
	s := Snapshot{Method: "GET", Path: "/library/metadata/1", Scope: "user:one", TTL: time.Minute}
	w.TrackAuthorized("key", s, "user-secret")
	now = now.Add(credentialTTL)
	w.RefreshOnce(context.Background())
	if hits != 0 || w.Stats().ActiveContexts != 0 || w.Stats().ColdScopes != 1 {
		t.Fatalf("expired context used: hits=%d stats=%+v", hits, w.Stats())
	}
	w.TrackAuthorized("key", s, "fresh-secret")
	now = now.Add(40 * time.Second)
	status = http.StatusForbidden
	w.RefreshOnce(context.Background())
	if hits != 1 || w.Stats().ActiveContexts != 0 || w.Stats().Tracked != 0 {
		t.Fatalf("rejected credential retained: hits=%d stats=%+v", hits, w.Stats())
	}
	w.TrackAuthorized("key", s, "another-secret")
	w.ForgetScope(s.Scope)
	now = now.Add(40 * time.Second)
	w.RefreshOnce(context.Background())
	if hits != 1 || w.Stats().Tracked != 0 {
		t.Fatal("revoked scope was refreshed")
	}
	restarted := New(w.store, origin.URL, testSecret, nil, nil, nil)
	if restarted.Stats().ActiveContexts != 0 || restarted.Stats().Tracked != 0 {
		t.Fatal("restart inherited in-memory credentials/jobs")
	}
}

func TestWarmRequestByteBudgetsAndBackoff(t *testing.T) {
	hits := 0
	fail := false
	body := strings.Repeat("x", cache.MaxEntryBytes)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if fail {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer origin.Close()
	w := New(cache.NewMemory(), origin.URL, testSecret, nil, nil, nil)
	now := time.Now()
	w.now = func() time.Time { return now }
	cfg := DefaultConfig()
	cfg.RequestsPerPass = 16
	cfg.MaxPerScopePerPass = 16
	cfg.BytesPerPass = 8 << 20
	if err := w.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		w.TrackAuthorized(fmt.Sprint(i), Snapshot{Method: "GET", Path: fmt.Sprintf("/library/metadata/%d", i), Scope: "one", TTL: time.Minute}, "secret")
	}
	now = now.Add(40 * time.Second)
	w.RefreshOnce(context.Background())
	if hits != 3 || w.Stats().WarmedBytes > cfg.BytesPerPass {
		t.Fatalf("byte budget exceeded: hits=%d stats=%+v", hits, w.Stats())
	}
	// One failing scope gets only its configured turn and is backed off.
	w = New(cache.NewMemory(), origin.URL, testSecret, nil, nil, nil)
	w.now = func() time.Time { return now }
	cfg = DefaultConfig()
	cfg.RequestsPerPass = 1
	cfg.MaxPerScopePerPass = 1
	_ = w.Configure(cfg)
	w.TrackAuthorized("failed", Snapshot{Method: "GET", Path: "/library/metadata/90", Scope: "one", TTL: time.Minute}, "secret")
	now = now.Add(40 * time.Second)
	fail = true
	w.RefreshOnce(context.Background())
	before := hits
	w.RefreshOnce(context.Background())
	if hits != before || w.Stats().Backoffs != 1 {
		t.Fatalf("failure immediately retried: %+v", w.Stats())
	}
}

func TestForegroundCancelsWarmPublication(t *testing.T) {
	entered := make(chan struct{})
	released := make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done(); close(released) }))
	defer origin.Close()
	store := cache.NewMemory()
	w := New(store, origin.URL, testSecret, nil, nil, nil)
	now := time.Now()
	w.now = func() time.Time { return now }
	w.TrackAuthorized("key", Snapshot{Method: "GET", Path: "/library/metadata/1", Scope: "one", TTL: time.Minute}, "secret")
	now = now.Add(40 * time.Second)
	done := make(chan struct{})
	go func() { w.RefreshOnce(context.Background()); close(done) }()
	<-entered
	foregroundDone := w.Foreground()
	defer foregroundDone()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("warm fetch did not yield to browsing")
	}
	<-released
	if _, ok, _ := store.Get(context.Background(), "key"); ok {
		t.Fatal("cancelled background body was published")
	}
}

func TestCredentialExpiresDuringFetch(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		_, _ = w.Write([]byte("private"))
	}))
	defer origin.Close()
	store := cache.NewMemory()
	w := New(store, origin.URL, testSecret, nil, nil, nil)
	var nanos atomic.Int64
	nanos.Store(time.Now().UnixNano())
	w.now = func() time.Time { return time.Unix(0, nanos.Load()) }
	w.TrackAuthorized("key", Snapshot{Method: "GET", Path: "/library/metadata/1", Scope: "one", TTL: time.Minute}, "secret")
	nanos.Add(int64(40 * time.Second))
	done := make(chan struct{})
	go func() { w.RefreshOnce(context.Background()); close(done) }()
	<-entered
	nanos.Add(int64(credentialTTL))
	close(release)
	<-done
	if _, ok, _ := store.Get(context.Background(), "key"); ok {
		t.Fatal("response published after credential context expired")
	}
	if w.Stats().ActiveContexts != 0 {
		t.Fatal("expired token remained in memory context")
	}
}

func TestQueueDedupeCapacityPauseAndObsoleteGeneration(t *testing.T) {
	hits := 0
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++; _, _ = w.Write([]byte("ok")) }))
	defer origin.Close()
	w := New(cache.NewMemory(), origin.URL, testSecret, nil, nil, nil)
	now := time.Now()
	w.now = func() time.Time { return now }
	cfg := DefaultConfig()
	cfg.MaxTracked = 2
	if err := w.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	s := Snapshot{Method: "GET", Path: "/library/metadata/1", Scope: "one", TTL: time.Minute}
	w.TrackAuthorized("old", s, "secret")
	w.TrackAuthorized("new", s, "secret")
	if w.Stats().Tracked != 1 {
		t.Fatal("same observed request was not deduplicated")
	}
	w.TrackAuthorized("collection", Snapshot{Method: "GET", Path: "/library/collections/8/children", Scope: "one", TTL: time.Minute}, "secret")
	w.TrackAuthorized("low", Snapshot{Method: "GET", Path: "/hubs/search", Scope: "two", TTL: time.Minute}, "secret-two")
	if w.Stats().Tracked != 2 || w.Stats().ByPriority["protected"] != 1 {
		t.Fatalf("capacity evicted protected work: %+v", w.Stats())
	}
	now = now.Add(40 * time.Second)
	w.SetPaused(true)
	w.RefreshOnce(context.Background())
	if hits != 0 {
		t.Fatal("paused warmer made requests")
	}
	w.SetPaused(false)
	w.CanWarm = func(context.Context, Snapshot) bool { return false }
	w.RefreshOnce(context.Background())
	if hits != 0 {
		t.Fatal("capacity rejection made requests")
	}
	w.CanWarm = nil
	w.KeyFunc = func(Snapshot) string { return "superseded" }
	w.RefreshOnce(context.Background())
	if hits != 0 || w.Stats().Tracked != 0 {
		t.Fatal("obsolete generation job was not cancelled")
	}
	w.served["expired-scope"] = 10
	_ = w.Stats()
	if len(w.served) != 0 {
		t.Fatal("served-scope history retained removed scopes")
	}
	bad := cfg
	bad.MaxTracked = 513
	if w.Configure(bad) == nil {
		t.Fatal("oversized queue accepted")
	}
}
