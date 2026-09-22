package logging

import (
	"bytes"
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

func TestRedactMalformedURLFailsClosed(t *testing.T) {
	got := RedactURLString("http://x/%zz?X-Plex-Token=s3cr3t")
	if strings.Contains(got, "s3cr3t") {
		t.Fatalf("malformed URL must not leak query: %q", got)
	}
	if !strings.HasSuffix(got, "?REDACTED") {
		t.Fatalf("malformed URL must truncate query: %q", got)
	}
	if got := RedactURLString("/library/sections"); got != "/library/sections" {
		t.Fatalf("queryless path must pass through: %q", got)
	}
}

func TestLogSinkFiltersSensitiveFields(t *testing.T) {
	var buf bytes.Buffer
	New(&buf).Log(Entry{Level: "info", Component: "t", Fields: map[string]any{
		"userToken": "s3cr3t",
		"location":  "https://origin/x?X-Plex-Token=s3cr3t",
		"note":      "plain",
	}})
	out := buf.String()
	if strings.Contains(out, "s3cr3t") {
		t.Fatalf("sink must filter secrets: %s", out)
	}
	if !strings.Contains(out, "plain") {
		t.Fatalf("innocent fields must survive: %s", out)
	}
}
