# Operations Runbook

## Healthy state

Healthy Replx Edge shows:

```text
PostgreSQL ready
Valkey ready
owner Plex authentication valid
PMS internal origin reachable
machineIdentifier verified
Custom Server Access URL verified
Cloudflare Tunnel connected
PMS event stream connected or degraded with reconciliation active
```

## Origin unavailable

Browsing may serve permitted stale user scoped cache. New playback does not start. Owner sync pauses. Admin remains available.

## Owner auth expired or revoked

Client pass through may continue with client user tokens, but indexing and owner administrative operations pause. Repair Plex onboarding from the admin UI.

## Tunnel unavailable

Public Replx Edge control access fails. Do not automatically publish a direct Replx Edge control port as a fallback.

## Problem client

Arm a protocol trace for the user and client, then reproduce one playback. Inspect redirect follow up, origin TLS, token auth, Range and manifest behaviour.

Do not globally change compatibility from one generic timeout.

## Media route unavailable

If the client cannot use direct origin:

```text
media gateway enabled -> test gateway
media gateway disabled -> unsupported playback mode
```

Never temporarily proxy the video through Cloudflare to make the test pass.

## Storage pressure

Check diagnostics, artwork, PostgreSQL playback history and Valkey. Janitors should enforce configured limits. Do not delete active diagnostic traces or database files manually.

## Upgrade

```text
docker compose pull
docker compose up -d
```

Verify migrations, owner auth and routing health after upgrade.

## Backup

Back up PostgreSQL, `.env` or equivalent secret source, `REPLX_EDGE_SECRET_KEY`, Replx Edge owner key material and configuration.

Loss of `REPLX_EDGE_SECRET_KEY` makes encrypted PMS credentials unrecoverable.
