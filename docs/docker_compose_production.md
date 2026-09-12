# Docker Compose Production Deployment

## Contract

Production 1.0 is supported through Docker Compose.

Canonical service names:

```text
cloudflared
replx-edge
postgres
valkey
```

No service name contains spaces.

## Default port exposure

```text
cloudflared          no host ports
replx-edge control   no host ports
replx-edge admin     127.0.0.1:8080
postgres             no host ports
valkey               no host ports
```

The optional media profile may expose a dedicated public TLS listener outside Cloudflare.

## Internal networks

Use a frontend network for `cloudflared` to `replx-edge` and a backend internal network for Replx Edge, PostgreSQL and Valkey.

PostgreSQL and Valkey never join the public facing network unless a future requirement proves it necessary.

## Startup

```text
PostgreSQL healthy
Valkey healthy
Replx Edge migrations
Replx Edge admin ready
Replx Edge control ready
owner auth validation
PMS origin validation
cloudflared tunnel ready
background workers start
```

PMS unavailability degrades origin health but does not crash the admin plane.

## Images

Pin tested versions in `.env`. Do not ship production examples using `latest`.

Support `linux/amd64` and `linux/arm64`.

## Writable storage

```text
postgres_data
valkey_data
replx_edge_cache
replx_edge_artwork
replx_edge_diagnostics
```

## Security

Replx Edge runs non root with read only root filesystem and `no-new-privileges`.

## Optional media profile

The media gateway is a separate Compose profile so enabling it is deliberate.

Example:

```text
docker compose --profile media up -d
```

Enabling the profile must not change the Cloudflare control hostname into a video proxy.
