# Replx Edge: bounded library acceleration implementation plan

Date: 29 October 2026

Status: proposed implementation plan; no runtime behavior is changed by this document.

Target deployment: one remote Plex Media Server, one nearby VPS, three core users.

## 1. Intended outcome

Users browse libraries and collections through Replx Edge using their normal Plex applications. Frequently used metadata and artwork are ready on the VPS. Missing data is fetched from Plex without breaking browsing. Video is delivered from Plex directly to the client using a validated media route. Plex remains authoritative for authentication, visibility, collection contents, watch state, playback negotiation and transcoding.

The objective is a fast, bounded cache, not a complete copy of every Plex response or image transformation. A 200 GB VPS must remain healthy as the library grows. Cache coverage is allowed to decrease under pressure; correctness, permissions and playback must not.

This document adds a deployment-specific implementation sequence to the existing roadmap. Existing specifications remain the runtime contract until each phase explicitly updates them. Proposed defaults, schema names and configuration controls below are not implemented interfaces. Existing playback policy must continue to work; this project does not remove it.

## 2. Confirmed requirements and unresolved inputs

| Area | Requirement or current understanding |
| --- | --- |
| Library | 84,291 movies and 12,209 TV shows: 96,500 top-level entries, with further growth expected |
| Users | Three core users; permissions and watched state remain separate |
| Collections | Approximately 10 movie collections and 10 TV collections |
| Collection automation | Aggregar rebuilds/updates collections daily around 03:00; timezone and completion integration are unconfirmed |
| Clients | Fire TV/Firestick, Apple TV, Hisense Plex application and Plex Web |
| Hisense | Model, operating system and application version are not yet known |
| Origin | HTTPS Plex endpoint; believed to be in Iceland |
| Administration | User has Plex administration and token access, but no bare-metal/server filesystem access |
| Cache host | Planned VPS in Wales with 200 GB disk; RAM, CPU, actual usable capacity and networking are unconfirmed |
| Artwork | Identical files should be shared across authorized users, not copied three times |
| Priority | Collection entry screens and posters ahead of rarely accessed backgrounds and speculative artwork |
| Watch state | Continue Watching must not acquire stale-cache delay when leaving playback |

Season/episode totals are not blockers: inventory can count them through permitted APIs. No implementation phase should require shell access to the Plex host. A completion hook from Aggregar is optional and requires separate access to wherever that automation runs.

## 3. Current implementation baseline

Source inspection established the following; it is not evidence of a successful production deployment or real-client test.

| Component | Existing behavior | Work needed |
| --- | --- | --- |
| `internal/cache/cache.go` | User-scoped responses, short TTLs, 2 MiB body cap, process-local invalidation generations | Explicit request variation, durable generations and measured body limits |
| `internal/proxy/proxy.go` | Cache lookup, origin fallback, request coalescing, artwork routing and watch-write recognition | Response-aware invalidation, cache admission and comprehensive route policies |
| `internal/warmer/warmer.go` | Refreshes observed owner entries; maximum 512 tracked requests | Priority scheduling, proactive preparation and bounded user-context warming |
| `internal/artwork/` | Shared content-addressed blobs, PostgreSQL transform references, seven-day expiry, durable write reservations, crash recovery, priority eviction and a free-space guard | Collection-aware protected artwork and measured client transform/priority coverage |
| `internal/sync/` | Owner index of libraries/items/media; periodic reconciliation | Explicit collection/member indexing and change publication |
| `internal/sync/events.go` | Selected library refresh events mark sections dirty | Prompt invalidation, event coverage validation and missed-event recovery |
| `internal/identity/` | Token fingerprints resolve identities; ordinary user credentials are not persisted | Separate identity from current PMS resource authorization |
| `internal/spike/` | Experimental direct-origin redirects with transient delegation tokens | Real-client evidence and production delivery configuration |
| `cmd/replx-edge/main.go` | Worker wiring; optional media gateway remains a health-only placeholder | Wire new services; avoid presenting the placeholder as a playback fallback |
| `deploy/compose.yml` | PostgreSQL, Valkey, Replx and optional tunnel infrastructure | Enforce RAM/disk budgets and account for persistence/maintenance overhead |
| `tests/integration/` | Fixtures and limited integration checks | Full proxy/storage tests rather than only key/route assertions |

