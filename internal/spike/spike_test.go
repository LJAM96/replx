package spike

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LJAM96/replx/internal/routing"
)

var errNoToken = errors.New("spike test: no token")

func stubStore(origin string) *Store {
	return &Store{
		Lookup: func(ctx context.Context) (string, string, bool) {
			if origin == "" {
				return "", "", false
			}
			return origin, "http://internal-origin:32400", true
		},
		FetchTransient: func(ctx context.Context, internalOrigin, userToken string) (string, error) {
			if userToken == "" {
				return "", errNoToken
			}
			return "transient-test-token", nil
		},
		PublicHost: "plex.example.com",
	}
}

func TestResolveRedirects(t *testing.T) {
	s := stubStore("https://origin.example:32400")
	req := httptest.NewRequest("GET", "/library/parts/11/file.mkv?foo=bar", nil)
	req.Header.Set("X-Plex-Token", "user-tok")
	req.Header.Set("Range", "bytes=0-99")
	loc, ok := s.Resolve(req, "req-1")
	if !ok {
		t.Fatal("expected resolve")
	}
	if !strings.HasPrefix(loc, "https://origin.example:32400/library/parts/11/file.mkv?") || !strings.Contains(loc, "foo=bar") {
		t.Fatalf("location: %s", loc)
	}
	if strings.Contains(loc, "user-tok") {
		t.Fatal("persistent caller token must never enter the redirect")
	}
	if !strings.Contains(loc, "transient-test-token") {
		t.Fatalf("expected transient token in location: %s", routing.RedactedLocation(loc))
	}
	events := s.Events()
	if len(events) != 1 || events[0].Decision != "redirected" || !events[0].RangePresent {
		t.Fatalf("events: %+v", events)
	}
	if strings.Contains(events[0].RedactedLocation, "user-tok") {
		t.Fatal("token in trace ring")
	}
}

func TestResolveFailsClosed(t *testing.T) {
	for name, store := range map[string]*Store{
		"no origin": stubStore(""),
	} {
		req := httptest.NewRequest("GET", "/library/parts/1/x", nil)
		req.Header.Set("X-Plex-Token", "t")
		if _, ok := store.Resolve(req, "r"); ok {
			t.Fatalf("%s: must not resolve", name)
		}
	}
	s := stubStore("https://origin.example:32400")
	req := httptest.NewRequest("GET", "/library/parts/1/x", nil) // no token anywhere
	if _, ok := s.Resolve(req, "r"); ok {
		t.Fatal("missing token must not resolve")
	}
	// Self-pointing origin is refused by the builder.
	self := stubStore("https://plex.example.com")
	req2 := httptest.NewRequest("GET", "/library/parts/1/x", nil)
	req2.Header.Set("X-Plex-Token", "t")
	if _, ok := self.Resolve(req2, "r"); ok {
		t.Fatal("loopback origin must not resolve")
	}
}

func TestDelegationFailureHasNoPersistentFallback(t *testing.T) {
	s := stubStore("https://origin.example:32400")
	s.FetchTransient = func(ctx context.Context, internalOrigin, userToken string) (string, error) {
		return "", errors.New("pms down")
	}
	req := httptest.NewRequest("GET", "/library/parts/1/x", nil)
	req.Header.Set("X-Plex-Token", "persistent-user-token")
	loc, ok := s.Resolve(req, "r")
	if ok || loc != "" {
		t.Fatalf("delegation failure must fail closed, got %q", loc)
	}
	for _, e := range s.Events() {
		if strings.Contains(e.RedactedLocation, "persistent-user-token") {
			t.Fatal("persistent token leaked into trace")
		}
	}
}

func TestExtractTokenPrecedence(t *testing.T) {
	req := httptest.NewRequest("GET", "/x?X-Plex-Token=query-tok", nil)
	req.Header.Set("X-Plex-Token", "header-tok")
	if got := ExtractToken(req); got != "header-tok" {
		t.Fatalf("header must win: %q", got)
	}
	req2 := httptest.NewRequest("GET", "/x?token=query-tok", nil)
	if got := ExtractToken(req2); got != "query-tok" {
		t.Fatalf("query fallback: %q", got)
	}
}

func TestRingCaps(t *testing.T) {
	s := stubStore("https://origin.example:32400")
	for i := 0; i < maxEvents+50; i++ {
		req := httptest.NewRequest("GET", "/library/parts/1/x", nil)
		req.Header.Set("X-Plex-Token", "t")
		s.Resolve(req, "r")
	}
	if len(s.Events()) != maxEvents {
		t.Fatalf("ring size: %d", len(s.Events()))
	}
}
