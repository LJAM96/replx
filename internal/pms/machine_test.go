package pms

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchIdentityXML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != "tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<MediaContainer machineIdentifier="abc123" friendlyName="Home" version="1.2.3"/>`))
	}))
	defer srv.Close()
	id, err := FetchIdentity(srv.URL, "tok")
	if err != nil || id.MachineIdentifier != "abc123" || id.FriendlyName != "Home" {
		t.Fatalf("id: %+v %v", id, err)
	}
}

func TestFetchIdentityMissingFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte(`<MediaContainer friendlyName="NoID"/>`))
	}))
	defer srv.Close()
	if _, err := FetchIdentity(srv.URL, ""); err == nil {
		t.Fatal("missing machineIdentifier must fail")
	}
}
