package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LJAM96/replx/internal/artwork"
	"github.com/LJAM96/replx/internal/cache"
	"github.com/LJAM96/replx/internal/capture"
	"github.com/LJAM96/replx/internal/identity"
	"github.com/LJAM96/replx/internal/logging"
	"github.com/LJAM96/replx/internal/metrics"
	"github.com/LJAM96/replx/internal/playback"
	"github.com/LJAM96/replx/internal/spike"
	"github.com/LJAM96/replx/internal/trace"
	"github.com/LJAM96/replx/internal/warmer"
)

func TestPassthroughPreservesSemantics(t *testing.T) {
	var logs bytes.Buffer
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "bytes=0-99" {
			t.Errorf("Range not preserved: %q", r.Header.Get("Range"))
		}
		if r.Header.Get("X-Plex-Client-Identifier") != "abc123" {
			t.Errorf("client id not preserved")
		}
		if r.Header.Get("X-Plex-Token") != "user-secret" {
			t.Errorf("user token must be forwarded to origin")
		}
		if r.Header.Get("Connection") != "" {
			t.Errorf("hop-by-hop Connection leaked to origin")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello"))
	}))
	defer origin.Close()
	h, err := New(Options{OriginBase: origin.URL, IngressMode: "cloudflare_tunnel", Logger: logging.New(&logs)})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/library/sections?X-Plex-Token=user-secret", nil)
	req.Header.Set("Range", "bytes=0-99")
	req.Header.Set("X-Plex-Client-Identifier", "abc123")
	req.Header.Set("X-Plex-Token", "user-secret")
	req.Header.Set("Connection", "close")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	res := rec.Result()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK || string(body) != "hello" {
		t.Fatalf("passthrough failed: %d %q", res.StatusCode, body)
	}
	if res.Header.Get(RequestIDHeader) == "" {
		t.Fatal("missing request id response header")
	}
	if res.Header.Get("Connection") != "" {
		t.Fatal("hop-by-hop Connection leaked to client")
	}
	if strings.Contains(logs.String(), "user-secret") {
		t.Fatalf("token leaked to logs: %s", logs.String())
	}
}

func TestMediaFailClosedInTunnelMode(t *testing.T) {
	var logs bytes.Buffer
	contacted := 0
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contacted++
		w.WriteHeader(http.StatusOK)
	}))
	defer origin.Close()
	h, err := New(Options{OriginBase: origin.URL, IngressMode: "cloudflare_tunnel", Logger: logging.New(&logs)})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/library/parts/11/file.mkv", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), MediaRouteUnavailable) {
		t.Fatalf("missing reason: %s", rec.Body.String())
	}
	if contacted != 0 {
		t.Fatal("origin must not be contacted for fail-closed media")
	}
	if rec.Header().Get(RequestIDHeader) == "" {
		t.Fatal("missing request id on fail-closed response")
	}
}

func TestMediaAllowedInDirectMode(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("bytes"))
	}))
	defer origin.Close()
	h, err := New(Options{OriginBase: origin.URL, IngressMode: "direct"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/library/parts/11/file.mkv", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("direct mode should proxy media, got %d", rec.Code)
	}
}

func TestOriginDownIsBadGateway(t *testing.T) {
	h, err := New(Options{OriginBase: "http://127.0.0.1:1", IngressMode: "direct"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/library/sections", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "ORIGIN_UNAVAILABLE") {
		t.Fatalf("want 502 ORIGIN_UNAVAILABLE, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestUnknownTranscodeDeniedInTunnelMode(t *testing.T) {
	paths := []string{
		"/video/:/transcode/future/new-media-route",
		"/music/:/transcode/future/chunk",
		"/video/:/transcode/sessions/123/unknown",
	}
	for _, p := range paths {
		contacted := false
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			contacted = true
			_, _ = w.Write([]byte("must never stream"))
		}))
		h, err := New(Options{OriginBase: origin.URL, IngressMode: "cloudflare_tunnel"})
		if err != nil {
			origin.Close()
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodGet, p, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		origin.Close()
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), MediaRouteUnavailable) {
			t.Errorf("%s: want fail-closed 403, got %d %s", p, rec.Code, rec.Body.String())
		}
		if contacted {
			t.Errorf("%s: origin must not be contacted", p)
		}
	}
}

