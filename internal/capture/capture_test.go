package capture

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestStartRequiresScope(t *testing.T) {
	s := New()
	if _, err := s.Start("", "", time.Minute, ""); err == nil {
		t.Fatal("want error for empty scope")
	}
}

func TestMatchClientAndRatingKey(t *testing.T) {
	s := New()
	if _, err := s.Start("client-1", "", 10*time.Minute, "debug tv"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start("", "4573984", 10*time.Minute, "ted lasso"); err != nil {
		t.Fatal(err)
	}
	hit := httptest.NewRequest("GET", "/library/parts/11/x", nil)
	hit.Header.Set("X-Plex-Client-Identifier", "client-1")
	if !s.Match(hit) {
		t.Error("client target must match")
	}
	hit2 := httptest.NewRequest("GET", "/library/metadata/4573984", nil)
	if !s.Match(hit2) {
		t.Error("rating key target must match")
	}
	miss := httptest.NewRequest("GET", "/library/sections", nil)
	miss.Header.Set("X-Plex-Client-Identifier", "other")
	if s.Match(miss) {
		t.Error("unrelated request must not match")
	}
}

func TestExpiryAndStop(t *testing.T) {
	now := time.Now()
	s := &Store{now: func() time.Time { return now }}
	if _, err := s.Start("c1", "", 10*time.Minute, ""); err != nil {
		t.Fatal(err)
	}
	now = now.Add(11 * time.Minute)
	r := httptest.NewRequest("GET", "/x", nil)
	r.Header.Set("X-Plex-Client-Identifier", "c1")
	if s.Match(r) {
		t.Error("expired target must not match")
	}
	if len(s.Targets()) != 0 {
		t.Error("expired target must prune")
	}

	s2 := New()
	if _, err := s2.Start("c9", "", time.Minute, ""); err != nil {
		t.Fatal(err)
	}
	if n := s2.Stop("c9", ""); n != 1 {
		t.Fatalf("want 1 removed, got %d", n)
	}
}

func TestRingCap(t *testing.T) {
	s := New()
	for i := 0; i < maxEvents+10; i++ {
		s.Record(Event{Method: "GET", Path: "/x"})
	}
	if len(s.Events()) != maxEvents {
		t.Fatalf("want capped ring %d, got %d", maxEvents, len(s.Events()))
	}
}
