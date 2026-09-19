package onboarding

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LJAM96/replx/internal/plextv"
	"github.com/LJAM96/replx/internal/proxy"
	"github.com/LJAM96/replx/internal/testdb"
)

// liveService wires the full flow against fakes when TEST_POSTGRES_URL is set.
func TestLiveOnboardingFlow(t *testing.T) {
	// Rolled-back transaction: full isolation from sibling packages
	// sharing the CI database (replaces the old TRUNCATE approach,
	// which destroyed other tests' rows mid-run).
	ctx, tx := testdb.Begin(t)

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
	sawDeviceJWT := false
	sawJWKBody := false
	tv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/api/v2/pins":
			var body struct {
				JWK map[string]string `json:"jwk"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			sawJWKBody = body.JWK["x"] != ""
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 9, "code": "WXYZ"})
		case "/api/v2/pins/9":
			if djwt := r.URL.Query().Get("deviceJWT"); djwt != "" {
				sawDeviceJWT = true
				if len(strings.Split(djwt, ".")) != 3 {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
			}
			var tok *string
			if claimed {
				// JWT-shaped owner credential expiring in 1h forces refresh coverage.
				exp := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":` + strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10) + `}`))
				s := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"EdDSA"}`)) + "." + exp + ".sig"
				tok = &s
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 9, "code": "WXYZ", "authToken": tok})
		case "/api/v2/auth/nonce":
			_ = json.NewEncoder(w).Encode(map[string]any{"nonce": "nonce-live"})
		case "/api/v2/auth/token":
			exp := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":` + strconv.FormatInt(time.Now().Add(7*24*time.Hour).Unix(), 10) + `}`))
			s := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"EdDSA"}`)) + "." + exp + ".sig2"
			_ = json.NewEncoder(w).Encode(map[string]any{"authToken": s})
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
		DB: tx,
		NewTV: func(clientID string) TVClient {
			return &plextv.Client{BaseURL: tv.URL, ClientIdentifier: clientID, Product: "t", Version: "t", Platform: "t"}
		},
		ControlBase: control.URL,
		Secret:      "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
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
	mu.Lock()
	jwtPath := sawJWKBody && sawDeviceJWT
	mu.Unlock()
	if !jwtPath {
		t.Fatal("expected the JWT device flow (JWK body + deviceJWT poll)")
	}
	if got := svc.Status(ctx)["authMode"]; got != "jwt" {
		t.Fatalf("authMode: %v", got)
	}
	// Owner JWT expires in 1h: refresh must mint a fresh credential.
	refreshed, err := svc.RefreshOwnerJWT(ctx)
	if err != nil || !refreshed {
		t.Fatalf("refresh: %v %v", refreshed, err)
	}
	// Reselecting voids verification and re-verify restores it.
	if _, err := svc.SelectResource(ctx, machine); err != nil {
		t.Fatalf("reselect: %v", err)
	}
	if got := svc.Status(ctx)["stage"]; got != StageSelected {
		t.Fatalf("reselect must clear verified, got %v", got)
	}
	if _, err := svc.Verify(ctx); err != nil {
		t.Fatalf("re-verify: %v", err)
	}
	if got := svc.Status(ctx)["stage"]; got != StageVerified {
		t.Fatalf("stage: %v", got)
	}
}

func TestLegacyFallbackLive(t *testing.T) {
	ctx, tx := testdb.Begin(t)
	// Fake rejects the JWT shape: onboarding must fall back to legacy.
	tv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/pins":
			var body struct {
				JWK map[string]string `json:"jwk"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.JWK["x"] != "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 5, "code": "LEG"})
		case "/api/v2/pins/5":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 5, "code": "LEG"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer tv.Close()
	svc := &Service{
		DB:          tx,
		NewTV:       func(clientID string) TVClient { return &plextv.Client{BaseURL: tv.URL, ClientIdentifier: clientID} },
		Secret:      "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		PublicURL:   "https://test-public.example",
		InternalURL: "http://127.0.0.1:9",
	}
	issue, err := svc.IssuePIN(ctx)
	if err != nil {
		t.Fatalf("pin: %v", err)
	}
	if issue.AuthMode != "legacy" {
		t.Fatalf("expected legacy fallback, got %+v", issue)
	}
}

func TestSubmitTokenLive(t *testing.T) {
	ctx, tx := testdb.Begin(t)
	tv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v2/user" && r.Header.Get("X-Plex-Token") == "pasted-good-token" {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 11, "username": "pasteowner"})
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer tv.Close()
	svc := &Service{
		DB:          tx,
		NewTV:       func(clientID string) TVClient { return &plextv.Client{BaseURL: tv.URL, ClientIdentifier: clientID} },
		Secret:      "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		PublicURL:   "https://test-public.example",
		InternalURL: "http://127.0.0.1:9",
	}
	if _, err := svc.SubmitToken(ctx, "bogus"); err == nil {
		t.Fatal("invalid token must be rejected")
	}
	if _, err := svc.SubmitToken(ctx, "short"); err == nil {
		t.Fatal("short token must be rejected")
	}
	username, err := svc.SubmitToken(ctx, "pasted-good-token")
	if err != nil || username != "pasteowner" {
		t.Fatalf("submit: %q %v", username, err)
	}
	if got := svc.Status(ctx)["stage"]; got != StageOwnerAuthenticated {
		t.Fatalf("stage: %v", got)
	}
	if got := svc.Status(ctx)["authMode"]; got != "legacy" {
		t.Fatalf("pasted tokens are always legacy (no refresh), got %v", got)
	}
}
