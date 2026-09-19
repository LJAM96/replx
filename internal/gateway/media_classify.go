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
	// ActionControl is ordinary metadata/control traffic: proxy it.
	ActionControl MediaRouteAction = iota
	// ActionMediaRedirect is bulk media that must leave the control path.
	ActionMediaRedirect
	// ActionDenyUnknownMedia is an unrecognised path under a known media
	// namespace. Uncertainty is not control: in tunnel mode it fails
	// closed rather than risking bulk bytes through Cloudflare.
	ActionDenyUnknownMedia
)

// Classify returns the routing action for a path.
//
// Playback negotiation stays CONTROL: /video/:/transcode/universal/decision
// (and the music equivalent) is the small request the future policy engine
// inspects and rewrites. Known manifests/segments route as media. Anything
// else under a transcode namespace is DENIED, not proxied: a future Plex
// route carrying bytes must never inherit control transparency it was
// never granted. Truly unknown non-transcode routes stay CONTROL per the
// pass-through principle.
func Classify(path string) MediaRouteAction {
	p := strings.ToLower(path)
	switch {
	case strings.HasPrefix(p, "/library/parts/"):
		return ActionMediaRedirect
	case isDecisionOrControl(p):
		return ActionControl
	case isManifest(p):
		return ActionMediaRedirect
	case isSegment(p):
		return ActionMediaRedirect
	case isSessionControl(p):
		return ActionControl
	case inTranscodeNamespace(p):
		return ActionDenyUnknownMedia
	default:
		return ActionControl
	}
}

// inTranscodeNamespace reports paths under a recognised media namespace
// whose semantics were not positively classified above.
func inTranscodeNamespace(p string) bool {
	return strings.HasPrefix(p, "/video/:/transcode/") ||
		strings.HasPrefix(p, "/music/:/transcode/") ||
		strings.Contains(p, "/transcode/")
}

// isDecisionOrControl matches negotiation/control endpoints (small, inspectable).
func isDecisionOrControl(p string) bool {
	return strings.Contains(p, "/transcode/universal/decision") ||
		strings.Contains(p, "/transcode/universal/fallback") ||
		strings.Contains(p, "/transcode/session/decision")
}

// isManifest matches initial HLS/DASH manifest or playlist requests whose
// relative segments then resolve against the redirected origin host.
// universal/subtitles returns transcoded subtitle file bytes, so it routes
// as media rather than proxied control.
func isManifest(p string) bool {
	return strings.Contains(p, "/transcode/universal/start") ||
		strings.Contains(p, "/transcode/universal/direct") ||
		strings.Contains(p, "/transcode/universal/subtitles") ||
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

// isSessionControl matches only known transcode session verbs. Bare or
// novel session subpaths are NOT control: unknown semantics under a media
// namespace deny rather than inherit transparency.
func isSessionControl(p string) bool {
	return strings.Contains(p, "/transcode/stop") ||
		strings.Contains(p, "/transcode/universal/stop") ||
		strings.Contains(p, "/transcode/statistics")
}

// IsBulkMediaRoute reports whether path carries bulk media bytes.
func IsBulkMediaRoute(path string) bool {
	return Classify(path) == ActionMediaRedirect
}

// IsDecision reports playback negotiation endpoints: the small requests
// the policy engine inspects and rewrites. Distinct from session control
// (stop/statistics) and from bulk media.
func IsDecision(path string) bool {
	return isDecisionOrControl(strings.ToLower(path))
}

// IsSessionStop reports transcode session termination verbs used to close
// playback sessions.
func IsSessionStop(path string) bool {
	p := strings.ToLower(path)
	return strings.Contains(p, "/transcode/stop") ||
		strings.Contains(p, "/transcode/universal/stop")
}

// IsDeniedUnknownMedia reports unrecognised paths under a media namespace:
// fail closed in tunnel mode, never proxied as control.
func IsDeniedUnknownMedia(path string) bool {
	return Classify(path) == ActionDenyUnknownMedia
}
