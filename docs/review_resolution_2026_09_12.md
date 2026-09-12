# Design Review Resolution 2026 09 12

## Status

The design review is accepted. The documentation was revised before implementation.

## P0 resolutions

### Direct origin routing

Resolved by ADR 001 and moved into Alpha. The first routing candidate is HTTP 307 to a validated origin HTTPS connection. Progressive playback redirects the allowed raw part. HLS and DASH redirect the initial manifest so the origin owns subsequent relative segment resolution. Token propagation is explicit through a PMS accepted query token because cross host header preservation is not assumed. Range, seek, resume, TLS and manifest behaviour are mandatory compatibility tests.

### Origin reachability

The no public ports statement now applies only to the Replx Edge VPS. Direct origin media requires a client reachable HTTPS PMS connection. `REPLX_EDGE_ORIGIN_INTERNAL_URL` is only for Replx Edge to PMS traffic. A separate client media origin is selected and verified during owner onboarding. The default profile no longer claims strict enforcement when clients can independently reach PMS.

### PMS identity and discovery

Replx Edge does not claim a new PMS. The Custom Server Access URL is published by the real PMS as another connection for the same resource. Replx Edge preserves the origin `machineIdentifier`. Onboarding verifies PMS root, Plex resource and Replx Edge proxied root identity before continuing.

### Canonical identifiers

Canonical names are now `replx-edge` for the binary, Compose service, image and Go module suffix, `replx_edge_` for Prometheus metrics, `replx_edge` for the database, and `REPLX_EDGE_` for environment variables. Spaces are prohibited in generated identifiers.

### Owner authentication

A full Plex PIN plus JWT onboarding flow is specified. Replx Edge keeps a stable client identifier and Ed25519 device key, obtains the owner Plex credential, discovers the PMS resource and stores the PMS access credential encrypted with `REPLX_EDGE_SECRET_KEY`. Owner credentials are used only for indexing and administration.

## P1 resolutions

### Media index mapping

Production 1.0 no longer filters or reorders `Media` arrays. This removes the fragile client index mapping requirement. Replex instead rewrites the playback decision to the selected origin media index and validates the actual requested part or manifest at the media boundary. Metadata hiding is post 1.0 experimental work.

### Transcode denied

Fail closed. If an eligible source requires a PMS transcode and effective policy denies transcoding, return `POLICY_TRANSCODE_FORBIDDEN`. Never fall back to a prohibited larger source.

### HDR and Dolby Vision normalization

The policy specification now defines normalized values `SDR`, `HDR10`, `HDR10_PLUS`, `HLG`, `DOLBY_VISION`, `HDR_OTHER` and `UNKNOWN`, plus explicit unknown handling.

### Cache scope

User scoped response caching is the Production 1.0 default. The owner synchronized library index is shared internal data but is not directly returned as a user response. CI includes an explicit two user isolation test.

### Retention and indexes

Playback decisions, sessions, traces and audit data now have indexes and retention rules. Search receives FTS and trigram GIN indexes.

### Multi server scope

Production 1.0 supports one active PMS. Schema keys remain server scoped only for forward compatibility.

### Performance

Large hot path responses are streamed when unmodified. Non streaming transformation is capped at 8 MiB by default. Production 1.0 avoids whole library DOM filtering.

### Roadmap

Routing proof moved to Alpha before index, policy and cache. Metadata filtering left the 1.0 roadmap.

### Admin API

List endpoints use cursor pagination. Full sync is single flight. Expensive operations are rate limited. Policy and cache mutations are audited.

## P2 resolutions

The multi file documents under `docs/` and `deploy/` are canonical. `REPLX_EDGE_COMPLETE_SPEC.md` is generated with `scripts/build_complete_spec.py`. Client product version changes reduce learned compatibility certainty. Backup documentation emphasizes that loss of `REPLX_EDGE_SECRET_KEY` makes encrypted Plex owner credentials unrecoverable. The Cloudflare fail closed media rule remains unchanged.
