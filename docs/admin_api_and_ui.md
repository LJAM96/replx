# Administration API and UI

## Listener

The admin API listens on the private admin listener and is not exposed through the public Plex hostname.

Default host binding:

```text
127.0.0.1:8080
```

## Authentication

Production 1.0 supports one local administrator account using Argon2id password hashing and secure session cookies.

First-run bootstrap: when `admin_users` is empty, the server logs a single-use setup URL with a random token valid for 15 minutes. The operator opens it via the private admin path and sets the initial password through `POST /api/v1/setup`. The setup route disables itself once an admin exists. No default password is shipped and no password is accepted via environment variable.

OIDC is future work.

## Pagination

Every collection endpoint must support cursor pagination.

Request example:

```text
GET /api/v1/devices?limit=50&cursor=<opaque>
```

Default limit is 50. Maximum limit is 200.

Responses include:

```json
{
  "data": [],
  "meta": {
    "nextCursor": null
  }
}
```

## Core endpoints

| Method | Path | Purpose |
| --- | --- | --- |
| GET | `/api/v1/status` | Overall health |
| GET | `/api/v1/server` | PMS and discovery status |
| POST | `/api/v1/server/test` | Test origin and identity |
| POST | `/api/v1/server/sync` | Queue reconciliation |
| GET | `/api/v1/users` | User list |
| GET | `/api/v1/users/{id}` | User detail |
| GET | `/api/v1/users/{id}/policy` | User policy |
| PATCH | `/api/v1/users/{id}/policy` | Change policy |
| GET | `/api/v1/devices` | Clients |
| GET | `/api/v1/devices/{id}` | Client detail |
| PATCH | `/api/v1/devices/{id}` | Friendly name and override metadata |
| GET | `/api/v1/devices/{id}/policy` | Device policy |
| PATCH | `/api/v1/devices/{id}/policy` | Device policy |
| GET | `/api/v1/library/items` | Search indexed items |
| GET | `/api/v1/library/items/{id}` | Variants and parts |
| GET | `/api/v1/cache` | Cache statistics |
| POST | `/api/v1/cache/invalidate` | Invalidate selected cache |
| GET | `/api/v1/storage` | Storage use |
| GET | `/api/v1/sessions` | Active and recent playback |
| GET | `/api/v1/playback/{id}` | Decision explanation |
| POST | `/api/v1/diagnostics/traces` | Arm targeted trace |
| GET | `/api/v1/diagnostics/traces/{id}` | Trace summary |
| GET | `/api/v1/logs` | Structured logs |
| GET | `/api/v1/settings` | Safe runtime settings |
| PATCH | `/api/v1/settings` | Change safe runtime settings |
| GET | `/api/v1/onboarding/status` | Onboarding stage and identity |
| POST | `/api/v1/onboarding/pin` | Issue Plex PIN (returns claim URL + code, never tokens) |
| GET | `/api/v1/onboarding/pin` | Poll PIN claim |
| GET | `/api/v1/onboarding/resources` | Selectable PMS resources (tokens stripped) |
| POST | `/api/v1/onboarding/select` | Bind one PMS (`{clientIdentifier}`) |
| POST | `/api/v1/onboarding/verify` | Identity triple-check + Custom URL report |
| GET | `/admin/onboarding` | Server-rendered onboarding and verification panel |
| GET | `/admin/login` | Bootstrap sign-in form (setup token) |
| POST | `/admin/login` | Exchange setup token for HttpOnly session cookie |
| POST | `/admin/logout` | Revoke bootstrap session |
| GET | `/api/v1/spike/events` | Redacted spike trace ring |
| GET | `/api/v1/spike/observations` | Compatibility matrix |
| POST | `/api/v1/spike/observations` | Record a client observation |
| GET | `/admin/spike` | Spike matrix panel |

Browser panels authenticate with the session cookie plus per-session CSRF token; API clients use the setup token bearer (no CSRF exposure).

## Rate limiting and job control

`POST /server/test` defaults to 5 requests per minute per admin session.

`POST /server/sync` is idempotent while a sync is already queued or running. Only one full reconciliation may run at once. A repeated call returns the current job ID instead of starting a second scan.

Expensive cache invalidation operations require explicit scope and are rate limited.

## Audit

Audit all:

```text
policy mutation
device override
server configuration change
cache invalidation
manual sync
trace creation
retention setting change
admin credential change
```

The audit record stores before and after state where appropriate.

## Overview page

Show:

```text
PMS health
origin internal latency
Custom Server Access URL verification
machineIdentifier match
owner auth status
Tunnel health
last event
last sync
users
devices
library item count
active playback
cache hit ratio
storage
routing compatibility summary
```

## Playback explanation

A playback page must show every candidate and rejection reason, the final origin media index and part, PMS decision, transcode status, routing strategy and whether the request ever risked traversing Cloudflare media path.

## Routing spike UI

Before Production 1.0, expose a compatibility test panel that records:

```text
progressive redirect result
Range seek result
HLS manifest redirect result
DASH manifest redirect result
origin TLS result
media gateway fallback result
```
