# Replx Edge

## Purpose

Replx Edge is a Plex aware edge service for very large Plex deployments. It accelerates browsing, applies deterministic media source policy when the client uses the Replx Edge connection, records detailed diagnostics, and coordinates playback without replacing Plex Media Server.

Plex remains authoritative for media, library state, watch state, transcoding, authentication and playback state. Replx Edge owns acceleration, policy evaluation, routing decisions, compatibility handling and observability.

## Production 1.0 scope

Production 1.0 supports exactly one Plex Media Server origin per Replx Edge instance. The database remains server scoped so future multi server support does not require a destructive redesign, but the admin UI and runtime reject attempts to configure more than one active origin.

## Preferred deployment

```text
Official Plex app
        |
        | metadata, browse, search, state, playback negotiation
        v
plex.example.com
        |
        v
Cloudflare public hostname
        |
        v
Cloudflare Tunnel
        |
        v
cloudflared
        |
        v
Replx Edge
   |         |
   v         v
Postgres   Valkey
        |
        | origin API
        v
Plex Media Server

Preferred media path when validated for that client and protocol

Plex Media Server  ------------------------------>  Plex app
```

The zero inbound port claim applies to the Replx Edge VPS. It does not imply that the Plex Media Server origin is private. Direct origin media requires a client reachable HTTPS origin connection with a valid certificate.

Cloudflare Tunnel is the Replx Edge control plane. Bulk media is never intentionally streamed through a Cloudflare public hostname in Production 1.0.

## Important policy limitation

The default Cloudflare deployment is an acceleration and deterministic selection deployment, not a cryptographic enforcement boundary.

If an official Plex client can discover and connect directly to the origin PMS, it can bypass Replx Edge entirely. Replx Edge can guarantee policy only for requests that traverse Replx Edge.

Strict end to end enforcement requires control of the media path. That means either the origin is not independently reachable by clients or all media is delivered through a controlled media gateway. This is a separate deployment profile and is not promised for installations where the Replx Edge administrator has no control over the PMS host or network.

## Media routing decision

Direct origin routing is a P0 implementation spike, not a late optimisation. The first supported candidate is a `307 Temporary Redirect` from Replx Edge to a client reachable PMS HTTPS connection.

For progressive Direct Play, Replx Edge redirects the allowed `/library/parts/...` request to the selected origin part URL.

For HLS or DASH, Replx Edge redirects the initial manifest request to the origin. The origin then owns the manifest and relative segment resolution. Replx Edge does not rewrite individual segments in the first implementation.

Redirects use an explicit token query parameter derived from the requesting user context because cross host preservation of `X-Plex-Token` cannot be assumed. Range behaviour, redirect behaviour and token behaviour must be validated per official client.

If a client or protocol does not work with direct origin routing, the zero port Cloudflare deployment cannot silently send the video through the Tunnel. The operator must either enable the separate DNS only media fallback or accept that the client or playback mode is unsupported.

See `docs/adr_001_media_routing_and_origin_exposure.md`.

## Server identity

Replx Edge is not a separately claimed Plex Media Server.

The origin PMS publishes the Replx Edge Custom Server Access URL through Plex server discovery. Replx Edge preserves the origin PMS identity responses, including `machineIdentifier`. It must never invent a second PMS machine identifier.

Onboarding verifies that the selected Plex resource, origin root response and Replx Edge proxied root response all identify the same PMS.

## Owner authentication

Replx Edge uses Plex PIN authentication with the current JWT flow for its own owner connection. It stores the device key material and owner credentials encrypted with `REPLX_EDGE_SECRET_KEY`, then uses the owner context to discover the PMS resource and obtain the PMS access token required for indexing and synchronisation.

Client requests continue to use the actual client user token. Replx Edge never substitutes the owner token for a normal user request.

## Canonical identifiers

Product display name is `Replx Edge` everywhere in prose.

Use these exact technical identifiers everywhere:

```text
Product           Replx Edge
Go binary         replx-edge
Compose service   replx-edge
Container image   ghcr.io/<owner>/replx-edge
Go module         github.com/LJAM96/replx (repository path; binary/service stay replx-edge)
Metric prefix     replx_edge_
Database          replx_edge
Env prefix        REPLX_EDGE_
```

No identifier may contain spaces. Hyphens are used where underscores are illegal in reverse (binary, service, image, module); underscores are used where hyphens are illegal (metrics, database, env).

## Default Compose services

```text
cloudflared
replx-edge
postgres
valkey
```

No public host ports are required in the preferred profile. The admin listener binds to host loopback and should be reached through Tailscale or another private administration path.

## Documentation authority

The files under `docs/` and `deploy/` are canonical source documents.

`REPLX_EDGE_COMPLETE_SPEC.md` is generated from those canonical files by `scripts/build_complete_spec.py`. Do not edit the generated combined file directly.

## Documentation map

| Document | Purpose |
| --- | --- |
| `docs/product_overview.md` | Product scope, guarantees and non goals |
| `docs/system_architecture.md` | Runtime topology and trust boundaries |
| `docs/adr_001_media_routing_and_origin_exposure.md` | P0 media routing decision and spike |
| `docs/plex_owner_auth_and_discovery.md` | Owner auth, PMS discovery and identity preservation |
| `docs/review_resolution_2026_09_12.md` | Resolution of the pre implementation architecture review |
| `docs/technical_implementation_specification.md` | Core implementation contract |
| `docs/docker_compose_production.md` | Production Compose design |
| `docs/cloudflare_tunnel_deployment.md` | Tunnel deployment and Cloudflare rules |
| `docs/security_model.md` | Threat model and security controls |
| `docs/plex_protocol_route_matrix.md` | Plex route handling and translation points |
| `docs/cache_and_sync_specification.md` | Cache, index, sync and invalidation |
| `docs/playback_policy_media_selection.md` | Policy, normalization and media selection |
| `docs/database_schema.md` | PostgreSQL model, indexes and retention |
| `docs/admin_api_and_ui.md` | Admin REST API and UI |
| `docs/diagnostics_observability.md` | Tracing, logs, metrics and retention |
| `docs/client_compatibility_testing.md` | Real client compatibility programme |
| `docs/spike_runbook.md` | P0 307 spike procedure and decision gate |
| `docs/configuration_reference.md` | Environment and runtime configuration |
| `docs/operations_runbook.md` | Production operations |
| `docs/ci_release_backup_upgrade.md` | CI, releases, backup and upgrade |
| `docs/implementation_roadmap.md` | Ordered build phases |
| `docs/acceptance_criteria.md` | Definition of done |
| `deploy/media_fallback_design.md` | Optional non Cloudflare media gateway |

## Official references

Plex PMS API: https://developer.plex.tv/pms/

Plex Custom Server Access URLs: https://support.plex.tv/articles/200430283-network/

Cloudflare Tunnel: https://developers.cloudflare.com/tunnel/

Cloudflare public hostname routing and large file note: https://developers.cloudflare.com/tunnel/concepts/routing/

Cloudflare video delivery policy: https://developers.cloudflare.com/fundamentals/reference/policies-compliances/delivering-videos-with-cloudflare/
