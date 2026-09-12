package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LJAM96/replx/internal/logging"
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

func (s stubSpike) Resolve(r *http.Request, requestID string) (string, bool) {
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
