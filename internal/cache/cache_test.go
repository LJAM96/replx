package cache

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestCodecRoundTrip(t *testing.T) {
	in := Entry{Status: 200, ContentType: "application/xml", Body: []byte("<MediaContainer/>")}
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
	if _, err := Unmarshal([]byte("bogus")); err == nil {
		t.Fatal("want error for garbage")
	}
	if _, err := Unmarshal(append([]byte{0x01, 0, 0, 0}, 200)); err == nil {
		t.Fatal("want error for truncated")
	}
}

func TestKeyStripsSecretsAndSorts(t *testing.T) {
	q1, _ := url.ParseQuery("X-Plex-Token=aaa&type=2&b=1")
	q2, _ := url.ParseQuery("b=1&type=2&X-Plex-Token=zzz")
	a := ResponseKey("fp-user-1", "GET", "/hubs/home/recentlyAdded", q1)
	b := ResponseKey("fp-user-1", "GET", "/hubs/home/recentlyAdded", q2)
	if a != b {
		t.Fatalf("token rotation and param order must not split keys:\n%s\n%s", a, b)
	}
	other := ResponseKey("fp-user-2", "GET", "/hubs/home/recentlyAdded", q1)
	if a == other {
		t.Fatal("different users must never share keys")
	}
	if !strings.HasPrefix(a, "replx_edge:v1:browse:user:fp-user-1:GET:") {
		t.Fatalf("key shape: %s", a)
	}
}

func TestPolicy(t *testing.T) {
	allow := []string{
		"/hubs/home/recentlyAdded",
		"/library/collections/4317478/children",
		"/library/metadata/4573984",
		"/library/sections",
		"/library/sections/22/all",
		"/identity",
	}
	for _, p := range allow {
		if _, ok := Cacheable("GET", p); !ok {
			t.Errorf("%s must be cacheable", p)
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
