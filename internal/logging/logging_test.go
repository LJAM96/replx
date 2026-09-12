package logging

import (
	"strings"
	"testing"
)

func TestRedaction(t *testing.T) {
	h := RedactHeaders(map[string]string{
		"X-Plex-Token":   "secret",
		"x-plex-token":   "secret",
		"Authorization":  "Bearer x",
		"X-Plex-Product": "Plex Web",
	})
	if h["X-Plex-Token"] != "REDACTED" || h["x-plex-token"] != "REDACTED" || h["Authorization"] != "REDACTED" {
		t.Fatalf("header redaction failed: %v", h)
	}
	if h["X-Plex-Product"] != "Plex Web" {
		t.Fatalf("non-secret header altered: %v", h)
	}
	u := RedactURLString("/library/parts/1/x?X-Plex-Token=abc&foo=bar")
	if strings.Contains(u, "abc") || !strings.Contains(u, "REDACTED") || !strings.Contains(u, "foo=bar") {
		t.Fatalf("url redaction failed: %s", u)
	}
}
