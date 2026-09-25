package onboarding

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/LJAM96/replx/internal/plextv"
)

func conns() []plextv.Connection {
	return []plextv.Connection{
		{URI: "http://192.168.1.2:32400", Protocol: "http", Local: true},
		{URI: "https://192-168-1-2.plex.direct:32400", Protocol: "https", Local: true},
		{URI: "https://origin.example:32400", Protocol: "https"},
		{URI: "https://plex.example.com", Protocol: "https"},
	}
}

func TestSelectMediaOriginPrefersRemote(t *testing.T) {
	got, err := SelectMediaOrigin(conns(), "plex.example.com")
	if err != nil || got != "https://origin.example:32400" {
		t.Fatalf("got %q %v", got, err)
	}
}

func TestSelectMediaOriginSkipsSelf(t *testing.T) {
	only := []plextv.Connection{{URI: "https://plex.example.com", Protocol: "https"}}
	if _, err := SelectMediaOrigin(only, "plex.example.com"); err == nil {
		t.Fatal("must not route media back at replx-edge")
	}
}

func TestSelectMediaOriginSkipsPrivateIP(t *testing.T) {
	// A docker-internal address published by PMS must never win, even
	// when plex.tv does not flag it local.
	conns := []plextv.Connection{
		{URI: "https://172.17.0.7:32400", Protocol: "https"},
		{URI: "https://origin.example:32400", Protocol: "https"},
	}
	resolve := func(host string) (bool, bool) {
		if host == "origin.example" {
			return true, false
		}
		return false, false
	}
	got, err := selectMediaOrigin(conns, "plex.example.com", resolve)
	if err != nil || got != "https://origin.example:32400" {
		t.Fatalf("got %q %v", got, err)
	}
	// Literal private IP alone fails closed.
	if _, err := selectMediaOrigin(conns[:1], "plex.example.com", resolve); err == nil {
		t.Fatal("private-only candidates must fail closed")
	}
	// Unresolvable names are kept as a last resort, never preferred.
	mixed := []plextv.Connection{
		{URI: "https://mystery.invalid:32400", Protocol: "https"},
		{URI: "https://origin.example:32400", Protocol: "https"},
	}
	unknown := func(host string) (bool, bool) {
		if host == "origin.example" {
			return true, false
		}
		return false, true
	}
	got, err = selectMediaOrigin(mixed, "plex.example.com", unknown)
	if err != nil || got != "https://origin.example:32400" {
		t.Fatalf("proven public must win, got %q %v", got, err)
	}
	got, err = selectMediaOrigin(mixed[:1], "plex.example.com", unknown)
	if err != nil || got != "https://mystery.invalid:32400" {
		t.Fatalf("unknown kept as fallback, got %q %v", got, err)
	}
}

func TestSelectMediaOriginFailsClosed(t *testing.T) {
	only := []plextv.Connection{{URI: "http://192.168.1.2:32400", Protocol: "http", Local: true}}
	if _, err := SelectMediaOrigin(only, "plex.example.com"); err == nil {
		t.Fatal("http-only origin must fail closed")
	}
}

func TestTripleMatch(t *testing.T) {
	if !TripleMatch("a", "a", "a") {
		t.Fatal("equal ids must match")
	}
	if TripleMatch("a", "a", "b") || TripleMatch("", "", "") || TripleMatch("a", "", "a") {
		t.Fatal("mismatch or empty must not match")
	}
}

func TestCustomURLPresent(t *testing.T) {
	resources := []plextv.Resource{{
		ClientIdentifier: "pms-1",
		Connections: []plextv.Connection{
			{URI: "https://origin.example:32400"},
			{URI: "https://plex.example.com:443"},
		},
	}}
	if !CustomURLPresent(resources, "pms-1", "https://plex.example.com") {
		t.Fatal("expected present")
	}
	if CustomURLPresent(resources, "pms-1", "https://other.example.com") {
		t.Fatal("expected absent")
	}
	if CustomURLPresent(resources, "pms-2", "https://plex.example.com") {
		t.Fatal("wrong resource must not match")
	}
	resources[0].Connections[1].URI = "https://plex.example.com:32400"
	if CustomURLPresent(resources, "pms-1", "https://plex.example.com") {
		t.Fatal("wrong published port must not pass verification")
	}
}

func TestConfigureCustomURLKeepsExistingAndAddsPublicPort(t *testing.T) {
	var saved string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != "owner-token" {
			t.Error("missing owner credential")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<MediaContainer><Setting id="customConnections" value="https://origin.example:42442, https://replex.example"/></MediaContainer>`))
		case http.MethodPut:
			saved = r.URL.Query().Get("customConnections")
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()
	if err := tryConfigureCustomURL(server.URL, "owner-token", "https://replex.example"); err != nil {
		t.Fatal(err)
	}
	if saved != "https://origin.example:42442, https://replex.example:443" {
		t.Fatalf("custom connections: %q", saved)
	}
}
