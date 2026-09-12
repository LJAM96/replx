# Acceptance Criteria

## P0 architecture acceptance

Production work beyond Alpha does not proceed until:

* owner Plex authentication succeeds through PIN and JWT onboarding
* one PMS resource is selected
* origin root and Replex root expose the same `machineIdentifier`
* Replex Custom Server Access URL appears for the same PMS resource
* the internal origin URL is distinct in configuration from client reachable media connections
* at least one browser and one physical Plex client have completed a routing trace

## Cloudflare acceptance

In `cloudflare_tunnel` ingress mode:

* Replex VPS exposes no public control port
* bulk media is never intentionally written through the Cloudflare control response
* unsupported routing produces `MEDIA_ROUTE_UNAVAILABLE` or uses the explicit media gateway

## Jodie policy acceptance

Given 4K and 1080p variants:

```text
Jodie max source = 1080p
```

Replex selects 1080p during playback negotiation.

If the client later requests the 4K part directly, the media boundary rejects or substitutes it with the already selected allowed part.

If Jodie target bandwidth is lower than the 1080p source and transcoding is allowed, PMS transcodes the 1080p source.

If transcoding is denied and the eligible source cannot play without transcode, playback fails with `POLICY_TRANSCODE_FORBIDDEN`.

## Luke acceptance

On a validated 4K capable client, 4K may be selected.

On a known 1080p limited client, 1080p is selected even though Luke allows 4K.

## HDR acceptance

Normalization tests cover Dolby Vision identifiers, HDR10, HDR10 Plus, HLG, SDR and unknown metadata.

A policy must not deny an unknown dynamic range unless `unknownDynamicRangeBehavior=deny`.

## Cache isolation acceptance

A two user integration test must prove that one user's watched state, Continue Watching, home hubs and restricted libraries never appear in another user's cache response.

## Routing acceptance

For each supported playback type, record whether the client:

```text
follows 307
accepts origin TLS
accepts token query auth
seeks with Range
resumes
keeps HLS or DASH segments on origin
```

Compatibility is stored per client product version and playback type.

## Performance acceptance

Warm cache internal processing targets:

```text
p50 < 5 ms
p95 < 15 ms
```

Uncached proxy processing overhead target excluding origin latency:

```text
p95 < 10 ms
```

Search target for a warm indexed query:

```text
p95 < 100 ms
```

These targets are only valid when large response hot paths stream rather than DOM parse. CI and benchmarks must report body sizes alongside latency.

## Scale benchmark

Synthetic benchmark at minimum:

```text
100,000 movies
25,000 shows
500,000 episodes
multiple variants where practical
```

Throughput targets on the reference VPS (2 vCPU, 4 GiB):

```text
initial owner sync >= 1,000 items per minute, resumable after restart
full consistency sweep of the benchmark corpus < 6 hours
```

## Retention acceptance

A test database with expired playback decisions, sessions and traces must be reduced by the retention worker without removing active sessions or unexpired audit data.

## Admin acceptance

List APIs paginate. Full sync is single flight. Server test and sync endpoints are rate limited. Every policy and cache mutation writes an audit event.

## MVP definition

MVP contains Alpha through Epsilon plus a minimal admin UI. It does not require metadata cache acceleration.

## Production 1.0 definition

Production 1.0 contains the supported Cloudflare control path, validated media routing for supported clients, owner sync, policy enforcement, user scoped cache, search, admin UI, diagnostics, retention, backups, upgrades and versioned Docker images.
