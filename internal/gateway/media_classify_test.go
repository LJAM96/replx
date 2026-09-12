package gateway

import "testing"

func TestClassify(t *testing.T) {
	media := []string{
		"/library/parts/123/abc/file.mkv",
		"/video/:/transcode/universal/start.m3u8",
		"/video/:/transcode/segments/a.ts",
		"/video/:/transcode/session/abc123/manifest.mpd",
		"/music/:/transcode/universal/start.m3u8?path=x",
	}
	for _, p := range media {
		if !IsBulkMediaRoute(p) {
			t.Errorf("expected media route for %s", p)
		}
	}
	// Playback negotiation is CONTROL: the policy engine inspects it.
	control := []string{
		"/", "/identity", "/library/sections", "/library/metadata/1", "/hubs/home", "/:/timeline",
		"/video/:/transcode/universal/decision?path=%2Flibrary%2Fmetadata%2F1&mediaIndex=1",
		"/music/:/transcode/universal/decision?path=%2Flibrary%2Fmetadata%2F2",
		"/video/:/transcode/session/abc123/stop",
		"/video/:/transcode/statistics",
		"/some/future/non-media-route",
	}
	for _, p := range control {
		if IsBulkMediaRoute(p) {
			t.Errorf("expected control route for %s", p)
		}
	}
	// Unknown transcode-namespace paths DENY: uncertainty is not control.
	denied := []string{
		"/video/:/transcode/future/new-media-route",
		"/music/:/transcode/future/chunk",
		"/video/:/transcode/sessions/123/unknown",
		"/video/:/transcode/session/abc123/something-new",
	}
	for _, p := range denied {
		if Classify(p) != ActionDenyUnknownMedia {
			t.Errorf("expected deny for %s, got %v", p, Classify(p))
		}
		if IsBulkMediaRoute(p) {
			t.Errorf("denied path must not read as bulk media: %s", p)
		}
	}
}
