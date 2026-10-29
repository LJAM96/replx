// Package warmer schedules bounded background refreshes for observed browse
// requests. Jobs contain only secret-stripped request metadata. User credentials
// remain in separate, short-lived memory contexts and never extend their own
// lifetime through warming.
package warmer

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/LJAM96/replx/internal/cache"
	"github.com/LJAM96/replx/internal/database"
	"github.com/LJAM96/replx/internal/logging"
	"github.com/LJAM96/replx/internal/metrics"
	"github.com/LJAM96/replx/internal/origin"
)

// maxTracked bounds memory when many distinct paths are browsed.
const maxTracked = 512

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
	Priority Priority
}

// Warmer refreshes due owner-scoped entries against the origin.
type Warmer struct {
	store      cache.Store
	origin     string
	secret     string
	ownerToken func(ctx context.Context) (string, bool)
	// OwnerAccount resolves the owner account ID for scope comparison,
	// cached for a minute. Refresh compares snapshot scopes against
	// the canonical owner scope: account identity, never token material.
	OwnerAccount func(ctx context.Context) (int64, bool)
	// DB resolves the owner account to its identity UUID for the
	// canonical "user:<uuid>" scope. Unset (tests) keeps the legacy
	// "acct:<id>" comparison.
	DB database.DBTX
	// KeyFunc recomputes a snapshot's cache key under current
	// invalidation generations. Unset keeps the tracked key as-is.
	KeyFunc func(s Snapshot) string
	// CanWarm is an optional capacity gate evaluated before each background
	// request. A false result leaves the job queued without consuming a request.
	CanWarm func(context.Context, Snapshot) bool
	log     *logging.Logger
	metrics *metrics.Registry
	client  *http.Client
	now     func() time.Time

	mu             sync.Mutex
	tracked        map[string]tracked
	refreshed      int64
	errors         int64
	warming        bool
	ownerAcct      int64
	ownerUUID      string
	ownerOK        bool
	ownerAt        time.Time
	config         Config
	paused         bool
	running        bool
	foreground     int
	activeCancel   context.CancelFunc
	activeScope    string
	contexts       map[string]credentialContext
	contextVersion uint64
	sequence       uint64
	served         map[string]uint64
	servedSequence uint64
	warmedBytes    int64
	backoffs       int64
	dropped        int64
}

type tracked struct {
	key             string
	snap            Snapshot
	last            time.Time
	observed        time.Time
	next            time.Time
	failures        int
	requiresContext bool
	sequence        uint64
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
		config:  DefaultConfig(), contexts: map[string]credentialContext{}, served: map[string]uint64{},
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
	w.track(key, s, "", false)
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
	w.refreshPass(ctx)
}

// Stats snapshots operator state.
func (w *Warmer) Stats() Stats {
	return w.stats()
}

func (w *Warmer) owner(ctx context.Context) (string, bool) {
	if w.ownerToken == nil {
		return "", false
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return w.ownerToken(cctx)
}

// ownerScope returns the canonical owner scope, caching the account
// lookup for a minute. With a database it resolves the owner account to
// its identity UUID ("user:<uuid>"), matching the proxy's canonical
// scope; without one (tests) it keeps the legacy "acct:<id>" form.
// Empty when the owner account is unknown: nothing refreshes.
func (w *Warmer) ownerScope(ctx context.Context) string {
	if w.OwnerAccount == nil {
		return ""
	}
	w.mu.Lock()
	if w.ownerOK && w.now().Sub(w.ownerAt) < time.Minute {
		id := w.ownerAcct
		uuid := w.ownerUUID
		w.mu.Unlock()
		return w.canonicalScope(id, uuid)
	}
	w.mu.Unlock()
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
	w.mu.Lock()
	w.ownerAcct, w.ownerUUID, w.ownerOK, w.ownerAt = id, uuid, ok, w.now()
	w.mu.Unlock()
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
// captured before fetching and checked again before publication so an
// invalidation during the fetch cannot publish an old body as current.
func (w *Warmer) refresh(ctx context.Context, key string, s Snapshot, owner string) error {
	_, err := w.refreshChecked(ctx, key, s, owner, nil)
	return err
}

func (w *Warmer) refreshChecked(ctx context.Context, key string, s Snapshot, credential string, permit func() bool) (int64, error) {
	storeKey := key
	if w.KeyFunc != nil {
		storeKey = w.KeyFunc(s)
		if storeKey == "" {
			return 0, refreshError{msg: "warmer: cache key unavailable"}
		}
	}
	target := w.origin + s.Path
	if s.RawQuery != "" {
		target += "?" + s.RawQuery
	}
	req, err := http.NewRequestWithContext(ctx, s.Method, target, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-Plex-Token", credential)
	if s.Accept != "" {
		req.Header.Set("Accept", s.Accept)
	}
	if _, ok := cache.RequestCacheable(req); !ok {
		return 0, refreshError{msg: "warmer: request is not cacheable"}
	}
	// Warmer refreshes feed the identity-normalized cache: ask for
	// identity encoding so stored bytes stay servable to any client.
	req.Header.Del("Accept-Encoding")
	resp, err := w.client.Do(req) //nolint:gosec // admin-configured origin only
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		n, _ := io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return n, errStatus(resp.StatusCode)
	}
	if !cache.StorableResponse(resp.Header) {
		n, _ := io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return n, refreshError{msg: "warmer: origin response not storable"}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, cache.MaxEntryBytes+1))
	if err != nil {
		return int64(len(body)), err
	}
	if len(body) > cache.MaxEntryBytes {
		return int64(len(body)), errTooLarge()
	}
	if w.KeyFunc != nil && w.KeyFunc(s) != storeKey {
		return int64(len(body)), refreshError{msg: "warmer: generation changed during refresh"}
	}
	if permit != nil && !permit() {
		return int64(len(body)), refreshError{msg: "warmer: authorization context or job expired"}
	}
	if err := w.store.Set(ctx, storeKey, cache.Entry{
		Status:      resp.StatusCode,
		ContentType: resp.Header.Get("Content-Type"),
		Headers:     cache.SafeHeaders(resp.Header),
		Body:        body,
	}, s.TTL); err != nil {
		return int64(len(body)), err
	}
	if storeKey != key {
		type deleter interface {
			Delete(ctx context.Context, key string) error
		}
		if d, ok := w.store.(deleter); ok {
			_ = d.Delete(ctx, key)
		}
	}
	return int64(len(body)), nil
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
		default:
			values := q[k]
			for i, value := range values {
				if nested, err := url.Parse(value); err == nil && nested.RawQuery != "" {
					nested.RawQuery = stripSecrets(nested.RawQuery)
					values[i] = nested.String()
				}
			}
			q[k] = values
		}
	}
	return q.Encode()
}

type refreshError struct {
	msg    string
	status int
}

func (e refreshError) Error() string { return e.msg }

func errStatus(code int) error {
	return refreshError{msg: "warmer: origin status " + http.StatusText(code), status: code}
}

func errTooLarge() error {
	return refreshError{msg: "warmer: body exceeds entry cap"}
}
