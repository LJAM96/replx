// Package capture implements targeted, time-limited protocol capture.
//
// Per docs/diagnostics_observability.md, protocol traces are targeted and
// time limited: an operator arms capture for one client and/or rating key
// for minutes-to-hours, matching requests log a second debug-level line
// with redacted protocol detail, and expiry is automatic. Raw tokens never
// enter targets or events; paths must be redacted by the caller.
package capture

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/LJAM96/replx/internal/trace"
)

// maxTargets bounds operator-armed captures; maxEvents bounds the ring.
const maxTargets = 32
const maxEvents = 200

// Target is one armed capture: at least one of ClientID/RatingKey.
type Target struct {
	ClientID  string `json:"clientId,omitempty"`
	RatingKey string `json:"ratingKey,omitempty"`
	Reason    string `json:"reason,omitempty"`
	ExpiresAt string `json:"expiresAt"`
	expires   time.Time
}

// Event is one captured protocol observation.
type Event struct {
	Timestamp       string `json:"timestamp"`
	RequestID       string `json:"requestId"`
	PlaybackTraceID string `json:"playbackTraceId,omitempty"`
	Method          string `json:"method"`
	Path            string `json:"path"`
	Client          string `json:"client,omitempty"`
	ClientProduct   string `json:"clientProduct,omitempty"`
	RatingKey       string `json:"ratingKey,omitempty"`
	Note            string `json:"note,omitempty"`
}

// Store holds capture targets and the recent event ring.
type Store struct {
	mu      sync.Mutex
	targets []Target
	events  []Event
	now     func() time.Time
}

// New returns an empty Store using wall-clock time.
func New() *Store { return &Store{now: time.Now} }

// Start arms capture for ttl. At least one of clientID/ratingKey is
// required; ttl is clamped to [1m, 8h].
func (s *Store) Start(clientID, ratingKey string, ttl time.Duration, reason string) (Target, error) {
	if clientID == "" && ratingKey == "" {
		return Target{}, fmt.Errorf("capture: clientId or ratingKey is required")
	}
	if ttl < time.Minute {
		ttl = time.Minute
	}
	if ttl > 8*time.Hour {
		ttl = 8 * time.Hour
	}
	now := s.time()
	t := Target{ClientID: clientID, RatingKey: ratingKey, Reason: reason, expires: now.Add(ttl)}
	t.ExpiresAt = t.expires.UTC().Format(time.RFC3339)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	if len(s.targets) >= maxTargets {
		return Target{}, fmt.Errorf("capture: too many active targets")
	}
	s.targets = append(s.targets, t)
	return t, nil
}

// Stop removes targets matching both fields exactly (empty matches empty).
// It reports how many were removed.
func (s *Store) Stop(clientID, ratingKey string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.targets[:0]
	removed := 0
	for _, t := range s.targets {
		if t.ClientID == clientID && t.RatingKey == ratingKey {
			removed++
			continue
		}
		kept = append(kept, t)
	}
	s.targets = kept
	return removed
}

// Match reports whether r hits a live target (client ID or rating key).
func (s *Store) Match(r *http.Request) bool {
	clientID := r.Header.Get("X-Plex-Client-Identifier")
	ratingKey := trace.ExtractRatingKey(r)
	if clientID == "" && ratingKey == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.time()
	s.pruneLocked(now)
	for _, t := range s.targets {
		if t.ClientID != "" && t.ClientID == clientID {
			return true
		}
		if t.RatingKey != "" && t.RatingKey == ratingKey {
			return true
		}
	}
	return false
}

// Record appends e to the ring (newest last, capped).
func (s *Store) Record(e Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.Timestamp == "" {
		e.Timestamp = s.time().UTC().Format(time.RFC3339Nano)
	}
	s.events = append(s.events, e)
	if len(s.events) > maxEvents {
		s.events = s.events[len(s.events)-maxEvents:]
	}
}

// Targets returns live targets, newest last.
func (s *Store) Targets() []Target {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(s.time())
	out := make([]Target, len(s.targets))
	copy(out, s.targets)
	return out
}

// Events returns a copy of the ring, newest last.
func (s *Store) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, len(s.events))
	copy(out, s.events)
	return out
}

func (s *Store) time() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *Store) pruneLocked(now time.Time) {
	kept := s.targets[:0]
	for _, t := range s.targets {
		if t.expires.After(now) {
			kept = append(kept, t)
		}
	}
	s.targets = kept
}
