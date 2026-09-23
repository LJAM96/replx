// Package warmer keeps hot owner-scoped browse entries fresh.
//
// The proxy tracks freshly stored cache entries; the warmer re-fetches the
// ones due (past TTL/2) with the owner credential before they expire, so
// active paths rarely miss. Only entries fingerprinted to the CURRENT owner
// token are ever refreshed: anything else is dropped from tracking, which
// is what keeps one user's refreshed response from ever landing in another
// user's cache slot. Owner token rotation therefore re-baselines tracking
// instead of poisoning it.
//
// Raw user tokens never enter the warmer: tracked snapshots carry the
// fingerprint and a secret-stripped query, and refreshes run under the
// owner token fetched per cycle from the encrypted credential store.
package warmer

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LJAM96/replx/internal/artwork"
	"github.com/LJAM96/replx/internal/cache"
	"github.com/LJAM96/replx/internal/database"
	"github.com/LJAM96/replx/internal/logging"
	"github.com/LJAM96/replx/internal/metrics"
	"github.com/LJAM96/replx/internal/origin"
)

// maxTracked bounds memory when many distinct paths are browsed.
const maxTracked = 512
const maxRefreshPerCycle = 8

// Snapshot captures how to reproduce one cache entry. Scope is the
// identity scope (canonical "user:<identity UUID>", legacy "acct:<id>" or
// "tok:<fingerprint>"): refresh compares against the owner scope, never
// raw tokens. Class is the invalidation namespace for key recomputation.
type Snapshot struct {
	Method   string
	Path     string
	RawQuery string // secret-stripped by Track
	Accept   string
	Scope    string
	Class    string
	TTL      time.Duration
}

// Stats is the operator-visible warmer state.
type Stats struct {
	Tracked         int   `json:"tracked"`
	Refreshed       int64 `json:"refreshed"`
	Errors          int64 `json:"errors"`
	OwnerWarming    bool  `json:"ownerWarming"`
	PreloadPages    int64 `json:"preloadPages"`
	PreloadArtwork  int64 `json:"preloadArtwork"`
	PreloadErrors   int64 `json:"preloadErrors"`
	LastPreloadUnix int64 `json:"lastPreloadUnix"`
}

// Warmer refreshes due owner-scoped entries against the origin.
type Warmer struct {
	store      cache.Store
	origin     string
	secret     string
	ownerToken func(ctx context.Context) (string, bool)
	// OwnerAccount resolves the current owner account for scope comparison.
	// Refresh compares snapshot scopes against
	// the canonical owner scope: account identity, never token material.
	OwnerAccount func(ctx context.Context) (int64, bool)
	// DB resolves the owner account to its identity UUID for the
	// canonical "user:<uuid>" scope. Unset (tests) keeps the legacy
	// "acct:<id>" comparison.
	DB database.DBTX
	// KeyFunc recomputes a snapshot's cache key under current
	// invalidation generations. Unset keeps the tracked key as-is.
	KeyFunc func(s Snapshot) string
	// Optional bounded candidate providers for proactive owner preloading.
	PreloadSections     func(ctx context.Context) ([]string, error)
	PreloadArtworkPaths func(ctx context.Context) ([]string, error)
	// PreloadHubQuery is an optional browser query profile for section hubs
	// and Continue Watching; credentials are stripped before storage.
	PreloadHubQuery string
	Artwork         *artwork.Store
	log             *logging.Logger
	metrics         *metrics.Registry
	client          *http.Client
	now             func() time.Time

	mu               sync.Mutex
	tracked          map[string]tracked
	refreshed        int64
	errors           int64
	warming          bool
	preloadPages     int64
	preloadedArtwork int64
	preloadErrors    int64
	lastPreloadUnix  int64
}

type tracked struct {
	snap Snapshot
	last time.Time
}

// New builds a Warmer. Nil store disables (Track/RefreshOnce no-op, Stats
// zero). ownerToken may be nil before onboarding completes.
func New(store cache.Store, origin, secret string,
	ownerToken func(ctx context.Context) (string, bool),
	logger *logging.Logger, reg *metrics.Registry) *Warmer {
	return &Warmer{
		store: store, origin: strings.TrimSuffix(origin, "/"), secret: secret,
		ownerToken: ownerToken, log: logger, metrics: reg,
		client:  originClientFor(origin),
		now:     time.Now,
		tracked: map[string]tracked{},
	}
}

