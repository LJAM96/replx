package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LJAM96/replx/internal/artwork"
	"github.com/LJAM96/replx/internal/capture"
	"github.com/LJAM96/replx/internal/health"
	"github.com/LJAM96/replx/internal/metrics"
	"github.com/LJAM96/replx/internal/spike"
	syncpkg "github.com/LJAM96/replx/internal/sync"
	"github.com/LJAM96/replx/internal/warmer"
)

func TestSetupGate(t *testing.T) {
	m := NewMux(health.Checks{}, nil, "tok123", true, nil, &spike.Observations{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/onboarding/status", nil)
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 without token, got %d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/v1/spike/events", nil)
	rec = httptest.NewRecorder()
	m.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("spike events must also gate, got %d", rec.Code)
	}
}

func TestStorageReportsArtworkCapacity(t *testing.T) {
	m := NewMux(health.Checks{}, nil, "tok123", false, nil, &spike.Observations{})
	m.SetArtworkStats(func(context.Context) (artwork.Stats, error) {
		return artwork.Stats{Shared: true, PayloadBytes: 123, ReservedBytes: 8, MaxBytes: 1000}, nil
	})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/storage", nil)
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"payloadBytes":123`) || !strings.Contains(rec.Body.String(), `"maxBytes":1000`) {
		t.Fatalf("storage response missing artwork capacity: %d %s", rec.Code, rec.Body.String())
	}
	m.SetArtworkStats(func(context.Context) (artwork.Stats, error) {
		return artwork.Stats{}, artwork.ErrInitializing
	})
	rec = httptest.NewRecorder()
	m.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `"state":"initializing"`) {
		t.Fatalf("initializing storage status missing: %s", rec.Body.String())
	}
}

func TestHealthOpen(t *testing.T) {
	m := NewMux(health.Checks{}, nil, "tok123", true, nil, &spike.Observations{})
	req := httptest.NewRequest(http.MethodGet, "/health/live", nil)
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("health must stay open, got %d", rec.Code)
	}
}

func TestSpikeEventsDisabled(t *testing.T) {
	m := NewMux(health.Checks{}, nil, "tok123", false, nil, &spike.Observations{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/spike/events", nil)
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Fatalf("disabled spike must report enabled=false, got %d %s", rec.Code, rec.Body.String())
	}
}

func loginCookie(t *testing.T, m *Mux) *http.Cookie {
	t.Helper()
	form := strings.NewReader("token=tok123")
	req := httptest.NewRequest(http.MethodPost, "/admin/login", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("login: %d %s", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie {
			return c
		}
	}
	t.Fatal("no session cookie")
	return nil
}

func TestBrowserLoginThenCookieAuth(t *testing.T) {
	m := NewMux(health.Checks{}, nil, "tok123", true, nil, &spike.Observations{})
	cookie := loginCookie(t, m)
	// Cookie-authenticated GET proves the browser flow without a bearer.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/spike/events", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cookie GET: %d %s", rec.Code, rec.Body.String())
	}
}

func TestCookieMutationNeedsCSRF(t *testing.T) {
	m := NewMux(health.Checks{}, nil, "tok123", true, nil, &spike.Observations{})
	cookie := loginCookie(t, m)
	// POST without CSRF must be rejected even with a valid cookie.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/spike/observations", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("want 403 CSRF_REQUIRED, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestMetricsOpen(t *testing.T) {
	m := NewMux(health.Checks{}, nil, "tok123", true, nil, &spike.Observations{})
	var reg metrics.Registry
	m.SetMetrics(&reg)
	// /metrics stays open like /health/*: the listener itself is private.
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), metrics.HTTPRequestsTotal) {
		t.Fatalf("metrics exposition: %d %s", rec.Code, rec.Body.String())
	}
}

func TestCaptureAPIGated(t *testing.T) {
	m := NewMux(health.Checks{}, nil, "tok123", true, nil, &spike.Observations{})
	m.SetCapture(capture.New())
	// Unauthenticated capture access must gate.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/diagnostics/capture", nil)
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("capture GET must gate, got %d", rec.Code)
	}
	// Bearer round trip: arm then list.
	post := httptest.NewRequest(http.MethodPost, "/api/v1/diagnostics/capture",
		strings.NewReader(`{"clientId":"c1","minutes":5,"reason":"test"}`))
	post.Header.Set("Content-Type", "application/json")
	post.Header.Set("Authorization", "Bearer tok123")
	postRec := httptest.NewRecorder()
	m.ServeHTTP(postRec, post)
	if postRec.Code != http.StatusCreated {
		t.Fatalf("capture POST: %d %s", postRec.Code, postRec.Body.String())
	}
	get := httptest.NewRequest(http.MethodGet, "/api/v1/diagnostics/capture", nil)
	get.Header.Set("Authorization", "Bearer tok123")
	getRec := httptest.NewRecorder()
	m.ServeHTTP(getRec, get)
	if getRec.Code != http.StatusOK || !strings.Contains(getRec.Body.String(), "c1") {
		t.Fatalf("capture GET: %d %s", getRec.Code, getRec.Body.String())
	}
}

func TestCacheStats(t *testing.T) {
	m := NewMux(health.Checks{}, nil, "tok123", true, nil, &spike.Observations{})
	var reg metrics.Registry
	reg.IncCacheHit()
	m.SetMetrics(&reg)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cache/stats", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"hits":1`) {
		t.Fatalf("cache stats: %d %s", rec.Code, rec.Body.String())
	}
	// Unauthenticated stats must gate like the rest of the API.
	anon := httptest.NewRequest(http.MethodGet, "/api/v1/cache/stats", nil)
	anonRec := httptest.NewRecorder()
	m.ServeHTTP(anonRec, anon)
	if anonRec.Code != http.StatusUnauthorized {
		t.Fatalf("cache stats must gate, got %d", anonRec.Code)
	}
}