func TestUnknownTranscodePassesInDirectMode(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("direct-safe"))
	}))
	defer origin.Close()
	h, err := New(Options{OriginBase: origin.URL, IngressMode: "direct"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/video/:/transcode/future/new-media-route", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("direct mode has no tunnel invariant: got %d", rec.Code)
	}
}

type stubSpike struct {
	location string
	ok       bool
}

func (s stubSpike) Resolve(r *http.Request, ctx spike.ResolveContext) (string, bool) {
	return s.location, s.ok
}

func TestSpikeRedirectUpgradesMedia(t *testing.T) {
	var logs bytes.Buffer
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("origin must not be contacted on spike redirect")
	}))
	defer origin.Close()
	h, err := New(Options{
		OriginBase:  origin.URL,
		IngressMode: "cloudflare_tunnel",
		Logger:      logging.New(&logs),
		Spike:       stubSpike{location: "https://origin.example:32400/library/parts/11/x?X-Plex-Token=s3cr3t", ok: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/library/parts/11/x", nil)
	req.Header.Set("X-Plex-Token", "s3cr3t")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	res := rec.Result()
	if res.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("want 307, got %d", res.StatusCode)
	}
	if got := res.Header.Get("Location"); !strings.HasPrefix(got, "https://origin.example:32400/") {
		t.Fatalf("location: %s", got)
	}
	if res.Header.Get("Cache-Control") != "no-store" || res.Header.Get(RequestIDHeader) == "" {
		t.Fatal("missing no-store or request id")
	}
	if strings.Contains(logs.String(), "s3cr3t") {
		t.Fatalf("token leaked to logs: %s", logs.String())
	}
}

func TestSpikeMissStaysFailClosed(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("origin must not be contacted")
	}))
	defer origin.Close()
	h, err := New(Options{
		OriginBase:  origin.URL,
		IngressMode: "cloudflare_tunnel",
		Spike:       stubSpike{ok: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/library/parts/11/x", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), MediaRouteUnavailable) {
		t.Fatalf("want fail-closed 403, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestFingerprintLoggedNotToken(t *testing.T) {
	const secret = "test-secret-key-for-beta-slice-0123456789"
	var logs bytes.Buffer
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello"))
	}))
	defer origin.Close()
	h, err := New(Options{OriginBase: origin.URL, IngressMode: "direct", Logger: logging.New(&logs), Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/library/sections", nil)
	req.Header.Set("X-Plex-Token", "user-secret-xyz")
	req.Header.Set("X-Plex-Client-Identifier", "client-1")
	req.Header.Set("X-Plex-Product", "Plex Web")
	h.ServeHTTP(httptest.NewRecorder(), req)

	want := trace.Fingerprint(secret, "user-secret-xyz")
	if !strings.Contains(logs.String(), want) {
		t.Fatalf("want fingerprint in logs, got: %s", logs.String())
	}
	if strings.Contains(logs.String(), "user-secret-xyz") {
		t.Fatalf("raw token leaked to logs: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "client-1") || !strings.Contains(logs.String(), "Plex Web") {
		t.Fatalf("want client identity in logs, got: %s", logs.String())
	}
}

func TestNoFingerprintWithoutSecret(t *testing.T) {
	var logs bytes.Buffer
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer origin.Close()
	h, err := New(Options{OriginBase: origin.URL, IngressMode: "direct", Logger: logging.New(&logs)})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/library/sections", nil)
	req.Header.Set("X-Plex-Token", "user-secret-xyz")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if strings.Contains(logs.String(), "userFingerprint") {
		t.Fatalf("no fingerprint without secret, got: %s", logs.String())
	}
	if strings.Contains(logs.String(), "user-secret-xyz") {
		t.Fatalf("raw token leaked to logs: %s", logs.String())
	}
}

func TestPlaybackTraceHeader(t *testing.T) {
	const secret = "test-secret-key-for-beta-slice-0123456789"
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer origin.Close()
	h, err := New(Options{OriginBase: origin.URL, IngressMode: "direct", Secret: secret})
	if err != nil {
		t.Fatal(err)
	}
	media := httptest.NewRequest(http.MethodGet, "/video/:/transcode/universal/start.mpd?session=abc", nil)
	media.Header.Set("X-Plex-Client-Identifier", "client-1")
	mediaRec := httptest.NewRecorder()
	h.ServeHTTP(mediaRec, media)
	if mediaRec.Header().Get(trace.PlaybackTraceHeader) == "" {
		t.Fatal("playback route must carry a playback trace header")
	}
	control := httptest.NewRequest(http.MethodGet, "/library/sections", nil)
	controlRec := httptest.NewRecorder()
	h.ServeHTTP(controlRec, control)
	if controlRec.Header().Get(trace.PlaybackTraceHeader) != "" {
		t.Fatal("control route must not carry a playback trace header")
	}
}

func TestMetricsObserved(t *testing.T) {
	var reg metrics.Registry
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer origin.Close()
	h, err := New(Options{OriginBase: origin.URL, IngressMode: "cloudflare_tunnel", Metrics: &reg})
	if err != nil {
		t.Fatal(err)
	}
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/library/sections", nil))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/library/parts/11/x", nil))
	s := reg.Snapshot()
	if s.ReqTotal["control|200"] != 1 {
		t.Fatalf("control total: %+v", s.ReqTotal)
	}
	if s.ReqTotal["media|403"] != 1 || s.MediaFailure["unavailable"] != 1 {
		t.Fatalf("media failure: %+v %+v", s.ReqTotal, s.MediaFailure)
	}
	if s.OriginTotal != 1 || s.OriginErrors != 0 {
		t.Fatalf("origin: total=%d errors=%d", s.OriginTotal, s.OriginErrors)
	}
}

func TestCaptureProtocolLog(t *testing.T) {
	var logs bytes.Buffer
	store := capture.New()
	if _, err := store.Start("client-1", "", 10*time.Minute, "test"); err != nil {
		t.Fatal(err)
	}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer origin.Close()
	h, err := New(Options{OriginBase: origin.URL, IngressMode: "direct", Logger: logging.New(&logs), Capture: store})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/library/sections", nil)
	req.Header.Set("X-Plex-Client-Identifier", "client-1")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if !strings.Contains(logs.String(), "diagnostics.protocol") {
		t.Fatalf("want protocol capture line, got: %s", logs.String())
	}
	if len(store.Events()) != 1 {
		t.Fatalf("want 1 capture event, got %d", len(store.Events()))
	}
	plain := httptest.NewRequest(http.MethodGet, "/library/sections", nil)
	h.ServeHTTP(httptest.NewRecorder(), plain)
	if len(store.Events()) != 1 {
		t.Fatal("untargeted request must not record")
	}
}

func cacheTestHandler(originHits *int, body func(r *http.Request) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		*originHits++
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(body(r)))
	}
}