Important distinction: `direct` ingress currently can proxy media. It does not itself promise a direct client-to-PMS media connection. Artwork misses also normally pass through Replx; they are not automatically redirected to Plex.

## 4. Architecture and invariants

```text
Plex client -- browse --> Replx on Wales VPS
                           |-- authorized response cache (Valkey)
                           |-- per-user artwork authorization/reference
                           |       `-- shared image blob on disk
                           `-- cache miss / live state --> remote PMS

Plex client -- validated media redirect ----------------> remote PMS

Owner API sync --> PostgreSQL inventory and collection snapshots
Changes        --> durable invalidation --> bounded warmer
```

Required invariants:

1. Owner inventory never becomes an unfiltered shared user response.
2. A cached blob's existence does not authorize a user to read it.
3. Identical image bytes can share storage; different transformations may remain distinct.
4. No media bytes are intentionally delivered through a Cloudflare control hostname.
5. A cache failure preserves origin browsing where origin access is available.
6. Disk pressure stops admission before exhausting the filesystem.
7. Failed or partial collection scans never establish authoritative deletion.
8. An in-flight response from an old generation cannot repopulate current state.
9. Credentials never appear in cache keys, filenames, metrics or logs.
10. No client or protocol is declared supported without playback evidence.

Location is not a latency guarantee. Measure all three legs: client-to-Replx, Replx-to-PMS and client-to-PMS. Cache misses take an additional hop; playback remains dependent on the origin connection.

## 5. Capacity and storage policy

### 5.1 Initial budgets

Use decimal GB in operator-facing planning and explicit bytes in implementation. Verify filesystem usable capacity before applying limits; a provider's 200 GB allocation is not 200 GiB of free application space.

| Area | Initial planning allowance | Enforcement |
| --- | ---: | --- |
| Shared artwork | 100 GB | Hard admission budget including temporary/reserved image space |
| PostgreSQL plus persistent response-cache files | 20 GB | Monitored allowance, retention and growth alarms; never delete authoritative DB data to satisfy a cache quota |
| Logs and diagnostics | 5 GB | Rotation and retention limits |
| Other capacity | Approximately 75 GB | OS, container images, maintenance, WAL/AOF rewrite, growth and free space |
| Minimum free space | 30 GB within the remaining capacity | Pause warming, reject new cache writes and evict reclaimable artwork |

These allowances are not independent guarantees: the global filesystem threshold overrides individual cache budgets. Temporary database maintenance and Valkey persistence rewrite space must be included in capacity testing. Critical free-space exhaustion must alert the operator; cache eviction cannot fix unbounded unrelated files indefinitely.

Choose Valkey `maxmemory` only after learning VPS RAM. Reserve memory for the OS, PostgreSQL, Replx and concurrent image transfers. Treat Valkey responses as disposable. Store correctness-critical invalidation epochs outside an evictable response namespace, preferably in PostgreSQL with a bounded in-process read cache for this single-instance deployment.

### 5.2 Preload estimate and measurement

For 96,500 top-level titles, illustrative averages of 150–400 KB per poster, 300 KB–1 MB per background, up to 50–200 KB per logo and 10–50 KB basic metadata produce roughly 49–159 GB for one variant of each asset. These are assumptions, not measurements. Logos are not universally present. Indexes, seasons, episodes, collections, multiple transforms and temporary versions are additional.

Inventory through PMS and sample at least 500 movies, 200 shows, representative seasons/episodes, and all 20 collections. Include old/new/popular/random titles and differing image sizes. Report mean, p50/p95 sizes, missing-asset rates, duplicate rates and actual client transform counts. Estimate database bytes separately from serialized response sizes.

Do not preload preview frames, trailers, extras or every cast image in the initial release. Report coverage as separate counts: inventory indexed, posters available, collection landing pages warm and per-user pages warm. A single “100% cached” label would be misleading.

### 5.3 Retention priorities

