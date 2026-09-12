package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LJAM96/replx-edge/internal/health"
	"github.com/LJAM96/replx-edge/internal/spike"
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
