# Playback Policy and Media Selection

## Policy precedence

Policy merges field by field:

```text
Replx Edge defaults
      ^
Global policy
      ^
User policy
      ^
Client or device override
```

A more specific configured field replaces only that field.

## Policy schema

```json
{
  "maxSourceWidth": null,
  "maxSourceHeight": null,
  "maxSourceBitrateKbps": null,
  "maxStreamingBitrateKbps": null,
  "allow4K": "inherit",
  "allowHDR": "inherit",
  "allowDolbyVision": "inherit",
  "allowTranscode": "inherit",
  "preferDirectPlay": "inherit",
  "maxAudioChannels": null,
  "unknownDynamicRangeBehavior": "inherit",
  "routingMode": "inherit"
}
```

Boolean style values:

```text
inherit
allow
deny
```

`unknownDynamicRangeBehavior`:

```text
inherit
allow
deny
```

Routing mode:

```text
inherit
automatic
```

`origin_preferred` and `media_fallback` are reserved values and are
rejected by the admin API in Production 1.0: transport selection is
validated direct-origin routing (or the explicit media gateway profile),
not a per-policy override. A policy field is not accepted as a
functioning setting until its effect is implemented and testable.

`preferDirectPlay` accepts `inherit` (or `allow`, the default ranking
behaviour which already prefers the cheapest playback). `deny` is
rejected as unimplemented: there is no defined semantics for penalizing
direct play, so the API refuses it rather than silently ignoring it.

## Defaults

Defaults approximate normal Plex behaviour:

```text
source resolution unrestricted
4K allowed
HDR allowed
Dolby Vision allowed
transcoding allowed
unknown dynamic range allowed
routing automatic
```

## Capability precedence

```text
administrator override
reliable observed capability
current Plex client report
known compatibility profile
unknown
```

Do not infer physical screen resolution from a model name alone unless a compatibility profile has been validated for that model and software version.

## Dynamic range normalization

Normalize origin metadata to one of:

```text
SDR
HDR10
HDR10_PLUS
HLG
DOLBY_VISION
HDR_OTHER
UNKNOWN
```

Initial normalization rules inspect all available variant and stream metadata, including fields commonly surfaced by PMS such as dynamic range, HDR format, codec, profile and extended display title.

String matching is case insensitive and normalized before comparison.

Minimum rules:

| Observed signal | Normalized value |
| --- | --- |
| explicit Dolby Vision, `dovi`, `dvhe`, `dvav` | `DOLBY_VISION` |
| explicit HDR10+ or HDR10 Plus | `HDR10_PLUS` |
| explicit HDR10 or PQ without Dolby Vision | `HDR10` |
| explicit HLG | `HLG` |
| explicit HDR not otherwise classified | `HDR_OTHER` |
| explicit SDR | `SDR` |
| insufficient evidence | `UNKNOWN` |

Do not classify `UNKNOWN` as HDR automatically. When a policy denies HDR, use `unknownDynamicRangeBehavior` to decide whether unknown variants remain eligible. Default is allow to avoid false blocks.

## Eligibility

Reject a variant when a hard rule is violated:

* source exceeds maximum source resolution
* 4K is denied
* dynamic range is denied by normalized policy
* Dolby Vision is denied
* source bitrate exceeds a hard source ceiling
* part is unavailable
* the user cannot access the item through PMS

## Ranking

Rank only eligible variants using a deterministic tuple:

```text
playback_cost
resolution_distance
hdr_penalty
video_codec_penalty
audio_penalty
unnecessary_bitrate_penalty
origin_media_index
```

Lower tuple wins.

If direct play compatibility cannot be determined reliably, use an unknown playback cost rather than inventing certainty.

## Bandwidth rule

Bandwidth target controls output, not permission to use an oversized source.

Example:

```text
4K source 60 Mbps
1080p source 15 Mbps
user max source 1080p
target stream 6 Mbps
```

Correct behaviour:

```text
select 1080p source
ask PMS for approximately 6 Mbps output
```

## Transcode denied

If `allowTranscode=deny` and PMS says the selected eligible source requires transcoding, fail closed.

Return an explainable policy result such as:

```text
POLICY_TRANSCODE_FORBIDDEN
```

Do not fall back to a prohibited 4K variant merely because it Direct Plays when the user is restricted to 1080p.

## Production 1.0 playback workflow

```text
resolve user
resolve client
load effective policy
load current origin variants
normalize dynamic range
filter variants
rank variants
select origin media index
rewrite playback decision request if needed
forward to PMS with user token
validate PMS decision
fail if transcode is forbidden and required
persist selected media and part in active session
route actual media using ADR 001
validate raw part boundary against session and policy
```

## Metadata filtering

Production 1.0 does not remove or reorder `Media` arrays in browse metadata.

This avoids media index translation races across cached metadata, play queues, multiple concurrent sessions and stale client state.

Optional metadata hiding is a post 1.0 experiment and requires its own compatibility design.

## PMS disagreement

If PMS attempts to use a source Replx Edge rejected:

```text
record POLICY_ORIGIN_MISMATCH
retry only if the correction path is deterministic
otherwise fail closed for restricted policy
```

## No eligible source

Return:

```text
NO_ALLOWED_MEDIA_VARIANT
```

with the rejected candidates and reasons visible in the admin trace.
