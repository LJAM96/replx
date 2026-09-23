package identity

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPMSValidatorAcceptsOnlyAuthenticatedSections(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/library/sections" {
			t.Errorf("wrong validation path: %s", r.URL.Path)
		}
		switch r.Header.Get("X-Plex-Token") {
		case "valid":
			w.WriteHeader(http.StatusOK)
		case "broken":
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer origin.Close()
	v, err := NewPMSValidator(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		token string
		valid bool
		err   bool
	}{{"valid", true, false}, {"invalid", false, false}, {"broken", false, true}} {
		valid, err := v.ValidateToken(context.Background(), tc.token)
		if valid != tc.valid || (err != nil) != tc.err {
			t.Fatalf("token %s: valid=%v err=%v", tc.token, valid, err)
		}
	}
}
