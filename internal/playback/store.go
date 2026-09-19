package playback

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Session is one negotiated playback selection.
type Session struct {
	ID                 string
	PlexSessionID      string
	RatingKey          string
	SelectedMediaIndex int
	SelectedPartPlexID string
	SelectedPartKey    string
	SelectedVariantID  string
	SelectedPartID     string
	PlaybackMode       string
	RoutingMode        string
	EffectivePolicy    []byte
}

// SessionStore persists playback sessions. Memory serves tests and
// login-free unit paths; Postgres serves production enforcement.
type SessionStore interface {
	Create(ctx context.Context, s Session) (Session, error)
	FindActive(ctx context.Context, plexSessionID string) (Session, bool, error)
	EndSession(ctx context.Context, plexSessionID string) error
}

// MemoryStore is an in-process SessionStore.
type MemoryStore struct {
	mu       sync.Mutex
	sessions map[string]Session
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore { return &MemoryStore{sessions: map[string]Session{}} }

func (m *MemoryStore) Create(_ context.Context, s Session) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.PlexSessionID == "" {
		s.PlexSessionID = "mem-" + s.RatingKey
	}
	m.sessions[s.PlexSessionID] = s
	return s, nil
}

func (m *MemoryStore) FindActive(_ context.Context, plexSessionID string) (Session, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[plexSessionID]
	return s, ok, nil
}

func (m *MemoryStore) EndSession(_ context.Context, plexSessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, plexSessionID)
	return nil
}

// PGStore is the production SessionStore.
type PGStore struct {
	DB *pgxpool.Pool
}

// Create inserts a playback session row.
func (p *PGStore) Create(ctx context.Context, s Session) (Session, error) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err := p.DB.QueryRow(cctx, `INSERT INTO playback_sessions(server_id, plex_session_identifier, rating_key, selected_media_variant_id, selected_media_part_id, playback_mode, routing_mode, effective_policy)
		VALUES((SELECT id FROM plex_servers WHERE enabled ORDER BY created_at DESC LIMIT 1),$1,$2,$3,$4,$5,$6,$7)
		RETURNING id`,
		s.PlexSessionID, s.RatingKey, nullIfEmpty(s.SelectedVariantID), nullIfEmpty(s.SelectedPartID),
		s.PlaybackMode, s.RoutingMode, s.EffectivePolicy).Scan(&s.ID)
	if err != nil {
		return Session{}, err
	}
	return s, nil
}

// FindActive returns the newest open session for a Plex session ID.
func (p *PGStore) FindActive(ctx context.Context, plexSessionID string) (Session, bool, error) {
	if plexSessionID == "" {
		return Session{}, false, nil
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var s Session
	var variantID, partID *string
	err := p.DB.QueryRow(cctx, `SELECT ps.id, ps.plex_session_identifier, ps.rating_key,
		COALESCE(ps.selected_media_variant_id::text,''), COALESCE(ps.selected_media_part_id::text,''),
		COALESCE(mp.plex_part_id,''), COALESCE(mp.plex_key,''), ps.playback_mode, ps.routing_mode,
		COALESCE(ps.effective_policy,'{}')
		FROM playback_sessions ps LEFT JOIN media_parts mp ON mp.id = ps.selected_media_part_id
		WHERE ps.plex_session_identifier=$1 AND ps.ended_at IS NULL
		ORDER BY ps.started_at DESC LIMIT 1`,
		plexSessionID).Scan(&s.ID, &s.PlexSessionID, &s.RatingKey, &variantID, &partID,
		&s.SelectedPartPlexID, &s.SelectedPartKey, &s.PlaybackMode, &s.RoutingMode, &s.EffectivePolicy)
	if err != nil {
		return Session{}, false, nil // no session reads as absent, never as failure
	}
	if variantID != nil {
		s.SelectedVariantID = *variantID
	}
	if partID != nil {
		s.SelectedPartID = *partID
	}
	return s, true, nil
}

// EndSession closes open sessions for a Plex session ID.
func (p *PGStore) EndSession(ctx context.Context, plexSessionID string) error {
	if plexSessionID == "" {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := p.DB.Exec(cctx, `UPDATE playback_sessions SET ended_at=now(), final_status='stopped'
		WHERE plex_session_identifier=$1 AND ended_at IS NULL`, plexSessionID)
	return err
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
