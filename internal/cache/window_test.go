package cache

import (
	"encoding/json"
	"net/url"
	"testing"
)

func TestCollectionWindowKeyPreservesUserAndFiltersPagination(t *testing.T) {
	base := url.Values{"includeMeta": {"1"}, "X-Plex-Container-Start": {"12"}, "X-Plex-Container-Size": {"24"}, "X-Plex-Token": {"secret"}, "X-Plex-Device-Screen-Resolution": {"832x1001"}, "pinnedContentDirectoryID": {"123"}}
	otherPage := url.Values{"includeMeta": {"1"}, "X-Plex-Container-Start": {"36"}, "X-Plex-Container-Size": {"39"}, "X-Plex-Device-Screen-Resolution": {"876x441"}, "pinnedContentDirectoryID": {"456"}}
	a := CollectionWindowKeyGen("tok:a", "collections", "GET", "/library/collections/1/children", base, "application/json", 2, 3)
	b := CollectionWindowKeyGen("tok:a", "collections", "GET", "/library/collections/1/children", otherPage, "application/json", 2, 3)
	if a != b {
		t.Fatal("browser display and sidebar context must share the collection window")
	}
	if a == CollectionWindowKeyGen("tok:b", "collections", "GET", "/library/collections/1/children", base, "application/json", 2, 3) {
		t.Fatal("one user's window cannot be used by another user")
	}
	otherPage.Set("includeMeta", "0")
	if a == CollectionWindowKeyGen("tok:a", "collections", "GET", "/library/collections/1/children", otherPage, "application/json", 2, 3) {
		t.Fatal("different fields must not share a window")
	}
}

func TestCollectionWindowPageSlicesOnlyCoveredRange(t *testing.T) {
	full := []byte(`{"MediaContainer":{"offset":0,"size":3,"totalSize":4,"Metadata":[{"title":"a"},{"title":"b"},{"title":"c"}],"librarySectionID":23}}`)
	body, ok := CollectionWindowPage(full, 1, 2)
	if !ok {
		t.Fatal("covered range rejected")
	}
	var got struct {
		MediaContainer struct {
			Offset           int `json:"offset"`
			Size             int `json:"size"`
			TotalSize        int `json:"totalSize"`
			LibrarySectionID int `json:"librarySectionID"`
			Metadata         []struct {
				Title string `json:"title"`
			} `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	c := got.MediaContainer
	if c.Offset != 1 || c.Size != 2 || c.TotalSize != 4 || c.LibrarySectionID != 23 || len(c.Metadata) != 2 || c.Metadata[0].Title != "b" || c.Metadata[1].Title != "c" {
		t.Fatalf("sliced response: %+v", c)
	}
	if _, ok := CollectionWindowPage(full, 2, 2); ok {
		t.Fatal("unfetched item must not be synthesized")
	}
	if _, ok := CollectionWindowPage([]byte(`{"MediaContainer":{"offset":1,"size":3,"totalSize":4,"Metadata":[1,2,3]}}`), 1, 2); ok {
		t.Fatal("a window not starting at zero must not be sliced")
	}
}
