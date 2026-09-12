package onboarding

import (
	"testing"

	"github.com/LJAM96/replx-edge/internal/plextv"
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
			{URI: "https://plex.example.com"},
		},
	}}
	if !CustomURLPresent(resources, "pms-1", "plex.example.com") {
		t.Fatal("expected present")
	}
	if CustomURLPresent(resources, "pms-1", "other.example.com") {
		t.Fatal("expected absent")
	}
	if CustomURLPresent(resources, "pms-2", "plex.example.com") {
		t.Fatal("wrong resource must not match")
	}
}