func TestCacheHitServesWithoutOrigin(t *testing.T) {
	const secret = "test-secret-key-for-beta-slice-0123456789"
	var logs bytes.Buffer
	hits := 0
	origin := httptest.NewServer(cacheTestHandler(&hits, func(r *http.Request) string {
		return "<MediaContainer size=\"1\"/>"
	}))
	defer origin.Close()
	var reg metrics.Registry
	h, err := New(Options{OriginBase: origin.URL, IngressMode: "direct",
		Logger: logging.New(&logs), Secret: secret, Metrics: &reg, Cache: cache.NewMemory()})
	if err != nil {
		t.Fatal(err)
	}
	get := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/hubs/home/recentlyAdded?contentDirectoryID=22", nil)
		req.Header.Set("X-Plex-Token", "user-a-token")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	first := get()
	if first.Header().Get(CacheHeader) != "miss" || hits != 1 {
		t.Fatalf("first must miss and hit origin: cache=%s hits=%d", first.Header().Get(CacheHeader), hits)
	}
	second := get()
	if second.Header().Get(CacheHeader) != "hit" || hits != 1 {
		t.Fatalf("second must hit without origin: cache=%s hits=%d", second.Header().Get(CacheHeader), hits)
	}
	if second.Body.String() != first.Body.String() {
		t.Fatal("hit body must equal miss body")
	}
	s := reg.Snapshot()
	if s.CacheHits != 1 || s.CacheMisses != 1 {
		t.Fatalf("cache counters: hits=%d misses=%d", s.CacheHits, s.CacheMisses)
	}
	if strings.Contains(logs.String(), "user-a-token") {
		t.Fatal("raw token leaked to logs")
	}
}

