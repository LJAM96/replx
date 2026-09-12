# P0 Spike Runbook: Direct Origin Media Routing

## Goal

Prove, per official client, that bulk media leaves the Cloudflare control
hostname via ADR 001 `307` redirects — or record exactly where it fails.
Nothing here streams video through the Tunnel at any point.

## Prerequisites

1. Onboarding `verified` (`/admin/onboarding`): origin, resource and
   proxied roots agree, Custom Server Access URL published.
2. `REPLX_EDGE_SPIKE_ROUTING=true` on the replx-edge service (default
   `false` keeps fail-closed 403s). Restart after changing it.
3. Test library on PMS: 4K+1080p movie, 1080p-only, HDR10+SDR, one title
   that forces a transcode on the test client.
4. Two clients minimum: Plex Web + one physical TV (per ADR 001).

## Automated protocol mechanics

`scripts/spike_check.sh` covers what curl can prove without a real client:

```text
CONTROL=http://127.0.0.1:32400 ADMIN=http://127.0.0.1:8080 \
SETUP_TOKEN=<token> PLEX_TOKEN=<user token> PART=/library/parts/11/file.mkv \
./scripts/spike_check.sh
```

Checks: readiness, 307 shape, `Cache-Control: no-store`, Location host
equals the onboarded media origin (never the Replex hostname), token
query present, Range round-trip through the redirect, manifest paths
redirect rather than proxy, and no token text in the trace ring.

## Manual matrix (one row per client × playback type)

For each of progressive Direct Play, HLS transcode, DASH (where offered),
seek, resume and subtitles, verify in the client and record:

| Check | How | Pass looks like |
| --- | --- | --- |
| follows 307 | play | playback starts, no `MEDIA_ROUTE_UNAVAILABLE` |
| origin TLS | play on TV | no certificate warning/failure |
| token auth | play | origin serves without extra sign-in |
| Range seek | seek mid-title | resumes within ~2s, no restart |
| resume | stop, resume | continues from offset |
| HLS manifest | transcode a title | plays; `/api/v1/spike/events` shows manifest redirect |
| segments stay on origin | trace ring | segment hosts are the origin, not Replex |
| subtitles | enable subs | render correctly |

Record each result (SUPPORTED / DEGRADED / UNSUPPORTED with notes) at
`/admin/spike` or `POST /api/v1/spike/observations`. Three reproducible
failures across two attempts mark UNSUPPORTED; a single generic timeout
never does. A product version change resets certainty for review.

## Decision gate

- At least Plex Web progressive + one TV progressive SUPPORTED, with
  seek/resume evidence, before policy work (Delta) treats direct origin
  as the default route.
- Anything else stays explicit per client/playback type: media gateway
  profile, or documented unsupported — never silent Tunnel proxying.
- If no client follows the redirect, reclassify the deployment as
  browse-only and revisit the media plane before continuing.
