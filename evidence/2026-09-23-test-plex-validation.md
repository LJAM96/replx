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

The correction is not yet deployed. Recheck all baseline counts after deployment.

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
