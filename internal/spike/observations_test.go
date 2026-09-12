package spike

import (
	"context"
	"os"
	"testing"

	"github.com/LJAM96/replx-edge/internal/database"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestObservationsLive(t *testing.T) {
	url := os.Getenv("REPLX_EDGE_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("REPLX_EDGE_TEST_POSTGRES_URL not set")
	}
	ctx := context.Background()
	migrator, err := database.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer migrator.Close()
	if err := migrator.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	o := &Observations{DB: pool}
	ctx = t.Context()
	in := Observation{Platform: "Web", Product: "Plex Web", ProductVersion: "4.1", PlaybackType: "progressive", Status: "supported", Notes: "follows 307"}
	if err := o.Record(ctx, in); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := o.Record(ctx, Observation{Platform: "Web", Product: "Plex Web", ProductVersion: "4.2", PlaybackType: "progressive", Status: "supported", Notes: "still good"}); err != nil {
		t.Fatalf("record again: %v", err)
	}
	rows, err := o.Matrix(ctx)
	if err != nil {
		t.Fatalf("matrix: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.Platform == "Web" && r.Product == "Plex Web" && r.PlaybackType == "progressive" {
			found = true
			if r.Observations < 2 {
				t.Fatalf("counter: %+v", r)
			}
			if r.ProductVersion != "4.2" {
				t.Fatalf("version: %+v", r)
			}
		}
	}
	if !found {
		t.Fatal("matrix missing test row")
	}
	if err := o.Record(ctx, Observation{Platform: "X", Product: "Y", PlaybackType: "Z", Status: "bogus"}); err == nil {
		t.Fatal("invalid status must fail")
	}
}
