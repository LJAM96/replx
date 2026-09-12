# Plex Owner Authentication and Discovery

## Purpose

Replx Edge needs an owner privileged PMS credential for library indexing, synchronization, server capability discovery and administrative inspection. Forwarded client requests continue to use their own Plex tokens.

## Replex application identity

At first start generate and persist:

```text
replx_edge_client_identifier
Ed25519 private key
Ed25519 public JWK
JWK key identifier
```

The client identifier is stable for the lifetime of the Replx Edge installation.

Private key material is encrypted at rest with a key derived from `REPLX_EDGE_SECRET_KEY`.

## Owner sign in

Use the current Plex PIN authentication flow with JWK support.

Onboarding flow:

```text
Generate PIN with public JWK
        |
        v
Show Plex authentication URL to administrator
        |
        v
Administrator signs in at Plex
        |
        v
Replex polls PIN using signed device JWT
        |
        v
Receive Plex owner JWT
```

The owner JWT is short lived and must be refreshed using the current Plex nonce and signed device JWT flow.

## PMS resource discovery

Using the owner Plex token, request the Plex resources endpoint with HTTPS, Relay and IPv6 information enabled.

From the returned resources:

* list only PMS resources owned or accessible to the signed in account
* administrator selects exactly one PMS for Production 1.0
* store its resource identity
* store its PMS access token encrypted
* record all candidate connection URLs
* select and verify one non Replx edge HTTPS connection as the client reachable media origin

Prefer an administrator selected `REPLX_EDGE_ORIGIN_INTERNAL_URL` for server to server traffic rather than blindly choosing the same URL that clients use.

## Internal origin URL

`REPLX_EDGE_ORIGIN_INTERNAL_URL` is the URL used by Replex itself.

Requirements:

* reachable from the VPS
* points to the selected PMS
* returns the expected `machineIdentifier`
* may be private or public
* may use Plex secure `plex.direct` infrastructure if appropriate

## Client reachable origin

Direct media routing uses the client reachable HTTPS PMS connection selected from the owner resources response or another administrator verified connection for the same `machineIdentifier`. It must not point back to the Replx Edge Cloudflare hostname.

Do not reuse the internal origin URL unless it is also valid and reachable from the client.

## Identity validation

On onboarding:

1. fetch PMS root using the owner PMS access token
2. record `machineIdentifier`
3. fetch or proxy `/identity` where available
4. verify the selected plex.tv resource refers to the same PMS
5. configure the origin PMS Custom Server Access URL with the Replx Edge public hostname
6. refresh resources and verify that the Replx Edge custom connection appears for the same PMS resource

Replex must not fabricate a machine identifier.

## Credential storage

Add an encrypted owner credential record containing:

```text
plex account identifier
replx_edge_client_identifier
owner JWT ciphertext
owner JWT expiry
PMS access token ciphertext
selected PMS resource identifier
JWK private key ciphertext
JWK public key
last refresh time
last validation status
```

Plaintext credentials must exist only in process memory for the shortest necessary time.

## Rotation

Before the owner JWT expires, refresh it using the Plex nonce flow. After refresh, refresh the PMS resources record and PMS access token.

If refresh fails:

```text
client pass through requests may continue
owner sync jobs pause
admin UI reports owner authentication degraded
```

Do not silently replace a valid client token with an expired owner token.

## Revocation

If Plex returns an authentication failure for the owner connection, mark owner credentials invalid and require onboarding to be repaired. Do not repeatedly hammer plex.tv or PMS with a revoked credential.

## Client user tokens

For normal proxied requests:

```text
incoming user token -> origin PMS
```

The owner token is never substituted.

For local cache responses, user identity and permission context must already have been resolved for that exact user scope.

## References

Plex authentication and PMS resources: https://developer.plex.tv/pms/
