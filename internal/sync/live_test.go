package sync

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LJAM96/replx/internal/database"
	"github.com/LJAM96/replx/internal/logging"
	"github.com/LJAM96/replx/internal/metrics"
	"github.com/LJAM96/replx/internal/testdb"
	"github.com/jackc/pgx/v5"
)

// liveDB returns a rolled-back transaction: full isolation from sibling
// packages sharing the CI database.
func liveDB(t *testing.T) (context.Context, pgx.Tx) {
	t.Helper()
	return testdb.Begin(t)
}

func seedServer(t *testing.T, db database.DBTX) {
	t.Helper()
	ctx := context.Background()
	_, _ = db.Exec(ctx, `DELETE FROM plex_servers WHERE machine_identifier='test-sync-box'`)
	var serverID string
	err := db.QueryRow(ctx, `INSERT INTO plex_servers(name, internal_origin_url, machine_identifier, enabled)
		VALUES('Test Box','http://test.invalid:32400','test-sync-box',true) RETURNING id`).Scan(&serverID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(ctx, `INSERT INTO plex_owner_credentials(server_id, replx_edge_client_identifier, jwk_public, jwk_private_ciphertext, status)
		VALUES($1,'test-client','{}','\x00','verified')
		ON CONFLICT (server_id) DO UPDATE SET status='verified'`, serverID)
	if err != nil {
		t.Fatal(err)
	}
	// Rollback at cleanup erases everything above: no residue for
	// sibling packages, no explicit DELETEs needed.
}

// fakePMS serves sections plus a mutable paginated item list.
type fakePMS struct {
	mu        atomic.Int64 // counts /all hits
	items     []string     // item JSON fragments
	sections  string
	omitTotal bool // legacy builds without totalSize
}

func (f *fakePMS) handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/library/sections":
		_, _ = fmt.Fprint(w, f.sections)
	case strings.HasSuffix(r.URL.Path, "/all"):
		f.mu.Add(1)
		q := r.URL.Query()
		start, size := atoi(q.Get("X-Plex-Container-Start")), atoi(q.Get("X-Plex-Container-Size"))
		if size <= 0 {
			size = 100
		}
		end := start + size
		if end > len(f.items) {
			end = len(f.items)
		}
		page := "[]"
		if start < len(f.items) {
			page = "[" + strings.Join(f.items[start:end], ",") + "]"
		}
		// Plex semantics: size counts this response, totalSize the
		// collection; offset echoes the request start. Legacy mode
		// omits totalSize entirely (short-page termination only).
		if f.omitTotal {
			_, _ = fmt.Fprintf(w, `{"MediaContainer":{"size":%d,"offset":%d,"Metadata":%s}}`,
				end-start, start, page)
		} else {
			_, _ = fmt.Fprintf(w, `{"MediaContainer":{"size":%d,"totalSize":%d,"offset":%d,"Metadata":%s}}`,
				end-start, len(f.items), start, page)
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}

const liveItemA = `{"ratingKey":"2001","key":"/library/metadata/2001","type":"movie","title":"Sync Film A","titleSort":"Sync Film A","year":2023,"duration":6000000,"thumb":"/t/a","addedAt":1700000000,"updatedAt":1700000100,"Guid":[{"id":"imdb://tt0000001"}],"Media":[{"id":21,"container":"mkv","videoCodec":"h264","width":1920,"height":1080,"bitrate":12000,"videoResolution":"1080","dynamicRange":"SDR","audioCodec":"aac","audioChannels":2,"Part":[{"id":201,"key":"/library/parts/201/file.mkv","container":"mkv","size":1000,"Stream":[{"id":2001,"streamType":1,"codec":"h264"},{"id":2002,"streamType":2,"codec":"aac","channels":2}]}]}]}`
const liveItemB = `{"ratingKey":"2002","key":"/library/metadata/2002","type":"movie","title":"Sync Film B","Guid":[],"Media":[{"id":22,"container":"mp4","videoCodec":"hevc","videoProfile":"main 10","width":3840,"height":2160,"bitrate":50000,"videoResolution":"2160","dynamicRange":"HDR","Part":[{"id":202,"key":"/library/parts/202/file.mp4"}]}]}`

func TestLiveFullSyncAndSweep(t *testing.T) {
	ctx, db := liveDB(t)
	seedServer(t, db)

	fx := &fakePMS{
		sections: `{"MediaContainer":{"Directory":[{"key":"22","type":"movie","title":"Movies","agent":"tv.plex.agents.movie","uuid":"sec-uuid-22","updatedAt":1700000000}]}}`,
		items:    []string{liveItemA, liveItemB},
	}
	origin := httptest.NewServer(http.HandlerFunc(fx.handler))
	defer origin.Close()

	var reg metrics.Registry
	w := New(db, origin.URL, func(ctx context.Context) (string, bool) { return "owner-test-token", true },
		logging.New(io.Discard), &reg)
	w.PageSize = 1 // force pagination + cursor resume across pages

	if err := w.SyncOnce(ctx, true); err != nil {
		t.Fatal(err)
	}
	count := func(q string, args ...any) int {
		var n int
		if err := db.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(`SELECT count(*) FROM libraries`); n != 1 {
		t.Fatalf("libraries=%d", n)
	}
	if n := count(`SELECT count(*) FROM library_items`); n != 2 {
		t.Fatalf("items=%d", n)
	}
	if n := count(`SELECT count(*) FROM media_variants`); n != 2 {
		t.Fatalf("variants=%d", n)
	}
	if n := count(`SELECT count(*) FROM media_parts`); n != 2 {
		t.Fatalf("parts=%d", n)
	}
	if n := count(`SELECT count(*) FROM media_streams`); n != 2 {
		t.Fatalf("streams=%d", n)
	}
	if n := count(`SELECT count(*) FROM item_guids`); n != 1 {
		t.Fatalf("guids=%d", n)
	}
	var dr string
	if err := db.QueryRow(ctx, `SELECT normalized_dynamic_range FROM media_variants WHERE plex_media_id='22'`).Scan(&dr); err != nil || dr != DRHDROther {
		t.Fatalf("bare HDR must stay HDR_OTHER, never assumed HDR10: %q %v", dr, err)
	}
	// Cursor completed and resume works: light pass must not refetch items.
	allBefore := fx.mu.Load()
	if err := w.SyncOnce(ctx, false); err != nil {
		t.Fatal(err)
	}
	if fx.mu.Load() != allBefore {
		t.Fatal("light pass must skip synced sections without refetch")
	}
	// Full sweep deletes the removed item and its cascade.
	fx.items = []string{liveItemA}
	if err := w.SyncOnce(ctx, true); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT count(*) FROM library_items`); n != 1 {
		t.Fatalf("after sweep items=%d", n)
	}
	if n := count(`SELECT count(*) FROM media_variants`); n != 1 {
		t.Fatalf("after sweep variants=%d", n)
	}
	if s := reg.Snapshot(); s.SyncItems < 3 {
		t.Fatalf("sync items counter: %+v", s)
	}
}

func TestLiveEventStreamDirtiesSection(t *testing.T) {
	sse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintln(w, `data: {"NotificationContainer":{"type":"activity","Activity":{"type":"library.refresh.items","subtype":"library.refresh.items","progress":100,"Context":{"key":"/library/sections/22/refresh"}}}}`)
		_, _ = fmt.Fprintln(w)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer sse.Close()
	w := &Worker{DB: nil, Origin: sse.URL,
		OwnerToken: func(ctx context.Context) (string, bool) { return "t", true },
		dirty:      map[string]bool{}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = w.subscribeOnce(ctx)
	if !w.isDirty("22") {
		t.Fatal("refresh-complete event must dirty section 22")
	}
}

func TestLiveCrashResumeAndHistory(t *testing.T) {
	ctx, db := liveDB(t)
	seedServer(t, db)

	fx := &fakePMS{
		sections: `{"MediaContainer":{"Directory":[{"key":"22","type":"movie","title":"Movies","updatedAt":1700000000}]}}`,
		items:    []string{liveItemA, liveItemB},
	}
	origin := httptest.NewServer(http.HandlerFunc(fx.handler))
	defer origin.Close()

	var reg metrics.Registry
	newWorker := func() *Worker {
		w := New(db, origin.URL, func(ctx context.Context) (string, bool) { return "owner-test-token", true },
			logging.New(io.Discard), &reg)
		w.PageSize = 1
		return w
	}
	// Simulate a crashed full sweep: stale cursor mid-section, old gen.
	var libraryID string
	if err := db.QueryRow(ctx, `INSERT INTO libraries(server_id, plex_section_id, title, media_type)
		VALUES((SELECT id FROM plex_servers WHERE machine_identifier='test-sync-box'),'22','Movies','movie')
		ON CONFLICT (server_id, plex_section_id) DO UPDATE SET title='Movies' RETURNING id`).Scan(&libraryID); err != nil {
		t.Fatal(err)
	}
	var serverID string
	if err := db.QueryRow(ctx, `SELECT id FROM plex_servers WHERE machine_identifier='test-sync-box'`).Scan(&serverID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO sync_cursors(server_id, sync_type, library_id, cursor, status)
		VALUES($1,'section',$2,'{"start":1,"generation":111}','running')
		ON CONFLICT (server_id, sync_type, library_id) DO UPDATE SET cursor='{"start":1,"generation":111}', status='running'`,
		serverID, libraryID); err != nil {
		t.Fatal(err)
	}
	before := fx.mu.Load()
	if err := newWorker().SyncOnce(ctx, true); err != nil {
		t.Fatal(err)
	}
	// Fresh generation restarts at zero: both pages fetched despite the
	// stale start=1 cursor.
	if got := fx.mu.Load() - before; got != 2 {
		t.Fatalf("crashed sweep must restart at zero: page fetches=%d", got)
	}
	var stamped int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM library_items WHERE library_id=$1 AND sweep_gen IS NOT NULL`, libraryID).Scan(&stamped); err != nil || stamped != 2 {
		t.Fatalf("generation stamps: %d %v", stamped, err)
	}
	// History preservation: attach a session to the 4K variant, drop the
	// variant from the origin, re-sweep. The session survives with the
	// reference nulled instead of violating.
	var variantID string
	if err := db.QueryRow(ctx, `SELECT v.id FROM media_variants v JOIN library_items li ON li.id=v.library_item_id
		WHERE li.rating_key='2002' AND v.media_index=0`).Scan(&variantID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO playback_sessions(server_id, plex_session_identifier, rating_key, selected_media_variant_id)
		VALUES($1,'hist-sess','2002',$2)`, serverID, variantID); err != nil {
		t.Fatal(err)
	}
	fx.items = []string{liveItemA} // 2002 gone from origin
	if err := newWorker().SyncOnce(ctx, true); err != nil {
		t.Fatal(err)
	}
	var kept int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM playback_sessions WHERE plex_session_identifier='hist-sess'`).Scan(&kept); err != nil || kept != 1 {
		t.Fatalf("history session must survive eviction: %d %v", kept, err)
	}
	var nulled *string
	if err := db.QueryRow(ctx, `SELECT selected_media_variant_id::text FROM playback_sessions WHERE plex_session_identifier='hist-sess'`).Scan(&nulled); err != nil || nulled != nil {
		t.Fatalf("evicted reference must null, not dangle: %+v %v", nulled, err)
	}
	var items int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM library_items WHERE library_id=$1`, libraryID).Scan(&items); err != nil || items != 1 {
		t.Fatalf("sweep must delete absent item: %d %v", items, err)
	}
}

