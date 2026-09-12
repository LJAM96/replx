# ADR 001: Media Routing and Origin Exposure

## Status

Accepted as the P0 implementation decision for the first routing spike. Client compatibility remains empirical and may revise the chosen strategy before Production 1.0.

## Context

Cloudflare Tunnel is the default public replx-edge ingress, but Cloudflare public hostname routes must not be used as a general Plex video delivery path on Free, Pro or Business plans without an appropriate paid video service.

Therefore Replex needs a media path that leaves the Cloudflare hostname before bulk media begins.

The earlier architecture deferred this question too late. It is now part of Alpha because even a transparent proxy demonstration must have a compliant playback path.

## Origin exposure model

The zero port claim applies only to the replx-edge VPS.

Direct origin media requires a client reachable PMS HTTPS connection. The selected origin connection must have a certificate accepted by official Plex clients. A self signed certificate is not a supported public media target.

replx-edge distinguishes two concepts:

```text
REPLX_EDGE_ORIGIN_INTERNAL_URL
```

This is the URL replx-edge itself uses to reach PMS for API calls. It can be private, Tailscale, LAN or public as long as the VPS can reach it.

```text
Client reachable origin connection
```

This is selected from the origin PMS connection URLs discovered during owner onboarding through Plex resources and is used only for direct origin media routing. replx-edge must select a non replx-edge HTTPS connection for the same PMS resource. It is not assumed to equal `REPLX_EDGE_ORIGIN_INTERNAL_URL`.

The implementation must not call this second value `REPLX_EDGE_ORIGIN_URL` because that conflates two different network perspectives.

## Server discovery model

The origin PMS remains the claimed Plex resource. Replx Edge is an alternate connection to that same resource, advertised through the PMS Custom Server Access URLs setting.

replx-edge proxies `/`, `/identity` and related identity information from the origin and preserves the origin `machineIdentifier`.

replx-edge does not register a second machine identifier with plex.tv.

Onboarding must verify:

```text
origin root machineIdentifier
=
selected plex.tv resource clientIdentifier or machine identifier
=
replx-edge proxied root machineIdentifier
```

The exact resource field name returned by the current Plex resources API should be treated according to the current API response, but all three views must refer to the same PMS resource.

## Direct Play routing candidate

The first candidate is a `307 Temporary Redirect`.

A permitted raw part request arriving at replx-edge:

```text
GET /library/parts/{partId}/...
Range: bytes=...
```

is translated to a client reachable origin HTTPS URL for the selected allowed part.

Response candidate:

```text
HTTP/1.1 307 Temporary Redirect
Location: https://<client-reachable-origin>/library/parts/{allowedPartId}/...?X-Plex-Token=<delegated-or-user-token>
Cache-Control: no-store
```

`307` is chosen because it preserves method semantics. Range preservation across the redirect is client dependent and must be validated.

## Token propagation

Do not rely on official clients to resend `X-Plex-Token` as a header to a different host.

For the routing spike, replx-edge explicitly places a PMS accepted token in the redirect URL query string.

Preferred token source:

1. request a PMS transient delegation token from the user's existing PMS token when the endpoint is supported
2. otherwise use the same user scoped PMS token already presented by that client for the minimum duration necessary for the spike

Never put the replx-edge owner token into a client media redirect.

Transient PMS tokens have the same access level as the caller and are not path scoped. They reduce persistence but do not create a strict media capability. Treat them as secrets.

Redirect responses and logs must redact token query values.

## Progressive playback

For Direct Play of progressive media, redirect the raw part request to the allowed part at the client reachable origin.

The spike must validate:

* client follows cross host `307`
* client accepts origin certificate
* token query authentication works
* `Range` survives seeking and resume
* repeated ranges remain on the origin host
* playback accounting and timeline updates continue

## HLS and DASH

Do not rewrite individual media segments in the first implementation.

Redirect the initial transcode or direct stream manifest request to the origin. If the official client accepts the redirect, the manifest is fetched from the origin and relative segment URLs resolve against the origin host.

The spike must separately test:

```text
HLS master manifest
HLS media playlist
HLS segments
DASH manifest
DASH segments
subtitle resources
```

If PMS emits absolute URLs or tokens in manifests, capture and document the actual behaviour. Do not assume it.

## Manifest rewriting

Manifest rewriting is not part of the first supported path.

It may be introduced later only when a specific client requires it and a trace proves the required transformation.

## Unsupported direct routing

In `cloudflare_tunnel` ingress mode, Replex must not fall back by streaming bulk media through the Cloudflare control hostname.

If direct origin routing fails:

```text
media fallback enabled  -> use separate DNS only media gateway
media fallback disabled -> fail with MEDIA_ROUTE_UNAVAILABLE
```

## Policy guarantee

A direct origin that is discoverable and usable by the official client means the client may bypass replx-edge. Therefore the zero port profile does not claim hard enforcement.

Hard enforcement requires an architecture in which the client cannot choose an unrestricted origin connection.

## Alpha spike matrix

Before the library index or policy engine is considered stable, validate at least:

```text
Plex Web progressive Direct Play
one physical TV client progressive Direct Play
Plex Web HLS transcode
one physical TV client HLS transcode or Direct Stream
seek and resume with Range
```

Record exact request and response traces.

## Decision gate

If no reliable client reachable media path can be established without routing video through Cloudflare, the zero port Cloudflare profile must be reclassified as browse only for that client. Do not proceed by violating the media plane rule.
