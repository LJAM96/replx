// Package pms probes the origin PMS for the admin Overview page.
//
// PMS status is reported separately as healthy, degraded or unavailable
// and never gates admin readiness: the admin plane stays up when PMS is
// down so operators can inspect and recover.
package pms

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// Status values for the admin Overview page.
const (
	StatusHealthy     = "healthy"
	StatusDegraded    = "degraded"
	StatusUnavailable = "unavailable"
	StatusUnknown     = "unknown"
)

// Check reaches base (the internal origin URL) and classifies it:
// any HTTP response (even 401, which proves PMS is alive but wants auth)
// is healthy; transport errors are unavailable. Owner-authenticated depth
// checks land with the onboarding phase.
func Check(base string) string {
	if base == "" {
		return StatusUnknown
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(base, "/")+"/identity", nil)
	if err != nil {
		return StatusUnknown
	}
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // admin-configured origin only
	if err != nil {
		// Fall back to the root: some PMS builds behave differently on
		// /identity for unauthenticated requests.
		req2, err2 := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(base, "/")+"/", nil)
		if err2 != nil {
			return StatusUnavailable
		}
		resp2, err2 := http.DefaultClient.Do(req2) //nolint:gosec // admin-configured origin only
		if err2 != nil {
			return StatusUnavailable
		}
		defer resp2.Body.Close()
		return StatusHealthy
	}
	defer resp.Body.Close()
	return StatusHealthy
}
