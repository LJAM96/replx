# System Architecture

## Preferred production topology

```text
                                  plex.tv
                                     |
                              resource discovery
                                     |
                                     v
                              Official Plex app
                                     |
                                     | control traffic
                                     v
                             plex.example.com
                                     |
                                     v
                                Cloudflare
                                     |
                              Tunnel transport
                                     |
                                     v
                                cloudflared
                                     |
                                     v
                                  Replx Edge
                         +-----------+-----------+
                         |                       |
                         v                       v
                    PostgreSQL                Valkey
                         |
                         | API traffic via internal origin URL
                         v
                       Plex PMS

Validated direct media path

Plex PMS client reachable HTTPS connection  ------------>  Plex app
```

The Replx Edge VPS needs no public inbound port in the preferred profile. The PMS origin may still be publicly reachable because direct origin media requires it.

## Network perspectives

Never use one ambiguous origin URL for both server to server and client to server traffic.

```text
REPLX_EDGE_ORIGIN_INTERNAL_URL
```

is from Replx Edge to PMS.

The client reachable origin connection is discovered and validated separately for media redirects.

## Discovery and identity

Replex is an alternate connection to the origin PMS, not a separate PMS.

The origin PMS Custom Server Access URL publishes the Replx Edge hostname to plex.tv. Replx Edge preserves the origin root and identity values including `machineIdentifier`.

This avoids creating two logical servers. There can still be multiple connection URLs for the same PMS resource, which is ordinary Plex discovery behaviour.

## Enforcement boundary

If the client can independently use another connection URL for the same PMS, it can bypass Replx Edge.

Therefore:

```text
zero port Cloudflare profile = deterministic policy on Replx Edge traversing requests
strict enforcement profile = requires network or media path control beyond the default profile
```

Do not label the default profile strict enforcement.

## Media plane

Cloudflare public hostname routes are not the media plane.

Initial direct routing behaviour is defined by ADR 001.

Progressive media uses a candidate `307` redirect to an allowed origin part.

HLS and DASH use a candidate redirect of the initial manifest to the origin so subsequent relative resources resolve there.

If a client fails this path, use the optional media gateway or fail. Never stream bulk media through the Cloudflare control hostname as an implicit fallback.

## Listeners

### Replx Edge control

```text
0.0.0.0:32400 inside Docker
```

Reachable from `cloudflared` over the Docker network. Not mapped to a public host port.

### Replx Edge admin

```text
0.0.0.0:8080 inside Docker
```

Mapped to host loopback only:

```text
127.0.0.1:8080
```

### Optional media gateway

Disabled by default.

When enabled it is public, DNS only, outside Cloudflare, TLS protected, and exposes only signed or policy validated media routes.

## Data stores

PostgreSQL stores persistent configuration, owner credentials metadata, identities, clients, policies, library index, variants, compatibility, playback explanations, audit events and sync state.

Valkey stores short lived user scoped response cache, locks, active playback correlation, capability observations and routing state.

The filesystem stores artwork and compressed diagnostic traces.

## Production 1.0 scale

One Replx Edge instance supports one active PMS origin. Horizontal application replicas and multiple active origins are future work.