// TestCacheIsolation is the acceptance gate: one user's watched state,
// Continue Watching and restricted libraries must never appear in another
// user's cached response.
func TestCacheIsolation(t *testing.T) {
	const secret = "test-secret-key-for-beta-slice-0123456789"
	hits := 0
	origin := httptest.NewServer(cacheTestHandler(&hits, func(r *http.Request) string {
		return "<MediaContainer user=\"" + r.Header.Get("X-Plex-Token") + "\"/>"
	}))
	defer origin.Close()
	h, err := New(Options{OriginBase: origin.URL, IngressMode: "direct", Secret: secret, Cache: cache.NewMemory(), Identity: identity.New(nil, nil)})
	if err != nil {
		t.Fatal(err)
	}
	get := func(token string) string {
		req := httptest.NewRequest(http.MethodGet, "/hubs/home/continueWatching", nil)
		req.Header.Set("X-Plex-Token", token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Body.String()
	}
	a1 := get("token-user-a")
	b1 := get("token-user-b")
	a2 := get("token-user-a")
	if hits != 2 {
		t.Fatalf("want 2 origin hits (one per user), got %d", hits)
	}
	if !strings.Contains(a1, "token-user-a") || !strings.Contains(a2, "token-user-a") {
		t.Fatalf("user A responses wrong: %q %q", a1, a2)
	}
	if !strings.Contains(b1, "token-user-b") || strings.Contains(b1, "token-user-a") {
		t.Fatalf("user B response leaked user A: %q", b1)
	}
}

func TestCacheSkipsTimelineAndMedia(t *testing.T) {
	const secret = "test-secret-key-for-beta-slice-0123456789"
	hits := 0
	origin := httptest.NewServer(cacheTestHandler(&hits, func(r *http.Request) string { return "ok" }))
	defer origin.Close()
	h, err := New(Options{OriginBase: origin.URL, IngressMode: "cloudflare_tunnel",
		Secret: secret, Cache: cache.NewMemory()})
	if err != nil {
		t.Fatal(err)
	}
	// Watch-state timeline must always reach the origin.
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/:/timeline?ratingKey=1&state=playing&time=100", nil)
		req.Header.Set("X-Plex-Token", "user-a-token")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Header().Get(CacheHeader) != "bypass" {
			t.Fatalf("timeline must bypass, got %s", rec.Header().Get(CacheHeader))
		}
	}
	// Fail-closed media carries no cache header at all.
	mreq := httptest.NewRequest(http.MethodGet, "/library/parts/11/file.mkv", nil)
	mreq.Header.Set("X-Plex-Token", "user-a-token")
	mrec := httptest.NewRecorder()
	h.ServeHTTP(mrec, mreq)
	if mrec.Header().Get(CacheHeader) != "" {
		t.Fatalf("media must not carry cache header, got %s", mrec.Header().Get(CacheHeader))
	}
	if hits != 2 {
		t.Fatalf("timeline must hit origin twice, hits=%d", hits)
	}
}

func TestCacheLargeBodyStreamsUncached(t *testing.T) {
	const secret = "test-secret-key-for-beta-slice-0123456789"
	hits := 0
	big := bytes.Repeat([]byte("x"), cache.MaxEntryBytes+1024)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write(big)
	}))
	defer origin.Close()
	h, err := New(Options{OriginBase: origin.URL, IngressMode: "direct", Secret: secret, Cache: cache.NewMemory()})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodGet, "/library/collections/1/children", nil)
		req.Header.Set("X-Plex-Token", "user-a-token")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Header().Get(CacheHeader) != "miss" {
			t.Fatalf("oversize must miss, got %s", rec.Header().Get(CacheHeader))
		}
		if rec.Body.String() != string(big) {
			t.Fatalf("oversize body truncated: %d", rec.Body.Len())
		}
	}
	if hits != 2 {
		t.Fatalf("oversize must never populate cache, hits=%d", hits)
	}
}

