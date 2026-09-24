package artwork

import (
	"bytes"
	"compress/gzip"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCompressedArtworkReturnsImageBytes(t *testing.T) {
	s, err := New(t.TempDir(), 1)
	if err != nil {
		t.Fatal(err)
	}
	jpeg := []byte{0xff, 0xd8, 0xff, 0xe0, 1, 2, 3}
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	_, _ = zw.Write(jpeg)
	_ = zw.Close()
	if err := s.Set("new", "image/jpeg", "gzip", compressed.Bytes()); err != nil {
		t.Fatal(err)
	}
	_, body, ok := s.Get("new")
	if !ok || !bytes.Equal(body, jpeg) {
		t.Fatal("new compressed artwork was not normalized")
	}
	// A cached gzip entry from the previous release must also render.
	if err := s.Set("legacy", "image/jpeg", "", compressed.Bytes()); err != nil {
		t.Fatal(err)
	}
	_, body, ok = s.Get("legacy")
	if !ok || !bytes.Equal(body, jpeg) {
		t.Fatal("legacy compressed artwork was not decoded")
	}
}

func TestKeyScopes(t *testing.T) {
	q1, _ := url.ParseQuery("X-Plex-Token=aaa&width=480&height=720")
	q2, _ := url.ParseQuery("width=480&height=720&X-Plex-Token=zzz")
	// Same account, token rotation: one entry.
	if Key("acct:7", "/photo/:/transcode", q1) != Key("acct:7", "/photo/:/transcode", q2) {
		t.Fatal("same account must share one file across token rotation")
	}
	// Different accounts: isolated even for identical transforms.
	if Key("acct:7", "/photo/:/transcode", q1) == Key("acct:9", "/photo/:/transcode", q1) {
		t.Fatal("accounts must never share artwork entries")
	}
	// Different transforms: isolated.
	q3, _ := url.ParseQuery("width=960&height=720")
	if Key("acct:7", "/photo/:/transcode", q1) == Key("acct:7", "/photo/:/transcode", q3) {
		t.Fatal("different transforms must not share")
	}
	if !Match("/photo/:/transcode?url=x") || Match("/library/metadata/1/thumb/2") {
		t.Fatal("match scope wrong")
	}
}

func TestKeyIgnoresNestedPlexToken(t *testing.T) {
	a, _ := url.ParseQuery("width=480&height=720&url=%2Flibrary%2Fmetadata%2F1%2Fthumb%2F2%3FX-Plex-Token%3Dold")
	b, _ := url.ParseQuery("width=480&height=720&url=%2Flibrary%2Fmetadata%2F1%2Fthumb%2F2%3FX-Plex-Token%3Dnew")
	if Key("user:owner", "/photo/:/transcode", a) != Key("user:owner", "/photo/:/transcode", b) {
		t.Fatal("nested token rotation must retain the owner artwork entry")
	}
	if Key("user:other", "/photo/:/transcode", a) == Key("user:owner", "/photo/:/transcode", b) {
		t.Fatal("different users must remain isolated")
	}
}

func TestRoundTripAndExpiry(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set("k1", "image/jpeg", "", []byte("bytes")); err != nil {
		t.Fatal(err)
	}
	ct, body, ok := s.Get("k1")
	if !ok || ct != "image/jpeg" || string(body) != "bytes" {
		t.Fatalf("round trip: %q %q %v", ct, body, ok)
	}
	// Age past TTL via mtime... Get uses StoredAt in meta: rewrite stale.
	stale := time.Now().Add(-8 * 24 * time.Hour).UTC().Format(time.RFC3339)
	_ = os.WriteFile(filepath.Join(dir, "k1.meta"), []byte(`{"contentType":"image/jpeg","storedAt":"`+stale+`"}`), 0o644)
	if _, _, ok := s.Get("k1"); ok {
		t.Fatal("expired entry must miss")
	}
	if _, err := os.Stat(filepath.Join(dir, "k1.body")); !os.IsNotExist(err) {
		t.Fatal("expired entry must be reclaimed")
	}
}

func TestNewRejectsUnwritableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write despite directory permissions")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	if _, err := New(dir, 1); err == nil {
		t.Fatal("unwritable artwork cache must be reported at startup")
	}
}

func TestSweepBudget(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, 0) // zero budget anyone exceeds after insert
	if err != nil {
		t.Fatal(err)
	}
	s.maxBytes = 10
	_ = s.Set("a", "image/jpeg", "", []byte("12345678"))
	_ = s.Set("b", "image/jpeg", "", []byte("12345678"))
	removed, freed := s.Sweep()
	if removed == 0 || freed == 0 {
		t.Fatalf("over-budget must evict: %d %d", removed, freed)
	}
}
