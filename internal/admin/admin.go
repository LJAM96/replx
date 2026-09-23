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
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/LJAM96/replx/internal/capture"
	"github.com/LJAM96/replx/internal/health"
	"github.com/LJAM96/replx/internal/metrics"
	"github.com/LJAM96/replx/internal/onboarding"
	"github.com/LJAM96/replx/internal/spike"
	syncpkg "github.com/LJAM96/replx/internal/sync"
	"github.com/LJAM96/replx/internal/warmer"
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
	setupIssued time.Time
	setupMu     sync.Mutex
	// setupConsumed permanently disables bootstrap authentication once
	// the administrator account is created. The setup capability is
	// single use: creation consumes it in the same process lifetime.
	setupConsumed bool
	requireAuth   bool
	sessions      *sessionStore
	registry      *metrics.Registry
	cap           *capture.Store
	warmerStats   func() warmer.Stats
	syncWorker    syncpkgWorker
	syncMu        sync.Mutex
	lastSyncFull  time.Time
	lastSyncJob   string
	checks        health.Checks
	rateMu        sync.Mutex
	rate          map[string][]time.Time
	// invalidator retires cache namespaces (wired to the proxy
	// generations in production). Nil keeps audit-only behaviour.
	invalidator func(scope, class string)
}

// SetCacheInvalidator wires the admin invalidation endpoint to real
// namespace invalidation.
func (m *Mux) SetCacheInvalidator(fn func(scope, class string)) {
	m.invalidator = fn
}

// syncpkgWorker is the sync surface admin needs (narrower than *sync.Worker
// for tests).
type syncpkgWorker interface {
	Status(ctx context.Context) ([]syncpkg.CursorStatus, error)
	SyncOnce(ctx context.Context, full bool) error
}

type ctxKey struct{}

// NewMux builds the admin mux. When requireAuth is true, API and panel
// routes require the setup token bearer or a bootstrap session cookie
// (CSRF-checked on mutation). spikeStore may be nil (spike disabled):
// the events endpoint then reports disabled instead of failing.
func NewMux(checks health.Checks, svc *onboarding.Service, setupToken string, requireAuth bool, spikeStore *spike.Store, spikeObs *spike.Observations) *Mux {
	m := &Mux{mux: http.NewServeMux(), svc: svc, spike: spikeStore, spikeObs: spikeObs, setupToken: setupToken, setupIssued: time.Now(), requireAuth: requireAuth, sessions: newSessionStore()}
	m.mux.Handle("/health/", health.AdminMux(checks))
	m.mux.HandleFunc("/admin/login", m.handleLogin)
	m.mux.HandleFunc("/admin/logout", m.auth(m.handleLogout))
	m.mux.HandleFunc("/api/v1/onboarding/status", m.auth(m.handleStatus))
	m.mux.HandleFunc("/api/v1/onboarding/pin", m.auth(m.handlePIN))
	m.mux.HandleFunc("/api/v1/onboarding/token", m.auth(m.handleToken))
	m.mux.HandleFunc("/api/v1/onboarding/resources", m.auth(m.handleResources))
	m.mux.HandleFunc("/api/v1/onboarding/select", m.auth(m.handleSelect))
	m.mux.HandleFunc("/api/v1/onboarding/verify", m.auth(m.handleVerify))
	m.mux.HandleFunc("/admin/onboarding", m.auth(m.handlePanel))
	m.mux.HandleFunc("/api/v1/spike/events", m.auth(m.handleSpikeEvents))
	m.mux.HandleFunc("/api/v1/spike/observations", m.auth(m.handleSpikeObservations))
	m.mux.HandleFunc("/api/v1/spike/report", m.auth(m.handleSpikeReport))
	m.mux.HandleFunc("/admin/spike", m.auth(m.handleSpikePanel))
	m.mux.HandleFunc("/api/v1/policies", m.auth(m.handlePolicies))
	m.mux.HandleFunc("/api/v1/playback/sessions", m.auth(m.handleSessions))
	m.mux.HandleFunc("/api/v1/compat/status", m.auth(m.handleCompat))
	m.registerMissing(checks)
	return m
}

