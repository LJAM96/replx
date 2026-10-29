package sync

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LJAM96/replx/internal/database"
)

type collectionFixture struct {
	catalog          []string
	members          map[string][]string
	failPath         string
	failOffset       int
	responseOverride func(path string, offset int) string
	t                *testing.T
}

func (f *collectionFixture) handler(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Plex-Token") != "collection-fixture-owner" {
		f.t.Error("collection request missing owner token")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if r.URL.Path == "/library/sections" {
		_, _ = fmt.Fprint(w, `{"MediaContainer":{"Directory":[{"key":"22","type":"movie","title":"Movies"}]}}`)
		return
	}
	offset, size := atoi(r.URL.Query().Get("X-Plex-Container-Start")), atoi(r.URL.Query().Get("X-Plex-Container-Size"))
	if r.URL.Path == f.failPath && offset == f.failOffset {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if f.responseOverride != nil {
		if raw := f.responseOverride(r.URL.Path, offset); raw != "" {
			_, _ = fmt.Fprint(w, raw)
			return
		}
	}
	var entries []string
	switch {
	case r.URL.Path == "/library/sections/22/collections":
		entries = f.catalog
	case strings.HasPrefix(r.URL.Path, "/library/collections/") && strings.HasSuffix(r.URL.Path, "/children"):
		entries = f.members[strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/library/collections/"), "/children")]
	default:
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if size <= 0 {
		size = 100
	}
	end := offset + size
	if end > len(entries) {
		end = len(entries)
	}
	var page []string
	if offset < len(entries) {
		page = entries[offset:end]
	}
	_, _ = fmt.Fprintf(w, `{"MediaContainer":{"offset":%d,"size":%d,"totalSize":%d,"Metadata":[%s]}}`, offset, len(page), len(entries), strings.Join(page, ","))
}

func newCollectionFixture(t *testing.T, db database.DBTX, fx *collectionFixture) *Worker {
	t.Helper()
	fx.t = t
	origin := httptest.NewServer(http.HandlerFunc(fx.handler))
	t.Cleanup(origin.Close)
	w := New(db, origin.URL, func(context.Context) (string, bool) { return "collection-fixture-owner", true }, nil, nil)
	w.PageSize = 1
	return w
}

func publishedCollection(t *testing.T, ctx context.Context, db database.DBTX, key string) (int64, string, []string) {
	t.Helper()
	var gen int64
	var title string
	var members []string
	if err := db.QueryRow(ctx, `SELECT c.published_generation,c.title,COALESCE((SELECT array_agg(m.rating_key ORDER BY m.position) FROM collection_members m WHERE m.collection_id=c.id AND m.generation=c.published_generation),ARRAY[]::text[]) FROM library_collections c WHERE c.rating_key=$1 AND c.published_generation IS NOT NULL`, key).Scan(&gen, &title, &members); err != nil {
		t.Fatal(err)
	}
	return gen, title, members
}

func TestLiveCollectionSnapshotsPublishOrderedGenerations(t *testing.T) {
	ctx, db := liveDB(t)
	seedServer(t, db)
	fx := &collectionFixture{catalog: []string{`{"ratingKey":"800","title":"Featured","thumb":"/collection/old"}`, `{"ratingKey":"801","title":"Smart","smart":1}`}, members: map[string][]string{"800": {`{"ratingKey":"2002","type":"movie","title":"B"}`, `{"ratingKey":"2001","type":"movie","title":"A"}`}, "801": {}}}
	w := newCollectionFixture(t, db, fx)
	invalidations := 0
	w.InvalidateCollections = func(ctx context.Context) error {
		invalidations++
		var n int
		if err := db.QueryRow(ctx, `SELECT count(*) FROM library_collections WHERE published_generation IS NOT NULL`).Scan(&n); err != nil {
			return err
		}
		if n != 2 {
			t.Errorf("invalidation before complete publication: %d", n)
		}
		return nil
	}
	if err := w.SyncCollectionsOnce(ctx); err != nil {
		t.Fatal(err)
	}
	first, _, members := publishedCollection(t, ctx, db, "800")
	if strings.Join(members, ",") != "2002,2001" {
		t.Fatalf("order=%v", members)
	}
	var smart bool
	var count int
	if err := db.QueryRow(ctx, `SELECT c.smart,s.item_count FROM library_collections c JOIN collection_snapshots s ON s.collection_id=c.id AND s.generation=c.published_generation WHERE c.rating_key='801' AND s.completed_at IS NOT NULL`).Scan(&smart, &count); err != nil || !smart || count != 0 {
		t.Fatalf("smart empty snapshot: %v %d %v", smart, count, err)
	}
	if err := w.SyncCollectionsOnce(ctx); err != nil {
		t.Fatal(err)
	}
	unchanged, _, _ := publishedCollection(t, ctx, db, "800")
	if unchanged != first || invalidations != 1 {
		t.Fatalf("unchanged scan replaced snapshot or invalidated: %d %d", unchanged, invalidations)
	}
	fx.members["800"] = []string{fx.members["800"][1], fx.members["800"][0]}
	fx.catalog[0] = `{"ratingKey":"800","title":"Featured Updated","thumb":"/collection/new"}`
	if err := w.SyncCollectionsOnce(ctx); err != nil {
		t.Fatal(err)
	}
	changed, title, members := publishedCollection(t, ctx, db, "800")
	if changed == first || title != "Featured Updated" || strings.Join(members, ",") != "2001,2002" || invalidations != 2 {
		t.Fatalf("rebuild not published: %d %q %v inv=%d", changed, title, members, invalidations)
	}
	if err := db.QueryRow(ctx, `SELECT count(*) FROM collection_snapshots`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("retained obsolete snapshots: %d %v", count, err)
	}
}

func TestLiveCollectionFailuresPreservePublishedLibrary(t *testing.T) {
	ctx, db := liveDB(t)
	seedServer(t, db)
	fx := &collectionFixture{catalog: []string{`{"ratingKey":"800","title":"Original"}`, `{"ratingKey":"801","title":"Second"}`}, members: map[string][]string{"800": {`{"ratingKey":"1","type":"movie"}`}, "801": {`{"ratingKey":"2","type":"movie"}`, `{"ratingKey":"3","type":"movie"}`}}}
	w := newCollectionFixture(t, db, fx)
	invalidations := 0
	w.InvalidateCollections = func(context.Context) error { invalidations++; return nil }
	if err := w.SyncCollectionsOnce(ctx); err != nil {
		t.Fatal(err)
	}
	old, _, _ := publishedCollection(t, ctx, db, "800")
	fx.catalog[0] = `{"ratingKey":"800","title":"Unpublished"}`
	fx.members["800"] = []string{`{"ratingKey":"9","type":"movie"}`}
	check := func() {
		t.Helper()
		gen, title, members := publishedCollection(t, ctx, db, "800")
		if gen != old || title != "Original" || strings.Join(members, ",") != "1" || invalidations != 1 {
			t.Fatalf("failed scan changed published data: %d %q %v inv=%d", gen, title, members, invalidations)
		}
		_, _, _ = publishedCollection(t, ctx, db, "801")
	}
	fx.failPath = "/library/collections/801/children"
	fx.failOffset = 1
	if err := w.SyncCollectionsOnce(ctx); err == nil {
		t.Fatal("expected failed child page")
	}
	check()
	fx.failPath = "/library/sections/22/collections"
	fx.failOffset = 1
	if err := w.SyncCollectionsOnce(ctx); err == nil {
		t.Fatal("expected failed catalog page")
	}
	check()
	fx.failPath = ""
	fx.responseOverride = func(path string, offset int) string {
		if path == "/library/sections/22/collections" {
			return `{"MediaContainer":{"size":0,"Metadata":[]}}`
		}
		return ""
	}
	if err := w.SyncCollectionsOnce(ctx); err == nil {
		t.Fatal("expected ambiguous empty catalog rejection")
	}
	check()
	fx.responseOverride = func(path string, offset int) string {
		if path == "/library/collections/801/children" && offset == 1 {
			return `{"MediaContainer":{"size":1,"offset":1,"totalSize":3,"Metadata":[{"ratingKey":"3","type":"movie"}]}}`
		}
		return ""
	}
	if err := w.SyncCollectionsOnce(ctx); err == nil {
		t.Fatal("expected changing total rejection")
	}
	check()
	fx.responseOverride = nil
	fx.catalog = nil
	if err := w.SyncCollectionsOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM library_collections`).Scan(&n); err != nil || n != 0 || invalidations != 2 {
		t.Fatalf("explicit complete empty catalog not reconciled: %d %d %v", n, invalidations, err)
	}
}

func TestLiveCollectionDeleteRecreateAndInvalidationRetry(t *testing.T) {
	ctx, db := liveDB(t)
	seedServer(t, db)
	fx := &collectionFixture{catalog: []string{`{"ratingKey":"800","title":"Daily"}`}, members: map[string][]string{"800": {`{"ratingKey":"1","type":"movie"}`}, "900": {`{"ratingKey":"2","type":"movie"}`}}}
	w := newCollectionFixture(t, db, fx)
	failInvalidation := false
	invalidations := 0
	w.InvalidateCollections = func(context.Context) error {
		invalidations++
		if failInvalidation {
			return fmt.Errorf("fixture invalidation unavailable")
		}
		return nil
	}
	if err := w.SyncCollectionsOnce(ctx); err != nil {
		t.Fatal(err)
	}
	fx.catalog = []string{`{"ratingKey":"900","title":"Daily"}`}
	failInvalidation = true
	if err := w.SyncCollectionsOnce(ctx); err == nil {
		t.Fatal("expected invalidation failure")
	}
	_, _, members := publishedCollection(t, ctx, db, "900")
	if strings.Join(members, ",") != "2" {
		t.Fatal(members)
	}
	var n int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM library_collections WHERE rating_key='800'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("recreated collection retained old ID: %d %v", n, err)
	}
	failInvalidation = false
	if err := w.SyncCollectionsOnce(ctx); err != nil {
		t.Fatal(err)
	}
	var pending bool
	if err := db.QueryRow(ctx, `SELECT invalidation_pending FROM library_collection_syncs`).Scan(&pending); err != nil || pending || invalidations != 3 {
		t.Fatalf("invalidation retry lost: %v %d %v", pending, invalidations, err)
	}
}
