package policy

import (
	"testing"

	"github.com/LJAM96/replx/internal/testdb"
)

func TestLiveLoadEffective(t *testing.T) {
	ctx, db := testdb.Begin(t)
	_, _ = db.Exec(ctx, `DELETE FROM plex_servers WHERE machine_identifier='test-policy-box'`)
	var serverID string
	if err := db.QueryRow(ctx, `INSERT INTO plex_servers(name, internal_origin_url, machine_identifier, enabled)
		VALUES('Policy Box','http://test.invalid:32400','test-policy-box',true) RETURNING id`).Scan(&serverID); err != nil {
		t.Fatal(err)
	}
	_, err := db.Exec(ctx, `INSERT INTO policies(server_id, scope_type, name, config)
		VALUES($1,'global','g','{"maxSourceHeight":2160,"allowTranscode":"deny"}')`, serverID)
	if err != nil {
		t.Fatal(err)
	}
	eff, scope, err := LoadEffective(ctx, db, serverID, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if scope != "GLOBAL" || eff.MaxSourceHeight == nil || *eff.MaxSourceHeight != 2160 {
		t.Fatalf("global: %+v %s", eff.MaxSourceHeight, scope)
	}
	if eff.AllowTranscode.Normalize() != Deny {
		t.Fatal("global deny must hold")
	}
	// Corrupt config on an applicable level fails the load instead of
	// silently inheriting past it. The JSON itself must be valid jsonb
	// (Postgres rejects malformed JSON at write time); the corruption is
	// a Go type error the loader must surface.
	if _, err := db.Exec(ctx, `UPDATE policies SET config='{"allowHDR":42}' WHERE server_id=$1`, serverID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadEffective(ctx, db, serverID, nil, nil); err == nil {
		t.Fatal("corrupt global config must fail closed")
	}
	// User level activates with an identity and outranks global.
	if _, err := db.Exec(ctx, `UPDATE policies SET config='{"maxSourceHeight":2160}' WHERE server_id=$1`, serverID); err != nil {
		t.Fatal(err)
	}
	var identityID string
	if err := db.QueryRow(ctx, `INSERT INTO plex_identities(server_id, plex_account_id, username, identity_type)
		VALUES($1,4242,'jodie','user') RETURNING id`).Scan(&identityID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO policies(server_id, scope_type, scope_id, name, config)
		VALUES($1,'user',$2,'jodie-1080p','{"maxSourceHeight":1080}')`, serverID, identityID); err != nil {
		t.Fatal(err)
	}
	eff, scope, err = LoadEffective(ctx, db, serverID, &identityID, nil)
	if err != nil || scope != "USER" || eff.MaxSourceHeight == nil || *eff.MaxSourceHeight != 1080 {
		t.Fatalf("user precedence: %+v %s %v", eff.MaxSourceHeight, scope, err)
	}
}
