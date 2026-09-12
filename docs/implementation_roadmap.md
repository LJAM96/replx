# Implementation Roadmap

## Alpha: Foundations, owner auth, discovery and routing spike

Implement:

```text
Docker Compose
replx-edge service
PostgreSQL
Valkey
cloudflared service
admin login
Plex PIN and JWT owner onboarding
PMS resource selection
REPLX_EDGE_ORIGIN_INTERNAL_URL validation
machineIdentifier preservation
Custom Server Access URL verification
transparent non media proxy
P0 media routing spike
```

The routing spike tests progressive and HLS against Plex Web and at least one physical TV client.

### Alpha acceptance

* Replx Edge and origin report the same PMS identity
* Plex resources show the Replx Edge custom connection for that PMS
* Plex Web can browse through Replx Edge
* one physical Plex client can browse through Replx Edge
* at least one end to end media routing strategy works without sending video through the Cloudflare public hostname
* Range seeking is tested for progressive playback
* exact trace evidence is captured

If Alpha cannot establish a compliant media path, revise the deployment architecture before continuing.

## Beta: Protocol observability

Implement route classification, user fingerprinting, client identity, request IDs, playback trace IDs, redaction and targeted protocol capture.

## Gamma: Persistent Plex index

Implement owner synchronized libraries, items, GUIDs, media variants, parts, streams, dynamic range normalization, PMS event consumer and reconciliation.

## Delta: Policy engine

Implement global, user and device policy, capability precedence, deterministic variant eligibility and ranking, transcode deny semantics and policy explanation.

No metadata `Media` filtering.

## Epsilon: Playback enforcement

Intercept universal playback decisions, rewrite origin media index, validate PMS result, persist selected media and part, and enforce again at raw part or manifest boundary.

## Zeta: User scoped cache

Implement Valkey browse, metadata, collection, home and Continue Watching caches with strict user isolation and invalidation.

## Eta: Search and artwork

Implement local search candidates with user visibility fallback and filesystem artwork cache.

## Theta: Optional media gateway

Only if real client tests require it, implement the DNS only media gateway. It remains outside Cloudflare and behind a separate explicit Compose profile.

## Iota: Production administration

Complete users, devices, policy editor, cache browser, storage, playback explanation, compatibility status and targeted diagnostics.

## Kappa: Hardening and release

Complete load testing, backup restore, retention tests, security review, ARM64 and AMD64 images, upgrade tests and documentation.

## Post 1.0 experiment

Metadata hiding and media array filtering may be investigated only after the enforcement path is stable. Any design must account for cached metadata, play queues, concurrent sessions and every index translation point.
