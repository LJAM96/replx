package plextv

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakePlexTV mimics the v2 PIN + resources surface.
func fakePlexTV(t *testing.T) (*httptest.Server, *bool) {
	t.Helper()
	var mu sync.Mutex
	claimed := false
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/pins", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("X-Plex-Client-Identifier") == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 7, "code": "ABCD"})
	})
	mux.HandleFunc("/api/v2/pins/7", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		var token *string
		if claimed {
			s := "owner-token-1"
			token = &s
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 7, "code": "ABCD", "authToken": token})
	})
	mux.HandleFunc("/api/v2/user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != "owner-token-1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 42, "username": "owner", "email": "o@example.com"})
	})
	mux.HandleFunc("/api/v2/resources", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != "owner-token-1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"clientIdentifier": "pms-machine-1",
			"name":             "Home PMS",
			"product":          "Plex Media Server",
			"provides":         "server",
			"accessToken":      "pms-token-1",
			"connections": []map[string]any{
				{"uri": "https://origin.example:32400", "protocol": "https", "local": false, "relay": false, "IPv6": false},
				{"uri": "http://192.168.1.2:32400", "protocol": "http", "local": true, "relay": false, "IPv6": false},
			},
		}, {
			"clientIdentifier": "player-1",
			"name":             "TV",
			"provides":         "player",
		}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	claimedPtr := &claimed
	return srv, claimedPtr
}

func testClient(srv *httptest.Server) *Client {
	return &Client{BaseURL: srv.URL, ClientIdentifier: "replx-edge-test-id", Product: "Replx Edge", Version: "dev", Platform: "Linux"}
}

func TestListUsersWithServerAccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/users" || r.Header.Get("X-Plex-Token") != "owner-token" || r.Header.Get("Accept") != "application/xml" {
			t.Errorf("unexpected Plex request")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`<MediaContainer><User id="10" username="Billy" title="Billy" restricted="0"><Server machineIdentifier="selected" numLibraries="2" pending="0"/></User><User id="11" username="Jodie" friendlyName="Jodie" restricted="1"><Server machineIdentifier="selected" numLibraries="1" pending="0"/></User><User id="12" username="other"><Server machineIdentifier="other" numLibraries="2" pending="0"/></User><User id="13" username="pending"><Server machineIdentifier="selected" numLibraries="2" pending="1"/></User></MediaContainer>`))
	}))
	defer srv.Close()
	users, err := testClient(srv).ListUsersWithServerAccess(t.Context(), "owner-token", "selected")
	if err != nil || len(users) != 2 || users[0].Username != "Billy" || users[1].FriendlyName != "Jodie" || !users[1].Restricted {
		t.Fatalf("shared users: %+v, %v", users, err)
	}
}

func TestPINFlow(t *testing.T) {
	srv, claimed := fakePlexTV(t)
	c := testClient(srv)
	pin, err := c.CreatePIN(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if pin.ID != 7 || pin.Code != "ABCD" {
		t.Fatalf("pin: %+v", pin)
	}
	if !strings.Contains(c.AuthURL(pin), "app.plex.tv/auth") {
		t.Fatalf("auth url: %s", c.AuthURL(pin))
	}
	if tok, _ := c.PollPIN(t.Context(), 7); tok != "" {
		t.Fatal("unclaimed PIN must poll empty")
	}
	*claimed = true
	tok, err := c.PollPIN(t.Context(), 7)
	if err != nil || tok != "owner-token-1" {
		t.Fatalf("claimed PIN: %q %v", tok, err)
	}
	u, err := c.GetUser(t.Context(), tok)
	if err != nil || u.ID != 42 {
		t.Fatalf("user: %+v %v", u, err)
	}
}

func TestListServersFiltersPlayers(t *testing.T) {
	srv, claimed := fakePlexTV(t)
	*claimed = true
	c := testClient(srv)
	servers, err := c.ListServers(t.Context(), "owner-token-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 || servers[0].ClientIdentifier != "pms-machine-1" {
		t.Fatalf("servers: %+v", servers)
	}
	if len(servers[0].Connections) != 2 {
		t.Fatalf("connections: %+v", servers[0].Connections)
	}
}
