package policy

import (
	"context"
	"os"
	"testing"

	"github.com/LJAM96/replx/internal/database"
)

func TestLiveLoadEffective(t *testing.T) {
	url := os.Getenv("REPLX_EDGE_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("REPLX_EDGE_TEST_POSTGRES_URL not set; CI go job covers live policy SQL")
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
	_, _ = db.Exec(ctx, `DELETE FROM plex_servers WHERE machine_identifier='test-policy-box'`)
	var serverID string
	if err := db.QueryRow(ctx, `INSERT INTO plex_servers(name, internal_origin_url, machine_identifier, enabled)
		VALUES('Policy Box','http://test.invalid:32400','test-policy-box',true) RETURNING id`).Scan(&serverID); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = db.Exec(context.Background(), `DELETE FROM plex_servers WHERE machine_identifier='test-policy-box'`)
	}()
	_, err = db.Exec(ctx, `INSERT INTO policies(server_id, scope_type, name, config)
		VALUES($1,'global','g','{"maxSourceHeight":2160,"allowTranscode":"deny"}')`, serverID)
	if err != nil {
		t.Fatal(err)
	}
	eff, scope := LoadEffective(ctx, db, serverID, nil, nil)
	if scope != "GLOBAL" || eff.MaxSourceHeight == nil || *eff.MaxSourceHeight != 2160 {
		t.Fatalf("global: %+v %s", eff.MaxSourceHeight, scope)
	}
	if eff.AllowTranscode.Normalize() != Deny {
		t.Fatal("global deny must hold")
	}
}
