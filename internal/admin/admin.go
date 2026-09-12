// Package admin serves the private admin listener: health plus the owner
// onboarding API and verification panel.
//
// The admin listener binds host loopback only and is never routed through
// the public Plex hostname. Until the local admin account lands, mutating
// onboarding endpoints require the per-process setup token issued at
// startup (logged once to server stderr by design) as
// Authorization: Bearer. Health endpoints stay open for orchestration.
package admin

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"

	"github.com/LJAM96/replx-edge/internal/health"
	"github.com/LJAM96/replx-edge/internal/onboarding"
)

// NewSetupToken generates a per-process bootstrap token.
func NewSetupToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Mux serves admin routes.
type Mux struct {
	mux         *http.ServeMux
	svc         *onboarding.Service
	setupToken  string
	requireAuth bool
}

// NewMux builds the admin mux. When requireAuth is true, onboarding routes
// (API + panel) require the setup token bearer.
func NewMux(checks health.Checks, svc *onboarding.Service, setupToken string, requireAuth bool) *Mux {
	m := &Mux{mux: http.NewServeMux(), svc: svc, setupToken: setupToken, requireAuth: requireAuth}
	m.mux.Handle("/health/", health.AdminMux(checks))
	m.mux.HandleFunc("/api/v1/onboarding/status", m.auth(m.handleStatus))
	m.mux.HandleFunc("/api/v1/onboarding/pin", m.auth(m.handlePIN))
	m.mux.HandleFunc("/api/v1/onboarding/resources", m.auth(m.handleResources))
	m.mux.HandleFunc("/api/v1/onboarding/select", m.auth(m.handleSelect))
	m.mux.HandleFunc("/api/v1/onboarding/verify", m.auth(m.handleVerify))
	m.mux.HandleFunc("/admin/onboarding", m.auth(m.handlePanel))
	return m
}

func (m *Mux) ServeHTTP(w http.ResponseWriter, r *http.Request) { m.mux.ServeHTTP(w, r) }

func (m *Mux) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if m.requireAuth {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(m.setupToken)) != 1 {
				writeError(w, http.StatusUnauthorized, "SETUP_TOKEN_REQUIRED", "provide the per-process setup token as Authorization: Bearer")
				return
			}
		}
		next(w, r)
	}
}

func writeData(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": message}})
}

func (m *Mux) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET only")
		return
	}
	writeData(w, http.StatusOK, m.svc.Status(r.Context()))
}

func (m *Mux) handlePIN(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		issue, err := m.svc.IssuePIN(r.Context())
		if err != nil {
			writeError(w, http.StatusBadGateway, "PIN_ISSUE_FAILED", err.Error())
			return
		}
		writeData(w, http.StatusCreated, issue)
	case http.MethodGet:
		claimed, err := m.svc.PollClaim(r.Context())
		if err != nil {
			writeError(w, http.StatusBadGateway, "PIN_POLL_FAILED", err.Error())
			return
		}
		writeData(w, http.StatusOK, map[string]any{"claimed": claimed, "stage": m.svc.Status(r.Context())["stage"]})
	default:
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET or POST")
	}
}

func (m *Mux) handleResources(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET only")
		return
	}
	servers, err := m.svc.ListServers(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "RESOURCES_FAILED", err.Error())
		return
	}
	writeData(w, http.StatusOK, map[string]any{"servers": servers})
}

func (m *Mux) handleSelect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "POST only")
		return
	}
	var body struct {
		ClientIdentifier string `json:"clientIdentifier"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil || body.ClientIdentifier == "" {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "JSON {clientIdentifier} required")
		return
	}
	report, err := m.svc.SelectResource(r.Context(), body.ClientIdentifier)
	if err != nil {
		writeError(w, http.StatusConflict, "SELECT_FAILED", err.Error())
		return
	}
	writeData(w, http.StatusOK, report)
}

func (m *Mux) handleVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "POST only")
		return
	}
	report, err := m.svc.Verify(r.Context())
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data":  report,
			"error": map[string]any{"code": "VERIFY_FAILED", "message": err.Error()},
		})
		return
	}
	writeData(w, http.StatusOK, report)
}

func (m *Mux) handlePanel(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		// Plain-HTML forms post here: dispatch by form name, then redirect.
		_ = r.ParseForm()
		msg := ""
		switch {
		case r.Form.Has("issue-pin"):
			if issue, err := m.svc.IssuePIN(r.Context()); err != nil {
				msg = "PIN failed: " + err.Error()
			} else {
				msg = "PIN " + issue.Code + ": open " + issue.AuthURL
			}
		case r.Form.Has("poll-pin"):
			if claimed, err := m.svc.PollClaim(r.Context()); err != nil {
				msg = "poll failed: " + err.Error()
			} else if claimed {
				msg = "owner authenticated"
			} else {
				msg = "not claimed yet"
			}
		case r.Form.Has("select"):
			if report, err := m.svc.SelectResource(r.Context(), r.Form.Get("clientIdentifier")); err != nil {
				msg = "select failed: " + err.Error()
			} else {
				msg = fmt.Sprintf("selected %s media=%s", report.MachineIdentifier, report.MediaOrigin)
			}
		case r.Form.Has("verify"):
			if report, err := m.svc.Verify(r.Context()); err != nil {
				msg = fmt.Sprintf("VERIFY FAILED: %s (origin=%s resource=%s proxied=%s)", err.Error(), report.OriginID, report.ResourceID, report.ProxiedID)
			} else {
				msg = fmt.Sprintf("VERIFIED: %s customUrl=%v media=%s", report.OriginID, report.CustomURLPresent, report.MediaOrigin)
			}
		}
		http.Redirect(w, r, "/admin/onboarding?msg="+url.QueryEscape(msg), http.StatusSeeOther)
		return
	}
	status := m.svc.Status(r.Context())
	servers, _ := m.svc.ListServers(r.Context())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><title>Replx Edge onboarding</title></head><body>
<h1>Replx Edge owner onboarding</h1>
<p>Stage: <b>%s</b></p>
<p>%s</p>
<h2>1. PIN</h2>
<form method="post"><button name="issue-pin" value="1" type="submit">Issue PIN</button>
<button name="poll-pin" value="1" type="submit">Poll claim</button></form>
<h2>2. Select PMS (exactly one)</h2>
<ul>`, html.EscapeString(fmt.Sprint(status["stage"])), html.EscapeString(r.URL.Query().Get("msg")))
	for _, s := range servers {
		_, _ = fmt.Fprintf(w, `<li>%s (%s) connections=%d httpsDirect=%v
<form method="post" style="display:inline"><input type="hidden" name="clientIdentifier" value="%s">
<button name="select" value="1" type="submit">Select</button></form></li>`,
			html.EscapeString(s.Name), html.EscapeString(s.ClientIdentifier), s.Connections, s.HTTPSDirect, html.EscapeString(s.ClientIdentifier))
	}
	_, _ = fmt.Fprint(w, `</ul>
<h2>3. Verify identity triple-check</h2>
<form method="post"><button name="verify" value="1" type="submit">Run verify</button></form>
<p>Verify proves origin root, plex.tv resource and proxied root name the same machineIdentifier, and the Custom Server Access URL is published.</p>
</body></html>`)
}
