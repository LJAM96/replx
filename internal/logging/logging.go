// Package logging emits structured JSON request logs with secret redaction.
//
// Never logged in cleartext: X-Plex-Token (header or query), Authorization,
// Cookie, Set-Cookie, owner JWT, PMS tokens, Tunnel token, REPLX_EDGE_SECRET_KEY,
// media redirect token query values.
package logging

import (
	"encoding/json"
	"io"
	"net/url"
	"strings"
	"time"
)

// RedactedHeaders are header names whose values are always REDACTED.
var RedactedHeaders = map[string]bool{
	"X-Plex-Token":          true,
	"Authorization":         true,
	"Cookie":                true,
	"Set-Cookie":            true,
	"X-Owner-JWT":           true,
	"X-PMS-Token":           true,
	"X-Tunnel-Token":        true,
	"X-Replx-Edge-Secret":   true,
	"Replx-Edge-Secret-Key": true,
}

// RedactedQueryParams are query keys whose values are always REDACTED.
var RedactedQueryParams = map[string]bool{
	"X-Plex-Token":  true,
	"X-Plex-Token ": true,
	"Token":         true,
	"token":         true,
	"AuthToken":     true,
	"authtoken":     true,
	"authToken":     true,
	"ownerJWT":      true,
	"owner_jwt":     true,
	"pmsToken":      true,
	"pms_token":     true,
	"tunnelToken":   true,
	"tunnel_token":  true,
	"secretKey":     true,
	"secret_key":    true,
}

// RedactHeaders returns a copy of h with sensitive values replaced.
func RedactHeaders(h map[string]string) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		if RedactedHeaders[canonicalHeader(k)] {
			out[k] = "REDACTED"
		} else {
			out[k] = v
		}
	}
	return out
}

func canonicalHeader(k string) string {
	for red := range RedactedHeaders {
		if strings.EqualFold(k, red) {
			return red
		}
	}
	return k
}

// RedactURLString strips sensitive query values from a URL string for logs.
// Malformed input fails closed: everything from the first query or
// fragment delimiter is removed, because malformed input is precisely
// where conservative redaction matters most.
func RedactURLString(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.RawQuery == "" {
		if err != nil {
			if i := strings.IndexAny(raw, "?#"); i >= 0 {
				return raw[:i] + "?REDACTED"
			}
		}
		return raw
	}
	q := u.Query()
	changed := false
	for k := range q {
		for red := range RedactedQueryParams {
			if strings.EqualFold(k, red) {
				q.Set(k, "REDACTED")
				changed = true
			}
		}
	}
	if !changed {
		return raw
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// Entry is one JSON log line.
type Entry struct {
	Level     string         `json:"level"`
	Timestamp string         `json:"timestamp"`
	Component string         `json:"component"`
	RequestID string         `json:"requestId,omitempty"`
	Method    string         `json:"method,omitempty"`
	Path      string         `json:"path,omitempty"`
	Status    int            `json:"status,omitempty"`
	Duration  int64          `json:"durationMs,omitempty"`
	Cache     string         `json:"cache,omitempty"`
	Route     string         `json:"routeClass,omitempty"`
	Fields    map[string]any `json:"fields,omitempty"`
}

// Logger writes JSON lines to w.
type Logger struct {
	w io.Writer
}

// New returns a Logger writing to w.
func New(w io.Writer) *Logger { return &Logger{w: w} }

// Log writes e with a UTC timestamp. Fields pass through a final
// sink-level filter: any string value under a sensitive key
// (token, secret, password, cookie, authorization, in any case) is
// replaced, so a caller mistake cannot leak a credential into the log
// stream. Structured callers must still prefer fingerprints and redacted
// URLs; this is defense in depth, not the primary control.
func (l *Logger) Log(e Entry) {
	e.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	e.Path = RedactURLString(e.Path)
	if e.Fields != nil {
		for k, v := range e.Fields {
			if sensitiveField(k) {
				e.Fields[k] = "REDACTED"
				continue
			}
			if s, ok := v.(string); ok && len(s) > 0 && looksLikeTokenURL(s) {
				e.Fields[k] = RedactURLString(s)
			}
		}
	}
	_ = json.NewEncoder(l.w).Encode(e)
}

func sensitiveField(k string) bool {
	lk := strings.ToLower(k)
	for _, sub := range []string{"token", "secret", "password", "cookie", "authorization", "set-cookie"} {
		if strings.Contains(lk, sub) {
			return true
		}
	}
	return false
}

// looksLikeTokenURL redacts string field values that carry a query string
// with a sensitive-looking parameter, e.g. a redirect Location that
// escaped RedactedLocation upstream.
func looksLikeTokenURL(s string) bool {
	i := strings.IndexByte(s, '?')
	if i < 0 || !strings.Contains(s[:i], "/") {
		return false
	}
	q := strings.ToLower(s[i:])
	for _, sub := range []string{"token", "secret", "password", "auth"} {
		if strings.Contains(q, sub) {
			return true
		}
	}
	return false
}
