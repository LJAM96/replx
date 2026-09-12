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
func Classify(path string) MediaRouteAction {
	p := strings.ToLower(path)
	switch {
	case strings.HasPrefix(p, "/library/parts/"):
		return ActionMediaRedirect
	case strings.HasPrefix(p, "/video/:/transcode/"):
		return ActionMediaRedirect
	case strings.HasPrefix(p, "/music/:/transcode/"):
		return ActionMediaRedirect
	case strings.Contains(p, "/transcode/segments"),
		strings.Contains(p, "/transcode/sessions"):
		return ActionMediaRedirect
	default:
		return ActionControl
	}
}

// IsBulkMediaRoute reports whether path carries bulk media bytes.
func IsBulkMediaRoute(path string) bool {
	return Classify(path) == ActionMediaRedirect
}
