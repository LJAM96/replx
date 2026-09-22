package warmer

import (
	"context"
	"testing"

	"github.com/LJAM96/replx/internal/testdb"
)

// TestLiveOwnerScopeCanonical proves the warmer and the proxy share one
// scope vocabulary: with an identity store the owner scope is the
// identity UUID form the proxy caches under, so owner refreshes land in
// live keys instead of a dead acct: namespace.
func TestLiveOwnerScopeCanonical(t *testing.T) {
	ctx, db := testdb.Begin(t)
	_, _ = db.Exec(ctx, `DELETE FROM plex_servers WHERE machine_identifier='test-warmer-box'`)
	var serverID string
	if err := db.QueryRow(ctx, `INSERT INTO plex_servers(name, internal_origin_url, machine_identifier, enabled)
		VALUES('Warmer Box','http://test.invalid:32400','test-warmer-box',true) RETURNING id`).Scan(&serverID); err != nil {
		t.Fatal(err)
	}
	var identityID string
	if err := db.QueryRow(ctx, `INSERT INTO plex_identities(server_id, plex_account_id, username, identity_type)
		VALUES($1,4242,'owner','user') RETURNING id::text`, serverID).Scan(&identityID); err != nil {
		t.Fatal(err)
	}
	// Prove the fixture is visible in this transaction before exercising
	// the scope resolution built on top of it.
	var visible string
	if err := db.QueryRow(ctx, `SELECT id::text FROM plex_identities WHERE server_id=$1 AND plex_account_id=$2`, serverID, 4242).Scan(&visible); err != nil || visible != identityID {
		t.Fatalf("fixture must be visible: %q %v", visible, err)
	}
	w := New(nil, "http://test.invalid:32400", "s", nil, nil, nil)
	w.DB = db
	w.OwnerAccount = func(ctx context.Context) (int64, bool) { return 4242, true }
	if got := w.ownerScope(ctx); got != "user:"+identityID {
		t.Fatalf("owner scope must be canonical identity form, got %q", got)
	}
	// Without an identity store the legacy account form is kept (tests).
	plain := New(nil, "http://test.invalid:32400", "s", nil, nil, nil)
	plain.OwnerAccount = func(ctx context.Context) (int64, bool) { return 7, true }
	if got := plain.ownerScope(ctx); got != "acct:7" {
		t.Fatalf("legacy owner scope: %q", got)
	}
}