func TestCacheAnonymousBypass(t *testing.T) {
	hits := 0
	origin := httptest.NewServer(cacheTestHandler(&hits, func(r *http.Request) string { return "ok" }))
	defer origin.Close()
	h, err := New(Options{OriginBase: origin.URL, IngressMode: "direct", Secret: "s", Cache: cache.NewMemory()})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/hubs/home/recentlyAdded", nil))
		if rec.Header().Get(CacheHeader) != "bypass" {
			t.Fatalf("anonymous must bypass, got %s", rec.Header().Get(CacheHeader))
		}
	}
	if hits != 2 {
		t.Fatalf("anonymous must always reach origin, hits=%d", hits)
	}
}

func TestWarmerTracksStoredEntry(t *testing.T) {
	const secret = "test-secret-key-for-beta-slice-0123456789"
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte("<ok/>"))
	}))
	defer origin.Close()
	mem := cache.NewMemory()
	wm := warmer.New(mem, origin.URL, secret, nil, nil, nil)
	h, err := New(Options{OriginBase: origin.URL, IngressMode: "direct", Secret: secret, Cache: mem, Warmer: wm})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/library/sections", nil)
	req.Header.Set("X-Plex-Token", "user-a-token")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if st := wm.Stats(); st.Tracked != 1 {
		t.Fatalf("stored entry must track, stats=%+v", st)
	}
}

type stubPlayback struct {
	handled    bool
	ended      []string
	statusCode int
	deny       *playback.Deny
}

func (s *stubPlayback) HandleDecision(w http.ResponseWriter, r *http.Request, id, fp, session, identity, client string) (bool, *playback.Deny) {
	if s.deny != nil {
		return false, s.deny
	}
	if s.handled {
		w.WriteHeader(s.statusCode)
		return true, nil
	}
	return false, nil
}

func (s *stubPlayback) EndSession(r *http.Request) {
	s.ended = append(s.ended, r.URL.Path)
}

func TestDecisionHookIntercepts(t *testing.T) {
	contacted := false
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contacted = true
	}))
	defer origin.Close()
	stub := &stubPlayback{handled: true, statusCode: http.StatusForbidden}
	h, err := New(Options{OriginBase: origin.URL, IngressMode: "direct", Playback: stub})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/video/:/transcode/universal/decision?path=%2Flibrary%2Fmetadata%2F1", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || contacted {
		t.Fatalf("engine must answer without origin: %d contacted=%v", rec.Code, contacted)
	}
}

func TestDecisionHookPassthrough(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer origin.Close()
	stub := &stubPlayback{}
	h, err := New(Options{OriginBase: origin.URL, IngressMode: "direct", Playback: stub})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/video/:/transcode/universal/decision?path=%2Flibrary%2Fmetadata%2F1", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("passthrough: %d", rec.Code)
	}
	stop := httptest.NewRequest(http.MethodGet, "/video/:/transcode/universal/stop?session=abc", nil)
	h.ServeHTTP(httptest.NewRecorder(), stop)
	if len(stub.ended) != 1 {
		t.Fatalf("stop must close session: %+v", stub.ended)
	}
}

