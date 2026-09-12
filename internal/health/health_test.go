package health

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLiveAlwaysOK(t *testing.T) {
	mux := AdminMux(Checks{})
	req := httptest.NewRequest(http.MethodGet, "/health/live", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("live = %d", rec.Code)
	}
}

func TestReadyRequiresDeps(t *testing.T) {
	mux := AdminMux(Checks{
		MigrationsComplete: func() bool { return false },
		PostgresOK:         func() bool { return true },
		ValkeyOK:           func() bool { return true },
	})
	req := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("ready = %d, want 503 when migrations incomplete", rec.Code)
	}
}
