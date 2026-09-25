package cache

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestCodecRoundTrip(t *testing.T) {
	in := Entry{Status: 200, ContentType: "application/xml",
		Headers: map[string]string{"Etag": `"abc"`, "Last-Modified": "Tue, 01 Jan 2030 00:00:00 GMT"},
		Body:    []byte("<MediaContainer/>")}
	raw, err := in.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	out, err := Unmarshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != 200 || out.ContentType != "application/xml" || string(out.Body) != "<MediaContainer/>" {
		t.Fatalf("codec: %+v", out)
	}
	if out.Headers["Etag"] != `"abc"` || out.Headers["Last-Modified"] == "" {
		t.Fatalf("headers: %+v", out.Headers)
	}
	if _, err := Unmarshal([]byte("bogus")); err == nil {
		t.Fatal("want error for garbage")
	}
	// Legacy v1 entries (no headers) still decode for rolling deploys.
	v1 := append([]byte{0x01, 0, 0, 0, 200, 0, 0, 0, 8}, []byte("text/xml")...)
	v1 = append(v1, []byte("body")...)
	legacy, err := Unmarshal(v1)
	if err != nil || legacy.Status != 200 || legacy.ContentType != "text/xml" || string(legacy.Body) != "body" {
		t.Fatalf("v1 legacy: %+v %v", legacy, err)
	}
}

func TestPromotedHomeKeyIgnoresScreenOnly(t *testing.T) {
	q := url.Values{"contentDirectoryID": {"22"}, "pinnedContentDirectoryID": {"22,23"}, "X-Plex-Device-Screen-Resolution": {"800x600"}}
	a := ResponseKey("user:luke", "GET", "/hubs/promoted", q, "application/json")
	q.Set("X-Plex-Device-Screen-Resolution", "1920x1080")
	if a != ResponseKey("user:luke", "GET", "/hubs/promoted", q, "application/json") {
		t.Fatal("viewport changed promoted Home key")
	}
	q.Set("pinnedContentDirectoryID", "22")
	if a == ResponseKey("user:luke", "GET", "/hubs/promoted", q, "application/json") {
		t.Fatal("pinned Home sections must remain separate")
	}
	if a == ResponseKey("user:other", "GET", "/hubs/promoted", q, "application/json") {
		t.Fatal("Home cache crossed users")
	}
}

func TestFallbackPolicyKeepsWatchStateLive(t *testing.T) {
	if ttl, ok := FallbackTTL("/hubs/promoted"); !ok || ttl != StructuralHubStaleTTL {
		t.Fatalf("home fallback: %v %v", ttl, ok)
	}
	if ttl, ok := FallbackTTL("/hubs/sections/23"); !ok || ttl != StructuralHubStaleTTL {
		t.Fatalf("section hub fallback: %v %v", ttl, ok)
	}
	if ttl, ok := FallbackTTL("/library/collections/101/children"); !ok || ttl != CollectionStaleTTL {
		t.Fatalf("collection fallback: %v %v", ttl, ok)
	}
	if ttl, ok := FallbackTTL("/library/sections/23/all"); !ok || ttl != LibraryPageStaleTTL {
		t.Fatalf("library page fallback: %v %v", ttl, ok)
	}
	for _, path := range []string{"/hubs/continueWatching", "/hubs/home/recentlyAdded", "/:/timeline"} {
		if _, ok := FallbackTTL(path); ok {
			t.Fatalf("watch-state path must not use fallback: %s", path)
		}
	}
}

func TestSafeHeaders(t *testing.T) {
	h := map[string][]string{
		"ETag":           {`"x"`},
		"Set-Cookie":     {"sess=1"},
		"X-Plex-Token":   {"tok"},
		"Authorization":  {"bearer"},
		"Cache-Control":  {"no-store"},
		"Content-Length": {"5"},
		"X-Replx-Foo":    {"1"},
	}
	got := SafeHeaders(h)
	if len(got) != 1 || got["Etag"] != `"x"` {
		t.Fatalf("allowlist: %+v", got)
	}
}

func TestKeyStripsSecretsAndSorts(t *testing.T) {
	q1, _ := url.ParseQuery("X-Plex-Token=aaa&type=2&b=1")
	q2, _ := url.ParseQuery("b=1&type=2&X-Plex-Token=zzz")
	a := ResponseKey("fp-user-1", "GET", "/hubs/home/recentlyAdded", q1, "application/xml")
	b := ResponseKey("fp-user-1", "GET", "/hubs/home/recentlyAdded", q2, "application/xml")
	if a != b {
		t.Fatalf("token rotation and param order must not split keys:\n%s\n%s", a, b)
	}
	other := ResponseKey("fp-user-2", "GET", "/hubs/home/recentlyAdded", q1, "application/xml")
	if a == other {
		t.Fatal("different users must never share keys")
	}
	json := ResponseKey("fp-user-1", "GET", "/hubs/home/recentlyAdded", q1, "application/json")
	if a == json {
		t.Fatal("representations must never share keys")
	}
	if !strings.HasPrefix(a, "replx_edge:v2:default:hubs:fp-user-1:xml:0:0:") {
		t.Fatalf("key shape: %s", a)
	}
}