// SetMetrics registers the open Prometheus exposition endpoint. The admin
// listener is private (loopback/Tailscale) by deployment contract, matching
// /health/*. Nil disables exposition with an explanatory comment.
func (m *Mux) SetMetrics(reg *metrics.Registry) {
	m.registry = reg
	m.mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET only")
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if m.registry == nil {
			_, _ = fmt.Fprintln(w, "# no metrics registry wired")
			return
		}
		m.registry.WritePrometheus(w)
	})
	m.mux.HandleFunc("/api/v1/cache/stats", m.auth(m.handleCacheStats))
}

// SetCapture registers the targeted protocol capture API behind setup auth:
// GET lists live targets plus recent events, POST arms a target, DELETE
// disarms. Capture is time-limited by the store; see internal/capture.
func (m *Mux) SetCapture(store *capture.Store) {
	m.cap = store
	m.mux.HandleFunc("/api/v1/diagnostics/capture", m.auth(m.handleCapture))
}

// SetWarmer attaches the owner-warmer snapshot for cache stats. Nil-safe:
// stats omit the warmer block until wired.
func (m *Mux) SetWarmer(stats func() warmer.Stats) {
	m.warmerStats = stats
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

func (m *Mux) setupTokenValid(got string) bool {
	if subtle.ConstantTimeCompare([]byte(got), []byte(m.setupToken)) != 1 {
		return false
	}
	m.setupMu.Lock()
	consumed := m.setupConsumed
	m.setupMu.Unlock()
	if consumed {
		return false
	}
	// Single-use setup URL valid 15 minutes from process start.
	if time.Since(m.setupIssued) > 15*time.Minute {
		return false
	}
	return true
}

// consumeSetup permanently retires the bootstrap capability and revokes
// every session minted from it. Called once administrator creation
// commits: a fifteen minute bootstrap credential must never outlive setup
// as a twelve hour browser session.
func (m *Mux) consumeSetup() {
	m.setupMu.Lock()
	m.setupConsumed = true
	m.setupMu.Unlock()
	m.sessions.revokeSubject("setup-token")
}

// bootstrapTTL caps browser sessions minted from the setup token at the
// remaining bootstrap window.
func (m *Mux) bootstrapTTL() time.Duration {
	return 15*time.Minute - time.Since(m.setupIssued)
}

func (m *Mux) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !m.requireAuth {
			next(w, r)
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got != "" && m.setupTokenValid(got) {
			next(w, withSubject(r, "setup-token")) // API-style auth: no CSRF exposure.
			return
		}
		sess, ok := m.sessionOf(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "SETUP_TOKEN_REQUIRED", "provide the per-process setup token as Authorization: Bearer (valid 15m from startup), or sign in at /admin/login")
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
		subj := "browser-session"
		if sess.subject != "" {
			subj = sess.subject
		}
		next(w, withSubject(withSession(r, sess), subj))
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
<form method="post"><label>Username <input name="username"></label> <label>Password <input type="password" name="password"></label>
<button type="submit">Sign in</button></form>
<p>The setup token is printed once in the server log at startup, valid 15 minutes. After <code>POST /api/v1/setup</code> creates the admin, sign in with username + password.</p>
</body></html>`)
	case http.MethodPost:
		if err := r.ParseForm(); err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_BODY", "unparseable form")
			return
		}
		if tok := r.FormValue("token"); tok != "" {
			if !m.setupTokenValid(tok) {
				writeError(w, http.StatusUnauthorized, "INVALID_TOKEN", "wrong, expired or consumed setup token (valid 15m from startup, single use)")
				return
			}
			ttl := m.bootstrapTTL()
			id, _, err := m.sessions.createWithSubjectTTL("setup-token", ttl)
			if err != nil {
				writeError(w, http.StatusUnauthorized, "INVALID_TOKEN", "bootstrap window expired")
				return
			}
			setSessionCookieTTL(w, id, ttl)
			http.Redirect(w, r, "/admin/onboarding", http.StatusSeeOther)
			return
		}
		username, password := r.FormValue("username"), r.FormValue("password")
		if username == "" || password == "" {
			writeError(w, http.StatusUnauthorized, "INVALID_CREDENTIALS", "username and password, or setup token, required")
			return
		}
		// Throttle by source and username. Failures stay
		// indistinguishable between unknown and existing usernames.
		if !m.checkRate("login:"+loginPeer(r)+":"+username, 5*time.Minute, 10) {
			writeError(w, http.StatusTooManyRequests, "RATE_LIMITED", "too many login attempts; retry later")
			return
		}
		hash, ok := m.verifyAdmin(r.Context(), username, password)
		if !ok {
			writeError(w, http.StatusUnauthorized, "INVALID_CREDENTIALS", "wrong username or password")
			return
		}
		// Transparent upgrade: a valid record with older parameters is
		// rehashed to current parameters without bothering the operator.
		if needsRehash(hash) {
			if fresh, err := hashPassword(password); err == nil {
				_, _ = m.svc.DB.Exec(r.Context(), `UPDATE admin_users SET password_hash=$1, updated_at=now() WHERE username=$2`, fresh, username)
			}
		}
		id, _, err := m.sessions.createWithSubject(username)
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
	// POST only with the normal cookie CSRF protection (enforced by
	// m.auth): logout is state mutation, and a cross-site navigation
	// must never terminate an administrator session.
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "POST only")
		return
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		m.sessions.revoke(c.Value)
	}
	clearSessionCookie(w)
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

func (m *Mux) verifyAdmin(ctx context.Context, username, password string) (string, bool) {
	if m.svc == nil {
		return "", false
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var hash string
	if err := m.svc.DB.QueryRow(cctx, `SELECT password_hash FROM admin_users WHERE username=$1`, username).Scan(&hash); err != nil {
		return "", false
	}
	if !verifyPassword(hash, password) {
		return "", false
	}
	return hash, true
}

// loginPeer keys login throttling by source address. Unparseable remotes
// collapse to one shared bucket (fail throttled, never open).
func loginPeer(r *http.Request) string {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = h
	}
	if host == "" {
		host = "unknown"
	}
	return host
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

// handleToken is the backup path: paste an existing Plex token. The token
// is validated before storage and never echoed back.
func (m *Mux) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "POST only")
		return
	}
	// Accept JSON {token} from API clients and form posts from the panel.
	var token string
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var body struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_BODY", "JSON {token} required")
			return
		}
		token = body.Token
	} else {
		if err := r.ParseForm(); err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_BODY", "unparseable form")
			return
		}
		token = r.FormValue("token")
	}
	username, err := m.svc.SubmitToken(r.Context(), token)
	if err != nil {
		writeError(w, http.StatusBadGateway, "TOKEN_REJECTED", err.Error())
		return
	}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		writeData(w, http.StatusOK, map[string]any{"authenticated": true, "username": username, "stage": m.svc.Status(r.Context())["stage"]})
		return
	}
	http.Redirect(w, r, "/admin/onboarding?msg="+url.QueryEscape("token accepted for "+username), http.StatusSeeOther)
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
<h2>Backup: paste a Plex token</h2>
<p>Only if the PIN flow is unreachable. The token is validated before storage, never shown back, and treated as legacy (no refresh).</p>
<form method="post" action="/api/v1/onboarding/token"><input type="hidden" name="csrf" value="%s"><input type="password" name="token" size="52" autocomplete="off">
<button type="submit">Submit token</button></form>
</body></html>`, csrf, csrf)
}

