package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// RequestCacheable excludes conditional, partial and explicitly live reads.
// Other end-to-end request headers enter HeaderVariation, including Plex
// pagination, language, client capabilities and otherwise unknown headers.
func RequestCacheable(r *http.Request) (time.Duration, bool) {
	ttl, ok := Cacheable(r.Method, r.URL.Path)
	if !ok || r.ContentLength > 0 || r.Header.Get("Range") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" {
		return 0, false
	}
	for name := range r.Header {
		if strings.HasPrefix(strings.ToLower(name), "if-") {
			return 0, false
		}
	}
	if forbidsStorage(r.Header.Values("Cache-Control")) || strings.Contains(strings.ToLower(r.Header.Get("Pragma")), "no-cache") {
		return 0, false
	}
	return ttl, true
}

// HeaderVariation hashes request headers not already represented by the user
// scope, Accept negotiation or origin encoding normalization. An empty result
// means the existing warmer snapshot can reproduce the request.
func HeaderVariation(h http.Header) string {
	values := url.Values{}
	for name, vv := range h {
		switch strings.ToLower(name) {
		case "accept":
			if len(vv) == 1 {
				continue
			}
		case "accept-encoding", "x-plex-token", "x-replx-edge-request-id":
			continue
		}
		values[strings.ToLower(name)] = vv
	}
	if len(values) == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(values.Encode()))
	return hex.EncodeToString(sum[:])
}

// RequestKeyGen extends the snapshot-compatible key with header variation.
// Header values, including client identifiers, are never exposed in keys.
func RequestKeyGen(scope, class string, r *http.Request, scopeGen, globalGen uint64) string {
	key := ResponseKeyGen(scope, class, r.Method, r.URL.Path, r.URL.Query(), r.Header.Get("Accept"), scopeGen, globalGen)
	if variation := HeaderVariation(r.Header); variation != "" {
		key += ":" + variation
	}
	return key
}

// StorableResponse is shared by foreground fills and background refreshes.
// Unknown Vary fields bypass admission until their origin request semantics
// have been established; every header line is checked.
func StorableResponse(h http.Header) bool {
	if ce := strings.TrimSpace(strings.ToLower(h.Get("Content-Encoding"))); ce != "" && ce != "identity" {
		return false
	}
	if len(h.Values("Set-Cookie")) != 0 || forbidsStorage(h.Values("Cache-Control")) {
		return false
	}
	for _, line := range h.Values("Vary") {
		for _, name := range strings.Split(line, ",") {
			switch strings.ToLower(strings.TrimSpace(name)) {
			case "", "accept", "accept-encoding":
			default:
				return false
			}
		}
	}
	return true
}

func forbidsStorage(lines []string) bool {
	for _, line := range lines {
		for _, directive := range strings.Split(line, ",") {
			name := strings.TrimSpace(strings.SplitN(directive, "=", 2)[0])
			if strings.EqualFold(name, "no-store") || strings.EqualFold(name, "no-cache") {
				return true
			}
		}
	}
	return false
}
