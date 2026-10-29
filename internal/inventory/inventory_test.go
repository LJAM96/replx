package inventory

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestInventoryPaginatesAndCountsNestedItems(t *testing.T) {
	const secret = "fixture-secret-never-output"
	var moviePages atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Plex-Token") != secret {
			t.Errorf("missing token header")
		}
		if r.URL.Query().Get("X-Plex-Container-Size") == "" {
			t.Errorf("missing page size")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/library/sections":
			_, _ = w.Write([]byte(`{"MediaContainer":{"totalSize":2,"Directory":[{"key":"1","title":"Films","type":"movie"},{"key":"2","title":"Shows","type":"show"}]}}`))
		case "/library/sections/1/all":
			switch r.URL.Query().Get("type") {
			case "1":
				o, _ := strconv.Atoi(r.URL.Query().Get("X-Plex-Container-Start"))
				moviePages.Add(1)
				if o == 0 {
					_, _ = w.Write([]byte(`{"MediaContainer":{"totalSize":3,"Metadata":[{"ratingKey":"m1"}]}}`))
				} else {
					_, _ = w.Write([]byte(`{"MediaContainer":{"totalSize":3,"Metadata":[{"ratingKey":"m3"}]}}`))
				}
			case "2":
				_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[{"ratingKey":"show1"}]}}`))
			case "3":
				_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[{"ratingKey":"season1"}]}}`))
			case "4":
				_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[{"ratingKey":"e1"},{"ratingKey":"e2"}]}}`))
			}
		case "/library/sections/1/collections":
			_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[{"ratingKey":"c1"}]}}`))
		case "/library/sections/2/collections":
			_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[]}}`))
		case "/library/sections/2/all":
			switch r.URL.Query().Get("type") {
			case "2":
				_, _ = w.Write([]byte(`{"MediaContainer":{"totalSize":1,"Metadata":[{"ratingKey":"show1"}]}}`))
			case "3":
				_, _ = w.Write([]byte(`{"MediaContainer":{"totalSize":1,"Metadata":[{"ratingKey":"season1"}]}}`))
			case "4":
				_, _ = w.Write([]byte(`{"MediaContainer":{"totalSize":2,"Metadata":[{"ratingKey":"e1"}]}}`))
			}
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(h)
	defer srv.Close()
	c, err := New(Config{BaseURL: srv.URL, Token: secret, PageSize: 2, Workers: 2, RequestTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	r := c.Inventory(context.Background())
	if r.Sections.Status != "measured" || r.Sections.Value != 2 {
		t.Fatalf("sections: %+v", r.Sections)
	}
	if r.Movies.Status != "measured" || r.Movies.Value != 3 {
		t.Fatalf("movies: %+v", r.Movies)
	}
	if r.Shows.Value != 1 || r.Seasons.Value != 1 || r.Episodes.Value != 2 || r.Collections.Value != 1 {
		t.Fatalf("report totals: %+v", r)
	}
	if moviePages.Load() != 1 {
		t.Fatalf("movie count requests=%d, want totalSize fast path", moviePages.Load())
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), secret) {
		t.Fatal("serialized report contains token")
	}
}

func TestCountFallsBackToPaginatedScanWithoutTotalSize(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Query().Get("X-Plex-Container-Size") == "1" {
			_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[{"ratingKey":"a"}]}}`))
			return
		}
		if r.URL.Query().Get("X-Plex-Container-Start") == "0" {
			_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[{"ratingKey":"a"},{"ratingKey":"b"}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[{"ratingKey":"c"}]}}`))
	}))
	defer srv.Close()
	c, err := New(Config{BaseURL: srv.URL, Token: "secret", PageSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	got := c.countItems(context.Background(), "/library/sections/1/all", nil)
	if got.Status != "measured" || got.Value != 3 {
		t.Fatalf("count=%+v", got)
	}
	if requests.Load() != 3 {
		t.Fatalf("requests=%d, want 3", requests.Load())
	}
}

func TestPaginationStopsWhenServerRepeatsPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[{"ratingKey":"same1"},{"ratingKey":"same2"}]}}`))
	}))
	defer srv.Close()
	c, err := New(Config{BaseURL: srv.URL, Token: "secret", PageSize: 2, MaxPages: 20})
	if err != nil {
		t.Fatal(err)
	}
	items, err := c.list(context.Background(), "/library/sections/1/all", nil)
	if err == nil || !strings.Contains(err.Error(), "pagination did not advance") {
		t.Fatalf("expected pagination guard, got %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("partial results=%d, want first page retained", len(items))
	}
}

func TestRedirectDoesNotForwardToken(t *testing.T) {
	var received atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Store(r.Header.Get("X-Plex-Token") != "")
		_, _ = w.Write([]byte(`{"MediaContainer":{"Directory":[]}}`))
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer redirect.Close()
	c, err := New(Config{BaseURL: redirect.URL, Token: "sensitive"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.list(context.Background(), "/library/sections", nil)
	if err == nil {
		t.Fatal("expected redirect status to fail")
	}
	if received.Load() {
		t.Fatal("token forwarded to redirect target")
	}
}

func TestNewRequiresHTTPSExceptLoopback(t *testing.T) {
	for _, tc := range []struct {
		url string
		ok  bool
	}{{"https://plex.example", true}, {"http://127.0.0.1:32400", true}, {"http://plex.example", false}, {"https://user:pass@plex.example", false}, {"https://plex.example/?X-Plex-Token=x", false}} {
		_, err := New(Config{BaseURL: tc.url, Token: "x"})
		if (err == nil) != tc.ok {
			t.Errorf("New(%q) error=%v, want ok=%v", tc.url, err, tc.ok)
		}
	}
}

func TestFailedSectionDoesNotBecomeZeroCounts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream echoed token", http.StatusUnauthorized)
	}))
	defer srv.Close()
	c, err := New(Config{BaseURL: srv.URL, Token: "sensitive"})
	if err != nil {
		t.Fatal(err)
	}
	r := c.Inventory(context.Background())
	if r.Sections.Status != "error" || r.Movies.Status != "error" {
		t.Fatalf("expected explicit errors: %+v", r)
	}
	b, _ := json.Marshal(r)
	if strings.Contains(string(b), "sensitive") || strings.Contains(string(b), "echoed") {
		t.Fatalf("unsafe error output: %s", b)
	}
}

func TestCollectionQueryDoesNotCarrySecretsInURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := url.ParseRequestURI(r.RequestURI); err != nil {
			t.Error(err)
		}
		if strings.Contains(r.RequestURI, "secret") {
			t.Fatal("token appeared in URL")
		}
		_, _ = w.Write([]byte(`{"MediaContainer":{"Directory":[]}}`))
	}))
	defer srv.Close()
	c, _ := New(Config{BaseURL: srv.URL, Token: "secret"})
	_, _ = c.list(context.Background(), "/library/sections", nil)
}
