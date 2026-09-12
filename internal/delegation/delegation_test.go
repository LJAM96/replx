package delegation

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/security/token" || r.URL.Query().Get("type") != "delegation" || r.URL.Query().Get("scope") != "all" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.Header.Get("X-Plex-Token") != "user-tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<MediaContainer size="0" token="transient-abc123"/>`))
	}))
	defer srv.Close()
	got, err := FetchWithClient(t.Context(), srv.Client(), srv.URL, "user-tok")
	if err != nil || got != "transient-abc123" {
		t.Fatalf("got %q %v", got, err)
	}
	if _, err := FetchWithClient(t.Context(), srv.Client(), srv.URL, "wrong"); err == nil {
		t.Fatal("bad token must fail")
	}
	if _, err := FetchWithClient(t.Context(), srv.Client(), srv.URL, ""); err == nil {
		t.Fatal("empty token must fail")
	}
}

func TestFetchMalformedFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<MediaContainer size="0"/>`))
	}))
	defer srv.Close()
	if _, err := FetchWithClient(t.Context(), srv.Client(), srv.URL, "t"); err == nil {
		t.Fatal("missing token attr must fail")
	}
}
