package playback

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/LJAM96/replx/internal/database"
	"github.com/LJAM96/replx/internal/testdb"
)

// TestLiveIndexVariantsBindsRatingKey covers indexed negotiation against
// real SQL: the rating key must bind to $1 so the owner index actually
// serves candidates instead of silently falling back to live metadata.
func TestLiveIndexVariantsBindsRatingKey(t *testing.T) {
	ctx, db := testdb.Begin(t)
	seedPlaybackIndex(t, ctx, db)

	e := &Engine{DB: db}
	out := e.indexVariants(ctx, "4242")
	if len(out) != 1 {
		t.Fatalf("indexed variants for 4242: got %d", len(out))
	}
	v := out[0]
	if v.MediaIndex != 0 || v.PartPlexID != "11" || v.PartKey != "/library/parts/11/file.mkv" {
		t.Fatalf("indexed variant: %+v", v)
	}
	if v.VariantUUID == "" || v.PartUUID == "" {
		t.Fatalf("indexed variant must carry index UUIDs: %+v", v)
	}
	if got := e.indexVariants(ctx, "no-such-title"); len(got) != 0 {
		t.Fatalf("unknown rating key must yield no candidates: %d", len(got))
	}
}

// TestLiveSessionSelectionRoundTrip proves the negotiated selection
// survives a production store reload, including live-negotiated sessions
// that carry no index UUIDs: the part boundary must substitute the stored
// allowed part for a prohibited request and allow the selected part.
func TestLiveSessionSelectionRoundTrip(t *testing.T) {
	ctx, db := testdb.Begin(t)
	seedPlaybackIndex(t, ctx, db)

	store := &PGStore{DB: db}
	// Live-style session: negotiated without index UUIDs.
	sess, err := store.Create(ctx, Session{
		PlexSessionID: "live-sess", RatingKey: "4242", SelectedMediaIndex: 0,
		SelectedPartPlexID: "11", SelectedPartKey: "/library/parts/11/file.mkv",
		PlaybackMode: "directPlay",
	})
	if err != nil {
		t.Fatal(err)
	}
	if sess.ID == "" {
		t.Fatal("session must persist")
	}
	got, ok, err := store.FindActive(ctx, "live-sess")
	if err != nil || !ok {
		t.Fatalf("session must reload: %v %v", ok, err)
	}
	if got.SelectedMediaIndex != 0 || got.SelectedPartPlexID != "11" ||
		got.SelectedPartKey != "/library/parts/11/file.mkv" {
		t.Fatalf("selection lost on reload: %+v", got)
	}

	e := &Engine{DB: db, Store: store}
	r := httptest.NewRequest("GET", "/library/parts/11/file.mkv", nil)
	if sub, deny, _ := e.EnforcePart(r, "11", "live-sess"); deny || sub != "" {
		t.Fatalf("selected part must pass: %q %v", sub, deny)
	}
	r2 := httptest.NewRequest("GET", "/library/parts/999/file.mkv", nil)
	sub, deny, _ := e.EnforcePart(r2, "999", "live-sess")
	if deny || sub != "/library/parts/11/file.mkv" {
		t.Fatalf("prohibited part must substitute stored selection: %q %v", sub, deny)
	}
}

func seedPlaybackIndex(t *testing.T, ctx context.Context, db database.DBTX) {
	t.Helper()
	if _, err := db.Exec(ctx, `INSERT INTO plex_servers(name, internal_origin_url, machine_identifier, enabled)
		VALUES('Playback Box','http://test.invalid:32400','test-playback-box',true)
		ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	var libraryID string
	if err := db.QueryRow(ctx, `INSERT INTO libraries(server_id, plex_section_id, title, media_type)
		VALUES((SELECT id FROM plex_servers WHERE machine_identifier='test-playback-box'),'22','Movies','movie')
		ON CONFLICT (server_id, plex_section_id) DO UPDATE SET title='Movies' RETURNING id`).Scan(&libraryID); err != nil {
		t.Fatal(err)
	}
	var itemID string
	if err := db.QueryRow(ctx, `INSERT INTO library_items(server_id, library_id, rating_key, item_type, title)
		VALUES((SELECT id FROM plex_servers WHERE machine_identifier='test-playback-box'),$1,'4242','movie','Indexed Title')
		ON CONFLICT (server_id, rating_key) DO UPDATE SET title='Indexed Title' RETURNING id`, libraryID).Scan(&itemID); err != nil {
		t.Fatal(err)
	}
	var variantID string
	if err := db.QueryRow(ctx, `INSERT INTO media_variants(library_item_id, media_index, container, video_codec, width, height, normalized_dynamic_range)
		VALUES($1,0,'mkv','h264',1920,1080,'SDR')
		ON CONFLICT (library_item_id, media_index) DO UPDATE SET container='mkv' RETURNING id`, itemID).Scan(&variantID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO media_parts(media_variant_id, plex_part_id, part_index, plex_key, container)
		VALUES($1,'11',0,'/library/parts/11/file.mkv','mkv')
		ON CONFLICT (media_variant_id, part_index) DO UPDATE SET plex_key='/library/parts/11/file.mkv'`, variantID); err != nil {
		t.Fatal(err)
	}
}