| Tier | Examples | Policy |
| --- | --- | --- |
| Protected, bounded | Collection covers and essential collection-list snapshots | Reserve a small capped pool; replace on updates |
| High | First visible collection posters, recent titles, actively accessed posters | Retain before lower tiers; not permanently pinned |
| Normal | Other movie/show posters and commonly used logos | Evict by recency/frequency within a size-aware policy |
| Low | Backgrounds, oversized/duplicate-size variants, speculative assets | First eviction candidates |
| On demand | Rare episode thumbnails and secondary images | Admit only when useful and budget permits |

Do not deprioritize letters V–Z. Learn actual usage and prepare initial pages for the client's actual sort order. A huge collection must not pin its entire membership. Per-collection first-page coverage and protected bytes have explicit caps; pinned content cannot exceed the hard budget.

Use low/high watermarks to avoid deleting one file on every incoming request. First choose a lower-priority tier, then prefer cold/large assets within that tier. Buffer or sample access accounting rather than performing a database write on every image hit. Expiring one user's reference must not delete a blob still referenced by another user.

## 6. Shared artwork design

Separate three concepts:

- Resource authorization: a user is currently allowed to reference this PMS artwork resource.
- Source/transform mapping: Plex scope, server identity, resource version and response-affecting transformation parameters map to a blob. Scope-bound references prevent identical URLs from exposing permission-dependent response bytes; the payload deduplicates across users by content digest.
- Blob identity: cryptographic digest of the completed image bytes, with size, MIME type and storage path.

Proposed records: `artwork_blobs`, `artwork_variants`, `artwork_authorizations`, and bounded warming jobs. Add indexes for eviction priority and last use. Never key authorization solely by a public blob hash or account identity.

Initially preserve PMS-generated client transformations. Normalize only proven-equivalent requests; unknown variation bypasses shared reuse. Validate permissions through an authorized PMS response, retain a bounded authorization validity interval and revalidate on uncertainty. Revoked access must not be extended by a long image TTL.

Write workflow: authorize request; reserve a bounded maximum transfer size; fetch/stream to a private temporary file; validate completion and size; hash content; atomically publish or reuse the blob; commit references; release reservations. Deduplicate after hashing even when source URLs differ. Coalesce equivalent in-flight authorized requests when safe. Oversized or failed transfers stream uncached without leaving partial objects.

Use sharded filesystem paths rather than a single directory containing millions of files. Reconcile orphan files and stale reservations after crashes. Eviction must coordinate with readers and refcounts so concurrent access cannot corrupt a response. Track payload, temporary and metadata usage, with filesystem free space as the final safeguard.

Use content/version changes and periodic validation instead of redownloading the entire preload every seven days. Bound retained superseded versions. If Plex changes image bytes without changing a URL, periodic revalidation must eventually detect it.

## 7. Collections and daily updates

Store collections and ordered membership explicitly, including source IDs, type, relevant metadata/art references and last complete snapshot. Smart collections remain PMS-evaluated; do not reproduce their filtering or ranking.

Preferred trigger: an authenticated Aggregar completion signal, if available. Fallback: bounded polling around the configured rebuild window and a regular reconciliation pass outside it. Confirm the timezone before activation; use an IANA timezone and handle daylight saving changes. Do not schedule a real automation as part of writing this plan.

On change, stage a new snapshot, fetch affected collection pages, prepare protected artwork and atomically publish the completed generation. Refresh only changed membership/artwork and their dependent pages. Owner snapshot publication does not automatically make user response caches warm.

A rebuild may delete and recreate collections with different IDs. Use PMS identifiers as authoritative; do not merge different collections merely because their titles match. A full completed enumeration establishes deletions. Without a completion signal, use repeated stable observations plus a maximum settling window and disclose that this is a heuristic. Mark refresh as incomplete rather than claiming an atomic view of a source changing mid-scan.

Keep the last complete display snapshot within a bounded stale allowance during transient rebuild failures, provided authorization remains valid. Confirmed removals or access revocations override stale serving. Rate-limit and debounce large update bursts. Continue ordinary browsing while collection refresh runs.

## 8. Watch-state freshness

For three users, start with live PMS reads for Continue Watching and every home/hub response containing equivalent state. Detect those routes using real client captures, not only a pathname substring. Do not implement Plex's ranking algorithm locally.

