package playback

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/LJAM96/replx/internal/identity"
	"github.com/LJAM96/replx/internal/testdb"
	"github.com/LJAM96/replx/internal/trace"
)

// TestLiveStatelessBoundary proves the review's core scenario against real
// SQL: Jodie's user policy denies the 4K part with no session state, while
// the allowed 1080p part passes. The identity link is seeded directly so
// no plex.tv call is needed.
func TestLiveStatelessBoundary(t *testing.T) {
	ctx, db := testdb.Begin(t)
	const secret = "playback-boundary-test-secret-0123456789abcdef"
	const userToken = "jodie-user-token"
	fp := trace.Fingerprint(secret, userToken)

	_, _ = db.Exec(ctx, `DELETE FROM plex_servers WHERE machine_identifier='test-playback-box'`)
	var serverID, libraryID, itemID, identityID string
	if err := db.QueryRow(ctx, `INSERT INTO plex_servers(name, internal_origin_url, machine_identifier, enabled)
		VALUES('Playback Box','http://test.invalid:32400','test-playback-box',true) RETURNING id`).Scan(&serverID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `INSERT INTO libraries(server_id, plex_section_id, title, media_type)
		VALUES($1,'22','Movies','movie') RETURNING id`, serverID).Scan(&libraryID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx, `INSERT INTO library_items(server_id, library_id, rating_key, item_type, title)
		VALUES($1,$2,'5001','movie','Boundary Film') RETURNING id`, serverID, libraryID).Scan(&itemID); err != nil {
		t.Fatal(err)
	}
	seedVariant := func(index, w, h int, dr string) string {
		var variantID string
		if err := db.QueryRow(ctx, `INSERT INTO media_variants(library_item_id, plex_media_id, media_index, width, height, normalized_dynamic_range)
			VALUES($1,$2,$3,$4,$5,$6) RETURNING id`,
			itemID, "m-"+string(rune('0'+index)), index, w, h, dr).Scan(&variantID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(ctx, `INSERT INTO media_parts(media_variant_id, plex_part_id, part_index, plex_key)
			VALUES($1,$2,$3,$4)`, variantID, "40"+string(rune('0'+index)), index, "/library/parts/40"+string(rune('0'+index))+"/f.mkv"); err != nil {
			t.Fatal(err)
		}
		return variantID
	}
	seedVariant(0, 3840, 2160, "HDR10")
	seedVariant(1, 1920, 1080, "SDR")
	if err := db.QueryRow(ctx, `INSERT INTO plex_identities(server_id, plex_account_id, username, identity_type)
		VALUES($1,4242,'jodie','user') RETURNING id`, serverID).Scan(&identityID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO plex_token_identities(server_id, identity_id, token_fingerprint, token_status)
		VALUES($1,$2,$3,'valid')`, serverID, identityID, fp); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ctx, `INSERT INTO policies(server_id, scope_type, scope_id, name, config)
		VALUES($1,'user',$2,'jodie-1080p','{"maxSourceHeight":1080}')`, serverID, identityID); err != nil {
		t.Fatal(err)
	}

	e := &Engine{DB: db, Secret: secret, Store: NewMemoryStore(),
		Identity: identity.New(db, nil), LoadPolicy: DefaultPolicyLoader(db)}
	part := func(id string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/library/parts/"+id+"/f.mkv", nil)
		r.Header.Set("X-Plex-Token", userToken)
		return r
	}
	// index 0 is the 4K variant (part 400), index 1 is 1080p (part 401).
	if _, deny, reason := e.EnforcePart(part("400"), "400", "ghost-session"); !deny || reason != DecisionRequired {
		t.Fatalf("Jodie 4K part must fail closed statelessly: %v %q", deny, reason)
	}
	if sub, deny, _ := e.EnforcePart(part("401"), "401", "ghost-session"); deny || sub != "" {
		t.Fatalf("Jodie 1080p part must pass: %q %v", sub, deny)
	}
}
