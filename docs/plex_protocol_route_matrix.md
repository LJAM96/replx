# Plex Protocol and Route Matrix

## Principle

Unknown Plex routes pass through unchanged unless the route would cause bulk media to traverse a forbidden ingress path.

Replex must preserve unknown Plex fields and must not parse and reserialize responses unless a feature requires transformation.

## Identity headers

Observe and forward Plex identity headers including:

```text
X-Plex-Client-Identifier
X-Plex-Token
X-Plex-Product
X-Plex-Version
X-Plex-Platform
X-Plex-Platform-Version
X-Plex-Device
X-Plex-Model
X-Plex-Device-Vendor
X-Plex-Device-Name
X-Plex-Session-Identifier
```

All `X-Plex-*` values may also appear as query parameters.

## Route matrix

| Route family | Cache | Scope | Transform | Policy | Media routing |
| --- | --- | --- | --- | --- | --- |
| `/` | Short | User | No | No | No |
| `/identity` | Short | User | No | No | No |
| `/media/providers` | Short | User | No | No | No |
| `/library/sections` | Medium | User | No | No | No |
| `/library/sections/*` | Medium | User | No in 1.0 | No | No |
| `/library/metadata/*` | Medium | User | No in 1.0 | Used for variant lookup | No |
| `/hubs/*` | Very short | User | No | No | No |
| Continue Watching | Very short | User | No | No | No |
| `/hubs/search` | Short or local | User | Result reconstruction | No | No |
| `/:/timeline` | Never | User | No | Session state | No |
| `/:/scrobble` | Never | User | No | State invalidation | No |
| `/:/unscrobble` | Never | User | No | State invalidation | No |
| `/status/sessions` | Never | Owner admin via admin API only | Deny on control with explanation | No | No |
| universal playback decision | Never | User | Query and response validation | Critical | No body media |
| transcode decision (`*/transcode/universal/decision`) | Never | User | Control: future policy inspection point, never bulk media | Critical | No |
| transcode session/stop control | Never | User | Control | Session state | No |
| play queue creation | Never | User | Observe + correlate; no queue rewrite in 1.0 | Correlation, enforced at part boundary | No |
| `/library/parts/*` | Never | User | Allowed part substitution only | Critical | Direct origin or media gateway |
| transcode start manifest | Never | User | Source query enforcement | Critical | Redirect initial manifest or media gateway |
| transcode segments | Never | User | No | Session validation | Origin or media gateway |
| artwork transcoder | Long | User | No | No | Control plane permitted |
| unknown non media | Never initially | User | No | No | Pass through |
| unknown large body media | Never | User | No | Fail closed in tunnel mode | Do not Cloudflare proxy |

## Playback decision translation points

Production 1.0 does not remove `Media` entries from browse metadata. This avoids client visible index translation races.

At universal playback decision:

1. identify requested rating key and origin media index
2. load current media variants from the owner index or user scoped origin metadata
3. evaluate policy
4. choose an allowed origin media index
5. rewrite the decision request `mediaIndex` if required
6. cap output bitrate and resolution when configured
7. send the request to PMS with the user's token
8. inspect PMS decision
9. fail closed if PMS requires a disallowed source or prohibited transcode
10. store the selected part and media IDs in the active playback session

## Raw part boundary

When `/library/parts/*` reaches Replx Edge:

1. identify the requested part ID
2. map it to the indexed media variant
3. resolve the current user, client and active playback session
4. reject the request if the part violates effective policy
5. if the request points at a prohibited part but an allowed selected part exists for the active session, route the allowed part instead
6. apply ADR 001 routing

This is the second enforcement boundary and protects against clients that ignore or retry around the playback decision. Auto-play-next that reuses a play queue without a fresh decision is still validated here: a prohibited part fails closed with `POLICY_ORIGIN_MISMATCH` rather than playing.

## Sessions and companion control

`GET /status/sessions` on the public control listener is owner-admin only. A normal user request receives `403` with a diagnostic reason and is never proxied to PMS with another user's context. Operators inspect sessions via `GET /api/v1/sessions` on the private admin listener, which uses the owner credential server side. Companion remote control across Replx Edge is unsupported in 1.0; browsing and playback are unaffected.

## Play queues

Play queue creation is observed for correlation (rating key, requested media index, session) but queue items are not rewritten in 1.0. Enforcement remains at the universal decision and the raw part or manifest boundary above.

## HLS and DASH

For transcode or Direct Stream manifests, enforce the selected source parameters before forwarding negotiation. The preferred direct route redirects the initial manifest to the origin. Segment rewriting is not a Production 1.0 requirement unless the compatibility spike proves it necessary.

## Range

`Range`, `If-Range`, `ETag`, `Last-Modified`, `Content-Range`, `Accept-Ranges` and `Content-Length` must be preserved in proxy mode.

For redirect mode, client preservation of Range semantics is a compatibility test, not an assumption.

## XML and JSON

Internal Replx Edge API calls should request JSON.

Official client responses preserve the requested representation. Production 1.0 avoids large browse response transformation, so most large responses can be streamed without DOM materialization.

## Transform budget

No hot path may parse an unbounded whole library response into a DOM.

Default maximum body size for non streaming transformations:

```text
8 MiB
```

Larger responses must either use a streaming parser designed for that route or bypass transformation.

## References

Plex PMS API: https://developer.plex.tv/pms/
