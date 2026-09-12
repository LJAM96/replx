# Configuration Reference

## Canonical environment variables

| Variable | Required | Description |
| --- | --- | --- |
| `REPLX_EDGE_IMAGE_OWNER` | Yes | Container image owner for `ghcr.io/<owner>/replx-edge` |
| `REPLX_EDGE_VERSION` | Yes | Pinned Replx Edge image version |
| `POSTGRES_VERSION` | Yes | Tested PostgreSQL image version |
| `VALKEY_VERSION` | Yes | Tested Valkey image version |
| `POSTGRES_PASSWORD` | Yes | Database password |
| `REPLX_EDGE_SECRET_KEY` | Yes | Root encryption and HMAC secret |
| `REPLX_EDGE_PUBLIC_URL` | Yes | Cloudflare public Plex connection URL |
| `REPLX_EDGE_ORIGIN_INTERNAL_URL` | Setup | PMS URL reachable from Replex |
| `REPLX_EDGE_INGRESS_MODE` | Yes | `cloudflare_tunnel` or `direct` |
| `TUNNEL_TOKEN` | Tunnel profile | Cloudflare Tunnel token (or use `TUNNEL_TOKEN_FILE`) |
| `TUNNEL_TOKEN_FILE` | Tunnel profile (secret file) | Path to file containing the Tunnel token; Docker secrets compatible |
| `REPLX_EDGE_ADMIN_PORT` | No | Host loopback admin port |
| `REPLX_EDGE_LOG_LEVEL` | No | Production log level |
| `REPLX_EDGE_CACHE_MAX_GB` | No | General cache budget |
| `REPLX_EDGE_ARTWORK_MAX_GB` | No | Artwork budget |
| `REPLX_EDGE_DIAGNOSTICS_MAX_GB` | No | Trace file budget |
| `REPLX_EDGE_TRACE_RETENTION_DAYS` | No | Trace retention |
| `REPLX_EDGE_PLAYBACK_RETENTION_DAYS` | No | Playback decision retention |
| `REPLX_EDGE_AUDIT_RETENTION_DAYS` | No | Audit retention |
| `REPLX_EDGE_MEDIA_FALLBACK_ENABLED` | No | Enable optional media gateway |
| `REPLX_EDGE_MEDIA_PUBLIC_URL` | Conditional | DNS only media gateway hostname |
| `POSTGRES_HOST` | No | Postgres host (`postgres` in Compose) |
| `POSTGRES_PORT` | No | Postgres port (`5432`) |
| `POSTGRES_DB` | No | Postgres database (`replx_edge`) |
| `POSTGRES_USER` | No | Postgres user (`replx_edge`) |
| `REPLX_EDGE_POSTGRES_URL` | No | Full Postgres URL override (tests, non-Compose) |
| `REPLX_EDGE_VALKEY_ADDR` | No | Valkey `host:port` (`valkey:6379`); down degrades cache, never readiness |

## Removed ambiguous variable

Do not use `REPLX_EDGE_ORIGIN_URL`.

It incorrectly implies that Replx Edge and Plex clients see the origin through the same URL.

Use `REPLX_EDGE_ORIGIN_INTERNAL_URL` for server to server API traffic. Client reachable media origins are discovered and validated separately.

## Owner credentials

Owner Plex credentials are not passed through a plaintext `REPLX_EDGE_PMS_TOKEN` environment variable in normal production.

The admin onboarding flow obtains owner credentials using Plex PIN and JWT authentication, then stores them encrypted using `REPLX_EDGE_SECRET_KEY`.

A temporary bootstrap token environment variable may be supported only for development and must be clearly marked insecure for long lived production configuration.

## Secret key

`REPLX_EDGE_SECRET_KEY` must contain at least 32 random bytes worth of entropy.

Key loss means encrypted owner PMS credentials cannot be recovered. Back up this key separately with access controls appropriate for credentials.

## Defaults

```text
REPLX_EDGE_INGRESS_MODE=cloudflare_tunnel
REPLX_EDGE_ADMIN_PORT=8080
REPLX_EDGE_LOG_LEVEL=info
REPLX_EDGE_CACHE_MAX_GB=20
REPLX_EDGE_ARTWORK_MAX_GB=50
REPLX_EDGE_DIAGNOSTICS_MAX_GB=10
REPLX_EDGE_TRACE_RETENTION_DAYS=7
REPLX_EDGE_PLAYBACK_RETENTION_DAYS=30
REPLX_EDGE_AUDIT_RETENTION_DAYS=180
REPLX_EDGE_MEDIA_FALLBACK_ENABLED=false
```

## Runtime editable settings

The admin UI may change safe settings such as cache budgets, retention, sync frequency, global policy and compatibility overrides.

Secrets, listener bindings, ingress mode and database connection settings require environment changes and restart in Production 1.0.
