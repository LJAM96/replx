// Package gateway classifies Plex routes for the Cloudflare media rule.
//
// In cloudflare_tunnel ingress mode Replx Edge must classify a media
// response before writing bulk bytes: route to origin, route to the media
// gateway, or fail with MEDIA_ROUTE_UNAVAILABLE. It must never silently
// proxy video through the Cloudflare control hostname.
package gateway

import "strings"

// MediaRouteAction is the routing decision for a request path.
type MediaRouteAction int

const (
	// ActionControl is ordinary metadata/control traffic.
	ActionControl MediaRouteAction = iota
	// ActionMediaRedirect is bulk media that must leave the control path.
	ActionMediaRedirect
)

// Classify returns ActionMediaRedirect for bulk media body routes.
//
// Playback negotiation stays CONTROL: /video/:/transcode/universal/decision
// (and the music equivalent) is the small request the future policy engine
// inspects and rewrites. Only manifests/segments/parts carrying bytes route
// as media. Unknown transcode subpaths fail closed to CONTROL (proxied,
// never treated as bulk) unless they match a known media pattern below.
func Classify(path string) MediaRouteAction {
	p := strings.ToLower(path)
	switch {
	case strings.HasPrefix(p, "/library/parts/"):
		return ActionMediaRedirect
	case isDecision(p):
		return ActionControl
	case isManifest(p):
		return ActionMediaRedirect
	case isSegment(p):
		return ActionMediaRedirect
	case isSessionControl(p):
		return ActionControl
	default:
		return ActionControl
	}
}

// isDecision matches playback negotiation endpoints (small, inspectable).
func isDecision(p string) bool {
	return strings.Contains(p, "/transcode/universal/decision") ||
		strings.Contains(p, "/transcode/session/decision")
}

// isManifest matches initial HLS/DASH manifest or playlist requests whose
// relative segments then resolve against the redirected origin host.
func isManifest(p string) bool {
	return strings.Contains(p, "/transcode/universal/start") ||
		strings.Contains(p, "/transcode/universal/direct") ||
		strings.Contains(p, "master.m3u8") ||
		strings.Contains(p, "manifest.mpd") ||
		strings.HasSuffix(p, ".m3u8") ||
		strings.HasSuffix(p, ".mpd")
}

// isSegment matches media segment and part byte routes.
func isSegment(p string) bool {
	return strings.Contains(p, "/transcode/segments") ||
		strings.Contains(p, "/transcode/sessions/") && strings.Contains(p, "/segments") ||
		strings.Contains(p, "/media/segments") ||
		strings.HasSuffix(p, ".ts") && strings.Contains(p, "transcode") ||
		strings.HasSuffix(p, ".m4s")
}

// isSessionControl matches transcode session/stop control calls.
func isSessionControl(p string) bool {
	return strings.Contains(p, "/transcode/sessions/") && !strings.Contains(p, "/segments") ||
		strings.Contains(p, "/transcode/stop") ||
		strings.Contains(p, "/transcode/statistics")
}

// IsBulkMediaRoute reports whether path carries bulk media bytes.
func IsBulkMediaRoute(path string) bool {
	return Classify(path) == ActionMediaRedirect
}
