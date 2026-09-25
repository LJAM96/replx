package cache

import (
	"net/url"
	"testing"
)

func TestSectionHubKeySharesBrowserContextButNotSelection(t *testing.T) {
	first, _ := url.ParseQuery("count=12&includeMeta=1&X-Plex-Client-Identifier=first&X-Plex-Model=bundled&X-Plex-Device-Screen-Resolution=800x600")
	second, _ := url.ParseQuery("count=12&includeMeta=1&X-Plex-Client-Identifier=second&X-Plex-Model=standalone&X-Plex-Device-Screen-Resolution=1800x1100")
	a := ResponseKey("user:one", "GET", "/hubs/sections/22", first, "application/json")
	b := ResponseKey("user:one", "GET", "/hubs/sections/22", second, "application/json")
	if a != b {
		t.Fatal("browser context split the same section hub")
	}
	second.Set("count", "24")
	if a == ResponseKey("user:one", "GET", "/hubs/sections/22", second, "application/json") {
		t.Fatal("hub item count was ignored")
	}
	if a == ResponseKey("user:two", "GET", "/hubs/sections/22", first, "application/json") {
		t.Fatal("section hub crossed user scopes")
	}
	if a == ResponseKey("user:one", "GET", "/hubs/sections/23", first, "application/json") {
		t.Fatal("section hub crossed libraries")
	}
}