func TestNoPlaybackPreservesControl(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer origin.Close()
	h, err := New(Options{OriginBase: origin.URL, IngressMode: "direct"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/video/:/transcode/universal/decision?path=%2Flibrary%2Fmetadata%2F1", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("nil engine must proxy decisions: %d", rec.Code)
	}
}

func TestArtworkAccountScoped(t *testing.T) {
	const secret = "test-secret-key-for-beta-slice-0123456789"
	hits := 0
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("thumb-bytes"))
	}))
	defer origin.Close()
	art, err := artwork.New(t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(Options{OriginBase: origin.URL, IngressMode: "direct", Secret: secret, Artwork: art})
	if err != nil {
		t.Fatal(err)
	}
	get := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/photo/:/transcode?width=480&height=720&url=%2Flibrary%2Fmetadata%2F1%2Fthumb", nil)
		req.Header.Set("X-Plex-Token", token)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	// Without an identity resolver every token stays token-scoped:
	// distinct tokens isolate, and the second fetch re-hits origin.
	first := get("token-a")
	second := get("token-b")
	if first.Header().Get(CacheHeader) != "miss" || second.Header().Get(CacheHeader) != "miss" {
		t.Fatalf("token scopes must isolate: %s then %s", first.Header().Get(CacheHeader), second.Header().Get(CacheHeader))
	}
	if hits != 2 {
		t.Fatalf("isolated scopes must refetch: hits=%d", hits)
	}
	// Same token repeats hit.
	third := get("token-a")
	if third.Header().Get(CacheHeader) != "hit" || hits != 2 {
		t.Fatalf("repeat must hit: %s hits=%d", third.Header().Get(CacheHeader), hits)
	}
	if third.Body.String() != "thumb-bytes" {
		t.Fatal("hit body wrong")
	}
	// Anonymous artwork bypasses: authorization to reference required.
	anon := httptest.NewRequest(http.MethodGet, "/photo/:/transcode?width=480&height=720", nil)
	anonRec := httptest.NewRecorder()
	h.ServeHTTP(anonRec, anon)
	if anonRec.Header().Get(CacheHeader) == "hit" {
		t.Fatal("anonymous artwork must never hit account entries")
	}
}

func TestDecisionDenyRenders403(t *testing.T) {
	contacted := false
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contacted = true
	}))
	defer origin.Close()
	stub := &stubPlayback{deny: &playback.Deny{Code: playback.DecisionUnsupported, Message: "unparseable"}}
	h, err := New(Options{OriginBase: origin.URL, IngressMode: "direct", Playback: stub})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/video/:/transcode/universal/decision?session=s", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || contacted {
		t.Fatalf("deny must 403 without origin: %d contacted=%v", rec.Code, contacted)
	}
	if !strings.Contains(rec.Body.String(), playback.DecisionUnsupported) {
		t.Fatalf("coded body: %s", rec.Body.String())
	}
}

func TestCacheSkipsTruncatedBody(t *testing.T) {
	const secret = "test-secret-key-for-beta-slice-0123456789"
	hits := 0
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			// Die mid-stream: declare 100 bytes, deliver 7, tear down.
			// The client sees unexpected EOF (not a clean close).
			hj := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 100\r\n\r\npartial"))
			_ = conn.Close()
			return
		}
		_, _ = w.Write([]byte("complete"))
	}))
	defer origin.Close()
	h, err := New(Options{OriginBase: origin.URL, IngressMode: "direct", Secret: secret, Cache: cache.NewMemory()})
	if err != nil {
		t.Fatal(err)
	}
	get := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/library/sections", nil)
		req.Header.Set("X-Plex-Token", "user-a-token")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	first := get()
	second := get()
	if hits != 2 {
		t.Fatalf("truncated body must never poison the cache: hits=%d", hits)
	}
	if first.Header().Get(CacheHeader) != "miss" || second.Header().Get(CacheHeader) != "miss" {
		t.Fatalf("both must miss: %s %s", first.Header().Get(CacheHeader), second.Header().Get(CacheHeader))
	}
	if second.Body.String() != "complete" {
		t.Fatalf("second body: %q", second.Body.String())
	}
}

func TestSessionsDeny(t *testing.T) {
	contacted := false
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contacted = true
	}))
	defer origin.Close()
	h, err := New(Options{OriginBase: origin.URL, IngressMode: "cloudflare_tunnel"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/status/sessions", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "SESSIONS_OWNER_ADMIN_ONLY") {
		t.Fatalf("sessions must 403: %d %s", rec.Code, rec.Body.String())
	}
	if contacted {
		t.Fatal("origin must not be contacted")
	}
}

