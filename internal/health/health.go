// Package health exposes liveness and readiness endpoints.
//
// Admin listener:
//
//	GET /health/live   always 200 when the process is running
//	GET /health/ready  200 when migrations are complete and Postgres+Valkey
//	                   are reachable; 503 otherwise. PMS status is reported
//	                   separately as healthy/degraded/unavailable and never
//	                   gates admin readiness.
//
// Media gateway listener:
//
//	GET /health/live   200 when the media process is running
package health

import (
	"encoding/json"
	"net/http"
)

// Checks are dependency probes wired at startup. Nil funcs report unknown.
type Checks struct {
	MigrationsComplete func() bool
	PostgresOK         func() bool
	ValkeyOK           func() bool
	PMSStatus          func() string
}

// AdminMux returns the private admin listener mux. It must never be routed
// through the public Plex hostname.
func AdminMux(c Checks) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "live"})
	})
	mux.HandleFunc("/health/ready", func(w http.ResponseWriter, _ *http.Request) {
		ready := true
		if c.MigrationsComplete != nil && !c.MigrationsComplete() {
			ready = false
		}
		if c.PostgresOK != nil && !c.PostgresOK() {
			ready = false
		}
		if c.ValkeyOK != nil && !c.ValkeyOK() {
			ready = false
		}
		pms := "unknown"
		if c.PMSStatus != nil {
			pms = c.PMSStatus()
		}
		status := http.StatusOK
		body := map[string]string{"status": "ready", "pms": pms}
		if !ready {
			status = http.StatusServiceUnavailable
			body["status"] = "not-ready"
		}
		writeJSON(w, status, body)
	})
	return mux
}

// MediaMux returns the media fallback listener mux. It exposes no admin,
// browsing, search or diagnostic routes.
func MediaMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/health/live", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "live"})
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
