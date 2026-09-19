package retention

import (
	"context"
	"os"
	"testing"

	"github.com/LJAM96/replx/internal/database"
	"github.com/LJAM96/replx/internal/testdb"
)

func getenvURL(t *testing.T) string {
	t.Helper()
	url := os.Getenv("REPLX_EDGE_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("REPLX_EDGE_TEST_POSTGRES_URL not set; CI go job covers live retention SQL")
	}
	return url
}

func TestLivePurge(t *testing.T) {
	ctx, db := testdb.Begin(t)
	_, _ = db.Exec(ctx, `DELETE FROM plex_servers WHERE machine_identifier='test-retention-box'`)
	var serverID string
	if err := db.QueryRow(ctx, `INSERT INTO plex_servers(name, internal_origin_url, machine_identifier, enabled)
		VALUES('Retention Box','http://test.invalid:32400','test-retention-box',true) RETURNING id`).Scan(&serverID); err != nil {
		t.Fatal(err)
	}

	// Ended session older than the bound, with a decision attached.
	var sessID string
	err := db.QueryRow(ctx, `INSERT INTO playback_sessions(server_id, plex_session_identifier, rating_key, ended_at, started_at)
		VALUES($1,'old-sess','1', now() - make_interval(days => 40), now() - make_interval(days => 40))
		RETURNING id`, serverID).Scan(&sessID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(ctx, `INSERT INTO playback_decisions(playback_session_id, server_id, decision, decision_reason, created_at)
		VALUES($1,$2,'allow','test', now() - make_interval(days => 40)), (NULL,$2,'deny','test', now())`, sessID, serverID)
	if err != nil {
		t.Fatal(err)
	}
	// Active session and fresh decision must survive.
	var liveID string
	err = db.QueryRow(ctx, `INSERT INTO playback_sessions(server_id, plex_session_identifier, rating_key)
		VALUES($1,'live-sess','2') RETURNING id`, serverID).Scan(&liveID)
	if err != nil {
		t.Fatal(err)
	}
	// Expired trace and ancient audit row.
	_, err = db.Exec(ctx, `INSERT INTO diagnostic_traces(server_id, trace_type, status, expires_at)
		VALUES($1,'spike','complete', now() - interval '1 hour')`, serverID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(ctx, `INSERT INTO audit_events(admin_subject, action, created_at)
		VALUES('test','test.action', now() - make_interval(days => 200))`)
	if err != nil {
		t.Fatal(err)
	}

	res, err := PurgeOnce(ctx, db, Policy{PlaybackDays: 30, AuditDays: 180})
	if err != nil {
		t.Fatal(err)
	}
	if res.Sessions != 1 || res.Decisions != 2 || res.Traces != 1 || res.Audits != 1 {
		t.Fatalf("purge counts: %+v", res)
	}
	var n int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM playback_sessions WHERE id=$1`, liveID).Scan(&n); err != nil || n != 1 {
		t.Fatal("active session must survive")
	}
}

func TestPurgeSurfacesDBFailures(t *testing.T) {
	url := getenvURL(t)
	ctx := context.Background()
	pool, err := database.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	tx, err := pool.Raw().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = tx.Rollback(ctx)
	if _, err := PurgeOnce(ctx, tx, Policy{PlaybackDays: 30}); err == nil {
		t.Fatal("purge on a dead transaction must return the error, never a clean zero")
	}
	if _, err := PurgeOnce(ctx, nil, Policy{PlaybackDays: 30}); err == nil {
		t.Fatal("purge without a database must fail, not log success")
	}
}