Forward timeline, stop, scrobble and unscrobble operations immediately. After successful origin acknowledgement, advance the user's relevant generation and invalidate affected details, resume positions, badges, home feeds and show/season counts. A provisional dirty marker before the write may prevent concurrent stale reads, but completion must retire responses fetched before acknowledgement. Failure must clear pending state without inventing a successful watch update.

Measure whether each client sends progress to Replx or directly to PMS after media redirection. Where events are available, use them as invalidation hints, not as the sole source of truth. Live reads and bounded active-session reconciliation cover bypassed or lost events. Avoid permanently polling all user state at high frequency.

Acceptance: once PMS reflects the update, the next requested Continue Watching response is fetched live and receives no stale-cache delay from Replx. Client rendering/request cadence and PMS update latency are measured separately. A background prefetch is optional and must use that user's valid context, never the owner token.

## 9. Phased implementation

Each phase produces reviewable commits and updates its affected canonical specifications. Code work should not start by implementing all phases in one patch. Dependencies are explicit; completion requires evidence, not only source code presence.

### Phase 0 — inventory and reproducible baseline

Dependencies: read access to the local repository; live measurements additionally require an approved secure Plex credential/configuration path.

Tasks:

1. Run the current test/CI baseline and record environment-related skips separately from passes.
2. Add a read-only inventory/report command with pagination, bounded concurrency and resumable sampling.
3. Count accessible libraries, seasons, episodes, collections and assets; measure representative sizes.
4. Capture sanitized request sequences from the available priority clients.
5. Measure cold/warm origin latency and actual VPS disk/RAM/CPU capacity.

Likely modules: `internal/pms`, `internal/capture`, `internal/metrics`, a CLI command under `cmd`, and fixtures.

Deliverables: inventory report, client request matrix, capacity worksheet and baseline test results. Gate: no performance promise is presented as measured without these results. Offline fixtures can proceed before server access is available.

### Phase 1 — prove discovery and direct-origin playback

Dependencies: Phase 0 captures and reachable HTTPS PMS.

Tasks:

1. Verify Custom Server Access URL discovery and preserved server identity.
2. Trace selected connection on each client; document clients that bypass Replx.
3. Test transient-token redirect compatibility, HTTPS trust, Range, seek/resume, subtitles and applicable transcode protocols.
4. Confirm manifest and segment hosts; measure bulk bytes through Replx.
5. Separate ingress transport configuration from media delivery strategy and add validated production routing controls.

Likely modules: `internal/spike`, `internal/routing`, `internal/delegation`, `internal/proxy`, `internal/config`, onboarding and compatibility administration.

Gate: Plex Web and at least one available physical client demonstrate the full path before broad cache rollout. Fire TV and Apple TV each require evidence before being advertised as supported; Hisense remains UNKNOWN until identified and tested. Do not silently enable the placeholder media gateway. Unsupported clients require a documented deployment decision.

### Phase 2 — cache correctness and live watch state

Dependencies: Phase 0 request matrix. Offline implementation may proceed alongside routing validation; deployment remains gated by Phase 1.

Tasks:

1. Introduce explicit route policies for cacheability, freshness, variation and user scope.
2. Account for header/query pagination, language, representation, client capability variation and `Vary` behavior; safely bypass unknown cases.
3. Add durable invalidation epochs and generation checks around concurrent fills.
4. Bypass home/Continue Watching response caching and implement acknowledgement-aware state invalidation.
5. Add tests for revoked credentials, managed/restricted users and resource authorization distinct from identity.

Likely modules: `internal/cache`, `internal/proxy`, `internal/identity`, `internal/valkey`, migrations and tests.

Gate: end-to-end isolation tests pass; changed watch state is never hidden by a Replx cache hit; restart and in-flight races cannot resurrect retired responses.

### Phase 3 — shared artwork and hard storage controls

Dependencies: Phase 2 authorization and cache invariants; Phase 0 size report for final defaults.

Suggested commit sequence:

1. Add blob/reference/authorization schema and filesystem interfaces.
2. Add content hashing, atomic publication, sharded paths and deduplication.
3. Add byte reservations, crash recovery and concurrent-reader-safe deletion.
4. Add priority eviction, admission watermarks and the global free-space guard.
5. Wire the new store behind a feature flag and add metrics/admin inspection.

