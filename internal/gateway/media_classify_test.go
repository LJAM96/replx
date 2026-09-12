package gateway

import "testing"

func TestClassify(t *testing.T) {
	media := []string{
		"/library/parts/123/abc/file.mkv",
		"/video/:/transcode/universal/start.m3u8",
		"/video/:/transcode/segments/a.ts",
	}
	for _, p := range media {
		if !IsBulkMediaRoute(p) {
			t.Errorf("expected media route for %s", p)
		}
	}
	control := []string{"/", "/identity", "/library/sections", "/library/metadata/1", "/hubs/home", "/:/timeline"}
	for _, p := range control {
		if IsBulkMediaRoute(p) {
			t.Errorf("expected control route for %s", p)
		}
	}
}