// handleCacheStats reports user-scoped cache counters plus the static v1
// policy (cacheable prefixes and TTLs) so operators can verify the fast
// path is serving without reading code.
func (m *Mux) handleCacheStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET only")
		return
	}
	var hits, misses, stale, warmed, warmErr int64
	if m.registry != nil {
		s := m.registry.Snapshot()
		hits, misses, stale, warmed, warmErr = s.CacheHits, s.CacheMisses, s.CacheStale, s.CacheWarmed, s.CacheWarmErr
	}
	data := map[string]any{
		"hits":     hits,
		"misses":   misses,
		"stale":    stale,
		"warmed":   warmed,
		"warmErrs": warmErr,
		"policy":   "GET /library/sections 5m, /library/sections/* 60s, /library/metadata/* 5m, /library/collections/* 2m, /identity 5m, /hubs/* 10s (CW 5s, RA 15s, search 30s); validated-user fallback: collections up to 15m, structural hubs up to 1m, refreshed in background; timeline/decisions/media never; artwork via filesystem 7d (/photo/:/transcode); stampede single-flight 2s; CW invalidated on timeline/scrobble",
	}
	if m.warmerStats != nil {
		data["warmer"] = m.warmerStats()
	}
	writeData(w, http.StatusOK, data)
}

