package warmer

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/LJAM96/replx/internal/cache"
	"github.com/LJAM96/replx/internal/logging"
)

const maxWindowScopes = 8
const maxWindowsPerPass = 4

// RunCollectionWindows gradually fills whole collection windows for recently
// active users. It stays separate from the owner preload so a slow PMS can
// never hold process readiness or other cache maintenance hostage.
func (w *Warmer) RunCollectionWindows(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	for {
		w.PreloadCollectionWindowsOnce(ctx)
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// PreloadCollectionWindowsOnce fetches at most four full JSON windows for
// one user. Every origin response is obtained with that user's credential;
// the cache key includes the same user's scope and the full non-page query.
func (w *Warmer) PreloadCollectionWindowsOnce(ctx context.Context) (pages, failures int) {
	if w == nil || w.store == nil || w.KeyFunc == nil || w.PreloadSections == nil || w.DB == nil {
		return 0, 0
	}
	paths := w.ownerCollectionPaths(ctx)
	if len(paths) == 0 {
		return 0, 0
	}
	profiles := w.collectionWindowProfiles()
	if len(profiles) == 0 {
		return 0, 0
	}
	scopes := w.windowScopes(ctx)
	if len(scopes) == 0 {
		return 0, 0
	}
	w.mu.Lock()
	scope := scopes[w.windowScopeCursor%len(scopes)]
	w.windowScopeCursor++
	cursor := w.windowCursors[scope]
	w.mu.Unlock()
	token := ""
	if scope == w.ownerScope(ctx) {
		token, _ = w.owner(ctx)
	} else {
		token = w.userToken(ctx, scope)
	}
	if token == "" {
		return 0, 0
	}
	workSize := len(paths) * len(profiles)
	for i := 0; i < maxWindowsPerPass && i < workSize && ctx.Err() == nil; i++ {
		idx := (cursor + i) % workSize
		path := paths[idx/len(profiles)]
		query := profiles[idx%len(profiles)]
		snap := Snapshot{Method: http.MethodGet, Path: path, RawQuery: query,
			Accept: preloadAccept, Scope: scope, Class: cache.ClassOf(path), TTL: 30 * time.Minute}
		key := w.KeyFunc(snap)
		if key == "" {
			failures++
			continue
		}
		windowSnap := snap
		q, _ := url.ParseQuery(query)
		q.Del("X-Plex-Container-Start")
		q.Del("X-Plex-Container-Size")
		windowSnap.RawQuery = q.Encode()
		windowKey := w.KeyFunc(windowSnap) + ":window"
		if _, hit, err := w.store.Get(ctx, windowKey); err == nil && hit {
			continue
		}
		requestCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		err := w.refresh(requestCtx, key, snap, token)
		cancel()
		if err != nil {
			failures++
			continue
		}
		if _, hit, _ := w.store.Get(ctx, windowKey); !hit {
			failures++
			continue
		}
		w.Track(key, snap)
		pages++
	}
	w.mu.Lock()
	w.windowCursors[scope] = (cursor + maxWindowsPerPass) % workSize
	w.userWindowPages += int64(pages)
	w.userWindowErrors += int64(failures)
	w.mu.Unlock()
	if w.log != nil && (pages > 0 || failures > 0) {
		w.log.Log(logging.Entry{Level: "info", Component: "cache",
			Fields: map[string]any{"event": "user_collection_windows", "pages": pages, "errors": failures}})
	}
	return pages, failures
}

func (w *Warmer) ownerCollectionPaths(ctx context.Context) []string {
	ownerScope := w.ownerScope(ctx)
	if ownerScope == "" {
		return nil
	}
	sections, err := w.PreloadSections(ctx)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var paths []string
	for _, section := range sections {
		if !safeSectionID(section) {
			continue
		}
		path := "/library/sections/" + section + "/collections"
		snap := Snapshot{Method: http.MethodGet, Path: path, Accept: preloadAccept,
			Scope: ownerScope, Class: cache.ClassOf(path)}
		key := w.KeyFunc(snap)
		entry, hit, err := w.store.Get(ctx, key)
		if err != nil || !hit {
			entry, hit, err = w.store.Get(ctx, cache.StaleKey(key))
		}
		if err != nil || !hit {
			continue
		}
		for _, child := range collectionChildren(entry.Body) {
			if !seen[child] {
				seen[child] = true
				paths = append(paths, child)
			}
		}
	}
	return paths
}

func (w *Warmer) collectionWindowProfiles() []string {
	var profiles []string
	seen := map[string]bool{}
	for _, raw := range strings.Split(w.PreloadCollectionQuery, "||") {
		if len(profiles) >= 4 {
			break
		}
		q, err := url.ParseQuery(stripSecrets(strings.TrimSpace(raw)))
		if err != nil || len(q) == 0 {
			continue
		}
		for k := range q {
			if strings.EqualFold(k, "X-Plex-Container-Start") || strings.EqualFold(k, "X-Plex-Container-Size") {
				q.Del(k)
			}
		}
		q.Set("X-Plex-Container-Start", "0")
		q.Set("X-Plex-Container-Size", "350")
		profile := q.Encode()
		if !seen[profile] {
			seen[profile] = true
			profiles = append(profiles, profile)
		}
	}
	return profiles
}

func (w *Warmer) windowScopes(ctx context.Context) []string {
	var scopes []string
	seen := map[string]bool{}
	if owner, ok := w.owner(ctx); ok && owner != "" {
		if scope := w.ownerScope(ctx); scope != "" {
			scopes = append(scopes, scope)
			seen[scope] = true
		}
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := w.DB.Query(cctx, `SELECT CASE WHEN t.token_status='pms_valid'
		THEN 'tok:' || t.token_fingerprint ELSE 'user:' || t.identity_id::text END
		FROM plex_token_identities t JOIN plex_servers s ON s.id=t.server_id
		WHERE s.enabled AND t.token_ciphertext IS NOT NULL
		AND t.last_seen_at > now() - make_interval(days => 1)
		AND (t.token_status='pms_valid' OR (t.token_status='valid' AND t.identity_id IS NOT NULL))
		ORDER BY t.last_seen_at DESC LIMIT $1`, maxWindowScopes)
	if err != nil {
		return scopes
	}
	defer rows.Close()
	for rows.Next() && len(scopes) < maxWindowScopes {
		var scope string
		if rows.Scan(&scope) == nil && scope != "" && !seen[scope] {
			seen[scope] = true
			scopes = append(scopes, scope)
		}
	}
	return scopes
}
