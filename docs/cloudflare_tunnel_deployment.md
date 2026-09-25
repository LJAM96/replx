# Cloudflare Tunnel Deployment

## Role

Cloudflare Tunnel is the default public ingress for replx-edge metadata and control traffic.

It creates outbound connections from `cloudflared` and therefore lets the replx-edge VPS operate without public inbound HTTP or HTTPS ports.

## Public hostname

Example:

```text
plex.example.com
```

The Cloudflare published application maps this hostname to:

```text
http://replx-edge:32400
```

on the private Docker network.

## PMS Custom Server Access URL

Configure the origin PMS Custom Server Access URL with the Replx Edge hostname.

Example:

```text
https://plex.example.com:443
```

This causes Plex to publish that URL as a connection for the existing PMS resource. Specify `:443` explicitly: Plex may otherwise publish its own remote access port (often 32400) for the custom hostname. It does not create a new PMS resource.

Onboarding must verify that the custom connection is visible through Plex resources with the correct port and still resolves to the same origin `machineIdentifier` through Replx Edge.

## No Cloudflare Access on the Plex hostname

Do not place an interactive Cloudflare Access login challenge in front of `plex.example.com`. Official Plex television and mobile clients are not expected to complete a browser Access flow.

The Plex endpoint remains authenticated by Plex.

## Edge caching

Disable Cloudflare edge caching for Plex API responses in Production 1.0. replx-edge owns cache semantics because it understands Plex user state and policy.

Artwork is a possible future exception after correctness and cache key behaviour are proven.

## Media prohibition

In `cloudflare_tunnel` ingress mode, replx-edge must classify media responses before writing bulk media bytes.

If the request is a media body route, replx-edge must either:

```text
route directly to a validated client reachable origin
route to the optional media gateway
fail with MEDIA_ROUTE_UNAVAILABLE
```

It must not silently proxy the video through Cloudflare.

## Origin requirement

Direct media routing means the PMS has a client reachable HTTPS connection with valid TLS.

The zero public port statement refers to the replx-edge VPS only.

If the origin is private, direct origin media is impossible. In that case the separate media gateway is required for remote playback or the zero port profile supports browsing only.

## Fail closed behaviour

If replx-edge cannot determine a safe media route before response body streaming starts, return a controlled error. Do not begin streaming and then discover that the request violates the Cloudflare media rule.

## Tunnel token

Run `cloudflared` using `TUNNEL_TOKEN` or Docker secret compatible injection. Do not place the token directly in the Compose command line.

## References

Cloudflare Tunnel: https://developers.cloudflare.com/tunnel/

Tunnel routing: https://developers.cloudflare.com/tunnel/concepts/routing/

Video delivery policy: https://developers.cloudflare.com/fundamentals/reference/policies-compliances/delivering-videos-with-cloudflare/
