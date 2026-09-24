# Administration API and UI

## Listener

The admin API listens on the private admin listener and is not exposed through the public Plex hostname.

Default host binding:

```text
127.0.0.1:8080
```

## Authentication

Production 1.0 supports one local administrator account using Argon2id password hashing and secure session cookies.

First-run bootstrap: when `admin_users` is empty, the server logs a single-use setup URL with a random token valid for 15 minutes. The operator opens it via the private admin path and sets the initial password through `POST /api/v1/setup`. Setup requires the bootstrap token (bearer or `setupToken` field) and runs inside one advisory-locked transaction against a database-enforced singleton administrator (at most one row). Creation consumes the bootstrap capability permanently: the bearer stops working and every setup-minted session is revoked. Browser sessions minted from the setup token never outlive the 15-minute window. No default password is shipped and no password is accepted via environment variable. Password logins are throttled per source and username; logout is POST-only with CSRF.

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
| POST | `/api/v1/cache/invalidate` | Retire cache namespaces by scope/class via generations (audited) |
| GET | `/api/v1/storage` | Storage use |
| GET | `/api/v1/sessions` | Active and recent playback |
| GET | `/api/v1/playback/{id}` | Decision explanation |
| POST | `/api/v1/diagnostics/traces` | Arm targeted trace |
| GET | `/api/v1/diagnostics/traces/{id}` | Trace summary |
| GET | `/api/v1/logs` | Not implemented (501; inspect container stderr) |
| GET | `/api/v1/settings` | Allowlisted runtime settings with spec |
| PATCH | `/api/v1/settings` | Change allowlisted runtime settings (validated, audited) |
| GET | `/api/v1/onboarding/status` | Onboarding stage and identity |
| POST | `/api/v1/onboarding/pin` | Issue Plex PIN (returns claim URL + code, never tokens) |
| GET | `/api/v1/onboarding/pin` | Poll PIN claim |
| POST | `/api/v1/onboarding/token` | Backup: validate + store a pasted Plex token (never echoed back; always legacy, no refresh) |
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

Expensive cache invalidation operations require explicit scope and are rate limited. Invalidation retires generations (per scope/class, or global), never reconstructed keys.

## Runtime settings allowlist

`GET /api/v1/settings` returns exactly the mutable settings plus their
spec (range, default, restart behaviour). `PATCH` accepts only
`replx.playback_retention_days` and `replx.audit_retention_days`
(validated integers, reloaded live by the retention worker); unknown
keys, secrets, bindings and onboarding state are rejected. Password
records store full Argon2id parameters, verify with the stored values,
and transparently rehash on login.

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

The private `/admin` page now provides an operator dashboard after sign-in:
cache hit and warmer counters, aggregate cache inventory by class, measured
Valkey memory and key count, artwork and diagnostics disk use, and the latest
completed warm activity. It edits per-user source resolution, source and
streaming bitrate, 4K/HDR and transcode rules through the existing audited
policy API. Playback and audit retention use the live settings API. All
mutations use the browser session's CSRF token. The page does not expose
tokens, cache keys or cached media metadata. Cache memory is capped by the
Valkey Compose `maxmemory` setting; artwork is bounded by its janitor.

The page intentionally labels its Home and Continue Watching inventory as
short lived; it does not claim every Home variant is warm. Access remains on
the private admin listener, never the public Plex hostname.

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
