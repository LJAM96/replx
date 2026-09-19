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
	mu       atomic.Int64 // counts /all hits
	items    []string     // item JSON fragments
	sections string
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
		_, _ = fmt.Fprintf(w, `{"MediaContainer":{"size":%d,"Metadata":%s}}`, len(f.items), page)
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
	if err := db.QueryRow(ctx, `SELECT normalized_dynamic_range FROM media_variants WHERE plex_media_id='22'`).Scan(&dr); err != nil || dr != DRHDR10 {
		t.Fatalf("4K variant DR: %q %v", dr, err)
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
