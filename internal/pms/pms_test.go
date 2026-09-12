package pms

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized) // alive but wants auth: still healthy
	}))
	defer srv.Close()
	if got := Check(srv.URL); got != StatusHealthy {
		t.Fatalf("want healthy, got %s", got)
	}
	if got := Check("http://127.0.0.1:1"); got != StatusUnavailable {
		t.Fatalf("want unavailable, got %s", got)
	}
	if got := Check(""); got != StatusUnknown {
		t.Fatalf("want unknown, got %s", got)
	}
}
