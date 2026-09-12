# Optional Media Fallback Deployment

> Alpha status: the `media-gateway` Compose profile runs a placeholder
> (health endpoint only). Enabling it today opens the listener but serves
> no media; capability validation, session lookup, policy enforcement and
> streaming land with the media gateway phase. Do not rely on it yet.

## Purpose

The media gateway exists only for clients or playback protocols that cannot use validated direct origin media routing.

It is outside Cloudflare and therefore requires a public inbound TLS endpoint.

## Security boundary

The gateway exposes only media related routes needed by active playback sessions.

It does not expose:

```text
admin API
library browsing
search
PostgreSQL
Valkey
general Plex API proxying
```

## Authorization

A gateway request must be associated with an active Replex playback session and the requested part or manifest must match the media variant selected by policy.

Optional signed capability URLs may be used, but the gateway must still validate session and policy state where practical.

## TLS

Use a publicly trusted certificate for `media.example.com`. Do not rely on self signed TLS for official TV clients.

## Cloudflare

The media hostname must be DNS only and must not be orange cloud proxied through a standard Cloudflare public hostname route.

## Failure

If neither direct origin nor media gateway is available, return `MEDIA_ROUTE_UNAVAILABLE`. Do not route through the Cloudflare control hostname.
