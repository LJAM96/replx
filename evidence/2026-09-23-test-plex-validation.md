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

## Evidence sequence

1. Deploy a versioned build containing the correction. Record its image digest and commit.
2. Trigger a full owner sync. Require four completed section cursors, a plausible nonzero item count, and no new sync errors. Confirm the event stream remains connected while idle for at least 15 minutes.
3. In Plex Web through the Replx connection, open Home, one large library, and one collection twice. Record screen load time and Replx cache counters before and after each repeat load.
4. Repeat the browse checks as a restricted Plex user. Confirm restricted libraries, watched state, and Continue Watching never cross users.
5. On Plex Web and a physical TV, play a progressive title, seek, stop, and resume. Record client version, playback type, request/session IDs, whether a 307 reached the origin, origin TLS, Range behavior, and any failure code.
6. Test a transcode and subtitles where supported. Record each mode separately in the compatibility matrix; leave untested modes `UNKNOWN`.
7. Run sustained normal use for at least 72 hours. Record restarts, 5xx responses, origin errors, sync errors, event stream uptime, cache hit rate, and p95 screen load times daily.
8. Restore a database and secret backup into a separate clean stack, then verify onboarding state, policies, and library index. Test one upgrade and a documented rollback path.

Production release requires complete client traces, user isolation, a stable index, a successful restore drill, and measured speed improvements. A healthy container alone does not satisfy these gates.
