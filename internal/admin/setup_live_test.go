package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// TestLiveConcurrentSetup proves the singleton is database-enforced: two
// simultaneous setups with independent muxes (separate setup tokens, as in
// two processes) yield exactly one administrator.
func TestLiveConcurrentSetup(t *testing.T) {
	_, db1 := testdb.Begin(t)
	_, db2 := testdb.Begin(t)
	m1 := NewMux(health.Checks{}, &onboarding.Service{DB: db1}, "bootstrap-one", true, nil, &spike.Observations{DB: db1})
	m2 := NewMux(health.Checks{}, &onboarding.Service{DB: db2}, "bootstrap-two", true, nil, &spike.Observations{DB: db2})
	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i, tc := range []struct {
		m     *Mux
		token string
		user  string
	}{
		{m1, "bootstrap-one", "first"},
		{m2, "bootstrap-two", "second"},
	} {
		wg.Add(1)
		go func(i int, m *Mux, token, user string) {
			defer wg.Done()
			body := `{"username":"` + user + `","password":"correct-horse-32"}`
			req := httptest.NewRequest(http.MethodPost, "/api/v1/setup", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			m.ServeHTTP(rec, req)
			codes[i] = rec.Code
		}(i, tc.m, tc.token, tc.user)
	}
	wg.Wait()
	created := 0
	for _, c := range codes {
		if c == http.StatusCreated {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("exactly one concurrent setup must win, codes=%v", codes)
	}
}
