package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/LJAM96/replx/internal/database"
	"github.com/LJAM96/replx/internal/health"
	"github.com/LJAM96/replx/internal/onboarding"
	"github.com/LJAM96/replx/internal/spike"
)

func liveMux(t *testing.T) (*Mux, string) {
	t.Helper()
	url := os.Getenv("REPLX_EDGE_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("REPLX_EDGE_TEST_POSTGRES_URL not set; CI go job covers live admin SQL")
	}
	ctx := context.Background()
	pool, err := database.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	db := pool.Raw()
	_, _ = db.Exec(ctx, `DELETE FROM plex_servers WHERE machine_identifier='test-ops-box'`)
	var serverID string
	if err := db.QueryRow(ctx, `INSERT INTO plex_servers(name, internal_origin_url, machine_identifier, enabled)
		VALUES('Ops Box','http://test.invalid:32400','test-ops-box',true) RETURNING id`).Scan(&serverID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(context.Background(), `DELETE FROM plex_servers WHERE machine_identifier='test-ops-box'`)
	})
	m := NewMux(health.Checks{}, &onboarding.Service{DB: db}, "tok123", true, nil, &spike.Observations{DB: db})
	return m, serverID
}

func TestLivePoliciesPutAndAudit(t *testing.T) {
	m, _ := liveMux(t)
	bearer := func(method, path, body string) *httptest.ResponseRecorder {
		var r *http.Request
		if body == "" {
			r = httptest.NewRequest(method, path, nil)
		} else {
			r = httptest.NewRequest(method, path, strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
		}
		r.Header.Set("Authorization", "Bearer tok123")
		rec := httptest.NewRecorder()
		m.ServeHTTP(rec, r)
		return rec
	}
	rec := bearer(http.MethodPut, "/api/v1/policies",
		`{"scopeType":"global","name":"house","config":{"maxSourceHeight":1080,"allowTranscode":"deny"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT global: %d %s", rec.Code, rec.Body.String())
	}
	if rec := bearer(http.MethodPut, "/api/v1/policies",
		`{"scopeType":"global","scopeId":"x","name":"bad","config":{}}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("global with id must 400: %d", rec.Code)
	}
	get := bearer(http.MethodGet, "/api/v1/policies?limit=10", "")
	if get.Code != http.StatusOK || !strings.Contains(get.Body.String(), `"house"`) {
		t.Fatalf("GET policies: %d %s", get.Code, get.Body.String())
	}
	var audits int
	if err := m.svc.DB.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_events WHERE action='policy.put'`).Scan(&audits); err != nil || audits < 1 {
		t.Fatalf("audit row missing: %v %d", err, audits)
	}
}

func TestLiveSessionsList(t *testing.T) {
	m, serverID := liveMux(t)
	ctx := context.Background()
	var sessID string
	err := m.svc.DB.QueryRow(ctx, `INSERT INTO playback_sessions(server_id, plex_session_identifier, rating_key, playback_mode, routing_mode)
		VALUES($1,'sess-ops','4242','directPlay','automatic') RETURNING id`, serverID).Scan(&sessID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.svc.DB.Exec(ctx, `INSERT INTO playback_decisions(playback_session_id, server_id, requested_media_index, selected_media_index, decision, decision_reason, plex_decision_code)
		VALUES($1,$2,0,1,'allow','directPlay',2000)`, sessID, serverID)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/playback/sessions?limit=5", nil)
	req.Header.Set("Authorization", "Bearer tok123")
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "sess-ops") ||
		!strings.Contains(rec.Body.String(), "directPlay") {
		t.Fatalf("sessions explain: %d %s", rec.Code, rec.Body.String())
	}
}
