package origin

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAPIClientRefusesCrossOriginRedirect(t *testing.T) {
	var sawToken bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawToken = r.Header.Get("X-Plex-Token") != ""
		_, _ = w.Write([]byte("evil"))
	}))
	defer other.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusFound)
	}))
	defer origin.Close()

	client, err := APIClient(origin.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodGet, origin.URL+"/security/token", nil)
	req.Header.Set("X-Plex-Token", "user-secret")
	_, err = client.Do(req)
	if err == nil || !strings.Contains(err.Error(), "cross-origin") {
		t.Fatalf("cross-origin redirect must fail closed, err=%v", err)
	}
	if sawToken {
		t.Fatal("credential must never reach the foreign host")
	}
}

func TestAPIClientFollowsSameOriginRedirect(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/old" {
			http.Redirect(w, r, "/new", http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("here"))
	}))
	defer origin.Close()

	client, err := APIClient(origin.URL, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Get(origin.URL + "/old")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "here" {
		t.Fatalf("same-origin redirect must be followed: %q", body)
	}
}

func TestTransparentClientReturnsRedirectUntouched(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://elsewhere.example/x", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	resp, err := TransparentClient(5 * time.Second).Get(origin.URL + "/media")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("redirect must pass through, got %d", resp.StatusCode)
	}
}
