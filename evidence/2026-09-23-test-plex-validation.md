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
