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
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"

	"github.com/LJAM96/replx/internal/health"
	"github.com/LJAM96/replx/internal/onboarding"
	"github.com/LJAM96/replx/internal/spike"
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
	spike       *spike.Store
	spikeObs    *spike.Observations
	setupToken  string
	requireAuth bool
	sessions    *sessionStore
}

type ctxKey struct{}

// NewMux builds the admin mux. When requireAuth is true, API and panel
// routes require the setup token bearer or a bootstrap session cookie
// (CSRF-checked on mutation). spikeStore may be nil (spike disabled):
// the events endpoint then reports disabled instead of failing.
func NewMux(checks health.Checks, svc *onboarding.Service, setupToken string, requireAuth bool, spikeStore *spike.Store, spikeObs *spike.Observations) *Mux {
	m := &Mux{mux: http.NewServeMux(), svc: svc, spike: spikeStore, spikeObs: spikeObs, setupToken: setupToken, requireAuth: requireAuth, sessions: newSessionStore()}
	m.mux.Handle("/health/", health.AdminMux(checks))
	m.mux.HandleFunc("/admin/login", m.handleLogin)
	m.mux.HandleFunc("/admin/logout", m.handleLogout)
	m.mux.HandleFunc("/api/v1/onboarding/status", m.auth(m.handleStatus))
	m.mux.HandleFunc("/api/v1/onboarding/pin", m.auth(m.handlePIN))
	m.mux.HandleFunc("/api/v1/onboarding/resources", m.auth(m.handleResources))
	m.mux.HandleFunc("/api/v1/onboarding/select", m.auth(m.handleSelect))
	m.mux.HandleFunc("/api/v1/onboarding/verify", m.auth(m.handleVerify))
	m.mux.HandleFunc("/admin/onboarding", m.auth(m.handlePanel))
	m.mux.HandleFunc("/api/v1/spike/events", m.auth(m.handleSpikeEvents))
	m.mux.HandleFunc("/api/v1/spike/observations", m.auth(m.handleSpikeObservations))
	m.mux.HandleFunc("/admin/spike", m.auth(m.handleSpikePanel))
	return m
}

func (m *Mux) ServeHTTP(w http.ResponseWriter, r *http.Request) { m.mux.ServeHTTP(w, r) }

// sessionOf returns the request's bootstrap session, if any.
func (m *Mux) sessionOf(r *http.Request) (session, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return session{}, false
	}
	return m.sessions.lookup(c.Value)
}

// csrfToken returns the CSRF token for rendering forms ("" for bearer auth).
func csrfToken(r *http.Request) string {
	if sess, ok := r.Context().Value(ctxKey{}).(session); ok {
		return sess.csrf
	}
	return ""
}

func (m *Mux) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !m.requireAuth {
			next(w, r)
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(m.setupToken)) == 1 {
			next(w, r) // API-style auth: no CSRF exposure.
			return
		}
		sess, ok := m.sessionOf(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "SETUP_TOKEN_REQUIRED", "provide the per-process setup token as Authorization: Bearer, or sign in at /admin/login")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			if err := r.ParseForm(); err != nil {
				writeError(w, http.StatusBadRequest, "INVALID_BODY", "unparseable form")
				return
			}
			if subtle.ConstantTimeCompare([]byte(r.FormValue("csrf")), []byte(sess.csrf)) != 1 &&
				subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(sess.csrf)) != 1 {
				writeError(w, http.StatusForbidden, "CSRF_REQUIRED", "valid CSRF token required for cookie-authenticated mutation")
				return
			}
		}
		next(w, withSession(r, sess))
	}
}

func withSession(r *http.Request, sess session) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), ctxKey{}, sess))
}

func (m *Mux) handleLogin(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = fmt.Fprint(w, `<!doctype html><html><head><meta charset="utf-8"><title>Replx Edge admin sign in</title></head><body>
<h1>Replx Edge admin sign in</h1>
<form method="post"><label>Setup token <input type="password" name="token" size="52"></label>
<button type="submit">Sign in</button></form>
<p>The token is printed once in the server log at startup and rotates on restart.</p>
</body></html>`)
	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_BODY", "unparseable form")
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.FormValue("token")), []byte(m.setupToken)) != 1 {
			writeError(w, http.StatusUnauthorized, "INVALID_TOKEN", "wrong setup token")
			return
		}
		id, _, err := m.sessions.create()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "SESSION_FAILED", "could not create session")
			return
		}
		setSessionCookie(w, id)
		http.Redirect(w, r, "/admin/onboarding", http.StatusSeeOther)
	default:
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET or POST")
	}
}

func (m *Mux) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		m.sessions.revoke(c.Value)
	}
	clearSessionCookie(w)
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
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
	csrf := html.EscapeString(csrfToken(r))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><title>Replx Edge onboarding</title></head><body>