// originClientFor binds refreshes to the configured origin: redirects
// leaving it are refused rather than followed with the owner credential.
func originClientFor(originBase string) *http.Client {
	if c, err := origin.APIClient(originBase, 30*time.Second); err == nil {
		return c
	}
	return origin.TransparentClient(30 * time.Second)
}

// Track records a freshly stored entry for future refresh. Snapshots with
// empty keys, fingerprints or TTLs are ignored.
func (w *Warmer) Track(key string, s Snapshot) {
	if w == nil || w.store == nil || key == "" || s.Scope == "" || s.TTL <= 0 {
		return
	}
	s.RawQuery = stripSecrets(s.RawQuery)
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.tracked) >= maxTracked {
		// Evict the stalest entry to bound memory.
		oldest := key
		var oldestTime time.Time
		first := true
		for k, t := range w.tracked {
			if first || t.last.Before(oldestTime) {
				oldest, oldestTime, first = k, t.last, false
			}
		}
		delete(w.tracked, oldest)
	}
	w.tracked[key] = tracked{snap: s, last: w.now()}
}

// Run refreshes due entries every interval until ctx ends.
func (w *Warmer) Run(ctx context.Context, interval time.Duration) {
	if w == nil || w.store == nil {
		return
	}
	if interval <= 0 {
		interval = 15 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.RefreshOnce(ctx)
		}
	}
}

// RefreshOnce refreshes every due entry exactly once. Testable single pass.
func (w *Warmer) RefreshOnce(ctx context.Context) {
	if w == nil || w.store == nil {
		return
	}
	now := w.now()
	w.mu.Lock()
	type dueEntry struct {
		key  string
		last time.Time
	}
	due := make([]dueEntry, 0)
	for k, t := range w.tracked {
		if t.snap.TTL <= 0 || now.Sub(t.last) >= t.snap.TTL/2 {
			due = append(due, dueEntry{key: k, last: t.last})
		}
	}
	w.mu.Unlock()
	if len(due) == 0 {
		return
	}
	sort.Slice(due, func(i, j int) bool { return due[i].last.Before(due[j].last) })
	if len(due) > maxRefreshPerCycle {
		due = due[:maxRefreshPerCycle]
	}
	owner, ok := w.owner(ctx)
	w.mu.Lock()
	w.warming = ok
	w.mu.Unlock()
	if !ok {
		return
	}
	ownerScope := w.ownerScope(ctx)
	if ownerScope == "" {
		return
	}
	for _, item := range due {
		k := item.key
		w.mu.Lock()
		t, exists := w.tracked[k]
		w.mu.Unlock()
		if !exists {
			continue
		}
		if t.snap.Scope != ownerScope {
			// Not owner-scoped (different account): drop rather than
			// refresh under the wrong identity. It re-tracks on its
			// next store if current.
			w.mu.Lock()
			delete(w.tracked, k)
			w.mu.Unlock()
			continue
		}
		requestCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		err := w.refresh(requestCtx, k, t.snap, owner)
		cancel()
		if err != nil {
			w.countErr()
			continue
		}
		w.mu.Lock()
		if cur, exists := w.tracked[k]; exists {
			cur.last = w.now()
			w.tracked[k] = cur
		}
		w.refreshed++
		w.mu.Unlock()
		if w.metrics != nil {
			w.metrics.IncCacheWarmed()
		}
	}
}

