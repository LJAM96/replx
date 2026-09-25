# Test Plex validation record

Date: 2026-09-22 23:27 UTC (2026-09-23 Europe/London)
Host: `oi-2`
Deployed revision: `a10f219`, Replx Edge `0.4.0`

## Baseline observed before parser fix

| Check | Result |
| --- | --- |
| Replx Edge, PostgreSQL, Valkey | Healthy, zero restarts |
| `/health/ready` | `ready`; PMS `healthy`; Valkey `ok` |
| Ingress | `cloudflare_tunnel` |
| Direct-origin spike routing | Enabled |
| Media fallback | Disabled |
| Active owner | Verified |
| Library sections | 4 |
| Indexed items | 0 |
| Section sync cursors | 4 errors; no completed section sync |
| Section sync error | Scalar `guid` string cannot decode into `Guid` array |
| PMS event stream | Disconnected; recurring whole-request timeout |
| Origin errors | 0 since process start |
| Media redirects | 0 since process start |
| Media route failures | 4 since process start |
| Cache | 0 hits, 1 miss, 0 entries since process start |
| Playback decisions | 0 since process start |

Existing compatibility rows record Plex Web progressive and Apple TV progressive as `SUPPORTED`, one observation each. These rows predate the current process and do not establish current-version playback success. No current playback session or redirect was observed.

## Local correction prepared

- Section item parser accepts either a scalar `guid` or `Guid` array.
- PMS event stream uses a client without a whole-request timeout while retaining the origin redirect boundary.
- `go test ./... -count=1` passed locally. Live database tests require `REPLX_EDGE_TEST_POSTGRES_URL` and were not exercised by this command.

The correction was not deployed when this baseline was captured.

## Test deployment update (2026-09-22 23:46 UTC)

The correction was deployed from commit `bdb1664` as
`ghcr.io/ljam96/replx-edge:0.4.0-bdb1664-test`, image ID
`sha256:aaebe6bf64589192a1bc32324942a4624af0dc16f29882f3ebb9c1d4f792e8ff`.
The previous `.env` was privately backed up on the test server before the
version change.

The container is healthy with zero restarts. Readiness reports PMS healthy
and Valkey ok. The first full sync completed with 112,078 items and all four
section cursors complete; none remained running or in error. Origin and sync
error counters were zero. No sync or event-stream reconnect log entry appeared
in its first 15 minutes. No current-version playback evidence has been captured.

The first Plex Web attempt at `replex.lukemulvaney.com` felt slow for
collections and Continue Watching. Recent Replx logs show the Plex Web page
assets and identity requests, but no `/library` or `/hubs` requests. Cache
hits and misses stayed at zero. This is evidence that those browse requests
did not traverse Replx during the attempt; it is not a valid cache-speed
measurement. Confirm the client's selected API connection before measuring
load times again.

The user's Firefox network export confirms this: its 11 captured requests
all targeted a `*.plex.direct` hostname (four `/hubs/promoted` requests and
seven artwork requests). No response carried `X-Replx-Edge-Cache`. The public
hostname itself returns Replx headers, so the Plex Web shell reaches Replx
but Plex Web selects a direct PMS connection for its browse data. The captured
successful hub responses took approximately 178–238 ms each; this does not
include the complete screen render time. The export is not copied into this
repository because it may contain authentication material.

