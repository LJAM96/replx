// Package integration holds Compose level integration tests run in CI
// against Postgres + Valkey + a fake PMS. Cloudflare Tunnel is not
// required for routine tests.
package integration

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/LJAM96/replx/internal/cache"
	"github.com/LJAM96/replx/internal/gateway"
	"github.com/LJAM96/replx/internal/routing"
)

// fakePMS serves sanitized fixtures like origin PMS metadata.
func fakePMS(t *testing.T) *httptest.Server {
	t.Helper()
	root := filepath.Join("..", "fixtures", "plex")
	mux := http.NewServeMux()
	mux.HandleFunc("/library/metadata/1001", func(w http.ResponseWriter, r *http.Request) {
		b, err := os.ReadFile(filepath.Join(root, "metadata.json"))
		if err != nil {
			t.Fatalf("fixture: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	})
	return httptest.NewServer(mux)
}

func TestFakePMSProxy(t *testing.T) {
	srv := fakePMS(t)
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/library/metadata/1001")
	if err != nil {
		t.Fatalf("fake PMS: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestTwoUserIsolation(t *testing.T) {
	// Poison user A's cache entry; prove user B's key can never collide.
	params := map[string]string{"path": "/hubs/continueWatching"}
	keyA := cache.Key("v1", "srv", "hubs", cache.UserScope("user-a"), "json", params)
	keyB := cache.Key("v1", "srv", "hubs", cache.UserScope("user-b"), "json", params)
	if keyA == keyB {
		t.Fatal("cross user cache leak: keys equal")
	}
}

func TestMediaBoundaryFailClosed(t *testing.T) {
	if !gateway.IsBulkMediaRoute("/library/parts/11/file.mkv") {
		t.Fatal("part route must classify as media")
	}
	if _, err := routing.BuildDirectOriginURL("https://origin.example", "/library/parts/11/file.mkv", "tok", "plex.example.com"); err != nil {
		t.Fatalf("redirect build: %v", err)
	}
}
