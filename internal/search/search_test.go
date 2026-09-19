package search

import (
	"context"
	"os"
	"testing"

	"github.com/LJAM96/replx/internal/database"
)

func TestLiveCandidates(t *testing.T) {
	url := os.Getenv("REPLX_EDGE_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("REPLX_EDGE_TEST_POSTGRES_URL not set; CI go job covers live search SQL")
	}
	ctx := context.Background()
	pool, err := database.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := pool.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	db := pool.Raw()
	_, _ = db.Exec(ctx, `DELETE FROM plex_servers WHERE machine_identifier='test-search-box'`)
	var serverID string
	if err := db.QueryRow(ctx, `INSERT INTO plex_servers(name, internal_origin_url, machine_identifier, enabled)
		VALUES('Search Box','http://test.invalid:32400','test-search-box',true) RETURNING id`).Scan(&serverID); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = db.Exec(context.Background(), `DELETE FROM plex_servers WHERE machine_identifier='test-search-box'`)
	}()
	_, err = db.Exec(ctx, `INSERT INTO library_items(server_id, rating_key, item_type, title, sort_title, original_title)
		VALUES($1,'9001','movie','Ted Lasso Chronicles','Ted Lasso Chronicles','Ted Lasso'),
		($1,'9002','movie','Unrelated Film','Unrelated Film','Unrelated')`, serverID)
	if err != nil {
		t.Fatal(err)
	}
	// Stale index: no completed sweep, must refuse with fresh=false.
	if _, fresh, err := Candidates(ctx, db, serverID, "lasso", 10); err != nil || fresh {
		t.Fatalf("stale index must refuse: fresh=%v err=%v", fresh, err)
	}
	_, err = db.Exec(ctx, `INSERT INTO sync_cursors(server_id, sync_type, status, last_completed_at)
		VALUES($1,'section','complete',now())`, serverID)
	if err != nil {
		t.Fatal(err)
	}
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