Migration strategy: treat the old artwork cache as disposable. Prefer lazy migration after reauthorization or a separate new cache directory. Count both stores against disk limits during transition; never blindly import old permission assumptions. Do not require downloading the complete library to turn on the new store.

Gate: the same image requested by three authorized users occupies one blob; an unauthorized user cannot retrieve it; concurrent writes stay inside the reserved budget; forced low-disk tests preserve ordinary origin responses.

### Phase 4 — collection snapshots and incremental synchronization

Dependencies: Phase 2 durable invalidation; Phase 3 for artwork preparation.

Tasks:

1. Add collection, ordered membership and snapshot-generation migrations.
2. Synchronize ordinary/smart collection results with pagination and completion markers.
3. Publish change sets for additions, deletions, reorderings and art changes.
4. Connect change sets to targeted cache invalidation and preparation queues.
5. Add optional completion-trigger endpoint and configurable polling/reconciliation schedules.

Likely modules: `internal/sync`, `internal/admin`, migrations, worker wiring and configuration.

Gate: all 20 collections reconcile; failed scans do not erase membership; daily updates replace only affected data; source delete/recreate behavior and changing snapshots are tested.

### Phase 5 — priority warming for three users

Dependencies: Phases 2–4.

Tasks:

1. Replace the owner-only observed-entry list with bounded priority jobs.
2. Prepare protected collection assets and useful posters first, using actual client transformations.
3. Warm first visible pages and likely next pages for recently active user/client contexts.
4. Add per-user fairness, byte/request/concurrency budgets, deduplication and backoff.
5. Cancel obsolete jobs on collection generations, permission changes and storage pressure.

User-token policy: use a short-lived, bounded in-memory context from actual authorized user requests; clear it on expiry/revocation. Persistent jobs contain no raw credential. After restart, user warming waits for a fresh context. Owner credentials prepare owner data and shared internal inventory, not another user's personalized pages. Warming while users are absent across restarts requires a separate approved credential-lifecycle design.

Gate: ordinary users benefit, warming does not starve browsing, and first-ever user access is honestly reported as cold where authorization or responses are unavailable. Success is useful warm coverage, not an impossible enumeration of every filter/sort combination.

### Phase 6 — administration and operational controls

Dependencies: Phases 3–5.

Expose used/reserved/free bytes; blob deduplication ratio; eviction by tier; queued/failed jobs; per-user and per-route hit rates; cache bypass reasons; collection generation/last completion; origin latency; and media delivery evidence. Avoid raw tokens and high-cardinality resource IDs in metric labels.

Add controls to pause warming, change budgets, rebuild one collection, evict low-priority assets and disable new behavior. Validate requested budgets against available storage; a protected tier cannot be configured above its parent budget. Dashboard text must distinguish configured behavior, observed operation and untested client support.

Gate: an operator can explain a slow page or storage-pressure event without reading secret-bearing logs.

### Phase 7 — integration, load and failure qualification

Dependencies: Phases 1–6.

Run the actual proxy with PostgreSQL, real Valkey and a controllable fake PMS. Add a Valkey service to the CI integration environment. Exercise large paginated synthetic inventories rather than loading hundreds of thousands of full response objects into memory.

Required cases:

- Three-user shared blob storage with differing library permissions and revoked access.
- Pagination/header variation, unsupported `Vary`, incomplete responses and oversized images.
- Collection rebuild during browsing; lost events; repeated triggers; interrupted sweeps.
- Stop/back-out/watch transitions including writes reaching PMS directly.
- Concurrent warm jobs and requests during eviction, restart and generation changes.
- Valkey outage, PostgreSQL outage, origin timeout, full/low disk and process termination during blob publication.
- Actual Fire TV, Apple TV and Plex Web playback; Hisense when available.

Run `go vet ./...`, `go test ./...`, existing race checks, migrations, relevant frontend checks and Docker builds. Run the generated-spec consistency check. Record skipped live tests explicitly; passing mocks does not establish client compatibility.

