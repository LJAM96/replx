package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LJAM96/replx/internal/capture"
	"github.com/LJAM96/replx/internal/health"
	"github.com/LJAM96/replx/internal/metrics"
	"github.com/LJAM96/replx/internal/spike"
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
