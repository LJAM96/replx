package admin

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
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
	subject string
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
	return s.createWithSubject("")
}

// createWithSubject issues a session bound to an admin subject for audit.
func (s *sessionStore) createWithSubject(subject string) (id, csrf string, err error) {
	return s.createWithSubjectTTL(subject, sessionTTL)
}

// createWithSubjectTTL issues a subject-bound session with an explicit
// lifetime. Bootstrap sessions use this to cap expiry at the bootstrap
// window instead of the full browser lifetime.
func (s *sessionStore) createWithSubjectTTL(subject string, ttl time.Duration) (id, csrf string, err error) {
	if ttl <= 0 {
		return "", "", errors.New("bootstrap window expired")
	}
	if id, err = randomHex(24); err != nil {
		return "", "", err
	}
	if csrf, err = randomHex(24); err != nil {
		return "", "", err
	}
	s.mu.Lock()
	s.sweepLocked()
	s.m[id] = session{csrf: csrf, expires: time.Now().Add(ttl), subject: subject}
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

// revokeSubject revokes every session carrying a subject, used to retire
// bootstrap sessions the moment the administrator account is created.
func (s *sessionStore) revokeSubject(subject string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, sess := range s.m {
		if sess.subject == subject {
			delete(s.m, id)
		}
	}
}

// sweepLocked removes expired sessions. Callers must hold s.mu. Sessions
// accumulate only on lookup misses otherwise, so creation and lookup both
// sweep to keep eviction deterministic without a background goroutine.
func (s *sessionStore) sweepLocked() {
	now := time.Now()
	for id, sess := range s.m {
		if now.After(sess.expires) {
			delete(s.m, id)
		}
	}
}

func setSessionCookie(w http.ResponseWriter, id string) {
	setSessionCookieTTL(w, id, sessionTTL)
}

func setSessionCookieTTL(w http.ResponseWriter, id string, ttl time.Duration) {
	if ttl <= 0 {
		ttl = time.Second
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		// No Secure flag: the admin listener is loopback/plain HTTP by
		// design (private path via Tailscale/SSH). Revisit if the admin
		// listener ever terminates TLS itself.
		MaxAge: int(ttl.Seconds()),
	})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
}
