package onboarding

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"github.com/LJAM96/replx-edge/internal/database"
	"github.com/LJAM96/replx-edge/internal/plextv"
	"github.com/LJAM96/replx-edge/internal/proxy"
)

// liveService wires the full flow against fakes when TEST_POSTGRES_URL is set.
func TestLiveOnboardingFlow(t *testing.T) {
	dbURL := os.Getenv("REPLX_EDGE_TEST_POSTGRES_URL")
	if dbURL == "" {
		t.Skip("REPLX_EDGE_TEST_POSTGRES_URL not set")
	}
	ctx := t.Context()
	pool, err := database.Open(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := pool.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Isolate from other live tests sharing this database.
	for _, q := range []string{
		"TRUNCATE plex_servers, plex_identities, client_instances, app_identity CASCADE",
		"DELETE FROM app_settings WHERE key LIKE 'onboarding.%'",
		"DELETE FROM compatibility_profiles WHERE platform='Web' AND product='Plex Web'",
	} {
		if _, err := pool.Raw().Exec(ctx, q); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
	}

	const machine = "pms-machine-1"
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<MediaContainer machineIdentifier="` + machine + `" friendlyName="Home" version="1.2.3"/>`))
	}))
	defer origin.Close()

	// Control listener is the REAL proxy pointed at the fake origin.
	proxyHandler, err := proxy.New(proxy.Options{OriginBase: origin.URL, IngressMode: "direct"})
	if err != nil {
		t.Fatal(err)
	}
	control := httptest.NewServer(proxyHandler)
	defer control.Close()

	var mu sync.Mutex
	claimed := false
	tv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/api/v2/pins":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 9, "code": "WXYZ"})
		case "/api/v2/pins/9":
			var tok *string
			if claimed {
				s := "owner-token-9"
				tok = &s
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 9, "code": "WXYZ", "authToken": tok})
		case "/api/v2/user":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 7, "username": "owner"})
		case "/api/v2/resources":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"clientIdentifier": machine,
				"name":             "Home PMS",
				"provides":         "server",
				"accessToken":      "pms-token-9",
				"connections": []map[string]any{
					{"uri": "https://origin-public.example:32400", "protocol": "https", "local": false},
					{"uri": "https://test-public.example", "protocol": "https", "local": false},
				},
			}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer tv.Close()

	svc := &Service{
		DB: pool.Raw(),
		NewTV: func(clientID string) TVClient {
			return &plextv.Client{BaseURL: tv.URL, ClientIdentifier: clientID, Product: "t", Version: "t", Platform: "t"}
		},
		ControlBase: control.URL,
		Secret:      "0123456789abcdef0123456789abcdef",
		PublicURL:   "https://test-public.example",
		InternalURL: origin.URL,
	}

	if _, err := svc.EnsureIdentity(ctx); err != nil {
		t.Fatalf("identity: %v", err)
	}
	issue, err := svc.IssuePIN(ctx)
	if err != nil {
		t.Fatalf("pin: %v", err)
	}
	if issue.Code != "WXYZ" {
		t.Fatalf("pin code: %+v", issue)
	}
	if ok, _ := svc.PollClaim(ctx); ok {
		t.Fatal("unclaimed must poll false")
	}
	mu.Lock()
	claimed = true
	mu.Unlock()
	if ok, err := svc.PollClaim(ctx); !ok || err != nil {
		t.Fatalf("claim: %v %v", ok, err)
	}
	servers, err := svc.ListServers(ctx)
	if err != nil || len(servers) != 1 {
		t.Fatalf("servers: %+v %v", servers, err)
	}
	sel, err := svc.SelectResource(ctx, machine)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if sel.MediaOrigin != "https://origin-public.example:32400" {
		t.Fatalf("media origin: %+v", sel)
	}
	report, err := svc.Verify(ctx)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !report.Match || !report.CustomURLPresent || report.OriginID != machine {
		t.Fatalf("report: %+v", report)
	}
	if got := svc.Status(ctx)["stage"]; got != StageVerified {
		t.Fatalf("stage: %v", got)
	}
}
