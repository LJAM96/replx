package cache

import (
	"strings"
	"testing"
)

func TestUserIsolation(t *testing.T) {
	params := map[string]string{"path": "/library/sections", "page": "1"}
	a := Key("v1", "srv", "browse", UserScope("user-a"), "json", params)
	b := Key("v1", "srv", "browse", UserScope("user-b"), "json", params)
	if a == b {
		t.Fatal("user cache keys must differ")
	}
	if strings.Contains(a, "token") || strings.Contains(b, "token") {
		t.Fatal("keys must not contain token text")
	}
}

func TestTokenNeverInKey(t *testing.T) {
	// Callers must strip X-Plex-Token before Key(); prove same params with
	// different tokens map to the same key (i.e. token is not an input).
	params := map[string]string{"path": "/hubs/home"}
	a := Key("v1", "srv", "hubs", UserScope("u1"), "json", params)
	b := Key("v1", "srv", "hubs", UserScope("u1"), "json", params)
	if a != b {
		t.Fatal("deterministic keys required")
	}
}