func TestMediaGatewayFallback(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("origin must not be contacted")
	}))
	defer origin.Close()
	h, err := New(Options{OriginBase: origin.URL, IngressMode: "cloudflare_tunnel", Spike: stubSpike{ok: false}, MediaFallbackURL: "https://media.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/library/parts/11/x", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusTemporaryRedirect {
		t.Fatalf("fallback must 307, got %d", rec.Code)
	}
	if !strings.HasPrefix(rec.Result().Header.Get("Location"), "https://media.example.com/") {
		t.Fatalf("location: %s", rec.Result().Header.Get("Location"))
	}
}

func TestResponseMediaGuard(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write([]byte("bytes"))
	}))
	defer origin.Close()
	h, err := New(Options{OriginBase: origin.URL, IngressMode: "cloudflare_tunnel"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/some/future/non-media-route", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), MediaRouteUnavailable) {
		t.Fatalf("bulk response must fail closed: %d %s", rec.Code, rec.Body.String())
	}
}

func TestDirectIngressEnforcesPartBoundary(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/302/file.mp4") {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("allowed-bytes"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer origin.Close()
	engine := &playback.Engine{Store: playback.NewMemoryStore()}
	_, _ = engine.Store.Create(t.Context(), playback.Session{
		PlexSessionID: "sess-dir", RatingKey: "999", SelectedMediaIndex: 1,
		SelectedPartPlexID: "302", SelectedPartKey: "/library/parts/302/file.mp4",
	})
	h, err := New(Options{OriginBase: origin.URL, IngressMode: "direct", Playback: engine, PartPolicy: engine.EnforcePart,
		Client: origin.Client()})
	if err != nil {
		t.Fatal(err)
	}
	// Prohibited part without negotiation context for another session:
	// the boundary substitutes the selected allowed part.
	req := httptest.NewRequest(http.MethodGet, "/library/parts/301/file.mkv?session=sess-dir", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "allowed-bytes" {
		t.Fatalf("prohibited part must substitute allowed selection: %d %q", rec.Code, rec.Body.String())
	}
	// Unknown session fails closed instead of proxying.
	ghost := httptest.NewRequest(http.MethodGet, "/library/parts/301/file.mkv?session=ghost", nil)
	ghostRec := httptest.NewRecorder()
	h.ServeHTTP(ghostRec, ghost)
	if ghostRec.Code != http.StatusForbidden {
		t.Fatalf("sessionless prohibited part must deny: %d", ghostRec.Code)
	}
}

func TestSearchPassthroughNeverServesLocal(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"MediaContainer":{"size":0}}`))
	}))
	defer origin.Close()
	h, err := New(Options{OriginBase: origin.URL, IngressMode: "cloudflare_tunnel"})
	if err != nil {
		t.Fatal(err)
	}
	// Arbitrary unvalidated token: must reach PMS untouched, never the
	// owner index (which carries no per-user library grants).
	req := httptest.NewRequest(http.MethodGet, "/hubs/search?query=secret-title", nil)
	req.Header.Set("X-Plex-Token", "arbitrary-unvalidated-token")
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"size":0`) {
		t.Fatalf("search must pass through to PMS: %d %q", rec.Code, rec.Body.String())
	}
}

func TestStateWriteBumpsCWGeneration(t *testing.T) {
	const secret = "test-secret-for-cw-invalidation-0123456789"
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte("<MediaContainer/>"))
	}))
	defer origin.Close()
	h, err := New(Options{OriginBase: origin.URL, IngressMode: "direct", Secret: secret, Cache: cache.NewMemory()})
	if err != nil {
		t.Fatal(err)
	}
	cw := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/hubs/home/continueWatching", nil)
		req.Header.Set("X-Plex-Token", "user-cw-token")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if got := cw(); got.Header().Get(CacheHeader) != "miss" {
		t.Fatalf("first CW must miss, got %s", got.Header().Get(CacheHeader))
	}
	if got := cw(); got.Header().Get(CacheHeader) != "hit" {
		t.Fatalf("second CW must hit, got %s", got.Header().Get(CacheHeader))
	}
	// Watch-state mutation retires the CW namespace even though the
	// timeline route itself is never cacheable.
	timeline := httptest.NewRequest(http.MethodPost, "/:/timeline?ratingKey=1&state=played", nil)
	timeline.Header.Set("X-Plex-Token", "user-cw-token")
	timelineRec := httptest.NewRecorder()
	h.ServeHTTP(timelineRec, timeline)
	if timelineRec.Code != http.StatusOK {
		t.Fatalf("timeline must proxy, got %d", timelineRec.Code)
	}
	if got := cw(); got.Header().Get(CacheHeader) != "miss" {
		t.Fatalf("CW after timeline must miss, got %s", got.Header().Get(CacheHeader))
	}
}

