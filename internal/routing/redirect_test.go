package routing

import (
	"strings"
	"testing"
)

func TestBuildDirectOriginURL(t *testing.T) {
	loc, err := BuildDirectOriginURL("https://origin.example:32400", "/library/parts/7/file.mkv", "user-token", "plex.example.com")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !strings.HasPrefix(loc, "https://origin.example:32400/library/parts/7/file.mkv?") {
		t.Fatalf("unexpected location: %s", loc)
	}
	if !strings.Contains(loc, "X-Plex-Token=") {
		t.Fatalf("expected token query in location: %s", RedactedLocation(loc))
	}
	if got := RedactedLocation(loc); strings.Contains(got, "user-token") || !strings.Contains(got, "REDACTED") {
		t.Fatalf("redaction failed: %s", got)
	}
}

func TestBuildRejectsLoopback(t *testing.T) {
	if _, err := BuildDirectOriginURL("https://plex.example.com", "/library/parts/1/x", "tok", "plex.example.com"); err == nil {
		t.Fatal("expected loopback rejection")
	}
	if _, err := BuildDirectOriginURL("http://origin.example/file", "/library/parts/1/x", "tok", ""); err == nil {
		t.Fatal("expected https requirement")
	}
}

func TestBuildRejectsForeignAuthority(t *testing.T) {
	paths := []string{
		"//other.example/video/:/transcode/universal/start.m3u8?session=s",
		"https://other.example/library/parts/1/file.mkv",
		"http://other.example/library/parts/1/file.mkv",
	}
	for _, p := range paths {
		loc, err := BuildDirectOriginURL("https://trusted.example", p, "transient-secret", "edge.example")
		if err == nil {
			t.Errorf("%q must be rejected, built %s", p, RedactedLocation(loc))
		}
	}
}
