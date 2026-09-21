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
func RedactURLString(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.RawQuery == "" {
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

// Log writes e with a UTC timestamp.
func (l *Logger) Log(e Entry) {
	e.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	_ = json.NewEncoder(l.w).Encode(e)
}