// Stats snapshots operator state.
func (w *Warmer) Stats() Stats {
	if w == nil {
		return Stats{}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return Stats{Tracked: len(w.tracked), Refreshed: w.refreshed, Errors: w.errors,
		OwnerWarming: w.warming, PreloadPages: w.preloadPages,
		PreloadArtwork: w.preloadedArtwork, PreloadErrors: w.preloadErrors,
		LastPreloadUnix: w.lastPreloadUnix}
}

func (w *Warmer) owner(ctx context.Context) (string, bool) {
	if w.ownerToken == nil {
		return "", false
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	token, ok := w.ownerToken(cctx)
	return token, ok && token != ""
}

// ownerScope returns the current canonical owner scope. It intentionally
// re-resolves each pass so a changed active server or owner cannot reuse a
// cached identity when a new credential is loaded. With a database it maps
// the owner account to
// its identity UUID ("user:<uuid>"), matching the proxy's canonical
// scope; without one (tests) it keeps the legacy "acct:<id>" form.
// Empty when the owner account is unknown: nothing refreshes.
func (w *Warmer) ownerScope(ctx context.Context) string {
	if w.OwnerAccount == nil {
		return ""
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	id, ok := w.OwnerAccount(cctx)
	var uuid string
	if ok && w.DB != nil {
		var serverID string
		if err := w.DB.QueryRow(cctx, `SELECT id FROM plex_servers
			WHERE enabled ORDER BY created_at DESC LIMIT 1`).Scan(&serverID); err == nil {
			_ = w.DB.QueryRow(cctx, `SELECT id::text FROM plex_identities
				WHERE server_id=$1 AND plex_account_id=$2
				ORDER BY updated_at DESC LIMIT 1`, serverID, id).Scan(&uuid)
		}
	}
	if !ok {
		return ""
	}
	return w.canonicalScope(id, uuid)
}

// canonicalScope prefers the identity UUID form and falls back to the
// legacy account form only when no identity store is wired.
func (w *Warmer) canonicalScope(accountID int64, uuid string) string {
	if uuid != "" {
		return cache.UserScope(uuid)
	}
	if w.DB != nil {
		return ""
	}
	return "acct:" + strconv.FormatInt(accountID, 10)
}

func (w *Warmer) countErr() {
	w.mu.Lock()
	w.errors++
	w.mu.Unlock()
	if w.metrics != nil {
		w.metrics.IncCacheWarmError()
	}
}

// refresh re-fetches one entry under the owner token and stores 200s
// within the entry cap. Anything else is an error (no negative caching:
// a flapping origin must not poison hot keys). The store key is
// recomputed under current invalidation generations so a refresh never
// resurrects a retired namespace; a superseded tracked key is dropped.
func (w *Warmer) refresh(ctx context.Context, key string, s Snapshot, owner string) error {
	target := w.origin + s.Path
	if s.RawQuery != "" {
		target += "?" + s.RawQuery
	}
	req, err := http.NewRequestWithContext(ctx, s.Method, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Plex-Token", owner)
	if s.Accept != "" {
		req.Header.Set("Accept", s.Accept)
	}
	// Warmer refreshes feed the identity-normalized cache: ask for
	// identity encoding so stored bytes stay servable to any client.
	req.Header.Del("Accept-Encoding")
	resp, err := w.client.Do(req) //nolint:gosec // admin-configured origin only
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return errStatus(resp.StatusCode)
	}
	if ce := strings.TrimSpace(strings.ToLower(resp.Header.Get("Content-Encoding"))); ce != "" && ce != "identity" {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return refreshError{msg: "warmer: encoded origin response not storable"}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, cache.MaxEntryBytes+1))
	if err != nil {
		return err
	}
	if len(body) > cache.MaxEntryBytes {
		return errTooLarge()
	}
	storeKey := key
	if w.KeyFunc != nil {
		if nk := w.KeyFunc(s); nk != "" {
			storeKey = nk
		}
	}
	if err := w.store.Set(ctx, storeKey, cache.Entry{
		Status:      resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type"),
		Body:        body,
	}, s.TTL); err != nil {
		return err
	}
	if storeKey != key {
		type deleter interface {
			Delete(ctx context.Context, key string) error
		}
		if d, ok := w.store.(deleter); ok {
			_ = d.Delete(ctx, key)
		}
	}
	return nil
}

// stripSecrets drops token-bearing params from a raw query so snapshots
// never retain user credentials (refreshes authenticate via header).
func stripSecrets(raw string) string {
	if raw == "" {
		return ""
	}
	q, err := url.ParseQuery(raw)
	if err != nil {
		return ""
	}
	for k := range q {
		switch strings.ToLower(k) {
		case "x-plex-token", "token", "authtoken":
			q.Del(k)
		}
	}
	return q.Encode()
}

type refreshError struct{ msg string }

func (e refreshError) Error() string { return e.msg }

func errStatus(code int) error {
	return refreshError{msg: "warmer: origin status " + http.StatusText(code)}
}

func errTooLarge() error {
	return refreshError{msg: "warmer: body exceeds entry cap"}
}
