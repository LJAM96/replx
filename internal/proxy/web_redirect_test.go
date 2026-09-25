package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWebRedirectStaysOnPublicConnection(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/web/index.html?x=1")
		w.WriteHeader(http.StatusFound)
	}))
	defer origin.Close()
	h, err := New(Options{OriginBase: origin.URL, PublicBase: "https://replex.example", IngressMode: "cloudflare_tunnel"})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/web", nil))
	if recorder.Code != http.StatusFound || recorder.Header().Get("Location") != "https://replex.example/web/index.html?x=1" {
		t.Fatalf("browser redirect escaped public connection: %d %q", recorder.Code, recorder.Header().Get("Location"))
	}
	// The origin host in the Location must match the configured PMS.
	location := origin.URL + "/web/index.html?x=1"
	if got := h.rewriteWebRedirect(location); got != "https://replex.example/web/index.html?x=1" {
		t.Fatalf("redirect rewrite: %q", got)
	}
	for _, raw := range []string{"https://other.example/web/index.html", origin.URL + "/library/parts/1/file", "javascript:alert(1)"} {
		if got := h.rewriteWebRedirect(raw); got != "" {
			t.Fatalf("unsafe redirect rewrite for %q: %q", raw, got)
		}
	}
}

func TestCachedCORSUsesOnlyAnOrigin(t *testing.T) {
	for _, raw := range []string{"https://app.plex.tv/path", "javascript:alert(1)", "https://user@example.com"} {
		req := httptest.NewRequest(http.MethodGet, "/hubs/promoted", nil)
		req.Header.Set("Origin", raw)
		header := make(http.Header)
		setCachedCORS(header, req)
		if header.Get("Access-Control-Allow-Origin") != "" {
			t.Fatalf("accepted malformed origin %q", raw)
		}
	}
}

func TestBadGatewayRetainsPlexWebCORS(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	base := origin.URL
	origin.Close()
	h, err := New(Options{OriginBase: base, IngressMode: "direct"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/library/sections", nil)
	req.Header.Set("Origin", "https://app.plex.tv")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway || rec.Header().Get("Access-Control-Allow-Origin") != "https://app.plex.tv" {
		t.Fatalf("bad gateway lost browser origin: status=%d cors=%q", rec.Code, rec.Header().Get("Access-Control-Allow-Origin"))
	}
}