Gate: isolation and storage invariants pass under failure, and performance results include dataset, response sizes, concurrency and machine details.

### Phase 8 — staged deployment and tuning

Dependencies: qualification and at least the Phase 1 minimum real-client gate.

Deploy with separate feature switches for the new artwork store, collection sync, user warming and production media delivery. Enable for the owner, then one shared user, then all three. Use a bounded initial preload: collection covers, initial collection posters, useful library posters, then opportunistic backgrounds.

Observe at least one actual daily collection rebuild and representative playback sessions before expanding preload coverage. Adjust budgets based on measured hit rates and eviction churn rather than making the cache larger automatically.

Rollback: pause jobs; disable new response/artwork behavior and return browsing to authorized PMS passthrough; retain the proven media path. Use additive schema migrations so disabling features does not require destructive rollback. Old cache data must not be treated as current merely because a feature is disabled. Document recovery from a failed upgrade and restore test results.

Gate: no regression in playback or watch freshness, bounded disk/RAM, verified collection updates and a tested operational rollback.

## 10. Performance and acceptance scorecard

Proposed user-facing targets supplement rather than silently replace the existing internal processing targets in `acceptance_criteria.md`.

| Measure | Proposed acceptance |
| --- | --- |
| Warm metadata response at Replx | p95 below 100 ms in a defined VPS benchmark, excluding client WAN latency; report body size |
| Prepared browse sequence hit rate | Above 95% for explicitly prepared cacheable requests; live home/watch-state requests excluded and reported separately |
| Continue Watching | No cached response after PMS reflects the update; measure PMS and client delays separately |
| Collection propagation | Initial goal: publish within 5 minutes of confirmed rebuild completion for these 20 collections, subject to baseline evidence |
| Disk | Artwork admission remains within configured reservations; free-space guard demonstrated under external disk growth |
| Deduplication | Three authorized requests for identical bytes produce one retained blob |
| Permissions | No cross-user restricted metadata, artwork or personalized-state disclosure |
| Media | Zero bulk video bytes through Replx for validated direct-origin playback scenarios |
| Failure | Cache failures fall through to PMS where possible; unknown permissions fail safely |

Retain the existing p50/p95 internal overhead benchmarks and report them separately from end-to-end API response time. UI time-to-visible-posters is a separate device measurement. Targets are goals until measured, not promises that the VPS location alone can deliver.

## 11. Risks and decisions to resolve

| Risk or open decision | Response |
| --- | --- |
| Clients choose origin for browsing | Discovery/connection traces in Phase 1; do not assume Custom Server Access URL forces selection |
| Unsupported cross-host media redirect | Per-client/per-protocol gate; revise delivery architecture before claiming support |
| No host filesystem access | Use supported remote APIs; if required event access is unavailable, use bounded reconciliation |
| Incomplete knowledge of permissions | Reauthorize through PMS; never infer a grant from owner visibility or account identity |
| Collection automation has no completion hook | Stable-observation polling with documented limits and periodic reconciliation |
| Episodes dominate artwork size | Keep most episode artwork on demand; hard budgets make unknown totals manageable |
| Per-client transforms multiply storage | Measure variants; retain useful sizes and deduplicate identical payloads |
| Growth or external files consume reserve | Stop speculative admission, evict reclaimable blobs and alert; never delete authoritative data |
| User context expires between visits | Keep safe shared blobs; allow cold authorized response fetch; no owner-token substitution |
| Live Continue Watching is slow at PMS | Measure separately; tune only with evidence, preserving the no-stale-cache requirement |

Outstanding inputs: VPS RAM/CPU and usable disk; Plex API/version and measured endpoint access; season/episode inventory; actual client versions; Hisense OS/model; Aggregar schedule timezone and completion integration. None of the unknown counts prevents bounded storage implementation.

## 12. First implementation milestone

The first milestone is deliberately small: baseline tests, a read-only inventory/sample report, sanitized client browsing/playback traces, and the route-policy/watch-state corrections from Phase 2. Phase 1 live routing evidence is the deployment gate. Then implement shared artwork with hard admission limits before starting broad preload.

Do not start by downloading the entire library. That would consume the storage budget before the project can deduplicate, prioritize or safely evict its contents.
