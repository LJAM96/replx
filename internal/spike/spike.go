// Package spike is the P0 routing spike harness (ADR 001).
//
// When enabled (REPLX_EDGE_SPIKE_ROUTING=true), media routes in tunnel
// mode resolve to a 307 redirect at the onboarded client reachable origin
// instead of failing closed. The redirect carries the same user-scoped
// token the client presented (cross-host header preservation is not
// assumed); the owner token is never used. Every decision is traced to a
// redacted in-memory ring plus structured logs, and operator observations
// feed the compatibility matrix. With the flag off, the fail-closed 403
// from Alpha-1 is unchanged.
package spike

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/LJAM96/replx-edge/internal/logging"
	"github.com/LJAM96/replx-edge/internal/routing"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Event is one traced spike decision. Locations are always redacted;
// raw tokens never enter the ring, logs or API responses.
type Event struct {
	Timestamp        string `json:"timestamp"`
	RequestID        string `json:"requestId"`
	Method           string `json:"method"`
	Path             string `json:"path"`
	Client           string `json:"client"`
	Decision         string `json:"decision"`
	RedactedLocation string `json:"redactedLocation,omitempty"`
	RangePresent     bool   `json:"rangePresent"`
	Reason           string `json:"reason,omitempty"`
}

const maxEvents = 200

// Store resolves media redirects and keeps the trace ring.
type Store struct {
	// Lookup returns the enabled server's client media origin URL.
	Lookup     func(ctx context.Context) (string, bool)
	PublicHost string
	Logger     *logging.Logger
	mu         sync.Mutex
	events     []Event
}

// NewPostgresStore builds a Store reading the onboarded server row.
func NewPostgresStore(db *pgxpool.Pool, publicHost string, logger *logging.Logger) *Store {
	return &Store{
		Lookup: func(ctx context.Context) (string, bool) {
			if db == nil {
				return "", false
			}
			cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			var origin *string
			if err := db.QueryRow(cctx, `SELECT client_media_origin_url FROM plex_servers
				WHERE enabled ORDER BY created_at DESC LIMIT 1`).Scan(&origin); err != nil || origin == nil || *origin == "" {
				return "", false
			}
			return *origin, true
		},
		PublicHost: publicHost,
		Logger:     logger,
	}
}

// Resolve maps a media request to its 307 Location. ok=false means fall
// back to fail-closed MEDIA_ROUTE_UNAVAILABLE.
func (s *Store) Resolve(r *http.Request, requestID string) (string, bool) {
	origin, ok := s.Lookup(r.Context())
	if !ok {
		s.record(Event{RequestID: requestID, Method: r.Method, Path: logging.RedactURLString(r.URL.RequestURI()),
			Client: r.Header.Get("X-Plex-Client-Identifier"), Decision: "unavailable", Reason: "no onboarded media origin",
			RangePresent: r.Header.Get("Range") != ""})
		return "", false
	}
	token := ExtractToken(r)
	if token == "" {
		s.record(Event{RequestID: requestID, Method: r.Method, Path: logging.RedactURLString(r.URL.RequestURI()),
			Client: r.Header.Get("X-Plex-Client-Identifier"), Decision: "unavailable", Reason: "no user token presented",
			RangePresent: r.Header.Get("Range") != ""})
		return "", false
	}
	loc, err := routing.BuildDirectOriginURL(origin, r.URL.RequestURI(), token, s.PublicHost)
	token = ""
	if err != nil {
		s.record(Event{RequestID: requestID, Method: r.Method, Path: logging.RedactURLString(r.URL.RequestURI()),
			Client: r.Header.Get("X-Plex-Client-Identifier"), Decision: "unavailable", Reason: err.Error(),
			RangePresent: r.Header.Get("Range") != ""})
		return "", false
	}
	s.record(Event{RequestID: requestID, Method: r.Method, Path: logging.RedactURLString(r.URL.RequestURI()),
		Client: r.Header.Get("X-Plex-Client-Identifier"), Decision: "redirected", RedactedLocation: routing.RedactedLocation(loc),
		RangePresent: r.Header.Get("Range") != ""})
	return loc, true
}

// Events returns a copy of the trace ring, newest last.
func (s *Store) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, len(s.events))
	copy(out, s.events)
	return out
}

func (s *Store) record(e Event) {
	e.Timestamp = time.Now().UTC().Format("2006-01-02T15:04:05.999Z07:00")
	s.mu.Lock()
	s.events = append(s.events, e)
	if len(s.events) > maxEvents {
		s.events = s.events[len(s.events)-maxEvents:]
	}
	s.mu.Unlock()
	if s.Logger != nil {
		fields := map[string]any{"event": "spike_" + e.Decision, "client": e.Client,
			"range": e.RangePresent, "location": e.RedactedLocation}
		if e.Reason != "" {
			fields["reason"] = e.Reason
		}
		s.Logger.Log(logging.Entry{Level: "info", Component: "routing.spike",
			RequestID: e.RequestID, Method: e.Method, Path: e.Path, Fields: fields})
	}
}

// ExtractToken returns the user-scoped token the client presented: header
// first, then query. Empty when the client sent none.
func ExtractToken(r *http.Request) string {
	if t := r.Header.Get("X-Plex-Token"); t != "" {
		return t
	}
	q := r.URL.Query()
	for _, k := range []string{"X-Plex-Token", "token", "authToken"} {
		if t := q.Get(k); t != "" {
			return t
		}
	}
	return ""
}
