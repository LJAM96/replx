package admin

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/LJAM96/replx-edge/internal/health"
)

func TestSetupGate(t *testing.T) {
	m := NewMux(health.Checks{}, nil, "tok123", true)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/onboarding/status", nil)
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 without token, got %d", rec.Code)
	}
}

func TestHealthOpen(t *testing.T) {
	m := NewMux(health.Checks{}, nil, "tok123", true)
	req := httptest.NewRequest(http.MethodGet, "/health/live", nil)
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("health must stay open, got %d", rec.Code)
	}
}
