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
	"strings"
	"sync"
	"time"

	"github.com/LJAM96/replx/internal/delegation"
	"github.com/LJAM96/replx/internal/gateway"
	"github.com/LJAM96/replx/internal/logging"
	"github.com/LJAM96/replx/internal/routing"
	"github.com/LJAM96/replx/internal/trace"
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
	// Lookup returns the enabled server's client media origin URL and
	// internal origin URL for delegation calls.
	Lookup func(ctx context.Context) (mediaOrigin, internalOrigin string, ok bool)
	// FetchTransient mints a delegation token under the caller's token.
	// Only transient tokens ever enter a redirect Location.
	FetchTransient func(ctx context.Context, internalOrigin, userToken string) (string, error)
	// PartPolicy enforces the Epsilon part boundary: substitute returns
	// an allowed part key replacing the requested path, deny fails
	// closed. Nil preserves pure ADR 001 redirect behaviour.
	PartPolicy func(r *http.Request, partID, sessionID string) (substituteKey string, deny bool, reason string)
	PublicHost string
	Logger     *logging.Logger
	mu         sync.Mutex
	events     []Event
}

// NewPostgresStore builds a Store reading the onboarded server row.
func NewPostgresStore(db *pgxpool.Pool, publicHost string, logger *logging.Logger) *Store {
	return &Store{
		Lookup: func(ctx context.Context) (string, string, bool) {
			if db == nil {
				return "", "", false
			}
			cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			var media, internal *string
			if err := db.QueryRow(cctx, `SELECT client_media_origin_url, internal_origin_url FROM plex_servers
				WHERE enabled ORDER BY created_at DESC LIMIT 1`).Scan(&media, &internal); err != nil ||
				media == nil || *media == "" || internal == nil || *internal == "" {
				return "", "", false
			}
			return *media, *internal, true
		},
		FetchTransient: delegation.Fetch,
		PublicHost:     publicHost,
		Logger:         logger,
	}
}

// Resolve maps a media request to its 307 Location. ok=false means fall
// back to fail-closed MEDIA_ROUTE_UNAVAILABLE (or the media gateway).
// Persistent caller tokens are never placed in the redirect: on delegation
// failure the request fails closed.
func (s *Store) Resolve(r *http.Request, requestID string) (string, bool) {
	base := spikeEventBase(r, requestID)
	mediaOrigin, internalOrigin, ok := s.Lookup(r.Context())
	if !ok {
		base.Decision, base.Reason = "unavailable", "no onboarded media origin"
		s.record(base)
		return "", false
	}
	userToken := ExtractToken(r)
	if userToken == "" {
		base.Decision, base.Reason = "unavailable", "no user token presented"
		s.record(base)
		return "", false
	}
	uri := r.URL.RequestURI()
	if s.PartPolicy != nil {
		// Parts carry an ID; manifests (empty ID) take the manifest
		// consistency check inside the same hook.
		partID := partIDFromPath(r.URL.Path)
		if partID != "" || gateway.IsBulkMediaRoute(r.URL.Path) {
			substitute, deny, reason := s.PartPolicy(r, partID, trace.ExtractSession(r))
			if deny {
				base.Decision, base.Reason = "unavailable", reason
				if reason == "" {
					base.Reason = "part boundary denied"
				}
				s.record(base)
				return "", false
			}
			if substitute != "" {
				uri = substitute
				if q := r.URL.RawQuery; q != "" && !strings.Contains(substitute, "?") {
					uri += "?" + q
				}
				base.Decision = "substituted"
			}
		}
	}
	transient, err := s.FetchTransient(r.Context(), internalOrigin, userToken)
	userToken = ""
	if err != nil {
		base.Decision, base.Reason = "unavailable", "delegation failed: "+err.Error()
		s.record(base)
		return "", false
	}
	loc, err := routing.BuildDirectOriginURL(mediaOrigin, uri, transient, s.PublicHost)
	transient = ""
	if err != nil {
		base.Decision, base.Reason = "unavailable", err.Error()
		s.record(base)
		return "", false
	}
	if base.Decision == "" {
		base.Decision = "redirected"
	}
	base.RedactedLocation = routing.RedactedLocation(loc)
	s.record(base)
	return loc, true
}

func spikeEventBase(r *http.Request, requestID string) Event {
	return Event{RequestID: requestID, Method: r.Method, Path: logging.RedactURLString(r.URL.RequestURI()),
		Client: r.Header.Get("X-Plex-Client-Identifier"), RangePresent: r.Header.Get("Range") != ""}
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

// partIDFromPath extracts the origin part ID from /library/parts/<id>/....
func partIDFromPath(path string) string {
	segs := strings.Split(strings.ToLower(path), "/")
	for i := 0; i+2 < len(segs); i++ {
		if segs[i] == "library" && segs[i+1] == "parts" && segs[i+2] != "" {
			return segs[i+2]
		}
	}
	return ""
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