<h1>Replx Edge owner onboarding</h1>
<p>Stage: <b>%s</b></p>
<p>%s</p>
<h2>1. PIN</h2>
<form method="post"><input type="hidden" name="csrf" value="%s"><button name="issue-pin" value="1" type="submit">Issue PIN</button>
<button name="poll-pin" value="1" type="submit">Poll claim</button></form>
<h2>2. Select PMS (exactly one)</h2>
<ul>`, html.EscapeString(fmt.Sprint(status["stage"])), html.EscapeString(r.URL.Query().Get("msg")), csrf)
	for _, s := range servers {
		_, _ = fmt.Fprintf(w, `<li>%s (%s) connections=%d httpsDirect=%v<br>hosts: %s
<form method="post" style="display:inline"><input type="hidden" name="csrf" value="%s"><input type="hidden" name="clientIdentifier" value="%s">
<button name="select" value="1" type="submit">Select</button></form></li>`,
			html.EscapeString(s.Name), html.EscapeString(s.ClientIdentifier), s.Connections, s.HTTPSDirect,
			html.EscapeString(strings.Join(s.ConnectionHosts, ", ")), csrf, html.EscapeString(s.ClientIdentifier))
	}
	_, _ = fmt.Fprintf(w, `</ul>
<h2>3. Verify identity triple-check</h2>
<form method="post"><input type="hidden" name="csrf" value="%s"><button name="verify" value="1" type="submit">Run verify</button></form>
<p>Verify proves origin root, plex.tv resource and proxied root name the same machineIdentifier, and the Custom Server Access URL is published.</p>
</body></html>`, csrf)
}

func (m *Mux) handleSpikeEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET only")
		return
	}
	events := []spike.Event{}
	enabled := m.spike != nil
	if enabled {
		events = m.spike.Events()
	}
	writeData(w, http.StatusOK, map[string]any{"enabled": enabled, "events": events})
}

func (m *Mux) handleSpikeObservations(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		rows, err := m.spikeObs.Matrix(r.Context())
		if err != nil {
			writeError(w, http.StatusBadGateway, "MATRIX_FAILED", err.Error())
			return
		}
		if rows == nil {
			rows = []spike.ObservationRow{}
		}
		writeData(w, http.StatusOK, map[string]any{"observations": rows})
	case http.MethodPost:
		var in spike.Observation
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_BODY", "observation JSON required")
			return
		}
		if err := m.spikeObs.Record(r.Context(), in); err != nil {
			writeError(w, http.StatusBadRequest, "RECORD_FAILED", err.Error())
			return
		}
		writeData(w, http.StatusCreated, map[string]any{"recorded": true})
	default:
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET or POST")
	}
}

func (m *Mux) handleSpikePanel(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		_ = r.ParseForm()
		msg := ""
		var in spike.Observation
		in.Platform = r.Form.Get("platform")
		in.Product = r.Form.Get("product")
		in.ProductVersion = r.Form.Get("productVersion")
		in.PlaybackType = r.Form.Get("playbackType")
		in.Status = r.Form.Get("status")
		in.Notes = r.Form.Get("notes")
		if err := m.spikeObs.Record(r.Context(), in); err != nil {
			msg = "record failed: " + err.Error()
		} else {
			msg = "recorded " + in.Product + " " + in.PlaybackType + " " + in.Status
		}
		http.Redirect(w, r, "/admin/spike?msg="+url.QueryEscape(msg), http.StatusSeeOther)
		return
	}
	rows, _ := m.spikeObs.Matrix(r.Context())
	events := []spike.Event{}
	enabled := m.spike != nil
	if enabled {
		events = m.spike.Events()
		if len(events) > 20 {
			events = events[len(events)-20:]
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	csrf := html.EscapeString(csrfToken(r))
	_, _ = fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><title>Replx Edge spike matrix</title></head><body>
<h1>P0 spike matrix</h1>
<p>Spike routing: <b>%v</b></p>
<p>%s</p>
<h2>Record observation</h2>
<form method="post">
<input type="hidden" name="csrf" value="%s">
platform <input name="platform" value="Web"> product <input name="product" value="Plex Web">
version <input name="productVersion"> playbackType <input name="playbackType" value="progressive">
status <select name="status"><option>SUPPORTED</option><option>DEGRADED</option><option>UNSUPPORTED</option><option>UNKNOWN</option><option>ADMIN_FORCED</option></select>
notes <input name="notes" size="60"> <button type="submit">Record</button></form>
<h2>Matrix</h2>
<ul>`, enabled, html.EscapeString(r.URL.Query().Get("msg")), csrf)
	for _, o := range rows {
		_, _ = fmt.Fprintf(w, "<li>%s %s %s %s n=%d: %s</li>", html.EscapeString(o.Platform),
			html.EscapeString(o.Product), html.EscapeString(o.PlaybackType), html.EscapeString(o.Status),
			o.Observations, html.EscapeString(o.Notes))
	}
	_, _ = fmt.Fprint(w, "</ul><h2>Recent spike traces (redacted, newest last)</h2><ul>")
	for _, e := range events {
		_, _ = fmt.Fprintf(w, "<li>%s %s %s %s range=%v %s</li>", html.EscapeString(e.Timestamp),
			html.EscapeString(e.Method), html.EscapeString(e.Path), html.EscapeString(e.Decision),
			e.RangePresent, html.EscapeString(e.RedactedLocation))
	}
	_, _ = fmt.Fprint(w, "</ul></body></html>")
}
