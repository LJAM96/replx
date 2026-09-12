// Package routing implements ADR 001 direct origin routing.
//
// First candidate: HTTP 307 Temporary Redirect from replx-edge to a
// validated client reachable PMS HTTPS connection. Redirects carry an
// explicit PMS accepted token query because cross host preservation of
// X-Plex-Token cannot be assumed. The full Location URL is a secret and
// must be redacted in logs and traces.
package routing

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// BuildDirectOriginURL translates a validated replx-edge part path to a
// client reachable origin URL. originBase must be https and must not point
// back at the replx-edge Cloudflare hostname.
func BuildDirectOriginURL(originBase, partPath, token, replxPublicHost string) (string, error) {
	if !strings.HasPrefix(partPath, "/") {
		return "", fmt.Errorf("part path must be absolute")
	}
	base, err := url.Parse(originBase)
	if err != nil {
		return "", fmt.Errorf("invalid origin base: %w", err)
	}
	if base.Scheme != "https" {
		return "", fmt.Errorf("origin base must be https")
	}
	if base.Host == "" {
		return "", fmt.Errorf("origin base missing host")
	}
	if replxPublicHost != "" && strings.EqualFold(base.Hostname(), replxPublicHost) {
		return "", fmt.Errorf("origin must not point back at replx-edge hostname")
	}
	if token == "" {
		return "", fmt.Errorf("media token is required")
	}
	ref, err := url.Parse(partPath)
	if err != nil {
		return "", fmt.Errorf("invalid part path: %w", err)
	}
	target := base.ResolveReference(ref)
	q := target.Query()
	q.Set("X-Plex-Token", token)
	target.RawQuery = q.Encode()
	return target.String(), nil
}

// WriteMediaRedirect issues the 307 with no-store. Callers must log only
// RedactedLocation, never location.
func WriteMediaRedirect(w http.ResponseWriter, location string) {
	w.Header().Set("Location", location)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusTemporaryRedirect)
}

// RedactedLocation strips token query values for logs and traces.
func RedactedLocation(location string) string {
	u, err := url.Parse(location)
	if err != nil {
		return "<unparseable-location>"
	}
	q := u.Query()
	for _, k := range []string{"X-Plex-Token", "token", "authToken"} {
		if q.Has(k) {
			q.Set(k, "REDACTED")
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}
