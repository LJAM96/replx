# Cache and Synchronisation Specification

## Cache layers

Replx Edge uses three distinct data classes:

```text
PostgreSQL internal library index
Valkey user scoped response cache
Filesystem artwork and trace storage
```

The library index is shared internal data. It is not a ready made user response cache.

## Safe default scope

Any PMS response that may vary by permissions, watched state, progress, recommendations, sharing, user restrictions or personalization is cached per internal Plex identity.

Production 1.0 defaults to user scoped response caching.

This includes library browse responses and item metadata unless a route has been explicitly proven user independent.

## Shared data

The owner synchronized library index may be shared internally for:

* media variant lookup
* canonical identity
* search candidate generation
* administration
* cache invalidation targeting

Before a shared index result is returned to a user, Replx Edge must ensure the user's PMS permissions and visibility are respected. Local search should fall back to PMS when Replex cannot prove that a result is visible to that user.

Artwork binaries may be shared when their URL and transformation key are identical and the user has already been authorized to reference the artwork.

## Cache isolation invariant

No user may receive another user's:

```text
Continue Watching
watched state
progress
home hubs
restricted library entries
personalized metadata
```

CI must include a two user isolation test that poisons one user's cache and proves the second user never receives it.

### Exact response authorization lease

Identity and a recently valid Plex.tv credential do not prove current access
to a cached PMS response. Batch B adds a memory-only lease keyed by the caller's
token fingerprint and the exact response cache key. Only a completed, successful
response fetched from PMS with that caller token grants reuse. Leases expire after
30 seconds without extending on cache hits. Expired, absent or invalidated leases
bypass the cached response and reauthorize through PMS. Local artwork uses the
same rule with its exact account-scoped transformation key. Warming never grants
user authorization. The lease is not a section ACL or a substitute for Plex
permission evaluation; real revocation behavior still needs client/PMS validation.

## Canonical cache key

```text
replx_edge:{schema}:{server}:{class}:{scope}:{representation}:{hash}
```

For user scoped entries:

```text
scope=user:{identity_uuid}
```

Raw Plex tokens never appear in keys.

## Default TTLs

Implementation update (agent execution batch A): home/hub, Continue Watching,
on-deck and resume feeds bypass the response cache and are read live from PMS.
The feed TTL/stale values below describe the earlier policy and are not active
for those routes. No stale serving is enabled in this implementation.

| Class | TTL | Stale allowance |
| --- | ---: | ---: |
| PMS identity | 5 minutes | 30 minutes |
| Library list | 5 minutes | 30 minutes |
| Browse page | 60 seconds | 10 minutes |
| Item metadata | 5 minutes | 30 minutes |
| Collections | 2 minutes | 15 minutes |
| Home hubs | 10 seconds | 30 seconds |
| Continue Watching | 5 seconds | 30 seconds |
| Recently Added | 15 seconds | 2 minutes |
| Search response | 30 seconds | 2 minutes |
| Artwork | 7 days | 30 days |

Never stale serve writes, playback decisions, session termination, timeline or scrobble operations.

## Stampede control

One request refreshes an expired object while other requests wait a
bounded duration and re-check. Acquisition is atomic (check-and-claim
under one lock): exactly one request per key becomes the refresher.

## Invalidation generations

Batch A expands successful watch-write invalidation to the writer's recorded
response classes after PMS acknowledges the operation. Foreground and background
fills check the generation captured before their fetch before publishing results.
Batch B persists global, per-scope and per-class counters in PostgreSQL. Scope
identifiers are stored as SHA-256 digests. Production cache reads bypass when a
generation cannot be read; a failed bump disables cache serving until a successful
global recovery bump. The migration seeds a nonzero default global counter and the
key schema advances to v4, retiring prior process-local generation namespaces.

Keys carry per-scope, per-class and global generation segments
(`...:{scope}:{representation}:{scopeGen}:{globalGen}:{hash}`).
Timeline, scrobble and unscrobble mutations retire the writer's Continue
Watching namespace in constant time; the admin invalidation API retires
arbitrary namespaces the same way. TTLs remain the backstop. The cache
scope is canonically `user:{identity_uuid}` everywhere (proxy, cache and
owner warmer); legacy `acct:`/`tok:` forms appear only where no identity
store is wired.

## Representation normalization

Cacheable origin requests are normalized to the identity encoding
(stored bytes are servable regardless of client `Accept-Encoding`);
encoded or wildcard-`Vary` responses are never stored. `Content-Encoding`
is not a persisted header.

## Owner library sync

Owner sync uses the encrypted owner PMS credential obtained during onboarding.

Initial sync indexes library sections, items, GUIDs, variants, parts and streams using pagination. Progress is persisted and resumable.

## Events and reconciliation

Use PMS event streams for targeted invalidation and refresh. Events are not assumed lossless, so reconcile periodically.

Default schedule:

```text
light reconciliation every 15 minutes
full consistency sweep every 6 hours
```

## Continue Watching

Fetch the actual user scoped PMS Continue Watching response live. Home/hub
containers also bypass caching because they may embed the same watch state.
Do not attempt to reimplement Plex's Continue Watching ranking algorithm.

Invalidate the writer's affected response namespaces after successful timeline,
scrobble and unscrobble changes. Updates sent directly to PMS remain a real-client
validation requirement; Replx cannot invalidate a write it did not observe.

## Search

PostgreSQL provides candidate search with FTS and trigram indexes. In Production 1.0 the local index covers `title`, `sort_title` and `original_title` only. Cast, director, collection and label search fall back to PMS. Search is an optimization. If visibility or index freshness is uncertain, use PMS.

Because the owner index carries no per-user library grants, locally
serving its candidates would bypass Plex visibility controls. In
Production 1.0 `/hubs/search` always passes through to PMS; local
candidates stay behind a future PMS-authorized path.

## Cloudflare

Cloudflare edge cache remains disabled for Plex API routes in Production 1.0. Replx Edge owns cache correctness.
