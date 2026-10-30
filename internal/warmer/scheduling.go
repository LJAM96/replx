package warmer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/LJAM96/replx/internal/cache"
)

const (
	credentialTTL = 2 * time.Minute
	maxContexts   = 32
)

// Priority orders useful observed work. Protected remains bounded and is never
// an authorization or a permanent storage pin.
type Priority int

const (
	Low Priority = iota
	Normal
	High
	Protected
)

func (p Priority) String() string { return [...]string{"low", "normal", "high", "protected"}[p] }

// Config bounds a background pass. Requests run serially, yielding their only
// in-flight request whenever foreground browsing starts.
type Config struct {
	Enabled            bool  `json:"enabled"`
	MaxTracked         int   `json:"maxTracked"`
	RequestsPerPass    int   `json:"requestsPerPass"`
	MaxPerScopePerPass int   `json:"maxPerScopePerPass"`
	BytesPerPass       int64 `json:"bytesPerPass"`
}

func DefaultConfig() Config {
	return Config{Enabled: true, MaxTracked: 512, RequestsPerPass: 16, MaxPerScopePerPass: 4, BytesPerPass: 64 << 20}
}

// Stats describes observed jobs only; it is not whole-library coverage.
type Stats struct {
	Tracked         int            `json:"tracked"`
	Refreshed       int64          `json:"refreshed"`
	Errors          int64          `json:"errors"`
	OwnerWarming    bool           `json:"ownerWarming"`
	Enabled         bool           `json:"enabled"`
	Paused          bool           `json:"paused"`
	Queued          int            `json:"queued"`
	Due             int            `json:"due"`
	ActiveContexts  int            `json:"activeContexts"`
	ColdScopes      int            `json:"coldScopes"`
	ByPriority      map[string]int `json:"byPriority"`
	WarmedBytes     int64          `json:"warmedBytes"`
	Backoffs        int64          `json:"backoffs"`
	Dropped         int64          `json:"dropped"`
	RequestsPerPass int            `json:"requestsPerPass"`
	BytesPerPass    int64          `json:"bytesPerPass"`
}

// Credential material is separate from queued jobs and has no serialization
// API. Foreground authorization can renew it; background refresh never does.
type credentialContext struct {
	token   string
	expires time.Time
	version uint64
}

func (w *Warmer) Config() Config {
	if w == nil {
		return DefaultConfig()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.config
}

func (w *Warmer) Configure(c Config) error {
	if c.MaxTracked < 1 || c.MaxTracked > maxTracked || c.RequestsPerPass < 1 || c.RequestsPerPass > 64 || c.MaxPerScopePerPass < 1 || c.MaxPerScopePerPass > 16 || c.MaxPerScopePerPass > c.RequestsPerPass || c.BytesPerPass < 8<<20 || c.BytesPerPass > 256<<20 {
		return fmt.Errorf("warmer: limits require maxTracked 1..512, requestsPerPass 1..64, maxPerScopePerPass 1..16 and <= requestsPerPass, bytesPerPass 8..256 MiB")
	}
	if w == nil {
		return errors.New("warmer: unavailable")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.config = c
	if !c.Enabled {
		clear(w.contexts)
		if w.activeCancel != nil {
			w.activeCancel()
		}
	}
	for len(w.tracked) > c.MaxTracked {
		w.evictJobLocked()
	}
	return nil
}

func (w *Warmer) SetPaused(paused bool) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.paused = paused
	if paused && w.activeCancel != nil {
		w.activeCancel()
	}
}

// Foreground gives browsing priority over warming. The returned function must
// be deferred by the proxy and is safe to call more than once.
func (w *Warmer) Foreground() func() {
	if w == nil {
		return func() {}
	}
	w.mu.Lock()
	w.foreground++
	if w.activeCancel != nil {
		w.activeCancel()
	}
	w.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { w.mu.Lock(); w.foreground--; w.mu.Unlock() }) }
}

func (w *Warmer) ForgetScope(scope string) {
	if w == nil || scope == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.contexts, scope)
	delete(w.served, scope)
	if w.activeScope == scope && w.activeCancel != nil {
		w.activeCancel()
	}
	for id, t := range w.tracked {
		if t.snap.Scope == scope {
			delete(w.tracked, id)
			w.dropped++
		}
	}
	// A current publication also checks that its job still exists.
}

// TrackAuthorized is called only after a complete successful caller-token PMS
// response has been accepted by the foreground cache. The credential is held
// for at most two minutes after this foreground observation.
func (w *Warmer) TrackAuthorized(key string, s Snapshot, credential string) {
	w.track(key, s, credential, true)
}