func TestForwardedForTrustModel(t *testing.T) {
	mk := func(remote, xff string) *http.Request {
		req := httptest.NewRequest(http.MethodGet, "/library/sections", nil)
		req.RemoteAddr = remote
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		return req
	}
	// Infrastructure peer (container loopback): history preserved.
	if got := forwardedFor(mk("127.0.0.1:40000", "203.0.113.7")); got != "203.0.113.7, 127.0.0.1" {
		t.Fatalf("trusted peer must preserve history: %q", got)
	}
	// Arbitrary client: forged history replaced with the observed peer.
	if got := forwardedFor(mk("203.0.113.9:40000", "10.9.9.9")); got != "203.0.113.9" {
		t.Fatalf("untrusted peer must not forward history: %q", got)
	}
	// IPv6 loopback parses via SplitHostPort, not naive colon split.
	if got := forwardedFor(mk("[::1]:40000", "")); got != "::1" {
		t.Fatalf("v6 peer: %q", got)
	}
	// Bare IP without port still parses; garbage yields nothing.
	if got := forwardedFor(mk("10.0.0.5", "")); got != "10.0.0.5" {
		t.Fatalf("bare IP: %q", got)
	}
	if got := forwardedFor(mk("not-an-address", "1.2.3.4")); got != "" {
		t.Fatalf("garbage peer must yield nothing: %q", got)
	}
}

func TestConcurrentMissesSingleOriginFetch(t *testing.T) {
	const secret = "test-secret-for-singleflight-0123456789abcdef"
	var hits int64
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		time.Sleep(200 * time.Millisecond) // hold the race window open
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte("<MediaContainer/>"))
	}))
	defer origin.Close()
	h, err := New(Options{OriginBase: origin.URL, IngressMode: "direct", Secret: secret, Cache: cache.NewMemory()})
	if err != nil {
		t.Fatal(err)
	}
	const n = 10
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/library/sections", nil)
			req.Header.Set("X-Plex-Token", "user-flock-token")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			codes[i] = rec.Code
		}(i)
	}
	wg.Wait()
	for _, c := range codes {
		if c != http.StatusOK {
			t.Fatalf("all must succeed, got %v", codes)
		}
	}
	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Fatalf("concurrent misses must cause exactly one origin fetch, got %d", got)
	}
}

func TestEncodingNormalizedAcrossClients(t *testing.T) {
	const secret = "test-secret-for-encoding-0123456789abcdef"
	var hits int64
	var sawAE []string
	var mu sync.Mutex
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		mu.Lock()
		sawAE = append(sawAE, r.Header.Get("Accept-Encoding"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte("<MediaContainer/>"))
	}))
	defer origin.Close()
	h, err := New(Options{OriginBase: origin.URL, IngressMode: "direct", Secret: secret, Cache: cache.NewMemory()})
	if err != nil {
		t.Fatal(err)
	}
	get := func(ae string) string {
		req := httptest.NewRequest(http.MethodGet, "/library/sections", nil)
		req.Header.Set("X-Plex-Token", "user-enc-token")
		if ae != "" {
			req.Header.Set("Accept-Encoding", ae)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
		return rec.Body.String()
	}
	a, b := get("gzip"), get("")
	if a != b || a != "<MediaContainer/>" {
		t.Fatalf("encoding must not split or corrupt responses: %q %q", a, b)
	}
	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Fatalf("normalized encoding must share one origin fetch, hits=%d ae=%v", got, sawAE)
	}
}
