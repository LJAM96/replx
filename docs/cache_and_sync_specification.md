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

| Class | TTL | Stale allowance |
| --- | ---: | ---: |
| PMS identity | 5 minutes | 30 minutes |
| Library list | 5 minutes | 30 minutes |
| Library browse page | 60 seconds | 5 minutes after one successful response |
| Item metadata | 5 minutes | 30 minutes |
| Collections | 2 minutes | 15 minutes after one successful response |
| Structural Home hubs | 10 seconds | 15 minutes after one successful response |
| Continue Watching | 5 seconds | none |
| Recently Added | 15 seconds | none |
| Search response | 30 seconds | 2 minutes |
| Artwork | 7 days | 30 days |

Never stale serve writes, playback decisions, session termination, timeline or scrobble operations.

## Stampede control

One request refreshes an expired object while other requests wait a
bounded duration and re-check. Acquisition is atomic (check-and-claim
under one lock): exactly one request per key becomes the refresher.

## Invalidation generations

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

## Owner preload

After startup and every five minutes, a bounded background pass uses the
current owner credential to fetch the library list, common home hubs, up to
eight section hubs and collection lists, and up to 96 recent poster transforms
directly from PMS.
The pass does not delay readiness, and failed requests are not cached. Cached
responses and artwork use the canonical owner scope. Other users still need
their own authorized requests before their responses can be cached.
After Plex validates a user token, Replx stores it encrypted under a separate
key purpose and refreshes that user's previously requested pages in the same
cache scope. Rejected tokens are cleared, and tokens unused for 30 days are
cleared by retention. Raw tokens are never logged or used in cache keys. This
Tracked-page refresh alone does not populate unseen scroll positions.

## Collection scroll windows

For JSON collection-child requests, a background pass fetches up to four
complete windows for one recently active user every 30 seconds. It rotates
through collection IDs found in the owner's collection lists and the
configured browser query profiles. Every fetch uses the target user's own
validated credential; Plex enforces that user's visibility. The resulting
window is cached under that user's scope, non-pagination query fields,
representation and invalidation generations. A later page request can be
served locally only when the requested range is fully covered by the window.
The proxy preserves Plex's `totalSize`, returns the requested `offset` and
`size`, and never uses a window for a different user or query profile.

Windows remain available for two hours while successful warm refreshes
replace them. A collection that has not completed its first full fetch, a
response larger than the cache limit, and a range beyond the fetched window
still go to Plex. This preloading is bounded to avoid flooding Plex and may
take several passes to cover a large collection catalogue.
The admin cache statistics expose cumulative preload page, artwork, and error
counts plus the time of the last completed pass.

The response key includes non-secret query parameters and representation.
Preloaded pages only hit when a client makes the same request variant; a Plex
Web request with extra parameters may still be a first miss. The warmer
refreshes entries that clients actually request under the matching user's
credential. Artwork keys remove a
Plex token nested inside the transcode URL, so the owner preloaded poster can
match the same transform after token rotation without crossing user scopes.

## Events and reconciliation

Use PMS event streams for targeted invalidation and refresh. Events are not assumed lossless, so reconcile periodically.

Default schedule:

```text
light reconciliation every 15 minutes
full consistency sweep every 6 hours
```

## Continue Watching

Cache the actual user scoped PMS Continue Watching response. Do not attempt to reimplement Plex's Continue Watching ranking algorithm.

Invalidate after successful timeline, scrobble and unscrobble changes.

## Search

PostgreSQL provides candidate search with FTS and trigram indexes. In Production 1.0 the local index covers `title`, `sort_title` and `original_title` only. Cast, director, collection and label search fall back to PMS. Search is an optimization. If visibility or index freshness is uncertain, use PMS.

Because the owner index carries no per-user library grants, locally
serving its candidates would bypass Plex visibility controls. In
Production 1.0 `/hubs/search` always passes through to PMS; local
candidates stay behind a future PMS-authorized path.

## Cloudflare

Cloudflare edge cache remains disabled for Plex API routes in Production 1.0. Replx Edge owns cache correctness.