func TestCacheStatsWarmerBlock(t *testing.T) {
	m := NewMux(health.Checks{}, nil, "tok123", true, nil, &spike.Observations{})
	m.SetMetrics(&metrics.Registry{})
	m.SetWarmer(func() warmer.Stats { return warmer.Stats{Tracked: 3, Refreshed: 9, OwnerWarming: true} })
	req := httptest.NewRequest(http.MethodGet, "/api/v1/cache/stats", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, `"tracked":3`) || !strings.Contains(body, `"warmed":0`) {
		t.Fatalf("cache stats warmer: %d %s", rec.Code, body)
	}
}

type testWarmingControl struct {
	cfg    warmer.Config
	paused bool
}

func (w *testWarmingControl) Stats() warmer.Stats               { return warmer.Stats{} }
func (w *testWarmingControl) Config() warmer.Config             { return w.cfg }
func (w *testWarmingControl) Configure(cfg warmer.Config) error { w.cfg = cfg; return nil }
func (w *testWarmingControl) SetPaused(v bool)                  { w.paused = v }

func TestWarmingControlsAndCoverage(t *testing.T) {
	m := NewMux(health.Checks{}, nil, "tok123", true, nil, &spike.Observations{})
	unauth := httptest.NewRecorder()
	m.ServeHTTP(unauth, httptest.NewRequest(http.MethodGet, "/api/v1/warming", nil))
	if unauth.Code != http.StatusUnauthorized {
		t.Fatalf("warming must require auth, got %d", unauth.Code)
	}
	control := &testWarmingControl{cfg: warmer.Config{Enabled: true, MaxTracked: 512, RequestsPerPass: 16, MaxPerScopePerPass: 4, BytesPerPass: 64 << 20}}
	m.SetWarmingControl(control)
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/warming", strings.NewReader(`{"paused":true,"requestsPerPass":8,"maxPerScopePerPass":3,"bytesPerPass":33554432}`))
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !control.paused || control.cfg.RequestsPerPass != 8 || control.cfg.MaxPerScopePerPass != 3 || control.cfg.BytesPerPass != 32<<20 {
		t.Fatalf("warming patch: %d paused=%v cfg=%+v %s", rec.Code, control.paused, control.cfg, rec.Body.String())
	}
	bad := httptest.NewRequest(http.MethodPatch, "/api/v1/warming", strings.NewReader(`{"requestsPerPass":65}`))
	bad.Header.Set("Authorization", "Bearer tok123")
	badRec := httptest.NewRecorder()
	m.ServeHTTP(badRec, bad)
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("invalid warm budget: %d %s", badRec.Code, badRec.Body.String())
	}
	coverage := httptest.NewRequest(http.MethodGet, "/api/v1/warming/coverage", nil)
	coverage.Header.Set("Authorization", "Bearer tok123")
	coverageRec := httptest.NewRecorder()
	m.ServeHTTP(coverageRec, coverage)
	if coverageRec.Code != http.StatusOK || !strings.Contains(coverageRec.Body.String(), "observed cache entries only") || strings.Contains(coverageRec.Body.String(), "coveragePercent") {
		t.Fatalf("coverage must state its limited basis: %d %s", coverageRec.Code, coverageRec.Body.String())
	}
}

