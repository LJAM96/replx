package admin

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
	"time"
)

// Bootstrap sessions let a browser use the HTML panels: the operator opens
// /admin/login once, enters the per-process setup token, and receives an
// HttpOnly session cookie. API clients keep using Authorization: Bearer,
// which needs no CSRF protection (custom headers are not ambient).
// Cookie-authenticated mutating requests require the per-session CSRF
// token as a form field or X-CSRF-Token header.

const (
	sessionCookie = "replx_admin"
	sessionTTL    = 12 * time.Hour
)

type session struct {
	csrf    string
	expires time.Time
}

type sessionStore struct {
	mu sync.Mutex
	m  map[string]session
}

func newSessionStore() *sessionStore { return &sessionStore{m: map[string]session{}} }

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// create issues a session id + csrf token pair.
func (s *sessionStore) create() (id, csrf string, err error) {
	if id, err = randomHex(24); err != nil {
		return "", "", err
	}
	if csrf, err = randomHex(24); err != nil {
		return "", "", err
	}
	s.mu.Lock()
	s.m[id] = session{csrf: csrf, expires: time.Now().Add(sessionTTL)}
	s.mu.Unlock()
	return id, csrf, nil
}

// lookup returns the session and sweeps it if expired.
func (s *sessionStore) lookup(id string) (session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.m[id]
	if !ok {
		return session{}, false
	}
	if time.Now().After(sess.expires) {
		delete(s.m, id)
		return session{}, false
	}
	return sess, true
}

func (s *sessionStore) revoke(id string) {
	s.mu.Lock()
	delete(s.m, id)
	s.mu.Unlock()
}

func setSessionCookie(w http.ResponseWriter, id string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		// No Secure flag: the admin listener is loopback/plain HTTP by
		// design (private path via Tailscale/SSH). Revisit if the admin
		// listener ever terminates TLS itself.
		MaxAge: int((sessionTTL).Seconds()),
	})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
}