func TestPolicy(t *testing.T) {
	allow := map[string]time.Duration{
		"/hubs/home/recentlyAdded":              15 * time.Second,
		"/hubs/home/continueWatching":           5 * time.Second,
		"/hubs/promoted":                        10 * time.Second,
		"/hubs/continueWatching/items":          5 * time.Second,
		"/hubs/search":                          30 * time.Second,
		"/library/collections/4317478/children": 2 * time.Minute,
		"/library/metadata/4573984":             5 * time.Minute,
		"/library/sections":                     5 * time.Minute,
		"/library/sections/22/all":              60 * time.Second,
		"/identity":                             5 * time.Minute,
	}
	for p, want := range allow {
		got, ok := Cacheable("GET", p)
		if !ok || got != want {
			t.Errorf("%s: want %v ok, got %v %v", p, want, got, ok)
		}
	}
	deny := []string{
		"/:/timeline",
		"/video/:/transcode/universal/decision",
		"/video/:/transcode/universal/start.mpd",
		"/library/parts/11/file.mkv",
		"/photo/:/transcode",
		"/:/eventsource/notifications",
		"/library/sections", // method gate below, path itself is fine
	}
	for _, p := range deny[:6] {
		if _, ok := Cacheable("GET", p); ok {
			t.Errorf("%s must never cache", p)
		}
	}
	if _, ok := Cacheable("POST", "/library/sections"); ok {
		t.Error("POST must never cache")
	}
}

func TestMemoryExpiry(t *testing.T) {
	now := time.Now()
	m := &Memory{items: map[string]memItem{}, now: func() time.Time { return now }}
	ctx := context.Background()
	if err := m.Set(ctx, "k", Entry{Status: 200, Body: []byte("x")}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := m.Get(ctx, "k"); !ok {
		t.Fatal("want hit before expiry")
	}
	now = now.Add(2 * time.Minute)
	if _, ok, _ := m.Get(ctx, "k"); ok {
		t.Fatal("want miss after expiry")
	}
}

func TestClassOf(t *testing.T) {
	cases := map[string]string{
		"/hubs/home/continueWatching":  "cw",
		"/hubs/continueWatching/items": "cw",
		"/hubs/search?query=x":         "search",
		"/hubs/home":                   "hubs",
		"/library/sections":            "sections",
		"/library/sections/22/all":     "sections",
		"/library/metadata/1":          "metadata",
		"/library/collections/x":       "collections",
		"/identity":                    "identity",
		"/photo/:/transcode":           "browse",
	}
	for p, want := range cases {
		if got := ClassOf(p); got != want {
			t.Errorf("%s: want %s, got %s", p, want, got)
		}
	}
}

func TestGenerationsBumpRetiresNamespace(t *testing.T) {
	g := NewGenerations()
	q, _ := url.ParseQuery("type=2")
	key := func(scope, class string) string {
		sg, gg := g.Get(scope, class)
		return ResponseKeyGen(scope, class, "GET", "/hubs/home/continueWatching", q, "xml", sg, gg)
	}
	a := key("user:u1", "cw")
	if key("user:u1", "cw") != a {
		t.Fatal("stable generation must keep keys")
	}
	g.Bump("user:u1", "cw")
	if key("user:u1", "cw") == a {
		t.Fatal("bump must retire the namespace")
	}
	// Other scopes and classes are unaffected.
	if key("user:u2", "cw") == a {
		t.Fatal("scopes must isolate generations")
	}
	b := key("user:u1", "hubs")
	g.BumpScope("user:u1")
	if key("user:u1", "hubs") == b {
		t.Fatal("scope bump must retire all its classes")
	}
	c := key("user:u1", "hubs")
	g.BumpAll()
	if key("user:u1", "hubs") == c {
		t.Fatal("global bump must retire everything")
	}
	var nilGens *Generations
	nilGens.Bump("s", "c")
	nilGens.BumpScope("s")
	nilGens.BumpAll()
	if sg, gg := nilGens.Get("s", "c"); sg != 0 || gg != 0 {
		t.Fatal("nil generations must be safe")
	}
}