func (w *Warmer) track(key string, s Snapshot, credential string, authorized bool) {
	if w == nil || w.store == nil || key == "" || len(key) > 512 || s.Scope == "" || len(s.Scope) > 256 || len(s.Path) > 4096 || len(s.RawQuery) > 8192 || len(s.Accept) > 512 || s.TTL <= 0 || (authorized && (credential == "" || len(credential) > 8192)) {
		return
	}
	s.RawQuery = stripSecrets(s.RawQuery)
	if s.Priority < Low || s.Priority > Protected {
		return
	}
	if s.Priority == Low {
		s.Priority = routePriority(s.Path)
	}
	id := snapshotID(s)
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.config.Enabled {
		return
	}
	now := w.now()
	w.expireContextsLocked(now)
	if authorized {
		if _, exists := w.contexts[s.Scope]; !exists && len(w.contexts) >= maxContexts {
			var oldest string
			var expiry time.Time
			for scope, c := range w.contexts {
				if oldest == "" || c.expires.Before(expiry) {
					oldest, expiry = scope, c.expires
				}
			}
			delete(w.contexts, oldest)
		}
		w.contextVersion++
		w.contexts[s.Scope] = credentialContext{token: credential, expires: now.Add(credentialTTL), version: w.contextVersion}
	}
	if _, exists := w.tracked[id]; !exists && len(w.tracked) >= w.config.MaxTracked {
		if !w.evictJobLocked(s.Priority) {
			return
		}
	}
	w.sequence++
	w.tracked[id] = tracked{key: key, snap: s, last: now, observed: now, requiresContext: authorized, sequence: w.sequence}
}

func routePriority(path string) Priority {
	p := strings.ToLower(path)
	if strings.Contains(p, "/collections") {
		return Protected
	}
	if strings.HasPrefix(p, "/library/sections") {
		return High
	}
	if strings.HasPrefix(p, "/library/metadata") {
		return Normal
	}
	return Low
}

