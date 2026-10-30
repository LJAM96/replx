// Package artwork stores shared content-addressed Plex artwork while leaving
// caller authorization to the proxy's exact-resource lease. The production
// store uses PostgreSQL variant references, filesystem blobs, reservations,
// and a hard byte budget. The legacy New constructor remains for migration
// compatibility and isolated tests.
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

// Key returns the legacy account-scoped file key. Production uses a hashed
// scope-bound transform reference and gates every read through ResourceGrants;
// the referenced content-addressed blob is shared by digest.
//
// Deprecated: use the shared transform key in the proxy with NewShared.
func Key(scope, path string, query url.Values) string {
	var b strings.Builder
	b.WriteString(scope)
	b.WriteByte(0)
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
	shared   *sharedStore
}

// New creates the legacy cache directory. Production uses NewShared.
// The directory is owner-only: cached artwork is user-accessible media,
// not world-readable state.
func New(dir string, maxGB int) (*Store, error) {
	if dir == "" {
		return nil, fmt.Errorf("artwork: empty directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
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
	if s != nil && s.shared != nil {
		return s.GetContext(context.Background(), key)
	}
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

// Set stores a transcode atomically. Each payload is written to a unique
// temporary file in the destination filesystem (os.CreateTemp, never a
// predictable sibling path) and renamed into place; concurrent writers
// for the same key can no longer share or interleave temporaries. The
// body lands before the metadata so a crash can only leave an orphaned
// body, which reads as a miss and is reclaimed by the sweeper — never a
// metadata record pointing at a torn body. Files are owner-only.
func (s *Store) Set(key, contentType string, body []byte) error {
	if s != nil && s.shared != nil {
		r, err := s.Reserve(context.Background(), key, Normal)
		if err != nil {
			return err
		}
		defer r.Cancel(context.Background())
		return r.Commit(context.Background(), contentType, body)
	}
	if s == nil || key == "" {
		return fmt.Errorf("artwork: empty key")
	}
	if len(body) > MaxBodyBytes {
		return fmt.Errorf("artwork: body exceeds entry cap")
	}
	metaPath, bodyPath := s.paths(key)
	m, _ := json.Marshal(meta{ContentType: contentType, StoredAt: time.Now().UTC().Format(time.RFC3339)})
	tmpBody, err := writeTemp(s.dir, body)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpBody)
		}
	}()
	tmpMeta, err := writeTemp(s.dir, m)
	if err != nil {
		return err
	}
	defer func() {
		if !ok {
			_ = os.Remove(tmpMeta)
		}
	}()
	if err := os.Rename(tmpBody, bodyPath); err != nil {
		return err
	}
	if err := os.Rename(tmpMeta, metaPath); err != nil {
		_ = os.Remove(bodyPath)
		return err
	}
	ok = true
	return nil
}

// writeTemp Durably writes b to a unique 0600 file in dir and returns its
// path. Unique names (not key-derived siblings) close the concurrent
// writer race; fsync keeps a crash from tearing the payload.
func writeTemp(dir string, b []byte) (string, error) {
	f, err := os.CreateTemp(dir, ".artwork-tmp-*")
	if err != nil {
		return "", err
	}
	name := f.Name()
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return "", err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

// Sweep deletes oldest bodies until the budget holds. Returns removals.
func (s *Store) Sweep() (removed int, freed int64) {
	if s != nil && s.shared != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return s.shared.sweep(ctx)
	}
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
		// Crash orphans from interrupted writes never become entries:
		// reclaim them on every sweep.
		if strings.HasPrefix(e.Name(), ".artwork-tmp-") {
			_ = os.Remove(filepath.Join(s.dir, e.Name()))
			continue
		}
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
	if s.shared != nil {
		// Migrations run asynchronously. Recover immediately, then retry briefly
		// while unavailable so the first client request never performs the scan.
		retry := time.NewTimer(0)
		defer retry.Stop()
		maintenance := time.NewTicker(interval)
		defer maintenance.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-retry.C:
				if !s.Ready() {
					if err := s.Recover(ctx); err != nil {
						retry.Reset(5 * time.Second)
						continue
					}
				}
				s.Sweep()
				if s.shared.expiredPending.Load() {
					retry.Reset(time.Second)
				}
			case <-s.shared.maintenanceWake:
				s.Sweep()
				if s.shared.expiredPending.Load() {
					retry.Reset(time.Second)
				}
			case <-maintenance.C:
				if !s.Ready() {
					if err := s.Recover(ctx); err != nil {
						retry.Reset(5 * time.Second)
						continue
					}
				}
				s.Sweep()
				if s.shared.expiredPending.Load() {
					retry.Reset(time.Second)
				}
			}
		}
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
