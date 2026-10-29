package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/LJAM96/replx/internal/database"
)

// GenerationStore supplies durable response-cache namespaces. A failed Get or
// bump must cause callers to bypass cached responses until persistence recovers;
// substituting zero generations could revive a retired namespace.
type GenerationStore interface {
	Get(context.Context, string, string) (uint64, uint64, error)
	Bump(context.Context, string, string) error
	BumpScope(context.Context, string) error
	BumpAll(context.Context) error
}

// PostgresGenerations persists independent server, scope, and class counters.
// The effective scope/class generation is the sum of its scope and class
// counters, so a scope bump retires every class, even after a process restart
// and without enumerating recorded classes. Every counter only increases.
type PostgresGenerations struct {
	db     database.DBTX
	server string
}

var _ GenerationStore = (*PostgresGenerations)(nil)

// NewPostgresGenerations uses the stable server namespace also used by response
// keys. The single-origin deployment uses "default". No process-local snapshot
// is retained, so Get observes committed invalidations across restarts.
func NewPostgresGenerations(db database.DBTX, serverID string) *PostgresGenerations {
	if serverID == "" {
		serverID = "default"
	}
	return &PostgresGenerations{db: db, server: serverID}
}

// Load verifies storage availability at startup and initializes the server row.
// Existing generations are preserved. Reads require no hydration step.
func (g *PostgresGenerations) Load(ctx context.Context) error {
	if g == nil || g.db == nil {
		return errors.New("cache: generation storage unavailable")
	}
	_, err := g.db.Exec(ctx, `INSERT INTO cache_generations (server_id, scope_hash, class)
		VALUES ($1, '', '') ON CONFLICT DO NOTHING`, g.server)
	if err != nil {
		return fmt.Errorf("cache: load generations: %w", err)
	}
	return nil
}

// Get reads all three counters in one statement snapshot. Scope/class counters
// are summed in Go to avoid signed bigint overflow when combining valid values.
func (g *PostgresGenerations) Get(ctx context.Context, scope, class string) (uint64, uint64, error) {
	if g == nil || g.db == nil {
		return 0, 0, errors.New("cache: generation storage unavailable")
	}
	if class == "" {
		class = "browse"
	}
	var scopeGeneration, classGeneration, globalGeneration int64
	err := g.db.QueryRow(ctx, `SELECT
		COALESCE((SELECT generation FROM cache_generations WHERE server_id=$1 AND scope_hash=$2 AND class=''), 0),
		COALESCE((SELECT generation FROM cache_generations WHERE server_id=$1 AND scope_hash=$2 AND class=$3), 0),
		COALESCE((SELECT generation FROM cache_generations WHERE server_id=$1 AND scope_hash='' AND class=''), 0)`,
		g.server, generationScopeHash(scope), class).Scan(&scopeGeneration, &classGeneration, &globalGeneration)
	if err != nil {
		return 0, 0, fmt.Errorf("cache: get generations: %w", err)
	}
	return uint64(scopeGeneration) + uint64(classGeneration), uint64(globalGeneration), nil
}

// Bump atomically retires one scope/class namespace.
func (g *PostgresGenerations) Bump(ctx context.Context, scope, class string) error {
	if scope == "" {
		return nil
	}
	if class == "" {
		class = "browse"
	}
	return g.bump(ctx, generationScopeHash(scope), class)
}

// BumpScope atomically retires every class for this scope, including classes
// whose responses were cached by an earlier process or before any Get here.
func (g *PostgresGenerations) BumpScope(ctx context.Context, scope string) error {
	if scope == "" {
		return nil
	}
	return g.bump(ctx, generationScopeHash(scope), "")
}

// BumpAll atomically retires every namespace for this server.
func (g *PostgresGenerations) BumpAll(ctx context.Context) error {
	return g.bump(ctx, "", "")
}

func (g *PostgresGenerations) bump(ctx context.Context, scopeHash, class string) error {
	if g == nil || g.db == nil {
		return errors.New("cache: generation storage unavailable")
	}
	_, err := g.db.Exec(ctx, `INSERT INTO cache_generations (server_id, scope_hash, class, generation)
		VALUES ($1, $2, $3, 1)
		ON CONFLICT (server_id, scope_hash, class)
		DO UPDATE SET generation = cache_generations.generation + 1`, g.server, scopeHash, class)
	if err != nil {
		return fmt.Errorf("cache: bump generation: %w", err)
	}
	return nil
}

func generationScopeHash(scope string) string {
	sum := sha256.Sum256([]byte(scope))
	return hex.EncodeToString(sum[:])
}