func TestWarmingManualOperations(t *testing.T) {
	m := NewMux(health.Checks{}, nil, "tok123", true, nil, &spike.Observations{})
	key := ""
	m.SetCollectionRebuilder(func(_ context.Context, ratingKey string) error { key = ratingKey; return nil })
	removed := 0
	m.SetLowPriorityEvictor(func(context.Context) (int, int64, error) { removed++; return 2, 4096, nil })
	call := func(method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer tok123")
		rec := httptest.NewRecorder()
		m.ServeHTTP(rec, req)
		return rec
	}
	if rec := call(http.MethodPost, "/api/v1/warming/collections/12345/rebuild"); rec.Code != http.StatusOK || key != "12345" {
		t.Fatalf("collection rebuild: %d key=%s %s", rec.Code, key, rec.Body.String())
	}
	if rec := call(http.MethodPost, "/api/v1/warming/collections/%2Fadmin/rebuild"); rec.Code != http.StatusBadRequest {
		t.Fatalf("unsafe collection key: %d %s", rec.Code, rec.Body.String())
	}
	if rec := call(http.MethodPost, "/api/v1/storage/evict-low-priority"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"freedBytes":4096`) {
		t.Fatalf("evict: %d %s", rec.Code, rec.Body.String())
	}
	if rec := call(http.MethodPost, "/api/v1/storage/evict-low-priority"); rec.Code != http.StatusTooManyRequests || removed != 1 {
		t.Fatalf("evict rate limit: %d removed=%d %s", rec.Code, removed, rec.Body.String())
	}
}

func TestValidatePolicy(t *testing.T) {
	if _, err := validatePolicy("global", "", "g", json.RawMessage(`{"allowTranscode":"deny"}`)); err != nil {
		t.Fatalf("valid global: %v", err)
	}
	if _, err := validatePolicy("user", "some-uuid", "u", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("valid user: %v", err)
	}
	for _, tc := range []struct {
		name, scope, id, pname, cfg string
	}{
		{"bad-scope", "tenant", "", "x", `{}`},
		{"global-with-id", "global", "uuid", "x", `{}`},
		{"user-no-id", "user", "", "x", `{}`},
		{"no-name", "global", "", "", `{}`},
		{"bad-json", "global", "", "x", `{oops`},
		{"bad-tristate", "global", "", "x", `{"allowHDR":"maybe"}`},
		{"bad-routing", "global", "", "x", `{"routingMode":"teleport"}`},
	} {
		if _, err := validatePolicy(tc.scope, tc.id, tc.pname, json.RawMessage(tc.cfg)); err == nil {
			t.Errorf("%s must fail validation", tc.name)
		}
	}
}

type stubSync struct {
	status []syncpkg.CursorStatus
	err    error
	calls  int
}

func (s *stubSync) Status(ctx context.Context) ([]syncpkg.CursorStatus, error) {
	return s.status, s.err
}

func (s *stubSync) SyncOnce(ctx context.Context, full bool) error {
	s.calls++
	return s.err
}

func (s *stubSync) SyncCollectionsOnce(ctx context.Context) error {
	s.calls++
	return s.err
}

func TestSyncEndpoints(t *testing.T) {
	ctx := context.Background()
	_ = ctx
	m := NewMux(health.Checks{}, nil, "tok123", true, nil, &spike.Observations{})
	m.SetSync(nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/sync/status", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"disabled":true`) {
		t.Fatalf("disabled sync: %d %s", rec.Code, rec.Body.String())
	}

	stub := &stubSync{status: []syncpkg.CursorStatus{{SyncType: "libraries", Status: "complete"}}}
	m2 := NewMux(health.Checks{}, nil, "tok123", true, nil, &spike.Observations{})
	m2.SetSync(stub)
	post := httptest.NewRequest(http.MethodPost, "/api/v1/sync/full", nil)
	post.Header.Set("Authorization", "Bearer tok123")
	postRec := httptest.NewRecorder()
	m2.ServeHTTP(postRec, post)
	if postRec.Code != http.StatusOK || stub.calls != 1 {
		t.Fatalf("sync full: %d calls=%d %s", postRec.Code, stub.calls, postRec.Body.String())
	}
	collections := httptest.NewRequest(http.MethodPost, "/api/v1/server/collections/refresh", nil)
	collections.Header.Set("Authorization", "Bearer tok123")
	collectionsRec := httptest.NewRecorder()
	m2.ServeHTTP(collectionsRec, collections)
	if collectionsRec.Code != http.StatusOK || stub.calls != 2 {
		t.Fatalf("collections refresh: %d calls=%d %s", collectionsRec.Code, stub.calls, collectionsRec.Body.String())
	}
	// Immediate repeat trips the 5-minute rate limit.
	post2 := httptest.NewRequest(http.MethodPost, "/api/v1/sync/full", nil)
	post2.Header.Set("Authorization", "Bearer tok123")
	postRec2 := httptest.NewRecorder()
	m2.ServeHTTP(postRec2, post2)
	if postRec2.Code != http.StatusTooManyRequests {
		t.Fatalf("rate limit: %d", postRec2.Code)
	}
}

func TestSpikeReportGates(t *testing.T) {
	m := NewMux(health.Checks{}, nil, "tok123", true, nil, &spike.Observations{})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/spike/report?session=s", nil)
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("report must gate, got %d", rec.Code)
	}
	bad := httptest.NewRequest(http.MethodGet, "/api/v1/spike/report", nil)
	bad.Header.Set("Authorization", "Bearer tok123")
	badRec := httptest.NewRecorder()
	m.ServeHTTP(badRec, bad)
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("missing session must 400, got %d", badRec.Code)
	}
	empty := httptest.NewRequest(http.MethodGet, "/api/v1/spike/report?session=ghost", nil)
	empty.Header.Set("Authorization", "Bearer tok123")
	emptyRec := httptest.NewRecorder()
	m.ServeHTTP(emptyRec, empty)
	if emptyRec.Code != http.StatusOK || !strings.Contains(emptyRec.Body.String(), "spikeEvents") {
		t.Fatalf("empty report: %d %s", emptyRec.Code, emptyRec.Body.String())
	}
}
