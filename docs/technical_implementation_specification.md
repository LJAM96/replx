# Replx Edge Technical Implementation Specification

## Status

Ready for implementation after the Alpha P0 routing spike validates at least one browser client and one physical TV client.

The architecture is intentionally gated on that spike. A failed spike changes the supported deployment profile before later features are built.

## Technology

```text
Backend          Go
Admin frontend   Svelte with TypeScript
Persistent DB    PostgreSQL
Hot cache        Valkey
Deployment       Docker Compose
Public control   Cloudflare Tunnel
Admin access     loopback plus Tailscale or equivalent
```

## Canonical names

```text
binary           replx-edge
Compose service  replx-edge
metrics prefix   replx_edge_
database         replx_edge
env prefix       REPLX_EDGE_
```

No generated directory, Go package, metric or service identifier may contain spaces. Binary, service, image and module use hyphens (`replx-edge`); metrics, database and env use underscores (`replx_edge_`, `replx_edge`, `REPLX_EDGE_`).

## Repository structure

```text
replx-edge/
  cmd/replx-edge/
  internal/admin/
  internal/auth/
  internal/cache/
  internal/capability/
  internal/config/
  internal/database/
  internal/diagnostics/
  internal/devices/
  internal/events/
  internal/gateway/
  internal/health/
  internal/identity/
  internal/library/
  internal/metrics/
  internal/playback/
  internal/plex/
  internal/policy/
  internal/routing/
  internal/search/
  internal/storage/
  internal/sync/
  migrations/
  web/admin/
  deploy/
  docs/
  scripts/
```

The Go module path is the repository location `github.com/LJAM96/replx`. The binary, Compose service, image and user-facing names stay `replx-edge` / Replx Edge; only the module/import path follows the repo.

## Production 1.0 scope

One active PMS origin per Replx Edge instance.

Unknown non media Plex routes pass through.

Browse metadata is not filtered to hide media variants in Production 1.0.

Playback policy is enforced at universal playback negotiation and again at the media part or manifest boundary.

## Owner authentication

Implement Plex PIN authentication using the current JWK and JWT flow.

Persist a stable Replx Edge client identifier and Ed25519 keypair. Encrypt private key, owner JWT and PMS access token with `REPLX_EDGE_SECRET_KEY` derived application encryption.

Use the owner PMS credential for indexing and administration. Use the incoming user's token for user requests.

## Identity and discovery

Replx Edge is a connection to the existing PMS resource.

It preserves origin `machineIdentifier` and does not invent a new server identity.

Onboarding must verify:

```text
selected plex.tv resource
origin PMS root
Replx Edge proxied PMS root
```

all identify the same server.

The Custom Server Access URL is configured on PMS and verified through the Plex resources response.

## Origin URLs

`REPLX_EDGE_ORIGIN_INTERNAL_URL` is used only from Replx Edge to PMS.

Direct media routing selects a separate client reachable HTTPS origin connection from verified Plex resource connections. This connection requires trusted TLS.

## Request pipeline

```text
receive request
assign UUIDv7 request ID
normalize Plex header and query identity
resolve user token fingerprint
resolve client instance
classify route
apply route specific rate and size limits
lookup user scoped cache if eligible
call PMS if needed
apply playback policy only on defined routes
record metrics and trace events
return response
```

## Response preservation

Internal Replx Edge API calls request JSON.

Official client proxy responses preserve XML or JSON requested by the client.

Do not parse a large response if no transformation is required.

Default non streaming transformation ceiling is 8 MiB. Larger bodies require a streaming parser or transparent pass through.

## Cache model

The persistent library index is shared internal data.

PMS response caches default to user scoped. Do not serve owner synchronized raw metadata as though it were a user response.

## Policy model

Merge defaults, global, user and device fields independently.

Hard restrictions are evaluated before preferences.

Dynamic range is normalized to `SDR`, `HDR10`, `HDR10_PLUS`, `HLG`, `DOLBY_VISION`, `HDR_OTHER` or `UNKNOWN`.

## Playback enforcement

Production 1.0 does not reorder or remove `Media` arrays.

At the universal decision route:

1. resolve current origin media variants
2. normalize variant capability data
3. reject policy incompatible variants
4. rank eligible variants
5. rewrite `mediaIndex` to the selected origin index
6. apply output limits
7. forward to PMS with the user token
8. validate PMS decision
9. fail if transcode is denied but required
10. persist selected media and part IDs

At raw media or manifest boundary:

1. resolve requested part or source
2. find active playback selection
3. reject prohibited source
4. substitute the selected allowed part when deterministic
5. route using ADR 001

This second boundary is mandatory because some clients may retry or ignore prior negotiation details.

## Transcode policy

If `allowTranscode=deny`, a PMS transcode decision for the selected eligible source produces `POLICY_TRANSCODE_FORBIDDEN`.

Never choose a prohibited larger source just to avoid a transcode.

## P0 media routing

Implement ADR 001 before cache or full policy work.

Candidate behaviour:

```text
progressive part request -> 307 to allowed origin part
HLS or DASH manifest -> 307 to origin manifest
```

Use explicit query token propagation because cross host header preservation is not assumed.

Validate Range, seeking, resume, TLS and manifest segment behaviour per client.

If direct origin routing fails in Cloudflare mode and the media gateway is disabled, return `MEDIA_ROUTE_UNAVAILABLE`.

## Enforcement semantics

Default Cloudflare deployment can enforce policy only on requests that traverse Replx Edge.

Do not present it as a strict security boundary if PMS remains independently reachable.

## Background workers

Run as goroutines inside the single Replx Edge application replica:

```text
owner token refresh
PMS event consumer
incremental sync
reconciliation
cache janitor
artwork janitor
diagnostics janitor
playback retention janitor
session reconciliation
```

Production 1.0 supports one application replica.

## Admin API

All list endpoints use cursor pagination.

Expensive endpoints use locks and rate limits. Full sync is single flight. All mutations are audited.

## Performance implementation rule

Hot path proxy overhead target requires streaming where possible.

Never DOM parse multi megabyte library pages solely to remove variants. This is another reason metadata filtering is not in Production 1.0.

## Production invariants

```text
PMS remains authoritative
owner token is never substituted for user token
machineIdentifier is preserved
user response caches are isolated
hard source restrictions are checked at negotiation and media boundary
transcode deny fails closed
Cloudflare control hostname never becomes a silent video proxy
all metrics use valid replx_edge_ names
one active origin in 1.0
all persistent high volume records have retention
```
