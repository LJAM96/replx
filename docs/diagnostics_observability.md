# Diagnostics and Observability

## Goal

Every important policy and routing decision must be reconstructable without logging secrets or media bytes.

## Request and playback IDs

Every request receives a UUIDv7 `request_id`.

Playback related requests join a `playback_trace_id` using Plex session identifier, user, client, rating key and bounded time correlation.

## Trace levels

```text
normal
debug
protocol
```

Protocol traces are targeted and time limited.

## Event schema

```json
{
  "timestamp": "2026-09-12T00:30:00Z",
  "traceId": "uuid",
  "requestId": "uuid",
  "component": "playback.policy",
  "event": "candidate_rejected",
  "identityId": "uuid",
  "clientInstanceId": "uuid",
  "ratingKey": "12345",
  "data": {
    "mediaIndex": 0,
    "reason": "USER_MAX_SOURCE_RESOLUTION"
  }
}
```

## Routing trace

For media redirects capture sanitized values for:

```text
incoming path
requested part ID
selected allowed part ID
redirect status
origin host identity, not token
Range header presence
client follow up host
follow up HTTP status
manifest or progressive type
```

Never persist the token bearing Location URL unredacted.

## Storage

Trace metadata lives in PostgreSQL. Full protocol events are compressed NDJSON files under the diagnostics volume.

Default trace retention is 7 days. Playback decision retention is 30 days. Audit retention is 180 days.

Storage janitors enforce both age and configured disk budgets.

## Redaction

Always redact:

```text
X-Plex-Token
Authorization
Cookie
Set-Cookie
owner JWT
PMS access token
Tunnel token
REPLX_EDGE_SECRET_KEY
media redirect token query values
```

## Metrics

All metric names use the canonical valid prefix `replx_edge_`.

Minimum metrics:

```text
replx_edge_http_requests_total
replx_edge_http_request_duration_seconds
replx_edge_cache_hits_total
replx_edge_cache_misses_total
replx_edge_cache_entries
replx_edge_cache_bytes
replx_edge_origin_requests_total
replx_edge_origin_request_duration_seconds
replx_edge_origin_errors_total
replx_edge_active_playback_sessions
replx_edge_playback_decisions_total
replx_edge_policy_rejections_total
replx_edge_media_origin_redirects_total
replx_edge_media_gateway_routes_total
replx_edge_media_route_failures_total
replx_edge_sync_items_total
replx_edge_sync_errors_total
replx_edge_eventstream_connected
replx_edge_diagnostics_active
```

## Explainability

The admin UI must be able to answer:

```text
what the client requested
what Replx edge selected
why other variants were rejected
what PMS decided
whether transcode was permitted
where the media was routed
whether Range seeking succeeded
```
