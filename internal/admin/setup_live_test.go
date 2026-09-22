package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LJAM96/replx/internal/health"
	"github.com/LJAM96/replx/internal/onboarding"
	"github.com/LJAM96/replx/internal/spike"
	"github.com/LJAM96/replx/internal/testdb"
)

// TestLiveSetupRequiresToken proves first-run administrator creation is
// gated on the bootstrap capability: no token, wrong token, then success,
// then permanent disablement including bearer reuse after consumption.
func TestLiveSetupRequiresToken(t *testing.T) {
	ctx, db := testdb.Begin(t)
	m := NewMux(health.Checks{}, &onboarding.Service{DB: db}, "bootstrap-test-token", true, nil, &spike.Observations{DB: db})
	post := func(token, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/setup", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		m.ServeHTTP(rec, req)
		return rec
	}
	const payload = `{"username":"owner","password":"correct-horse-32"}`
	if rec := post("", payload); rec.Code != http.StatusUnauthorized {
		t.Fatalf("tokenless setup must 401, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := post("wrong-token", payload); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-token setup must 401, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := post("bootstrap-test-token", payload); rec.Code != http.StatusCreated {
		t.Fatalf("token setup must 201, got %d %s", rec.Code, rec.Body.String())
	}
	// Consumed: bearer reuse and a second username both fail.
	if rec := post("bootstrap-test-token", payload); rec.Code != http.StatusUnauthorized {
		t.Fatalf("consumed token must 401, got %d %s", rec.Code, rec.Body.String())
	}
	other := `{"username":"second","password":"correct-horse-32","setupToken":"bootstrap-test-token"}`
	if rec := post("", other); rec.Code == http.StatusCreated {
		t.Fatalf("second administrator must never be created: %d", rec.Code)
	}
	var n int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM admin_users`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("exactly one administrator: %d %v", n, err)
	}
	_ = ctx
}
