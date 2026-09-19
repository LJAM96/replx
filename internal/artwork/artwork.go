// Package artwork caches Plex artwork transcodes on the filesystem.
//
// Entries are shared across users when URL plus transformation key match:
// identical bytes need no per-user copies, and every stored entry was
// requested with that user's own credential (authorization to reference).
// Anonymous requests bypass. TTL is 7 days; the janitor bounds disk usage
// oldest-first under the configured budget.
package artwork

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// TTL bounds artwork freshness. Thumbs change identity (new thumb IDs)
// when the underlying art changes, so expiry only reclaims the orphaned.
const TTL = 7 * 24 * time.Hour

// MaxBodyBytes caps a stored transcode (route-matrix 8 MiB ceiling).
const MaxBodyBytes = 8 << 20

// Match reports artwork-transcode routes eligible for the filesystem
// cache. Metadata thumb redirects stay in the user-scoped response cache.
func Match(path string) bool {
	return strings.HasPrefix(strings.ToLower(path), "/photo/:/transcode")
}

// Key hashes path plus sorted non-secret params: identical URL plus
// identical transformation shares one file across users.
func Key(path string, query url.Values) string {
	var b strings.Builder
	b.WriteString(strings.ToLower(path))
	b.WriteByte(0)
	keys := make([]string, 0, len(query))
	for k := range query {
		if isSecret(k) {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		vals := append([]string(nil), query[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			b.WriteString(strings.ToLower(k))
			b.WriteByte('=')
			b.WriteString(v)
			b.WriteByte('&')
		}
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func isSecret(k string) bool {
	switch strings.ToLower(k) {
	case "x-plex-token", "token", "authtoken":
		return true
	}
	return false
}

// Store persists transcodes under dir with a disk budget.
type Store struct {
	dir      string
	maxBytes int64
}

// New creates the cache directory. maxGB<=0 disables the budget check
// frequency, not the budget itself: pass a positive budget in production.
func New(dir string, maxGB int) (*Store, error) {
	if dir == "" {
		return nil, fmt.Errorf("artwork: empty directory")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("artwork: mkdir: %w", err)
	}
	return &Store{dir: dir, maxBytes: int64(maxGB) << 30}, nil
}

type meta struct {
	ContentType string `json:"contentType"`
	StoredAt    string `json:"storedAt"`
}

func (s *Store) paths(key string) (string, string) {
	return filepath.Join(s.dir, key+".meta"), filepath.Join(s.dir, key+".body")
}

// Get returns a fresh entry. Expired or corrupt entries read as misses
// and are removed lazily.
func (s *Store) Get(key string) (string, []byte, bool) {
	if s == nil || key == "" {
		return "", nil, false
	}
	metaPath, bodyPath := s.paths(key)
	raw, err := os.ReadFile(metaPath)
	if err != nil {
		return "", nil, false
	}
	var m meta
	if err := json.Unmarshal(raw, &m); err != nil {
		_ = os.Remove(metaPath)
		_ = os.Remove(bodyPath)
		return "", nil, false
	}
	stored, err := time.Parse(time.RFC3339, m.StoredAt)
	if err != nil || time.Since(stored) > TTL {
		_ = os.Remove(metaPath)
		_ = os.Remove(bodyPath)
		return "", nil, false
	}
	body, err := os.ReadFile(bodyPath)
	if err != nil {
		return "", nil, false
	}
	return m.ContentType, body, true
}

// Set stores a transcode atomically (tmp+rename). Oversize bodies and
// empty keys are rejected, never partially stored.
func (s *Store) Set(key, contentType string, body []byte) error {
	if s == nil || key == "" {
		return fmt.Errorf("artwork: empty key")
	}
	if len(body) > MaxBodyBytes {
		return fmt.Errorf("artwork: body exceeds entry cap")
	}
	metaPath, bodyPath := s.paths(key)
	m, _ := json.Marshal(meta{ContentType: contentType, StoredAt: time.Now().UTC().Format(time.RFC3339)})
	tmpMeta, tmpBody := metaPath+".tmp", bodyPath+".tmp"
	if err := os.WriteFile(tmpMeta, m, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(tmpBody, body, 0o644); err != nil {
		_ = os.Remove(tmpMeta)
		return err
	}
	if err := os.Rename(tmpMeta, metaPath); err != nil {
		return err
	}
	if err := os.Rename(tmpBody, bodyPath); err != nil {
		_ = os.Remove(metaPath)
		return err
	}
	return nil
}

// Sweep deletes oldest bodies until the budget holds. Returns removals.
func (s *Store) Sweep() (removed int, freed int64) {
	if s == nil {
		return 0, 0
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0, 0
	}
	type file struct {
		name    string
		size    int64
		modTime time.Time
	}
	var bodies []file
	var total int64
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".body") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		bodies = append(bodies, file{name: e.Name(), size: info.Size(), modTime: info.ModTime()})
		total += info.Size()
	}
	if s.maxBytes <= 0 || total <= s.maxBytes {
		return 0, 0
	}
	sort.Slice(bodies, func(i, j int) bool { return bodies[i].modTime.Before(bodies[j].modTime) })
	for _, b := range bodies {
		if total <= s.maxBytes {
			break
		}
		base := strings.TrimSuffix(b.name, ".body")
		_ = os.Remove(filepath.Join(s.dir, b.name))
		_ = os.Remove(filepath.Join(s.dir, base+".meta"))
		total -= b.size
		freed += b.size
		removed++
	}
	return removed, freed
}

// Run sweeps on interval until ctx ends.
func (s *Store) Run(ctx context.Context, interval time.Duration) {
	if s == nil {
		return
	}
	if interval <= 0 {
		interval = 30 * time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Sweep()
		}
	}
}
