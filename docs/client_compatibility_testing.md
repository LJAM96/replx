# Client Compatibility Testing

## Purpose

Plex client routing behaviour is empirical. replx-edge must not declare support from API theory alone.

## Priority clients

At minimum test:

```text
Plex Web
iOS
tvOS
Android
Android TV
LG webOS
Samsung TV
Roku
```

Only clients actually available to the project need block the initial release, but untested clients remain `UNKNOWN`.

## Controlled library

Use deterministic fixtures:

```text
Movie A: 4K + 1080p
Movie B: 1080p only
Movie C: 4K only
Movie D: HDR10 + SDR
Movie E: Dolby Vision + HDR10 + SDR where available
Movie F: source requiring transcode on selected test client
```

## P0 routing tests

For each client and playback type capture:

```text
follows 307 cross host redirect
accepts origin TLS
accepts token query authentication
preserves or reissues Range correctly
seeks successfully
resumes successfully
HLS initial manifest redirect
HLS segment origin
DASH initial manifest redirect when applicable
subtitle behaviour
```

## Compatibility dimensions

Track separately:

```text
progressive Direct Play
Direct Stream
HLS transcode
DASH transcode
subtitle sidecar
Range seek
```

Do not use one global `direct origin supported` boolean for every protocol.

## States

```text
UNKNOWN
SUPPORTED
UNSUPPORTED
DEGRADED
ADMIN_FORCED
```

## Automatic learning

Do not mark a strategy unsupported from a single generic network timeout.

Default automatic downgrade threshold:

```text
3 reproducible failures
across at least 2 playback attempts
```

Explicit protocol rejection can be classified earlier.

A significant product version change resets or reduces certainty for learned compatibility. Store the observed product version with each profile.

## Version regression

CI fixture tests cannot replace real client regression. Before a major replx-edge release, repeat at least the supported critical path on the current versions of the project's primary clients.
