package cache

import (
	"context"
	"sync"
)

// MemoryGenerationStore supplies in-process counters for tests and deployments
// without persistent response caching. Persistent response caches must use
// PostgresGenerations so retired namespaces remain retired after a restart.
type MemoryGenerationStore struct {
	mu      sync.Mutex
	scopes  map[string]uint64
	classes map[string]uint64
	global  uint64
}

var _ GenerationStore = (*MemoryGenerationStore)(nil)

func NewMemoryGenerationStore() *MemoryGenerationStore {
	return &MemoryGenerationStore{scopes: map[string]uint64{}, classes: map[string]uint64{}}
}

func (g *MemoryGenerationStore) Get(ctx context.Context, scope, class string) (uint64, uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	if class == "" {
		class = "browse"
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.scopes[scope] + g.classes[scope+"\x00"+class], g.global, nil
}

func (g *MemoryGenerationStore) Bump(ctx context.Context, scope, class string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if scope == "" {
		return nil
	}
	if class == "" {
		class = "browse"
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.classes[scope+"\x00"+class]++
	return nil
}

func (g *MemoryGenerationStore) BumpScope(ctx context.Context, scope string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if scope == "" {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.scopes[scope]++
	return nil
}

func (g *MemoryGenerationStore) BumpAll(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.global++
	return nil
}
