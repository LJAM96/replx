package warmer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/LJAM96/replx/internal/cache"
)

const homeProfileTTL = 30 * 24 * time.Hour
const maxHomeSectionsPerPass = 2

type homeProfile struct {
	Query  string `json:"query"`
	Accept string `json:"accept"`
}

func homeProfileKey(scope string) string { return "replx_edge:home-profile:" + scope }

// saveHomeProfile remembers only a safe request shape. The credential is
// resolved anew for each refresh and is never part of this Valkey entry.
func (w *Warmer) saveHomeProfile(s Snapshot) {
	if len(s.RawQuery) > 4096 || len(s.Accept) > 256 || s.Scope == "" {
		return
	}
	q, err := url.ParseQuery(s.RawQuery)
	if err != nil {
		return
	}
	// Reject unrecognized token-bearing parameter names, not just the
	// conventional Plex spelling stripped by Track.
	for k := range q {
		lower := strings.ToLower(k)
		if strings.Contains(lower, "token") || strings.Contains(lower, "auth") || strings.Contains(lower, "password") || strings.Contains(lower, "secret") {
			return
		}
	}
	if q.Get("contentDirectoryID") == "" || q.Get("pinnedContentDirectoryID") == "" {
		return
	}
	body, err := json.Marshal(homeProfile{Query: q.Encode(), Accept: s.Accept})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = w.store.Set(ctx, homeProfileKey(s.Scope), cache.Entry{Status: http.StatusOK, Body: body}, homeProfileTTL)
}

func (w *Warmer) loadHomeProfile(ctx context.Context, scope string) (homeProfile, bool) {
	entry, ok, err := w.store.Get(ctx, homeProfileKey(scope))
	if err != nil || !ok {
		return homeProfile{}, false
	}
	var profile homeProfile
	if json.Unmarshal(entry.Body, &profile) != nil || len(profile.Query) > 4096 || len(profile.Accept) > 256 {
		return homeProfile{}, false
	}
	return profile, true
}

// RunUserHome runs apart from collection and owner warming because Plex can
// take tens of seconds to render a promoted Home section.
func (w *Warmer) RunUserHome(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	for {
		w.PreloadUserHomeOnce(ctx)
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

// PreloadUserHomeOnce fills at most two missing section responses for one
// recently active user, using only that user's validated Plex credential.
func (w *Warmer) PreloadUserHomeOnce(ctx context.Context) (pages, failures int) {
	if w == nil || w.store == nil || w.KeyFunc == nil || w.PreloadSections == nil || w.DB == nil {
		return 0, 0
	}
	scopes := w.windowScopes(ctx)
	if len(scopes) == 0 {
		return 0, 0
	}
	w.mu.Lock()
	scope := scopes[w.homeScopeCursor%len(scopes)]
	w.homeScopeCursor++
	w.mu.Unlock()
	profile, ok := w.loadHomeProfile(ctx, scope)
	if !ok {
		return 0, 0
	}
	q, err := url.ParseQuery(profile.Query)
	if err != nil {
		return 0, 0
	}
	sections, err := w.PreloadSections(ctx)
	if err != nil {
		return 0, 1
	}
	allowed := map[string]bool{}
	for _, id := range sections {
		if safeSectionID(id) {
			allowed[id] = true
		}
	}
	token := ""
	if scope == w.ownerScope(ctx) {
		token, _ = w.owner(ctx)
	} else {
		token = w.userToken(ctx, scope)
	}
	if token == "" {
		return 0, 0
	}
	seen := map[string]bool{}
	for _, id := range strings.Split(q.Get("pinnedContentDirectoryID"), ",") {
		id = strings.TrimSpace(id)
		if !allowed[id] || seen[id] || ctx.Err() != nil {
			continue
		}
		seen[id] = true
		sectionQuery := url.Values{}
		for k, values := range q {
			sectionQuery[k] = append([]string(nil), values...)
		}
		sectionQuery.Set("contentDirectoryID", id)
		s := Snapshot{Method: http.MethodGet, Path: "/hubs/promoted", RawQuery: sectionQuery.Encode(),
			Accept: profile.Accept, Scope: scope, Class: cache.ClassOf("/hubs/promoted"), TTL: 10 * time.Second}
		key := w.KeyFunc(s)
		if key == "" {
			failures++
			continue
		}
		if _, hit, err := w.store.Get(ctx, cache.StaleKey(key)); err == nil && hit {
			continue
		}
		requestCtx, cancel := context.WithTimeout(ctx, 55*time.Second)
		err := w.refresh(requestCtx, key, s, token)
		cancel()
		if err != nil {
			failures++
			continue
		}
		w.Track(key, s)
		pages++
		if pages+failures >= maxHomeSectionsPerPass {
			break
		}
	}
	w.mu.Lock()
	w.userHomePages += int64(pages)
	w.userHomeErrors += int64(failures)
	if pages > 0 {
		w.lastHomeUnix = w.now().Unix()
	}
	w.mu.Unlock()
	return pages, failures
}
