package warmer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/LJAM96/replx/internal/cache"
	"github.com/LJAM96/replx/internal/logging"
)

const maxUserSectionsPerPass = 2

type sectionHubProfile struct {
	Query  string `json:"query"`
	Accept string `json:"accept"`
}

func sectionHubPath(path string) bool {
	return strings.HasPrefix(path, "/hubs/sections/") &&
		safeSectionID(strings.TrimPrefix(path, "/hubs/sections/"))
}

func sectionHubProfileKey(scope string) string { return "replx_edge:section-hub-profile:" + scope }

func (w *Warmer) saveSectionHubProfile(s Snapshot) {
	if w.store == nil || s.Scope == "" || len(s.RawQuery) > 4096 || len(s.Accept) > 256 {
		return
	}
	q, err := url.ParseQuery(s.RawQuery)
	if err != nil || !safeSectionQuery(q) {
		return
	}
	body, err := json.Marshal(sectionHubProfile{Query: q.Encode(), Accept: s.Accept})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = w.store.Set(ctx, sectionHubProfileKey(s.Scope), cache.Entry{Status: http.StatusOK, Body: body}, homeProfileTTL)
}

func (w *Warmer) loadSectionHubProfile(ctx context.Context, scope string) (sectionHubProfile, bool) {
	entry, hit, err := w.store.Get(ctx, sectionHubProfileKey(scope))
	if err == nil && hit {
		var profile sectionHubProfile
		if json.Unmarshal(entry.Body, &profile) == nil && len(profile.Query) <= 4096 && len(profile.Accept) <= 256 {
			if q, err := url.ParseQuery(profile.Query); err == nil && safeSectionQuery(q) {
				return profile, true
			}
		}
	}
	// Until a user browses a section, derive the current Plex Web section
	// request from the configured browser hub profile. The user's own token
	// and cache scope are still used for every origin request.
	q, err := url.ParseQuery(stripSecrets(w.PreloadHubQuery))
	if err != nil || len(q) == 0 {
		return sectionHubProfile{}, false
	}
	for _, key := range []string{"contentDirectoryID", "pinnedContentDirectoryID", "excludeContinueWatching"} {
		q.Del(key)
	}
	q.Set("includeExternalMetadata", "1")
	if q.Get("count") == "" {
		q.Set("count", "12")
	}
	if !safeSectionQuery(q) || len(q.Encode()) > 4096 {
		return sectionHubProfile{}, false
	}
	return sectionHubProfile{Query: q.Encode(), Accept: preloadAccept}, true
}

func safeSectionQuery(q url.Values) bool {
	count, err := strconv.Atoi(q.Get("count"))
	if err != nil || count < 1 || count > 50 {
		return false
	}
	for k := range q {
		lower := strings.ToLower(k)
		if strings.Contains(lower, "token") || strings.Contains(lower, "auth") ||
			strings.Contains(lower, "password") || strings.Contains(lower, "secret") {
			return false
		}
	}
	return true
}

// RunUserSections gradually prepares library hubs for recently active users.
func (w *Warmer) RunUserSections(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 20 * time.Second
	}
	for {
		w.PreloadUserSectionsOnce(ctx)
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// PreloadUserSectionsOnce fills at most two missing section hubs for one
// user's validated credential. A stale copy is enough to make a later cold
// browser visit immediate while the normal proxy refreshes it in background.
func (w *Warmer) PreloadUserSectionsOnce(ctx context.Context) (pages, failures int) {
	if w == nil || w.store == nil || w.KeyFunc == nil || w.PreloadSections == nil || w.DB == nil {
		return 0, 0
	}
	defer func() {
		w.mu.Lock()
		w.userSectionPages += int64(pages)
		w.userSectionErrors += int64(failures)
		if pages > 0 {
			w.lastSectionUnix = w.now().Unix()
		}
		w.mu.Unlock()
		if w.log != nil && pages+failures > 0 {
			w.log.Log(logging.Entry{Level: "info", Component: "cache",
				Fields: map[string]any{"event": "user_section_hubs", "pages": pages, "errors": failures}})
		}
	}()
	scopes := w.windowScopes(ctx)
	if len(scopes) == 0 {
		return 0, 0
	}
	w.mu.Lock()
	scope := scopes[w.sectionScopeCursor%len(scopes)]
	w.sectionScopeCursor++
	w.mu.Unlock()
	profile, ok := w.loadSectionHubProfile(ctx, scope)
	if !ok {
		return 0, 0
	}
	sections, err := w.PreloadSections(ctx)
	if err != nil {
		return 0, 1
	}
	var valid []string
	for _, id := range sections {
		if safeSectionID(id) && len(valid) < maxPreloadSections {
			valid = append(valid, id)
		}
	}
	if len(valid) == 0 {
		return 0, 0
	}
	var token string
	if scope == w.ownerScope(ctx) {
		token, _ = w.owner(ctx)
	} else {
		token = w.userToken(ctx, scope)
	}
	if token == "" {
		return 0, 0
	}
	w.mu.Lock()
	cursor := w.sectionCursors[scope] % len(valid)
	w.sectionCursors[scope] = (cursor + maxUserSectionsPerPass) % len(valid)
	w.mu.Unlock()
	for i := 0; i < maxUserSectionsPerPass && i < len(valid) && ctx.Err() == nil; i++ {
		path := "/hubs/sections/" + valid[(cursor+i)%len(valid)]
		ttl, _ := cache.Cacheable(http.MethodGet, path)
		s := Snapshot{Method: http.MethodGet, Path: path, RawQuery: profile.Query,
			Accept: profile.Accept, Scope: scope, Class: cache.ClassOf(path), TTL: ttl}
		key := w.KeyFunc(s)
		if key == "" {
			failures++
			continue
		}
		if _, hit, err := w.store.Get(ctx, cache.StaleKey(key)); err == nil && hit {
			continue
		}
		requestCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		err := w.refresh(requestCtx, key, s, token)
		cancel()
		if err != nil {
			failures++
		} else {
			pages++
		}
	}
	return pages, failures
}
