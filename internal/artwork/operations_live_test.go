package artwork

import (
	"testing"

	"github.com/LJAM96/replx/internal/testdb"
)

func TestEvictLowPriorityPreservesSharedPoster(t *testing.T) {
	ctx, db := testdb.Begin(t)
	s, err := NewShared(db, t.TempDir(), sharedOpts(t.Name(), 64<<20))
	if err != nil {
		t.Fatal(err)
	}
	sharedPut(t, s, sharedKey("background"), Low, []byte("low-only"))
	sharedPut(t, s, sharedKey("background-shared"), Low, []byte("shared-image"))
	sharedPut(t, s, sharedKey("poster-shared"), High, []byte("shared-image"))
	before, err := s.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.EvictLowPriority(ctx)
	if err != nil {
		t.Fatal(err)
	}
	after, err := s.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if result.Removed != 1 || result.FreedBytes <= 0 || after.TotalBytes >= before.TotalBytes {
		t.Fatalf("eviction=%+v before=%+v after=%+v", result, before, after)
	}
	if _, _, ok := s.GetContext(ctx, sharedKey("background")); ok {
		t.Fatal("low-only background survived")
	}
	for _, key := range []string{sharedKey("background-shared"), sharedKey("poster-shared")} {
		if _, body, ok := s.GetContext(ctx, key); !ok || string(body) != "shared-image" {
			t.Fatal("low alias removed a shared high-priority blob")
		}
	}
	result, err = s.EvictLowPriority(ctx)
	if err != nil || result.Removed != 0 {
		t.Fatalf("second pass removed protected references: %+v %v", result, err)
	}
}
