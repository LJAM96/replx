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

// TestDocumentedUniversalEndpoints pins the complete currently documented
// PMS universal transcode surface for both media types, so future
// classifier edits cannot silently block (or proxy) a known endpoint.
func TestDocumentedUniversalEndpoints(t *testing.T) {
	cases := []struct {
		path string
		want MediaRouteAction
	}{
		// Negotiation / control (small, inspectable, proxied).
		{"/video/:/transcode/universal/decision", ActionControl},
		{"/music/:/transcode/universal/decision", ActionControl},
		{"/video/:/transcode/universal/fallback", ActionControl},
		{"/music/:/transcode/universal/fallback", ActionControl},
		{"/video/:/transcode/universal/stop", ActionControl},
		{"/music/:/transcode/universal/stop", ActionControl},
		// Byte-carrying routes (leave the Tunnel control path).
		{"/video/:/transcode/universal/start.m3u8", ActionMediaRedirect},
		{"/video/:/transcode/universal/start.mpd", ActionMediaRedirect},
		{"/music/:/transcode/universal/start.m3u8", ActionMediaRedirect},
		{"/video/:/transcode/universal/direct/abc/file.mp4", ActionMediaRedirect},
		{"/video/:/transcode/universal/subtitles", ActionMediaRedirect},
		{"/music/:/transcode/universal/subtitles", ActionMediaRedirect},
	}
	for _, tc := range cases {
		if got := Classify(tc.path); got != tc.want {
			t.Errorf("%s: want %v, got %v", tc.path, tc.want, got)
		}
	}
}