func TestLiveNoTotalSizePaginatesFully(t *testing.T) {
	ctx, db := liveDB(t)
	seedServer(t, db)
	// 250 items, page size 100, no totalSize anywhere: the sweep must
	// still index all 250 via short-page termination (100+100+50).
	items := make([]string, 0, 250)
	for i := 0; i < 250; i++ {
		key := fmt.Sprint(9000 + i)
		items = append(items, `{"ratingKey":"`+key+`","type":"movie","title":"Bulk Film `+key+`"}`)
	}
	fx := &fakePMS{
		sections:  `{"MediaContainer":{"Directory":[{"key":"22","type":"movie","title":"Movies","updatedAt":1700000000}]}}`,
		items:     items,
		omitTotal: true,
	}
	origin := httptest.NewServer(http.HandlerFunc(fx.handler))
	defer origin.Close()
	var reg metrics.Registry
	w := New(db, origin.URL, func(ctx context.Context) (string, bool) { return "owner-test-token", true },
		logging.New(io.Discard), &reg)
	w.PageSize = 100
	if err := w.SyncOnce(ctx, true); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM library_items`).Scan(&n); err != nil || n != 250 {
		t.Fatalf("all 250 must index without totalSize: %d %v", n, err)
	}
}

func TestLiveDirtyLightRefreshRevisits(t *testing.T) {
	ctx, db := liveDB(t)
	seedServer(t, db)
	title := "Original Title"
	fx := &fakePMS{
		sections: `{"MediaContainer":{"Directory":[{"key":"22","type":"movie","title":"Movies","updatedAt":1700000000}]}}`,
	}
	fx.items = []string{`{"ratingKey":"3001","type":"movie","title":"` + title + `"}`}
	origin := httptest.NewServer(http.HandlerFunc(fx.handler))
	defer origin.Close()
	var reg metrics.Registry
	w := New(db, origin.URL, func(ctx context.Context) (string, bool) { return "owner-test-token", true },
		logging.New(io.Discard), &reg)
	w.PageSize = 100
	if err := w.SyncOnce(ctx, true); err != nil {
		t.Fatal(err)
	}
	// Cursor now points at the end with status complete. Change the
	// origin, flag dirty, run a light pass: the refresh must restart at
	// zero and pick up the change, not resume at the end into nothing.
	fx.items = []string{`{"ratingKey":"3001","type":"movie","title":"Changed Title"}`}
	w.MarkDirty("22")
	if err := w.SyncOnce(ctx, false); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := db.QueryRow(ctx, `SELECT title FROM library_items WHERE rating_key='3001'`).Scan(&got); err != nil || got != "Changed Title" {
		t.Fatalf("light refresh must revisit changed records: %q %v", got, err)
	}
}

func TestLiveDeletedLibraryReconciled(t *testing.T) {
	ctx, db := liveDB(t)
	seedServer(t, db)

	sections := `{"MediaContainer":{"Directory":[{"key":"22","type":"movie","title":"Movies"},{"key":"99","type":"movie","title":"Retired"}]}}`
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/library/sections":
			_, _ = fmt.Fprint(w, sections)
		case strings.HasSuffix(r.URL.Path, "/all"):
			_, _ = fmt.Fprint(w, `{"MediaContainer":{"size":1,"totalSize":1,"offset":0,"Metadata":[ `+liveItemA+` ]}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer origin.Close()

	var reg metrics.Registry
	w := New(db, origin.URL, func(ctx context.Context) (string, bool) { return "owner-test-token", true },
		logging.New(io.Discard), &reg)
	count := func(q string, args ...any) int {
		var n int
		if err := db.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if err := w.SyncOnce(ctx, true); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT count(*) FROM libraries`); n != 2 {
		t.Fatalf("libraries=%d", n)
	}
	// Origin removes section 99: the next full sweep reconciles it away
	// (items, variants, parts and cursors cascade; playback history is
	// preserved by SET NULL references).
	sections = `{"MediaContainer":{"Directory":[{"key":"22","type":"movie","title":"Movies"}]}}`
	if err := w.SyncOnce(ctx, true); err != nil {
		t.Fatal(err)
	}
	if n := count(`SELECT count(*) FROM libraries`); n != 1 {
		t.Fatalf("retired library must reconcile away, libraries=%d", n)
	}
	if n := count(`SELECT count(*) FROM sync_cursors WHERE library_id NOT IN (SELECT id FROM libraries)`); n != 0 {
		t.Fatalf("orphaned cursors: %d", n)
	}
}
