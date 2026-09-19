// Package metrics defines the canonical Prometheus metric names.
// All names use the replx_edge_ prefix. No identifier contains spaces.
package metrics

// Canonical metric names (minimum Production 1.0 set).
const (
	HTTPRequestsTotal            = "replx_edge_http_requests_total"
	HTTPRequestDurationSeconds   = "replx_edge_http_request_duration_seconds"
	CacheHitsTotal               = "replx_edge_cache_hits_total"
	CacheMissesTotal             = "replx_edge_cache_misses_total"
	CacheWarmedTotal             = "replx_edge_cache_warmed_total"
	CacheWarmErrorsTotal         = "replx_edge_cache_warm_errors_total"
	CacheEntries                 = "replx_edge_cache_entries"
	CacheBytes                   = "replx_edge_cache_bytes"
	OriginRequestsTotal          = "replx_edge_origin_requests_total"
	OriginRequestDurationSeconds = "replx_edge_origin_request_duration_seconds"
	OriginErrorsTotal            = "replx_edge_origin_errors_total"
	ActivePlaybackSessions       = "replx_edge_active_playback_sessions"
	PlaybackDecisionsTotal       = "replx_edge_playback_decisions_total"
	PolicyRejectionsTotal        = "replx_edge_policy_rejections_total"
	MediaOriginRedirectsTotal    = "replx_edge_media_origin_redirects_total"
	MediaGatewayRoutesTotal      = "replx_edge_media_gateway_routes_total"
	MediaRouteFailuresTotal      = "replx_edge_media_route_failures_total"
	SyncItemsTotal               = "replx_edge_sync_items_total"
	SyncErrorsTotal              = "replx_edge_sync_errors_total"
	EventstreamConnected         = "replx_edge_eventstream_connected"
	DiagnosticsActive            = "replx_edge_diagnostics_active"
)
