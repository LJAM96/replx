package warmer

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/LJAM96/replx/internal/artwork"
	"github.com/LJAM96/replx/internal/cache"
	"github.com/LJAM96/replx/internal/logging"
)

const (
	maxPreloadSections = 8
	maxPreloadArtwork  = 96
	preloadAccept      = "application/json, text/plain, */*"
)

// PreloadResult counts work done by one bounded owner-only pass.
type PreloadResult struct {
	Pages   int
	Artwork int
	Errors  int
	Ready   bool
}

// RunPreload starts immediately and revisits the synced index periodically.
// It never holds readiness hostage if Plex or the index is unavailable.
func (w *Warmer) RunPreload(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	for {
		w.PreloadOnce(ctx)
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// PreloadOnce fetches a small set of owner pages and poster transforms
// directly from Plex. All entries use the canonical owner scope; no owner
// response is written into another user's cache namespace.
func (w *Warmer) PreloadOnce(ctx context.Context) PreloadResult {
	var result PreloadResult
	if w == nil || w.store == nil || w.KeyFunc == nil {
		return result
	}
	owner, ok := w.owner(ctx)
	if !ok || owner == "" {
		return result
	}
	scope := w.ownerScope(ctx)
	if scope == "" {
		return result
	}
	result.Ready = true
	paths := []string{"/library/sections", "/hubs/promoted", "/hubs/home/recentlyAdded", "/hubs/continueWatching"}
	if w.PreloadSections != nil {
		sections, err := w.PreloadSections(ctx)
		if err != nil {
			result.Errors++
		} else {
			for i, section := range sections {
				if i >= maxPreloadSections {
					break
				}
				if safeSectionID(section) {
					paths = append(paths, "/hubs/sections/"+section)
					paths = append(paths, "/library/sections/"+section+"/collections")
				}
			}
		}
	}
	for _, path := range paths {
		ttl, ok := cache.Cacheable(http.MethodGet, path)
		if !ok {
			continue
		}
		queries := []string{""}
		if strings.HasPrefix(path, "/hubs/sections/") || strings.Contains(strings.ToLower(path), "continuewatching") {
			if profile := stripSecrets(w.PreloadHubQuery); profile != "" && len(profile) <= 4096 {
				queries = append(queries, profile)
			}
		}
		for _, query := range queries {
			s := Snapshot{Method: http.MethodGet, Path: path, RawQuery: query,
				Accept: preloadAccept, Scope: scope, Class: cache.ClassOf(path), TTL: ttl}
			key := w.KeyFunc(s)
			if key == "" {
				result.Errors++
				continue
			}
			if _, hit, err := w.store.Get(ctx, key); err == nil && hit {
				w.Track(key, s)
				continue
			}
			requestCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
			err := w.refresh(requestCtx, key, s, owner)
			cancel()
			if err != nil {
				result.Errors++
				continue
			}
			w.Track(key, s)
			result.Pages++
		}
	}
	if w.Artwork != nil && w.PreloadArtworkPaths != nil {
		thumbs, err := w.PreloadArtworkPaths(ctx)
		if err != nil {
			result.Errors++
		} else {
			for i, thumb := range thumbs {
				if i >= maxPreloadArtwork || ctx.Err() != nil {
					break
				}
				stored, err := w.preloadArtwork(ctx, scope, owner, thumb)
				if err != nil {
					result.Errors++
				} else if stored {
					result.Artwork++
				}
			}
		}
	}
	if w.log != nil {
		w.log.Log(logging.Entry{Level: "info", Component: "cache",
			Fields: map[string]any{"event": "owner_preload", "pages": result.Pages,
				"artwork": result.Artwork, "errors": result.Errors}})
	}
	w.mu.Lock()
	w.preloadPages += int64(result.Pages)
	w.preloadedArtwork += int64(result.Artwork)
	w.preloadErrors += int64(result.Errors)
	w.lastPreloadUnix = w.now().Unix()
	w.mu.Unlock()
	return result
}

func safeSectionID(id string) bool {
	if id == "" || len(id) > 12 {
		return false
	}
	for _, c := range id {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func (w *Warmer) preloadArtwork(ctx context.Context, scope, token, thumb string) (bool, error) {
	u, err := url.Parse(thumb)
	if err != nil || u.IsAbs() || u.Host != "" || !strings.HasPrefix(u.Path, "/library/metadata/") {
		return false, nil
	}
	inner := u.Query()
	for k := range inner {
		switch strings.ToLower(k) {
		case "x-plex-token", "token", "authtoken":
			inner.Del(k)
		}
	}
	inner.Set("X-Plex-Token", token)
	u.RawQuery = inner.Encode()
	q := url.Values{"width": {"480"}, "height": {"720"}, "minSize": {"1"},
		"upscale": {"1"}, "url": {u.String()}}
	path := "/photo/:/transcode"
	key := artwork.Key(scope, path, q)
	if _, _, ok := w.Artwork.Get(key); ok {
		return false, nil
	}
	requestCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, w.origin+path+"?"+q.Encode(), nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("X-Plex-Token", token)
	resp, err := w.client.Do(req) //nolint:gosec // admin-configured origin only
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "image/") {
		return false, errStatus(resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, artwork.MaxBodyBytes+1))
	if err != nil {
		return false, err
	}
	if len(body) == 0 || len(body) > artwork.MaxBodyBytes {
		return false, errTooLarge()
	}
	if err := w.Artwork.Set(key, resp.Header.Get("Content-Type"), body); err != nil {
		return false, err
	}
	return true, nil
}
