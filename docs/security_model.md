# Security Model

## Assets

Sensitive assets include Plex user tokens, Replx Edge owner credentials, Ed25519 private key, `REPLX_EDGE_SECRET_KEY`, Tunnel token, policy data, diagnostic traces and user activity.

## Public attack surface

Default Cloudflare profile exposes no public host port on the Replx Edge VPS.

The public logical surface is the Cloudflare Plex hostname. `cloudflared` reaches Replx Edge over a private Docker network.

The optional media gateway is a separate public attack surface and must remain disabled unless required.

## Admin isolation

Admin binds to loopback and should be reached through Tailscale or another private path. It is not routed through `plex.example.com`.

Admin bootstrap uses a single-use setup token from server logs on first run. There is no default admin password and no password-via-environment in production.

## Owner credentials

Owner Plex onboarding uses PIN and JWT authentication. Owner JWT, PMS access token and private device key are encrypted at rest.

Owner credentials are used for indexing and administration only. They are never substituted for a user request.

## User tokens

Client tokens are fingerprinted for identity lookup. Persist raw user token ciphertext only when a feature requires it. Normal logs never contain the token.

## Cache isolation

User scoped PMS responses are keyed by internal user identity. CI must prove cross user cache isolation.

## Origin bypass

The default Cloudflare profile cannot claim hard policy enforcement if a client can independently connect to PMS.

The UI must display this clearly. Do not use security language that implies Replx Edge prevents deliberate bypass in that topology.

## Media redirect token

A direct origin redirect may include a PMS accepted token in the query. Treat the full Location URL as a secret.

Prefer a user scoped transient PMS delegation token when supported. Never expose the owner token.

## SSRF

`REPLX_EDGE_ORIGIN_INTERNAL_URL` is administrator configured and validated. Client supplied URLs never become arbitrary outbound targets.

Origin redirects from PMS are not blindly followed to arbitrary hosts.

## Proxy hardening

Use Go standard HTTP handling, bounded headers, explicit hop by hop header rules and sane timeouts.

## Media gateway

If enabled:

* use valid public TLS
* expose only required media routes
* require active session or signed short lived capability
* never expose admin endpoints
* never expose PostgreSQL or Valkey
* rate limit invalid capabilities

## Diagnostics

Protocol tracing is targeted, expiring and sanitized. Raw media bytes are never stored.

## Container hardening

Replx Edge runs as non root, with read only root filesystem, no new privileges and only required writable volumes.