Replx's active server is labeled `Lukeflix` and uses the same public
`*.plex.direct` origin observed in the browser export. Its machine identity
does not match the local `plex` container on `oi-2` (confirmed against that
container's live `/identity` response). The local Plex preferences also have
no `customConnections` setting. Confirm which Plex server is intended for
this test before changing origin or discovery configuration.

The operator confirmed `Lukeflix` is the intended origin. A temporary
Firefox request block for `plex.direct` caused Plex Web to send browse
requests through Replx, proving fallback is possible for this browser.
During that attempt, counters reached 400 cache misses, 2 hits, 95 control
502 responses, and 97 origin transport errors. Collection child and home hub
requests were among the 502s. The process remained healthy with zero
restarts, but this is a release-blocking user-visible failure. Five
credential-free `/identity` probes from `oi-2` to the public origin all
succeeded in roughly 0.20–0.22 seconds; the failures appear under real
browse traffic rather than simple connectivity. The current request logs
omit the underlying transport error, so its cause remains unverified.

A second browser export captured three `/hubs/sections/23` requests to
Replx, all with browser status 0 and no usable timing. Replx's own logs
showed repeated roughly 10-second 502s on that route, alongside occasional
successful responses and one 1 ms cache hit. By the end of the browser-only
test, process counters reached 762 misses, 37 hits, 406 control 502s, and
612 origin transport errors. These are cumulative process counters, not a
single-page failure rate. The operator reported that collections eventually
appeared but were very slow.

A diagnostic change categorizes origin transport failures without logging
tokens or request URLs. An additional change makes the event-stream
connection metric report its true state. Automatic approval review initially
rejected uploading these private-source changes; the operator explicitly
authorized a retry, and both were then deployed to the test stack.

## Artwork cache root cause and correction

After the operator explicitly authorized retrying the GitHub upload, commits
`7670954` and `e0cddb8` were pushed and deployed to the test stack as image
`ghcr.io/ljam96/replx-edge:0.4.0-e0cddb8-test` (image ID
`sha256:c9d8e49cf695869f1e4c3a6f3c1937266afcf729b4f80b56bfcfd831ba44e168`).
The container was healthy with zero restarts. Safe error classes show browser
request cancellation on slow collection/hub requests and an unexpected EOF
on the notification WebSocket; they do not establish that the PMS origin
itself failed.

The Firefox retest still felt slow, including the repeated collection opening.
Recent logs showed 137 successful artwork transcodes, all cache misses.
The `replx-edge` container runs as UID/GID 65532, while the named artwork
volume root was mode 0755 and owned by root. There were zero files in the
artwork volume. Replx ignored `Store.Set` errors, so the unwritable volume
caused every poster to be fetched again. The same ownership mismatch affected
the cache and diagnostics volume roots, although browse metadata uses Valkey.

Commit `751567d` adds a one-shot Compose permissions service before Replx
starts and checks artwork write access at startup. It was deployed as
`ghcr.io/ljam96/replx-edge:0.4.0-751567d-test` (image ID
`sha256:1437cd23f73c0787dc379bdf0e24ef129aa7665ca450c27ecd5616a0e8327c02`).
The container is healthy with zero restarts. All three volume roots are now
mode 0700 and owned by UID/GID 65532; startup reports the Valkey cache enabled
and no artwork degradation. The artwork cache was empty immediately after
deployment. This addresses the measured repeated-poster cache failure; it
does not prove first-load speed or resolve browser request cancellations.
Re-test cold and warm collection loads before claiming a speed improvement.

## Direct owner preload prepared

A bounded background preloader now fetches common owner browse pages,
section collection lists, and up to 96 recent poster transforms directly from Plex using the encrypted owner
credential. It stores only in the canonical owner scope and skips absent
credentials. Artwork keys omit nested Plex tokens so a preloaded transform
matches a later owner request after token rotation. Full Go tests, `go vet`,
and Compose configuration validation pass locally. Exact Plex Web query
variants and other users' personalized pages are not preloaded.

The test host became reachable and commit `f33df26` was deployed as
`ghcr.io/ljam96/replx-edge:0.4.0-f33df26-test` (image ID
`sha256:4b03074c65bc429c04571ede055a49c132bd8bd19cfa07d3b7031e51d455ae73`).
The previous `.env` remains privately backed up on the test server.
Readiness reports `ready`, PMS healthy and Valkey ok; container restarts remain
zero; the owner index still contains 112,078 items. The first preload pass
stored 8 pages and 96 posters, with 4 page errors. The artwork volume had
208 files after the pass (two per entry plus a few existing entries).

Using the diagnostic browser token without printing or storing it, a poster
selected from the preload candidate query returned `200 hit` twice through
the public Replx hostname (about 0.16 and 0.08 seconds). `/library/sections`
and `/hubs/continueWatching` also returned `200 hit` (about 0.07–0.08
seconds). A section hub variant with Plex Web query parameters initially
timed out twice at 12 seconds via Replx, then returned `200 miss` in about
0.22 seconds and `200 hit` in about 0.10 seconds. The same variant returned
200 directly from the Plex origin in about 2.5 seconds from the Mac and
0.59 seconds from `oi-2`. The intermittent slow path is not resolved or
explained by the preload; keep it as a production release blocker.

## Firefox hub profile preload on test

Commit `4d94c24` adds an optional token-stripped Plex Web hub query profile
to the owner preloader. The Firefox capture's collection request carried
options absent from the previous plain preload, so it mapped to a different
cache key. The profile preloads section hubs and Continue Watching under the
owner scope without merging users or dropping response-shaping options.

The commit passed `go test ./... -count=1`, `go vet ./...`, and Compose
configuration validation. It is deployed on `oi-2` as
`ghcr.io/ljam96/replx-edge:0.4.0-4d94c24-test` (image ID
`sha256:aa054cf36dad851872a10a2572475c9edaaa9c91f13eac2e5f9081f1a9187690`).
The container is healthy with zero restarts. The first preload pass stored
8 pages and reported 4 errors; the artwork cache was already warm.

Replaying the exact captured Firefox `/hubs/sections/23` request through the
public hostname yielded `200 hit` twice, at about 0.21 and 0.11 seconds.
The same query profile on `/hubs/continueWatching` yielded `200 hit` in
0.08 seconds. After more than a hub TTL, both routes still yielded hits
at about 0.07 and 0.23 seconds. This proves the captured owner query is
preloaded and refreshed. It does not prove other clients or Plex users get
the same result, nor explain the earlier intermittent 12-second timeout.

## Evidence sequence

The 12:50 BST browser capture supplied after the Firefox preload test shows
454 requests from a page whose referrer was `replex.lukemulvaney.com`, but
all requests targeted `plex.direct` hosts and none targeted Replx. Its creator
identifies as Zen. Fifty requests were collection children; six completed
with 200 after a median of about 11.1 seconds and 44 were recorded with
status 0. Of 394 poster requests, 393 completed with 200; timed successes
had a median of about 2.9 seconds. These timings are direct Plex traffic,
so Replx's preload and cache could not accelerate this browser load. The
captured collection query replayed through Replx returned 200 miss in about
3.21 seconds, then 200 hit in about 0.08 seconds. Routing browser control
requests through Replx is therefore a release blocker; a Replx page URL
alone does not prove that the Plex app selected Replx for data traffic.

## Zen forced-route failure and notification correction

After temporarily blocking `plex.direct` in Zen, the operator saw fast
initial content followed by Lukeflix libraries toggling offline. The test
container stayed healthy with zero restarts. The Replx gateway received
real browser traffic: cached section hubs were sub-10ms inside the server,
but many uncached collection-child requests were canceled by the browser
after about 30 seconds, and `/media/providers` was canceled repeatedly.
`/status/sessions` returned the deliberate 403 specified by the protocol
matrix. The live notification request used `/:/websockets/notifications`
(plural), while the streaming dispatcher recognized only the singular
form, causing generic proxy failures.

Commit `0982f96` recognizes both notification paths as raw WebSocket
tunnels. The regression test failed on the plural form before the change
and passed afterward; full Go tests, vet and Compose validation passed.
The fix is deployed as `0.4.0-0982f96-test` (image ID
`sha256:9ebf8566329796aa5d7c5529a66774cdeb31c0faa1531c0f6fcffd17a11f568b`);
the container is healthy with zero restarts. A browser-shaped handshake using the captured Plex options
returned `101 Switching Protocols` through both direct Plex and Replx.
This proves the route protocol fix, but the Zen offline behavior and
collection burst still need a fresh browser retest before release.

## Zen retest and bounded browse experiment

On the next Zen test, Lukeflix stayed online and the notification WebSocket
returned 101. Repeated `/hubs/sections/23` requests were mostly cache hits
served by Replx in 0–3ms internally. The same query missed twice and waited
about 3.8 and 10 seconds for origin headers; the browser's brief content
refresh is consistent with those hard-expiry misses. Across nearby browser
loads, collection-child requests reached 28 concurrent origin fetches;
74 of 77 recorded requests were canceled before Plex answered. One
`/library/sections/23/collections` request made directly from `oi-2` to the
configured Lukeflix origin timed out at 12 seconds, proving that path can
stall without the Replx public ingress. A later direct request returned 200
in 0.43 seconds (0.34 seconds connecting, 0.06 seconds to headers), showing
the slowness is intermittent.

The operator identified a Docker container named `agregarr` on `oi-2`.
It was running, but its container logs contained no entries during the
12:00–12:09 slow-load window. That silence does not establish that it was
idle. Its generated collection count may make Plex Web issue many child
queries. The `plex` container on `oi-2` has a different machine identity
from Lukeflix, so its process metrics do not describe the target PMS.

Commit `030300a` limits Replx to eight simultaneous uncached browse origin
requests and records queue time separately. A concurrency regression test,
full Go tests, vet and Compose validation passed. It is deployed to test as
`0.4.0-030300a-test` (image ID
`sha256:d972e2c2e51ccebccd3ed80182cbe0fa1ae0dd365011579cc199399bdeebc1db`),
healthy with zero restarts. Its first preload pass stored 9 pages with 1
error. A bounded replay of 12 captured collection-child requests returned
eight 200 misses and four client timeouts at 18 seconds (median 9.83
seconds). The limit has **not** proved the cold collection path reliable.
Do not promote this test build until a full browser retest and Lukeflix PMS
log review explain the remaining stalls.

## Lukeflix Plex log review (operator ZIP, 23 September)

The operator supplied a Plex Media Server log archive covering the browser
test. Request IDs were matched between Plex's `Request` and `Completed`
records; raw URLs, tokens, IPs and media titles were excluded from the
analysis. Times below are Plex log times.

| Interval | Completed collection-child requests | Median | p95 | Longest | Peak Plex live requests | Slow-query warnings |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| 11:49–11:56 | 34 | 10.01s | 11.07s | 11.29s | 23 | 1 |
| 11:56–12:09 | 324 | 49.45s | 100.8s | 123.95s | 77 | 392 |
| 12:09–12:18 | 120 | 21.09s | 60.73s | 65.42s | 52 | 76 |

For example, one collection-child request took about 54 seconds from
11:59:29 to 12:00:23. A logged 36-item query inside it took 240ms, so that
query alone does not explain the end-to-end delay. Plex also logged many
scanner-related lines during the test and slow-query warnings during the
browser's collection request burst. The severe period had 21–46 new
collection-child requests per minute from 11:56 to 12:04, apart from
12:02. There were no `PUT /library/metadata/*` requests from 11:56 through
12:04, so concurrent metadata edits cannot by themselves explain that
period. The number of simultaneous collection requests and Plex-side
latency are the strongest evidence for the cold-path bottleneck; the logs
do not isolate CPU, disk, database locking, or another internal wait as
the precise cause.

The word `agregarr` appears in some Plex request query values for metadata
labels. Those lines show label-related GET and PUT operations before 11:56
and again around 12:05 and 12:09–12:15. The label does **not** identify the
requesting client, so these records alone cannot prove which process made
the changes. The prior absence of container log entries should not be used
as evidence of inactivity.

This confirms the test build is not production ready: a cold collection
burst can take tens of seconds at the actual Lukeflix origin, even with
Replx limiting its own concurrent origin fetches. A warm Replx cache can
serve a matching request quickly, but Plex Web may issue new query variants
or bypass the Replx route. Further work needs a controlled cold/warm
browser trace and a Plex-side performance window with collection and scan
activity observed together.

1. Deploy a versioned build containing the correction. Record its image digest and commit.
2. Trigger a full owner sync. Require four completed section cursors, a plausible nonzero item count, and no new sync errors. Confirm the event stream remains connected while idle for at least 15 minutes.
3. In Plex Web through the Replx connection, open Home, one large library, and one collection twice. Record screen load time and Replx cache counters before and after each repeat load.
4. Repeat the browse checks as a restricted Plex user. Confirm restricted libraries, watched state, and Continue Watching never cross users.
5. On Plex Web and a physical TV, play a progressive title, seek, stop, and resume. Record client version, playback type, request/session IDs, whether a 307 reached the origin, origin TLS, Range behavior, and any failure code.
6. Test a transcode and subtitles where supported. Record each mode separately in the compatibility matrix; leave untested modes `UNKNOWN`.
7. Run sustained normal use for at least 72 hours. Record restarts, 5xx responses, origin errors, sync errors, event stream uptime, cache hit rate, and p95 screen load times daily.
8. Restore a database and secret backup into a separate clean stack, then verify onboarding state, policies, and library index. Test one upgrade and a documented rollback path.

Production release requires complete client traces, user isolation, a stable index, a successful restore drill, and measured speed improvements. A healthy container alone does not satisfy these gates.

## Collection cache implementation after Plex log review

The next test build preloads collection-child responses from Plex's section
collection lists in rotating batches of four per pass. Collection preload
and owner refresh allow up to 90 seconds for slow Plex responses. A
successfully fetched collection response also has a 15-minute fallback
copy under the same user, query and invalidation generation key. When the
normal two-minute entry expires, Replx can serve that copy immediately to
a recently validated user while at most two background refreshes run.
Continue Watching is excluded and retains its short TTL and watch-state
invalidation. The owner preloader never writes its response into another
user's cache scope. A collection query profile can be configured with
`REPLX_EDGE_PRELOAD_COLLECTION_QUERY`; query tokens are removed before
fetching or key construction. Collection preloading remains gradual, not
instantaneous, and other users' first requests are still cold until their
own cache is filled. The build requires a live browser test before any
speed or production claim.

Commit `528039f` was deployed to the `oi-2` test container as
`0.4.0-528039f-test` (image ID
`sha256:1ae93bc5e937049e93241a9fcd2ef65b97b95b875c5524b3ca0b89824f74556a`).
Two token-free collection request profiles from the Zen capture were
configured. The container became healthy with zero restarts. Its first
preload pass stored 17 pages and reported three errors; eight collection
fallback keys were present shortly afterward. A captured collection-child
request replayed through the internal Replx listener returned 200 in 4.7s
on its first miss, then 200 cache hits in 31ms and 1ms. A direct public
script request received a 403 before Replx, so the internal replay is the
valid cache measurement. The browser experience, stale fallback after TTL,
and restricted-user behavior still need live validation.
For the same validated user, deleting only the fresh entry in Valkey caused
the next request to return 200 `stale` in 32ms; the following request was a
fresh cache hit in 34ms after background refresh. This validates the
fallback mechanism for one collection query. A separate stale-response
counter was added to cache stats and Prometheus metrics for later monitoring.
The metric build `0.4.0-2a5ec99-test` (image ID
`sha256:6361f96d4f0a7f0ec25c3097ffdeae47a27ac903226b928348949dfb3fbe0212`)
was deployed on `oi-2`. It became healthy with zero restarts, no application
errors in the initial log sample, and an initial preload pass of 11 pages
with zero errors. The earlier cache replay measurements used the same cache
behavior in the immediately preceding test image.

## Older logs and account-switch retest

The operator reported an empty Home screen on admin sign-in, then slow
collection browsing after switching to the Luke user. Replx's current
container started at 12:45:55 UTC and remained healthy with zero restarts.
Its logs through 22:17 UTC contain 266 HTTP requests and no
`/library/collections/*` request at all. Home, library section and Continue
Watching requests are present. The Luke collection traffic therefore did
not traverse this Replx container during the captured period, if the
reported browsing occurred after that start time. A browser HAR is needed
to identify its actual hostname; direct Plex routing is the leading
hypothesis, not yet confirmed.

Around 12:47 UTC, Plex Web made four `/hubs/promoted` requests with distinct
`contentDirectoryID` values. Two returned nonempty 200 responses in 141ms
and 433ms; two other variants waited about 30 seconds and were canceled
with Replx 502 responses. An `/library/sections/*/all` request was also
canceled, while two others returned nonempty 200s. The empty Home view is
consistent with the canceled Home requests, but the logs cannot establish
exactly what the browser rendered.

The supplied Lukeflix PMS archive covers 11:20–12:18 UTC only, before this
account-switch retest. Earlier collection-child requests took a median
3.55s (23 requests, 11:20–11:35) and 3.17s (23 requests, 11:35–11:49),
with no slow-query warnings. At 11:56–12:09, 324 requests took a median
49.52s and Plex recorded 392 slow-query warnings. The burst, rather than
metadata updates alone, tracks the severe origin slowdown. The archive
cannot diagnose the later Luke session directly.

## Luke managed-user cache bypass

The next Luke test, after the structural Home preload deployment, still
showed slow loading and "No Content Available". Replx recorded no
collection-child requests in that window. Luke's Home and Continue
Watching requests did reach Replx, but were marked `bypass`; two Home
requests were canceled after about 30 seconds. The token fingerprint's
identity row had status `invalid` with no linked identity. A Luke Continue
Watching request returned 200 from Lukeflix under that fingerprint, while
an intentionally invalid token returned 401 for both the same feed and
`/library/sections`. Thus plex.tv account validation alone was an
incorrect authority for this PMS-accepted token. Managed Plex Home users
cannot sign in directly to a Plex account, which is consistent with the
observed account API rejection; the exact plex.tv status is inferred from
Replx's invalid-token handling, not captured as raw response in this log.

The correction verifies a plex.tv-rejected token against the configured
Lukeflix `/library/sections` endpoint. On a PMS 200 it marks the token
`pms_valid`, serves only its own fingerprint cache scope, and revalidates
after five minutes. PMS 401/403 or an uncertain transport result cannot
authorize cached data. It does not make the first slow Luke collection
request fast; its effect on repeat loads needs a live browser retest.

## Managed-user validation test deployment

Commit `f7267fb` was deployed to `oi-2` as the local test image
`ghcr.io/ljam96/replx-edge:0.4.0-f7267fb-test` (image ID
`sha256:ae17dd96db5f6e94bece978c64e02749c54caae1a90477073b1d69129574abcc`).
The container became healthy with zero restarts. Its first owner preload pass
reported 11 pages and zero errors.

During Luke's first observed session after deployment, the identity table had
one `pms_valid` token and three account-valid tokens. Replx received 26
collection-child 200 cache misses and one 200 cache hit, establishing that
Luke's collection requests now traverse Replx under the PMS-validated scope.
The misses reached 16.1 seconds. Three Home hub requests returned 200 as
cache misses; another returned 502 after 30.0 seconds. 192 image requests
were 200 cache misses with a 2.57-second maximum. All captured requests
were concentrated in one minute. This is evidence of improved routing and
cache eligibility, but not yet evidence of fast repeat browsing or a reliable
Home experience. Browser retest feedback is pending.

## Repeat Luke browse and structural Home fallback

A repeat Luke visit about four minutes after the first showed 27 collection
child responses served stale in a median 1ms; three other collection child
requests missed (0.8–2.9s). The slow path was Home: two `/hubs/promoted`
requests repeated exactly the earlier non-secret query signatures, but were
misses after 22.3s and a 30.0s 502 respectively. The earlier successful
Home response had a one-minute fallback TTL, which had expired before the
repeat visit. Continue Watching returned live in 363ms; it remains outside
fallback caching. This isolates the repeat-view delay to structural Home
requests rather than collection child cache throughput.

Commit `176eaea` extends the structural Home fallback (`/hubs/promoted` and
`/hubs/sections/*`) to 15 minutes. Fresh cache TTL stays ten seconds;
validated users get the last successful exact-user, exact-query structural
Home response immediately while a bounded refresh runs in the background.
Continue Watching and Recently Added are unchanged. The full Go test suite
passed. Test image `ghcr.io/ljam96/replx-edge:0.4.0-176eaea-test` (image ID
`sha256:a7260c63ad1129c1e56fa7d2a180d0975373105fab41a4456a3d94d14b6bd17e`)
was deployed to `oi-2`; the container is healthy with zero restarts, and its
initial owner preload reported eight pages and zero errors. Luke browser
validation after this build is pending. The first new Home variant remains
dependent on an origin success before a fallback can exist.

## Immediate browser retest after Home fallback change

In the first Luke browser session after deploying `176eaea`, 27 collection
children were served as 200 stale responses in at most 1ms. The first
`/hubs/promoted` variant missed and took 27.8s; another was canceled after
30.1s, so the first view remained slow. In the next visit, one promoted
variant returned 200 stale in 1ms, while another missed and returned 200 in
302ms. A concurrent group of collection misses was canceled by request
context; these logs do not establish that PMS itself rejected those queries.
Later cold child misses took up to 27.6s and some were canceled at around
30s. Thus the longer fallback improves already populated exact-query
responses, but does not eliminate cold variants. Operator visual feedback
on poster display time is still pending.

## Blank deeper collection pages and library browse

In Luke's latest visual test, initial collection items appeared fairly quickly,
but deeper horizontal items lacked names and artwork, while a library was slow
or showed no content. The matching Replx log window had 137 collection-child
requests. Their pagination was not limited to the preloaded first profile:
114 requested start 12, size 24; the rest requested start 36, size 39 or
other later offsets. Some of these uncached deeper pages took 19–28 seconds
before returning 200; others were canceled at about 30 seconds and returned
502 through Replx. A library `/library/sections/*/all` request was canceled
after 26 seconds; a later request returned 200 after 21 seconds. The blank
items are consistent with missing or canceled page data; logs do not prove
which individual response the browser rendered.

The configured collection preload profiles both target start 12, size 24, and
the owner-only warmer cannot populate Luke's user-specific cache. Aggregarr
on `oi-2` has 55 active collections, all marked library-promoted; 52 have
`maxItems` 350. This is a large set of paged content for Plex Web to request.
No Aggregarr settings were changed.

Commit `6309e9a` adds a five-minute same-user, exact-query stale fallback for
successful library `/all` pages. Continue Watching remains live and uncached
by fallback. Full Go tests passed. The test image
`ghcr.io/ljam96/replx-edge:0.4.0-6309e9a-test` (image ID
`sha256:74d7cc80bfd2e9b2fb62fbd53ea9996a75e8210f709d1b096ad5baca98c2c85b`)
is deployed and healthy with zero restarts. This improves repeat library loads
after one successful response; it does not cover uncached deeper offsets.

## User-scoped full collection windows (24 September)

The operator confirmed that blank deeper items occurred across all
collections, so a fix limited to named collections would not address the
pattern. Replx previously warmed only two exact first-page profiles
(start 12, size 24); Plex Web requested other starts and sizes as the
operator scrolled. Aggregarr's 55 active promoted collections make that
mismatch repeat across the catalogue.

The operator approved encrypted retention of validated user tokens for
per-user background warming. Commit `9ce2d56` stores a validated token only
as AES-GCM ciphertext under a separate key purpose, clears it after a
confirmed rejection, and uses it to refresh tracked pages in the matching
user scope. Daily retention clears tokens unused for 30 days. Commit
`161df4d` backs off failed refreshes to avoid retrying a slow PMS every two
seconds. The live PostgreSQL tests for encryption, revocation, scope
isolation, and retention passed on an isolated `replx_edge_test` database.
Image `0.4.0-161df4d-test` was deployed healthy with zero restarts.

Commit `603443f` adds a per-user complete JSON collection window. A bounded
background pass rotates through the collection catalogue and exact browser
query profiles, fetching up to four windows for one user at a time with that
user's credential. The cache key removes only the two pagination parameters;
it retains user scope, all other query fields, response representation, and
invalidation generations. Replx slices a covered window into the requested
page while preserving Plex's `totalSize`; incomplete windows fall through to
Plex. Unit tests and live PostgreSQL tests passed. Test image
`0.4.0-603443f-test` (image ID
`sha256:7d57546caaff5c7a0b798330afc0c4ce205310babd98688c7aa6c64162399857`)
was deployed healthy with zero restarts. The first four reported window
passes fetched four pages each with zero errors. Cached window keys were
present for both the owner and managed-user scopes. A direct request for
a managed-user deep page (start 36, size 39) returned HTTP 200 with Replx's
`window` cache state and 39 items in 0.75 seconds including token validation.
That is a server-side proof for one page, not yet a complete browser proof
for all 55 collections. Background coverage is still filling.

The first window rollout used a 30-second pause between bounded passes.
Cached entries reached both scopes (28 owner and 24 managed-user windows)
without reported preload errors. Commit `5e3523a` reduced that pause to ten
seconds after each pass and limited full-window refreshes to 45 seconds.
The test image `0.4.0-5e3523a-test` (image ID
`sha256:b730d2d8b88495c24b5b7ec4a0230dcdde41c674a6b0640c429044177c405b10`)
was deployed healthy with zero restarts. Two subsequent full-window passes
reported four pages and zero errors each; user-specific window count continued
to rise. This is still a staged test, not a production reliability gate.

After the accelerated pass completed, Replx held 110 owner and 110 managed-user
collection windows. A check against Aggregarr's 55 active collection IDs and
the two configured browser profiles found all 110 Luke variants present.
Every Luke window was complete (`Metadata` count equaled Plex `totalSize`),
with collection sizes from 0 to 347 items; none exceeded the requested
350-item window. Recent pass logs reported zero errors. This proves cache
coverage for the configured Zen profiles at that moment. It does not prove
that Plex Web will request only those profiles, nor that image loading and
library browsing are fast. Operator scroll feedback is pending.

## 2026-09-24: Luke browser profile mismatch and test correction

Luke reported a slow collection opening in Zen with `plex.direct` blocked.
Sanitized edge logs from the session showed 25 collection-child requests, all
cache misses, median 13.45 seconds and maximum 21.65 seconds. Two hub requests
also took more than 21 seconds and a third timed out at 30 seconds. This
confirmed the user-visible delay was upstream collection and hub work, not
merely poster delivery. Comparing query key names and values without printing
credentials showed every collection-child request differed from the two
configured warmer profiles only in `X-Plex-Device-Screen-Resolution` and
`pinnedContentDirectoryID`. The former changed with the browser window, and
the latter changed with the Plex sidebar context. User, path, content-shaping
query fields and JSON representation matched.

Commit `03e9b50` normalizes those two context fields in the *collection window*
key only. The same normalization is used by the preloader and refresh path.
User scope, all other query fields, representation and invalidation generations
remain in the key; the proxy still revalidates credentials and slices only
complete JSON windows. The added test checks different display and sidebar
values share one window, while different users and `includeMeta` values do not.
Focused cache, warmer and proxy tests and the full `go test ./...` suite passed.
The test server now runs `0.4.0-03e9b50-test`, initially healthy with zero
restarts. Background rebuilding started with repeated four-page passes and zero
reported errors. Browser confirmation and full rebuilt-window coverage remain
pending; this is not a production gate result.

The rebuilt window count reached 440 keys: 220 retained old-profile keys plus
220 new normalized keys (55 collections × two profiles × two user scopes).
The warmer reported 220 newly fetched pages and zero errors. The container
remained healthy with zero restarts. Luke's post-fix Zen retest is requested
and still pending, so the server-side coverage does not establish browser
latency yet.

## 2026-09-25: Luke browser result and compressed poster fix

Luke reported substantially faster collections but missing posters for
Monster (2022) and Neagley. The corresponding session had 52 collection-child
requests, all served as `window` hits (median 87 ms, maximum 186 ms). Artwork
had 338 cache hits at a median 3 ms. The two named poster requests returned
HTTP 200 from the artwork cache, with 61,118 and 66,341 byte bodies. Inspecting
those two cached bodies on the test host found gzip headers; decompressing them
produced valid JPEG headers. The artwork hit path had returned the stored gzip
bytes without a Content-Encoding header, explaining blank images despite 200s.
Three other poster requests returned Plex-origin 404s for different titles;
those are separate source failures and were not attributed to the named titles.

Commit `fb20242` normalizes gzip artwork to image bytes before saving, and
decodes legacy gzip cache entries on read. Unsupported encodings are not
cached. A proxy test reproduces a gzip origin image and verifies the cache hit
returns JPEG bytes with no Content-Encoding header. Storage tests cover new and
legacy gzip entries. The full `go test ./...` suite passed. Image
`0.4.0-fb20242-test` (ID
`sha256:7d429a178d9b28cadd7ec7b6a7ffaa176d90129f0af41f689f44bc3fe862b643`)
was deployed to `oi-2`; it became healthy with zero restarts. Browser poster
confirmation is pending.

Luke's browser retest confirmed Monster (2022) and Neagley posters now display.
The follow-up server logs showed 35/35 collection-child requests served from
complete `window` cache entries, median 57 ms and maximum 122 ms. Artwork had
214 hits, median under 1 ms, plus 51 successful misses, median 135 ms. The
container remained healthy with zero restarts. Luke still perceived a slight
slowdown; the remaining delay was in Home hub requests: three 200 misses took
7–11 seconds, and one `/hubs/promoted` miss timed out at about 30 seconds with
502. The browser's Home hub query differs from the owner-only preloader profile
in account/sidebar context fields. No unsafe cross-user hub normalization was
applied. Home first-visit performance and Plex origin timeout resilience remain
open production gates.

## 2026-09-25: Home promotion reduction and private Home warmer

At the operator's request, Aggregarr's Home promotion flags were reduced from 55
active collections to 35. The 20 hidden from Home remain active and available
in Collections and library recommendations. IMDb Top 250 was removed from Home;
Horror and Romance movie and TV rows were restored. The Aggregarr settings and
Plex's live Home hub flags were verified to agree for all 20 changed rows.
Aggregarr's full sync stalled loading its shared library cache, so the 20 Plex
flags were changed through the same Plex hub management endpoint Aggregarr uses.
Backups of the Aggregarr settings and prior Plex flags were saved on oi-2.

A direct Plex origin probe after the reduction still took 26.2 seconds for one
promoted TV Home section, so the lower row count by itself does not establish
fast Home loads. Commit `bd2e41a` adds persistent, secret-stripped per-user Home
query profiles and a bounded separate warmer for active users' pinned sections.
Each refresh uses that user's validated retained token and exact user cache
scope. Promoted Home cache keys ignore viewport size while preserving user,
section, pinned sections, other query fields, representation, and invalidation
namespaces. Continue Watching keeps its separate short-refresh policy. The
admin dashboard now displays Home warming counts and time alongside collection
warming. A focused managed-user test checked exact token use and absence of
owner-scope leakage. `go vet ./...`, `go test ./...`, and dashboard JavaScript
syntax checks passed.

Image `0.4.0-bd2e41a-test` (ID
`sha256:6533c142b2d1859cfe6fd5ea8227b1eb25da498efbbc401f4eb7c954397ab8ed`)
was deployed to oi-2. Docker reported healthy and zero restarts;
`/health/ready` returned 200, `/admin` without a session returned 401, and
`/admin/login` returned 200. One Home profile had been saved shortly after
deployment. Luke's browser latency and completeness retest is pending, so this
remains a staged test, not a production reliability gate result.

## 2026-09-25: browser “No content available” and connection correction

Luke reported “No content available” in collections and libraries, and the
Plex admin user also saw it. Replx Edge received no browser library or hub
requests around that report. The new Home build was rolled back to
`0.4.0-463e75e-test` while investigating; the older image became healthy with
zero restarts. Direct PMS checks with the owner token returned all four
libraries, 27 collections in section 22, and Continue Watching (HTTP 200).
The same owner token through the Replx Edge container returned the four
libraries and 27 collections. This rules out an empty Plex library or a
server-side cached empty response in those checks, but does not identify the
client's failed connection.

Plex resource discovery listed `replex.lukemulvaney.com` as an HTTPS
connection for the same Lukeflix machine alongside several `plex.direct`
connections. A public `/web` request returned a 302 to the blocked
`plex.direct` hostname. Commit `11bbb03` rewrites only Plex Web redirects
back to the configured public Replx URL when the destination is the same PMS
origin. Tests cover the redirect and rejection of unrelated external/media
redirects. `go vet ./...` and `go test ./...` passed. Image
`0.4.0-11bbb03-test` (ID
`sha256:c6efcb2fe4f3403f54cb9300e4dc51796ef40170a76bde390e02fc01765752b1`)
was deployed to oi-2 healthy with zero restarts. A public `/web` probe now
redirects to `https://replex.lukemulvaney.com/web/index.html`. Owner API
probes through the public Replx URL returned HTTP 200 with four libraries and
27 collections as cache hits, about 0.1 seconds each. Browser retest and the
failed request's hostname are pending; production readiness remains unproven.

## 2026-09-25: wrong advertised public port and fresh Luke latency

Zen's Plex Web console reported Lukeflix unavailable at
`https://replex.lukemulvaney.com:32400/media/providers` (status 0). The
actual Cloudflare public connection is HTTPS port 443. A fresh plex.tv
resource lookup confirmed Plex published the Replx hostname with port 32400,
although PMS `customConnections` held a hostname-only URL. The existing
onboarding check matched the hostname only and had accepted the unusable
connection. The PMS setting was backed up on oi-2 and changed to
`https://replex.lukemulvaney.com:443`, preserving its other custom URL.
A fresh plex.tv resource lookup then published the Replx connection on port
443. Luke reported that content returned after a browser reload, but was
still slow. The corrected setup is confirmed by direct resource discovery
and browser content, not yet a complete browsing performance pass.

That browser session reached Replx Edge: 176 likely browser requests in the
sample, including 133 artwork cache hits. Collection-child requests were
mostly misses and several took 5–10 seconds; one promoted Home request timed
out after about 30 seconds with HTTP 502. The collection preloader used the
Plex Web `bundled` model and a different client identifier, while the new
browser session used `standalone`; all other content-shaping fields for the
movie collection profile matched. Commit `087b462` ignores those two client
context fields in the complete collection-window key while retaining user
scope, section, content fields, representation and invalidation generations.
Onboarding now verifies the actual published HTTPS port and writes an explicit
`:443` while preserving other custom URLs. Focused and full Go tests and
`go vet ./...` passed. Test image `0.4.0-087b462-test` (ID
`sha256:65dfeb707f0eb54475deac1b12e5c90d0b3ea27c80a079225b4690b51772527d`)
was deployed healthy with zero restarts. The first two window passes filled
eight pages with zero errors; full coverage and browser retest are pending.

## 2026-09-25: Plex Web could not read cached Home or error responses

Luke's screenshot showed the global Home “No content available” state while
Lukeflix appeared online. The matching edge trace showed both promoted Home
sections returned HTTP 200 from user-specific stale entries in 1–2 ms.
Inspection of those cached JSON bodies found 17 and 18 populated Home hubs.
The browser console then reported CORS failures on Replx 502 responses and
marked Lukeflix unavailable. Direct PMS probes showed it echoes a request's
Origin and sends `Vary: Origin, X-Plex-Token`; the Replx cache hit/stale/window
paths had omitted `Access-Control-Allow-Origin` entirely. Thus an HTTP 200
cache hit could be unreadable to Plex Web, and a 502 without CORS could look
like a lost server connection. Most contemporaneous 502s were requests that
the browser had already cancelled, not proven PMS connection failures.

Commit `99333f5` adds request-specific Plex-style CORS headers to user-scoped
cache hits, stale responses, collection windows and cached artwork. Commit
`78538f6` applies them to edge-generated error responses too and avoids
writing a 502 after the browser cancels its request. Regression tests cover
those cases and reject malformed Origins; `go vet ./...` and `go test ./...`
passed. Test image `0.4.0-78538f6-test` (ID
`sha256:1c664bc23861dd0de91765c5636a2745bae37c35e36d8811a3e39beecbf06b82`)
was deployed healthy with zero restarts. A public owner Home probe returned
HTTP 200 as a cache hit in 0.23 seconds and echoed
`Access-Control-Allow-Origin: https://app.plex.tv`. Luke's browser retest is
pending. This verifies the header fix, not full page usability or production
reliability.

A second Zen console capture showed `Access-Control-Allow-Origin` missing on
HTTP 502 responses. Plex Web treated those as network errors, retested all
Lukeflix connections and removed the server from its usable list, explaining
the flash from visible collection rows to the global error page. Commit
`78538f6` was verified through the public hostname: a cached owner Home
response returned HTTP 200 with the expected CORS header in 0.23 seconds.
After that rollout, Luke reported Home remained visible without flashing and
the Movies library loaded in about three seconds on one attempt. Those are
positive browser observations, but Home still felt slow and the large/deep
collection retest is pending.

Luke next opened a large collection: first appearance took about 11 seconds,
while scrolling to deeper items worked and showed content. Neither Replx Edge
nor the legacy Replex container recorded a collection-child or poster request
in the corresponding observation window. The client may have used its own
cache or another Plex connection; the browser request hostname and repeat
opening time are needed before attributing that 11-second delay to the edge
cache or claiming a performance improvement. The edge container remained
healthy with zero restarts.

## 2026-09-25: library route and CORS connection failure

Zen's Movies opening took roughly 30 seconds with `plex.direct` unblocked.
The matching Replx trace had no `/library/sections/*` request, and the
browser's Network panel confirmed a `plex.direct` hostname. A controlled
owner `/library/sections/23/all` probe took 3.68 seconds directly against
PMS and 0.76 seconds via Replx (a Replx cache miss). Those numbers do not
explain the entire 30-second browser wait, but they prove this library test
bypassed Replx.

Lukeflix's custom server access URLs listed the working direct Plex address
and Replx. Reordering them in PMS settings did not change the plex.tv
resource order: direct remained first. The direct custom URL was removed for
a reversible discovery test, leaving `https://replex.lukemulvaney.com:443`
as the sole custom URL. Plex then published Replx as the first usable remote
address. The original direct endpoint still answers when called explicitly;
Plex's automatically detected remote address timed out. The previous settings
were backed up on `oi-2` at
`deploy/custom-connections.before-direct-removal-test.json`. Automatic
direct fallback is therefore unavailable in this test configuration; this
tradeoff needs a production decision.

With Replx selected, Zen reported Lukeflix offline. Its Console identified
the exact failure: `/media/providers` had two identical
`Access-Control-Allow-Origin` headers, which Firefox rejected. A public
authenticated probe confirmed two values on Replx and one on PMS. Replx had
seeded a CORS header before proxying, then appended the origin's CORS header.
Commit `c66d628` makes copied origin headers replace preseeded values and adds
a regression test. Full Go tests and `go vet ./...` passed. Test image
`0.4.0-c66d628-test` was deployed healthy with zero restarts. Public
`/media/providers` and `/library/sections` probes then returned HTTP 200 with
exactly one allowed-origin value. Zen subsequently sent Movies requests
through Replx, and Home and collections appeared quickly.

## 2026-09-25: cold library hub and per-user preload

After the CORS fix, Luke's first Movies opening still took about 24 seconds;
its repeat opening was immediate. The matching Movies listing request through
Replx took 0.27 seconds, and 55 poster requests completed within two seconds
(52 artwork hits). Those server timings leave the first-open browser delay
unexplained. A first TV Shows opening took 13 seconds. Its cold
`/hubs/sections/22` request took 14.04 seconds and returned about 610 KB,
matching that delay; no edge 5xx was recorded.

Commit `2e9b9a4` adds bounded, user-scoped library hub preloading, using
retained encrypted user tokens and the observed Plex Web query shape. It
normalizes browser client ID, model, and screen resolution for exact section
hub keys while retaining user, section, item count and content-selection
parameters. It derives a current Plex Web profile for users who have not yet
opened a library. The warmer fetches up to two hubs per pass for one recent
user, to limit PMS load. Full Go tests and `go vet ./...` passed. Test image
`0.4.0-2e9b9a4-test` (ID
`sha256:5c82b81bc52f91f4ced9b9fc1f7e4f9861d67f78d8137c47720352c725cc83b3`)
was deployed healthy with zero restarts. Owner library hub probes returned
200 `stale` in 0.21 seconds for TV and 200 `hit` in 0.12 seconds for Movies,
each with one CORS origin value. Luke reported 4K TV Shows opened almost
immediately; its first hub request was still a 1.43-second cache miss, and
some deeper library page requests took 3–7 seconds. Warm coverage for every
user and library remains to be measured before production readiness.
