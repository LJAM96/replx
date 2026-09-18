// Package trace derives Beta observability identity: client fingerprinting,
// user fingerprint correlation and playback_trace_id joining.
//
// Raw Plex tokens never leave this package boundary except back to the
// caller for origin forwarding. Fingerprints are HMAC-SHA256 hex keyed by
// REPLX_EDGE_SECRET_KEY: safe to log, index and join, never reversible
// without the secret.
package trace

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
)

// PlaybackTraceHeader is returned on playback-related control responses so
// operators can join client behaviour across decision, media redirect,
// timeline and segment requests.
const PlaybackTraceHeader = "X-Replx-Edge-Playback-Trace-ID"

// Client is the Plex client identity presented on every request.
type Client struct {
	ID       string
	Product  string
	Platform string
	Version  string
	Device   string
	Model    string
}

// ExtractClient reads the X-Plex-* client identity headers. Empty when the
// client sent none (never an error: identity is best-effort).
func ExtractClient(r *http.Request) Client {
	return Client{
		ID:       r.Header.Get("X-Plex-Client-Identifier"),
		Product:  r.Header.Get("X-Plex-Product"),
		Platform: r.Header.Get("X-Plex-Platform"),
		Version:  r.Header.Get("X-Plex-Version"),
		Device:   r.Header.Get("X-Plex-Device"),
		Model:    r.Header.Get("X-Plex-Model"),
	}
}

// ExtractToken returns the user-scoped token the client presented: header
// first, then query. Empty when the client sent none. Callers must never
// log the return value; fingerprint it via Fingerprint first.
func ExtractToken(r *http.Request) string {
	if t := r.Header.Get("X-Plex-Token"); t != "" {
		return t
	}
	q := r.URL.Query()
	for _, k := range []string{"X-Plex-Token", "token", "authToken"} {
		if t := q.Get(k); t != "" {
			return t
		}
	}
	return ""
}

// Fingerprint returns the HMAC-SHA256 hex of a Plex token. Empty token or
// empty secret yields "" (no identity) rather than a keyed MAC of nothing.
func Fingerprint(secret, token string) string {
	if secret == "" || token == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(token))
	return hex.EncodeToString(mac.Sum(nil))
}

// ExtractSession returns the playback session correlation value: the
// playback session ID first, then the session identifier, then the
// transcode session key. Empty for non-playback requests.
func ExtractSession(r *http.Request) string {
	if s := r.Header.Get("X-Plex-Playback-Session-Id"); s != "" {
		return s
	}
	if s := r.Header.Get("X-Plex-Session-Identifier"); s != "" {
		return s
	}
	if s := r.URL.Query().Get("session"); s != "" {
		return s
	}
	return r.Header.Get("X-Plex-Session-Id")
}

// ExtractRatingKey returns the library rating key for playback-related
// requests, best-effort: explicit query param, key/path params containing
// /library/metadata/<id>, or a /library/metadata/<id> request path.
func ExtractRatingKey(r *http.Request) string {
	q := r.URL.Query()
	if k := q.Get("ratingKey"); k != "" {
		return k
	}
	for _, k := range []string{"key", "path", "url"} {
		if id := metadataID(q.Get(k)); id != "" {
			return id
		}
	}
	if id := metadataID(r.URL.Path); id != "" {
		return id
	}
	return ""
}

// metadataID extracts the numeric ID from a /library/metadata/<id> value,
// which may be a full path, URL-encoded path or bare ID context.
func metadataID(v string) string {
	if v == "" {
		return ""
	}
	if decoded, err := url.QueryUnescape(v); err == nil {
		v = decoded
	}
	const marker = "/library/metadata/"
	i := strings.Index(v, marker)
	if i < 0 {
		return ""
	}
	rest := v[i+len(marker):]
	end := strings.IndexAny(rest, "/?&")
	if end >= 0 {
		rest = rest[:end]
	}
	return rest
}

// IsPlaybackRoute reports whether path participates in playback: media
// bytes, negotiation decisions, transcode namespace or watch-state
// timeline. Only these routes receive a playback trace ID.
func IsPlaybackRoute(path string) bool {
	p := strings.ToLower(path)
	// Artwork transcodes share the /transcode/ substring but are not
	// playback: exclude before the general transcode match.
	if strings.HasPrefix(p, "/photo/:/transcode") {
		return false
	}
	switch {
	case strings.HasPrefix(p, "/library/parts/"):
		return true
	case strings.Contains(p, ":/timeline"):
		return true
	case strings.Contains(p, "/transcode/"):
		return true
	default:
		return false
	}
}

// PlaybackTraceID joins playback-related requests into one trace using the
// Plex session identifier, user fingerprint, client and rating key. Empty
// unless the route is playback-related and carries at least a session or
// rating key plus some identity. Deterministic: identical inputs join,
// bounded by session/identity rotation (no time window stored here).
func PlaybackTraceID(secret, userFingerprint, clientID, sessionID, ratingKey string) string {
	if secret == "" || (sessionID == "" && ratingKey == "") {
		return ""
	}
	if userFingerprint == "" && clientID == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(userFingerprint))
	mac.Write([]byte{0x00})
	mac.Write([]byte(clientID))
	mac.Write([]byte{0x00})
	mac.Write([]byte(sessionID))
	mac.Write([]byte{0x00})
	mac.Write([]byte(ratingKey))
	return hex.EncodeToString(mac.Sum(nil))
}
