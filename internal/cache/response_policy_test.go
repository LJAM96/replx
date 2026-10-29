package cache

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestKeyPreservesResponseVariation(t *testing.T) {
	key := func(rawQuery, accept string) string {
		q, err := url.ParseQuery(rawQuery)
		if err != nil {
			t.Fatal(err)
		}
		return ResponseKey("user:1", "GET", "/library/sections/1/all", q, accept)
	}
	for _, pair := range [][2]string{{"a=x%26b%3Dy", "a=x&b=y"}, {"a=x&a=y", "a=y&a=x"}} {
		if key(pair[0], "") == key(pair[1], "") {
			t.Fatalf("query collision: %v", pair)
		}
	}
	if key("", "application/json;q=0.1,application/xml") == key("", "application/json,application/xml;q=0.1") {
		t.Fatal("Accept preference must vary the key")
	}
	r := httptest.NewRequest("GET", "/library/sections/1/all", nil)
	base := RequestKeyGen("user:1", "sections", r, 0, 0)
	for _, header := range []string{"X-Plex-Container-Start", "X-Plex-Container-Size", "Accept-Language", "X-Plex-Client-Capabilities", "X-Plex-Product", "User-Agent", "X-Unknown-Client-Feature"} {
		r.Header = http.Header{header: {"a"}}
		first := RequestKeyGen("user:1", "sections", r, 0, 0)
		r.Header[header] = []string{"b"}
		second := RequestKeyGen("user:1", "sections", r, 0, 0)
		if first == base || first == second {
			t.Errorf("%s must vary the key", header)
		}
		if _, ok := RequestCacheable(r); !ok {
			t.Errorf("%s should remain cacheable on demand", header)
		}
	}
}

func TestRequestAdmission(t *testing.T) {
	for _, header := range []string{"Range", "If-None-Match", "If-Modified-Since", "Cookie", "Authorization", "Cache-Control", "Pragma"} {
		r := httptest.NewRequest("GET", "/library/metadata/1", nil)
		r.Header.Set(header, "no-cache")
		if _, ok := RequestCacheable(r); ok {
			t.Errorf("%s should bypass", header)
		}
	}
	if _, ok := Cacheable("HEAD", "/library/metadata/1"); ok {
		t.Fatal("HEAD should stay live")
	}
}

func TestResponseAdmission(t *testing.T) {
	for _, h := range []http.Header{
		{"Vary": {"Accept", "X-Unknown-Feature"}},
		{"Vary": {"*"}},
		{"Vary": {"Accept-Language"}},
		{"Set-Cookie": {"session=value"}},
		{"Cache-Control": {"private", "max-age=60, NO-STORE"}},
		{"Cache-Control": {`no-cache="Etag"`}},
		{"Content-Encoding": {"gzip"}},
	} {
		if StorableResponse(h) {
			t.Errorf("unsafe response admitted: %v", h)
		}
	}
	if !StorableResponse(http.Header{"Vary": {"Accept", "Accept-Encoding"}}) {
		t.Fatal("modeled variation should be admitted")
	}
}
