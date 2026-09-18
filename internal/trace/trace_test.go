package trace

import (
	"net/http/httptest"
	"testing"
)

func TestExtractTokenHeaderFirst(t *testing.T) {
	r := httptest.NewRequest("GET", "/x?X-Plex-Token=query-token", nil)
	r.Header.Set("X-Plex-Token", "header-token")
	if got := ExtractToken(r); got != "header-token" {
		t.Fatalf("want header token, got %q", got)
	}
}

func TestExtractTokenQueryFallback(t *testing.T) {
	r := httptest.NewRequest("GET", "/x?token=abc123", nil)
	if got := ExtractToken(r); got != "abc123" {
		t.Fatalf("want query token, got %q", got)
	}
	if got := ExtractToken(httptest.NewRequest("GET", "/x", nil)); got != "" {
		t.Fatalf("want empty, got %q", got)
	}
}

func TestFingerprintEmptySafe(t *testing.T) {
	if Fingerprint("", "tok") != "" || Fingerprint("secret", "") != "" {
		t.Fatal("empty secret or token must yield no fingerprint")
	}
	a, b := Fingerprint("s3cret", "tok"), Fingerprint("s3cret", "tok")
	if a == "" || a != b {
		t.Fatal("fingerprint must be deterministic and non-empty")
	}
	if Fingerprint("other", "tok") == a {
		t.Fatal("fingerprint must be keyed by secret")
	}
}

func TestExtractSessionPriority(t *testing.T) {
	r := httptest.NewRequest("GET", "/x?session=query-session", nil)
	r.Header.Set("X-Plex-Playback-Session-Id", "playback-session")
	r.Header.Set("X-Plex-Session-Identifier", "session-id")
	if got := ExtractSession(r); got != "playback-session" {
		t.Fatalf("playback session must win, got %q", got)
	}
}

func TestExtractRatingKey(t *testing.T) {
	cases := map[string]string{
		"/library/metadata/4573984":                          "4573984",
		"/x?ratingKey=99":                                    "99",
		"/x?path=%2Flibrary%2Fmetadata%2F4573984":            "4573984",
		"/x?url=%2Flibrary%2Fmetadata%2F123%2Fthumb%2F456":   "123",
		"/x?key=%2Flibrary%2Fmetadata%2F789":                 "789",
		"/video/:/transcode/universal/start.mpd?session=abc": "",
	}
	for raw, want := range cases {
		r := httptest.NewRequest("GET", raw, nil)
		if got := ExtractRatingKey(r); got != want {
			t.Errorf("%s: want %q, got %q", raw, want, got)
		}
	}
}

func TestIsPlaybackRoute(t *testing.T) {
	for _, p := range []string{"/library/parts/11/x", "/:/timeline?x=1", "/video/:/transcode/universal/start.mpd", "/video/:/transcode/universal/decision?x=1"} {
		if !IsPlaybackRoute(p) {
			t.Errorf("%s must be playback", p)
		}
	}
	for _, p := range []string{"/library/sections", "/hubs/home/recentlyAdded", "/identity", "/photo/:/transcode?x=1"} {
		if IsPlaybackRoute(p) {
			t.Errorf("%s must not be playback", p)
		}
	}
}

func TestPlaybackTraceIDJoins(t *testing.T) {
	a := PlaybackTraceID("s", "userfp", "client1", "sess1", "123")
	b := PlaybackTraceID("s", "userfp", "client1", "sess1", "123")
	if a == "" || a != b {
		t.Fatal("identical inputs must join to a stable trace")
	}
	if PlaybackTraceID("s", "userfp", "client1", "sess2", "123") == a {
		t.Fatal("session rotation must split the trace")
	}
	if PlaybackTraceID("s", "", "", "sess1", "123") != "" {
		t.Fatal("no identity must yield no trace")
	}
	if PlaybackTraceID("s", "userfp", "client1", "", "") != "" {
		t.Fatal("neither session nor rating key must yield no trace")
	}
	if PlaybackTraceID("", "userfp", "client1", "sess1", "123") != "" {
		t.Fatal("no secret must yield no trace")
	}
}