func snapshotID(s Snapshot) string {
	raw, _ := json.Marshal([]string{s.Scope, s.Method, s.Path, s.RawQuery, s.Accept})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (w *Warmer) expireContextsLocked(now time.Time) {
	for scope, c := range w.contexts {
		if !now.Before(c.expires) {
			delete(w.contexts, scope)
		}
	}
	scopes := map[string]bool{}
	for _, job := range w.tracked {
		scopes[job.snap.Scope] = true
	}
	for scope := range w.served {
		if !scopes[scope] {
			delete(w.served, scope)
		}
	}
}

func (w *Warmer) evictJobLocked(incoming ...Priority) bool {
	var victim string
	var oldest tracked
	for id, t := range w.tracked {
		if victim == "" || t.snap.Priority < oldest.snap.Priority || (t.snap.Priority == oldest.snap.Priority && t.observed.Before(oldest.observed)) {
			victim, oldest = id, t
		}
	}
	if victim != "" {
		if len(incoming) > 0 && oldest.snap.Priority > incoming[0] {
			w.dropped++
			return false
		}
		delete(w.tracked, victim)
		w.dropped++
	}
	return true
}

type candidate struct {
	id      string
	job     tracked
	token   string
	version uint64
	owner   bool
	served  uint64
}

func (w *Warmer) nextCandidateLocked(now time.Time, owner, ownerScope string, perScope map[string]int) (candidate, bool) {
	byScope := map[string]candidate{}
	for id, t := range w.tracked {
		if now.Sub(t.last) < t.snap.TTL/2 || now.Before(t.next) || perScope[t.snap.Scope] >= w.config.MaxPerScopePerPass {
			continue
		}
		c := candidate{id: id, job: t, served: w.served[t.snap.Scope]}
		if context, ok := w.contexts[t.snap.Scope]; ok && now.Before(context.expires) {
			c.token, c.version = context.token, context.version
		} else if !t.requiresContext && owner != "" && t.snap.Scope == ownerScope {
			c.token, c.owner = owner, true
		} else {
			if !t.requiresContext && ownerScope != "" && t.snap.Scope != ownerScope {
				delete(w.tracked, id)
				w.dropped++
			}
			continue
		}
		old, exists := byScope[t.snap.Scope]
		if !exists || t.snap.Priority > old.job.snap.Priority || (t.snap.Priority == old.job.snap.Priority && t.last.Before(old.job.last)) {
			byScope[t.snap.Scope] = c
		}
	}
	if len(byScope) == 0 {
		return candidate{}, false
	}
	choices := make([]candidate, 0, len(byScope))
	for _, c := range byScope {
		choices = append(choices, c)
	}
	sort.Slice(choices, func(i, j int) bool {
		if choices[i].served != choices[j].served {
			return choices[i].served < choices[j].served
		}
		if choices[i].job.snap.Priority != choices[j].job.snap.Priority {
			return choices[i].job.snap.Priority > choices[j].job.snap.Priority
		}
		return choices[i].job.sequence < choices[j].job.sequence
	})
	return choices[0], true
}

func (w *Warmer) refreshPass(ctx context.Context) {
	if w == nil || w.store == nil {
		return
	}
	w.mu.Lock()
	if w.running || w.foreground > 0 || w.paused || !w.config.Enabled {
		w.mu.Unlock()
		return
	}
	w.running = true
	cfg := w.config
	w.mu.Unlock()
	defer func() { w.mu.Lock(); w.running = false; w.activeCancel = nil; w.activeScope = ""; w.mu.Unlock() }()
	owner, ownerOK := w.owner(ctx)
	ownerScope := ""
	if ownerOK {
		ownerScope = w.ownerScope(ctx)
	}
	w.mu.Lock()
	w.warming = ownerOK && ownerScope != ""
	w.expireContextsLocked(w.now())
	w.mu.Unlock()
	perScope := map[string]int{}
	var used int64
	for requests := 0; ; {
		w.mu.Lock()
		cfg = w.config
		w.mu.Unlock()
		if requests >= cfg.RequestsPerPass || ctx.Err() != nil || cfg.BytesPerPass-used < int64(cache.MaxEntryBytes)+1 {
			return
		}
		w.mu.Lock()
		w.expireContextsLocked(w.now())
		if w.foreground > 0 || w.paused || !w.config.Enabled {
			w.mu.Unlock()
			return
		}
		c, ok := w.nextCandidateLocked(w.now(), owner, ownerScope, perScope)
		w.mu.Unlock()
		if !ok {
			return
		}
		if w.KeyFunc != nil && w.KeyFunc(c.job.snap) != c.job.key {
			w.mu.Lock()
			delete(w.tracked, c.id)
			w.dropped++
			w.mu.Unlock()
			continue
		}
		if w.CanWarm != nil && !w.CanWarm(ctx, c.job.snap) {
			return
		}
		w.mu.Lock()
		if w.foreground > 0 || w.paused || !w.config.Enabled {
			w.mu.Unlock()
			return
		}
		jobCtx, cancel := context.WithCancel(ctx)
		w.activeCancel = cancel
		w.activeScope = c.job.snap.Scope
		w.servedSequence++
		w.served[c.job.snap.Scope] = w.servedSequence
		w.mu.Unlock()
		permit := func() bool {
			w.mu.Lock()
			defer w.mu.Unlock()
			current, ok := w.tracked[c.id]
			if !ok || current.sequence != c.job.sequence || w.foreground > 0 || w.paused || !w.config.Enabled {
				return false
			}
			if c.owner {
				return true
			}
			credential, ok := w.contexts[c.job.snap.Scope]
			return ok && credential.version == c.version && w.now().Before(credential.expires)
		}
		n, err := w.refreshChecked(jobCtx, c.job.key, c.job.snap, c.token, permit)
		cancel()
		requests++
		perScope[c.job.snap.Scope]++
		used += n
		w.mu.Lock()
		w.activeCancel = nil
		w.activeScope = ""
		if err != nil {
			if status, ok := err.(refreshError); ok && (status.status == http.StatusUnauthorized || status.status == http.StatusForbidden) {
				delete(w.contexts, c.job.snap.Scope)
				for id, t := range w.tracked {
					if t.snap.Scope == c.job.snap.Scope {
						delete(w.tracked, id)
						w.dropped++
					}
				}
			}
			if current, exists := w.tracked[c.id]; exists && current.sequence == c.job.sequence {
				current.failures++
				delay := 15 * time.Second << min(current.failures-1, 4)
				current.next = w.now().Add(delay)
				w.tracked[c.id] = current
				w.backoffs++
			}
			if !errors.Is(err, context.Canceled) {
				w.errors++
				if w.metrics != nil {
					w.metrics.IncCacheWarmError()
				}
			}
		} else {
			if current, exists := w.tracked[c.id]; exists && current.sequence == c.job.sequence {
				current.last = w.now()
				current.next = time.Time{}
				current.failures = 0
				w.tracked[c.id] = current
			}
			w.refreshed++
			w.warmedBytes += n
			if w.metrics != nil {
				w.metrics.IncCacheWarmed()
			}
		}
		w.mu.Unlock()
	}
}

func (w *Warmer) stats() Stats {
	if w == nil {
		return Stats{}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.now()
	w.expireContextsLocked(now)
	st := Stats{Tracked: len(w.tracked), Queued: len(w.tracked), Refreshed: w.refreshed, Errors: w.errors, OwnerWarming: w.warming, Enabled: w.config.Enabled, Paused: w.paused, ActiveContexts: len(w.contexts), ByPriority: map[string]int{}, WarmedBytes: w.warmedBytes, Backoffs: w.backoffs, Dropped: w.dropped, RequestsPerPass: w.config.RequestsPerPass, BytesPerPass: w.config.BytesPerPass}
	cold := map[string]bool{}
	for _, t := range w.tracked {
		st.ByPriority[t.snap.Priority.String()]++
		if now.Sub(t.last) >= t.snap.TTL/2 && !now.Before(t.next) {
			st.Due++
		}
		if t.requiresContext {
			if _, ok := w.contexts[t.snap.Scope]; !ok {
				cold[t.snap.Scope] = true
			}
		}
	}
	st.ColdScopes = len(cold)
	return st
}
