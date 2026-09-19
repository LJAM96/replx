package artwork

import (
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

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

func TestRoundTripAndExpiry(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Set("k1", "image/jpeg", []byte("bytes")); err != nil {
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

func TestSweepBudget(t *testing.T) {
	dir := t.TempDir()
	s, err := New(dir, 0) // zero budget anyone exceeds after insert
	if err != nil {
		t.Fatal(err)
	}
	s.maxBytes = 10
	_ = s.Set("a", "image/jpeg", []byte("12345678"))
	_ = s.Set("b", "image/jpeg", []byte("12345678"))
	removed, freed := s.Sweep()
	if removed == 0 || freed == 0 {
		t.Fatalf("over-budget must evict: %d %d", removed, freed)
	}
}