// handleSpikeReport joins one playback session to its decisions and its
// spike-ring redirects: the per-request evidence bundle for the 0.4.0
// compatibility runs (request ID, session, client, selected mediaIndex,
// redirect Location, follow-through observed client-side).
func (m *Mux) handleSpikeReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET only")
		return
	}
	sessionID := r.URL.Query().Get("session")
	if sessionID == "" {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "?session=<plex-session-id> required")
		return
	}
	var session any
	var decisions []any
	if m.svc != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		var id, ratingKey, mode, routing, started string
		var ended, finalStatus *string
		err := m.svc.DB.QueryRow(ctx, `SELECT id::text, rating_key, playback_mode, routing_mode,
			to_char(started_at,'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'),
			to_char(ended_at,'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'), final_status
			FROM playback_sessions WHERE plex_session_identifier=$1
			ORDER BY started_at DESC LIMIT 1`, sessionID).Scan(
			&id, &ratingKey, &mode, &routing, &started, &ended, &finalStatus)
		if err == nil {
			session = map[string]any{"id": id, "ratingKey": ratingKey, "mode": mode,
				"routing": routing, "startedAt": started, "endedAt": ended, "finalStatus": finalStatus}
			rows, err := m.svc.DB.Query(ctx, `SELECT requested_media_index, selected_media_index,
				decision, decision_reason, plex_decision_code, details,
				to_char(created_at,'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')
				FROM playback_decisions WHERE playback_session_id=$1 ORDER BY created_at`, id)
			if err == nil {
				defer rows.Close()
				scanFailed := false
				for rows.Next() {
					var req, sel, code int
					var dec, reason string
					var details json.RawMessage
					var at string
					if err := rows.Scan(&req, &sel, &dec, &reason, &code, &details, &at); err != nil {
						scanFailed = true
						break
					}
					decisions = append(decisions, map[string]any{
						"requestedIndex": req, "selectedIndex": sel, "decision": dec,
						"reason": reason, "plexCode": code, "details": details, "at": at})
				}
				if scanFailed || rows.Err() != nil {
					// Best-effort evidence bundle: drop partial decisions
					// rather than present a silently truncated report.
					decisions = nil
				}
			}
		}
	}
	events := []spike.Event{}
	if m.spike != nil {
		for _, e := range m.spike.Events() {
			if e.SessionID == sessionID {
				events = append(events, e)
			}
		}
	}
	writeData(w, http.StatusOK, map[string]any{
		"session": session, "decisions": decisions, "spikeEvents": events,
	})
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

func (m *Mux) handleCapture(w http.ResponseWriter, r *http.Request) {
	if m.cap == nil {
		writeError(w, http.StatusConflict, "CAPTURE_DISABLED", "no capture store wired")
		return
	}
	switch r.Method {
	case http.MethodGet:
		events := m.cap.Events()
		if len(events) > 50 {
			events = events[len(events)-50:]
		}
		targets := m.cap.Targets()
		if targets == nil {
			targets = []capture.Target{}
		}
		writeData(w, http.StatusOK, map[string]any{"targets": targets, "events": events})
	case http.MethodPost:
		var body struct {
			ClientID  string `json:"clientId"`
			RatingKey string `json:"ratingKey"`
			Minutes   int    `json:"minutes"`
			Reason    string `json:"reason"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_BODY", "capture JSON required")
			return
		}
		ttl := time.Duration(body.Minutes) * time.Minute
		if ttl <= 0 {
			ttl = 30 * time.Minute
		}
		t, err := m.cap.Start(body.ClientID, body.RatingKey, ttl, body.Reason)
		if err != nil {
			writeError(w, http.StatusBadRequest, "CAPTURE_FAILED", err.Error())
			return
		}
		m.auditEvent(r.Context(), subjectOf(r), "trace.create", "capture", body.ClientID+"/"+body.RatingKey, map[string]any{"minutes": body.Minutes, "reason": body.Reason})
		writeData(w, http.StatusCreated, map[string]any{"target": t})
	case http.MethodDelete:
		var body struct {
			ClientID  string `json:"clientId"`
			RatingKey string `json:"ratingKey"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_BODY", "capture JSON required")
			return
		}
		writeData(w, http.StatusOK, map[string]any{"removed": m.cap.Stop(body.ClientID, body.RatingKey)})
	default:
		writeError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "GET, POST or DELETE")
	}
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
