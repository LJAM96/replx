package search

import (
	"testing"

	"github.com/LJAM96/replx/internal/testdb"
)

func TestLiveCandidates(t *testing.T) {
	ctx, db := testdb.Begin(t)
	_, _ = db.Exec(ctx, `DELETE FROM plex_servers WHERE machine_identifier='test-search-box'`)
	var serverID string
	if err := db.QueryRow(ctx, `INSERT INTO plex_servers(name, internal_origin_url, machine_identifier, enabled)
		VALUES('Search Box','http://test.invalid:32400','test-search-box',true) RETURNING id`).Scan(&serverID); err != nil {
		t.Fatal(err)
	}
	_, err := db.Exec(ctx, `INSERT INTO library_items(server_id, rating_key, item_type, title, sort_title, original_title)
		VALUES($1,'9001','movie','Ted Lasso Chronicles','Ted Lasso Chronicles','Ted Lasso'),
		($1,'9002','movie','Unrelated Film','Unrelated Film','Unrelated')`, serverID)
	if err != nil {
		t.Fatal(err)
	}
	// Stale index: libraries exist with no completed sweeps.
	mkLib := func(section string) {
		t.Helper()
		if _, err := db.Exec(ctx, `INSERT INTO libraries(server_id, plex_section_id, title, media_type)
			VALUES($1,$2,$2,'movie')
			ON CONFLICT (server_id, plex_section_id) DO NOTHING`, serverID, section); err != nil {
			t.Fatal(err)
		}
	}
	mkLib("22")
	mkLib("23")
	if _, fresh, err := Candidates(ctx, db, serverID, "lasso", 10); err != nil || fresh {
		t.Fatalf("stale index must refuse: fresh=%v err=%v", fresh, err)
	}
	complete := func(section string) {
		t.Helper()
		var libID string
		if err := db.QueryRow(ctx, `SELECT id FROM libraries WHERE server_id=$1 AND plex_section_id=$2`,
			serverID, section).Scan(&libID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(ctx, `INSERT INTO sync_cursors(server_id, sync_type, library_id, status, last_completed_at)
			VALUES($1,'section',$2,'complete',now())
			ON CONFLICT (server_id, sync_type, library_id) DO UPDATE SET status='complete', last_completed_at=now()`, serverID, libID); err != nil {
			t.Fatal(err)
		}
	}
	// One fresh section must not vouch for a stale sibling.
	complete("22")
	if _, fresh, err := Candidates(ctx, db, serverID, "lasso", 10); err != nil || fresh {
		t.Fatalf("partial freshness must refuse: fresh=%v err=%v", fresh, err)
	}
	complete("23")
	got, fresh, err := Candidates(ctx, db, serverID, "lasso", 10)
	if err != nil || !fresh {
		t.Fatalf("fresh index must serve: fresh=%v err=%v", fresh, err)
	}
	if len(got) != 1 || got[0].RatingKey != "9001" {
		t.Fatalf("candidates: %+v", got)
	}
	if _, fresh, _ := Candidates(ctx, db, serverID, "   ", 10); fresh {
		t.Fatal("blank query must not serve")
	}
}
