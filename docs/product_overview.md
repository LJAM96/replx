# Product Overview

## Problem

Large Plex servers can be slow to browse because home hubs, collections, metadata, search and user state repeatedly travel to and execute against a remote PMS. Multiple physical versions of the same title can also cause Plex clients to choose a source that is inappropriate for the user, device or connection.

Replx Edge provides a nearby Plex aware control plane without replacing PMS.

## Product goals

Replx Edge should:

* accelerate user scoped browsing and state reads
* maintain a local searchable library index
* apply deterministic source selection to playback requests that traverse Replex
* prefer a 1080p source over unnecessary 4K transcoding for restricted users
* combine user limits with observed client capability
* preserve ordinary Plex authentication and permissions
* provide precise playback traces and explanations
* run as a Docker Compose stack
* use Cloudflare Tunnel for the public control hostname by default
* keep bulk media outside Cloudflare public hostname routes

## Non goals

Production 1.0 does not:

* store or duplicate media files
* replace Plex transcoding
* replace Plex authentication
* modify the PMS database
* require bare metal access to PMS
* claim a second Plex Media Server identity
* guarantee strict policy enforcement when clients can bypass Replx Edge and connect to PMS directly
* support multiple active PMS origins in one instance
* hide disallowed `Media` array entries from official clients

The last item is intentional. Metadata filtering creates fragile media index translation across cached metadata, play queues and concurrent sessions. Production 1.0 keeps the origin media indices visible and enforces selection at playback negotiation and the media request boundary.

## Operating profiles

### Zero port Cloudflare profile

This is the preferred deployment.

The Replx Edge VPS has no public inbound ports. Control traffic reaches Replx Edge through Cloudflare Tunnel. Media is sent directly from a client reachable PMS HTTPS connection after Replx Edge validates and selects the source.

Policy is deterministic for requests that traverse Replx Edge. It is not a hard security boundary if the client can choose a direct PMS connection.

### Media gateway profile

A separate DNS only media hostname exposes a narrow public media gateway outside Cloudflare.

This profile exists for client and protocol combinations that cannot follow direct origin routing. It requires a public TLS listener and therefore gives up the zero inbound port property for that media endpoint.

### Strict enforcement profile

Strict enforcement is only supported when the operator controls every path by which a client can retrieve media and metadata. A client must not be able to independently select an unrestricted PMS connection.

This profile is out of scope for deployments where the Replx Edge administrator cannot control the PMS host or network.

## Example user behaviour

Jodie can have a maximum source resolution of 1080p. If 4K and 1080p variants exist, Replx Edge selects 1080p. If her target bitrate is below the source bitrate, PMS may transcode the 1080p source. Replx Edge does not intentionally select 4K merely to transcode it down.

Luke can allow 4K. On a known 4K capable client Replx Edge may select 4K. On a known 1080p limited client it selects 1080p.

## Core invariant

```text
Plex owns truth.
Replx Edge owns acceleration, policy evaluation, routing decisions and observability.
```
