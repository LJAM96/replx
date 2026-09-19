// Package policy implements the Delta deterministic media policy engine.
//
// Precedence merges field by field: defaults < global < user < device. A
// more specific configured field replaces only that field. Eligibility is
// hard (rejections carry scope-prefixed reasons for the admin trace);
// ranking is a deterministic penalty tuple, lower wins:
//
//	playback_cost, resolution_distance, hdr_penalty, video_codec_penalty,
//	audio_penalty, unnecessary_bitrate_penalty, origin_media_index
//
// Bandwidth targets control OUTPUT, never permission: an oversized source
// is rejected even when the target is small, and the selected source is
// output-capped instead. Transcode-deny fails closed AFTER selection when
// PMS requires transcoding the eligible source.
package policy

import (
	"strings"
)

// TriState is a boolean-style policy value: inherit, allow or deny.
// The zero value ("") behaves as inherit so omitted JSON fields merge.
type TriState string

const (
	Inherit TriState = "inherit"
	Allow   TriState = "allow"
	Deny    TriState = "deny"
)

// Normalize returns the effective value: empty and inherit stay inherit.
func (t TriState) Normalize() TriState {
	switch TriState(strings.ToLower(string(t))) {
	case Allow:
		return Allow
	case Deny:
		return Deny
	default:
		return Inherit
	}
}

// Policy is one scope level. Nil numerics mean unrestricted; inherit
// tristates defer to the less specific level.
type Policy struct {
	MaxSourceWidth          *int     `json:"maxSourceWidth"`
	MaxSourceHeight         *int     `json:"maxSourceHeight"`
	MaxSourceBitrateKbps    *int     `json:"maxSourceBitrateKbps"`
	MaxStreamingBitrateKbps *int     `json:"maxStreamingBitrateKbps"`
	Allow4K                 TriState `json:"allow4K"`
	AllowHDR                TriState `json:"allowHDR"`
	AllowDolbyVision        TriState `json:"allowDolbyVision"`
	AllowTranscode          TriState `json:"allowTranscode"`
	PreferDirectPlay        TriState `json:"preferDirectPlay"`
	MaxAudioChannels        *int     `json:"maxAudioChannels"`
	UnknownDRBehavior       TriState `json:"unknownDynamicRangeBehavior"`
	RoutingMode             string   `json:"routingMode"`
}

// Defaults approximates normal Plex behaviour: everything allowed,
// routing automatic.
func Defaults() Policy {
	return Policy{
		Allow4K: Allow, AllowHDR: Allow, AllowDolbyVision: Allow,
		AllowTranscode: Allow, PreferDirectPlay: Allow,
		UnknownDRBehavior: Allow, RoutingMode: "automatic",
	}
}

// Merge overlays specific onto base field by field: only configured
// (non-nil, non-inherit) fields replace.
func Merge(base, specific Policy) Policy {
	out := base
	if specific.MaxSourceWidth != nil {
		out.MaxSourceWidth = specific.MaxSourceWidth
	}
	if specific.MaxSourceHeight != nil {
		out.MaxSourceHeight = specific.MaxSourceHeight
	}
	if specific.MaxSourceBitrateKbps != nil {
		out.MaxSourceBitrateKbps = specific.MaxSourceBitrateKbps
	}
	if specific.MaxStreamingBitrateKbps != nil {
		out.MaxStreamingBitrateKbps = specific.MaxStreamingBitrateKbps
	}
	if v := specific.Allow4K.Normalize(); v != Inherit {
		out.Allow4K = v
	}
	if v := specific.AllowHDR.Normalize(); v != Inherit {
		out.AllowHDR = v
	}
	if v := specific.AllowDolbyVision.Normalize(); v != Inherit {
		out.AllowDolbyVision = v
	}
	if v := specific.AllowTranscode.Normalize(); v != Inherit {
		out.AllowTranscode = v
	}
	if v := specific.PreferDirectPlay.Normalize(); v != Inherit {
		out.PreferDirectPlay = v
	}
	if specific.MaxAudioChannels != nil {
		out.MaxAudioChannels = specific.MaxAudioChannels
	}
	if v := specific.UnknownDRBehavior.Normalize(); v != Inherit {
		out.UnknownDRBehavior = v
	}
	if specific.RoutingMode != "" && !strings.EqualFold(specific.RoutingMode, "inherit") {
		out.RoutingMode = specific.RoutingMode
	}
	return out
}

// Effective folds defaults < global < user < device.
func Effective(global, user, device Policy) Policy {
	return Merge(Merge(Merge(Defaults(), global), user), device)
}

// Playability is how the client can play a variant, cheapest first.
// Unknown sorts with DirectStream: never invent certainty about direct
// play, but never punish an unprofiled client into transcoding either.
type Playability int

const (
	DirectPlay Playability = iota
	DirectStream
	UnknownPlayability
	Transcode
)

// Variant is one origin media candidate.
type Variant struct {
	MediaIndex    int
	Width         *int
	Height        *int
	BitrateKbps   *int
	DynamicRange  string // sync vocabulary: SDR, HDR10, ..., UNKNOWN
	VideoCodec    string
	AudioChannels *int
	Playability   Playability
	PartAvailable bool
}

// Rejection explains one ineligible candidate. Scope names the winning
// policy level (GLOBAL, USER, DEVICE) so traces read USER_MAX_SOURCE_....
type Rejection struct {
	MediaIndex int    `json:"mediaIndex"`
	Scope      string `json:"scope"`
	Reason     string `json:"reason"`
}

// Decision is the evaluated outcome.
type Decision struct {
	Selected          *Variant    `json:"-"`
	SelectedIndex     int         `json:"selectedIndex"`
	OutputBitrateKbps *int        `json:"outputBitrateKbps,omitempty"`
	Rejected          []Rejection `json:"rejected"`
}

// Policy errors are stable diagnostic codes, never bare text.
type Error struct {
	Code     string
	Rejected []Rejection
}

func (e *Error) Error() string { return e.Code }

const (
	NoAllowedVariant   = "NO_ALLOWED_MEDIA_VARIANT"
	TranscodeForbidden = "POLICY_TRANSCODE_FORBIDDEN"
	OriginMismatch     = "POLICY_ORIGIN_MISMATCH"
)

// reason codes (scope-prefixed at render: SCOPE_REASON).
const (
	RMaxSourceResolution = "MAX_SOURCE_RESOLUTION"
	RSource4KDenied      = "SOURCE_4K_DENIED"
	RHDRDenied           = "HDR_DENIED"
	RDolbyVisionDenied   = "DOLBY_VISION_DENIED"
	RSourceBitrateCeil   = "SOURCE_BITRATE_CEILING"
	RPartUnavailable     = "PART_UNAVAILABLE"
	RUnknownDRDenied     = "UNKNOWN_DYNAMIC_RANGE_DENIED"
)

// Evaluate filters, ranks and selects. scope names the most specific
// configured level for rejection provenance ("USER", "DEVICE", "GLOBAL").
func Evaluate(p Policy, scope string, variants []Variant) (Decision, error) {
	if scope == "" {
		scope = "GLOBAL"
	}
	var eligible []Variant
	var rejected []Rejection
	reject := func(v Variant, reason string) {
		rejected = append(rejected, Rejection{MediaIndex: v.MediaIndex, Scope: scope, Reason: reason})
	}
	for _, v := range variants {
		if !v.PartAvailable {
			reject(v, RPartUnavailable)
			continue
		}
		if is4K(v) && p.Allow4K.Normalize() == Deny {
			reject(v, RSource4KDenied)
			continue
		}
		if overResolution(v, p) {
			reject(v, RMaxSourceResolution)
			continue
		}
		if deniesDR(p, v) {
			if v.DynamicRange == "UNKNOWN" {
				reject(v, RUnknownDRDenied)
			} else if v.DynamicRange == "DOLBY_VISION" && p.AllowDolbyVision.Normalize() == Deny {
				reject(v, RDolbyVisionDenied)
			} else {
				reject(v, RHDRDenied)
			}
			continue
		}
		if p.MaxSourceBitrateKbps != nil && v.BitrateKbps != nil && *v.BitrateKbps > *p.MaxSourceBitrateKbps {
			reject(v, RSourceBitrateCeil)
			continue
		}
		eligible = append(eligible, v)
	}
	if len(eligible) == 0 {
		return Decision{Rejected: rejected}, &Error{Code: NoAllowedVariant, Rejected: rejected}
	}
	best := eligible[0]
	bestKey := rankKey(p, best)
	for _, v := range eligible[1:] {
		if k := rankKey(p, v); lessKey(k, bestKey) {
			best, bestKey = v, k
		}
	}
	if best.Playability == Transcode && p.AllowTranscode.Normalize() == Deny {
		return Decision{SelectedIndex: best.MediaIndex, Rejected: rejected},
			&Error{Code: TranscodeForbidden, Rejected: rejected}
	}
	d := Decision{Selected: &best, SelectedIndex: best.MediaIndex, Rejected: rejected}
	// Bandwidth target caps OUTPUT, never reopens permission.
	if p.MaxStreamingBitrateKbps != nil && best.BitrateKbps != nil && *best.BitrateKbps > *p.MaxStreamingBitrateKbps {
		cap := *p.MaxStreamingBitrateKbps
		d.OutputBitrateKbps = &cap
	}
	return d, nil
}

func is4K(v Variant) bool {
	if v.Width != nil && *v.Width > 1920 {
		return true
	}
	return v.Height != nil && *v.Height > 1080
}

func pixels(v Variant) int {
	w, h := 0, 0
	if v.Width != nil {
		w = *v.Width
	}
	if v.Height != nil {
		h = *v.Height
	}
	return w * h
}

func overResolution(v Variant, p Policy) bool {
	if p.MaxSourceWidth != nil && v.Width != nil && *v.Width > *p.MaxSourceWidth {
		return true
	}
	return p.MaxSourceHeight != nil && v.Height != nil && *v.Height > *p.MaxSourceHeight
}

func deniesDR(p Policy, v Variant) bool {
	switch v.DynamicRange {
	case "DOLBY_VISION":
		return p.AllowDolbyVision.Normalize() == Deny
	case "HDR10", "HDR10_PLUS", "HLG", "HDR_OTHER":
		return p.AllowHDR.Normalize() == Deny
	case "UNKNOWN":
		return p.UnknownDRBehavior.Normalize() == Deny
	default:
		return false
	}
}

// rankKey builds the deterministic tuple. Lower wins everywhere. The
// design prefers the best permitted quality: cheapest playback first,
// then closest-to-cap (or largest, when uncapped) resolution, then
// richest dynamic range, then most compatible codec, then least excess
// audio, then lowest bitrate, then lowest origin index.
//
// SDR wins over premium HDR only through playback_cost (direct play beats
// transcode): an unrestricted 4K direct-play variant always outranks an
// SDR one, which is the entire point of owning a 4K screen.
func rankKey(p Policy, v Variant) [7]int {
	key := [7]int{}
	switch v.Playability {
	case DirectPlay:
		key[0] = 0
	case DirectStream, UnknownPlayability:
		key[0] = 1
	case Transcode:
		key[0] = 2
	}
	if cap := maxPixels(p); cap > 0 {
		key[1] = cap - pixels(v)
	} else {
		key[1] = -pixels(v)
	}
	switch v.DynamicRange {
	case "DOLBY_VISION":
		key[2] = -4
	case "HDR10_PLUS":
		key[2] = -3
	case "HDR10", "HLG":
		key[2] = -2
	case "HDR_OTHER", "UNKNOWN":
		key[2] = -1
	default: // SDR and unset
		key[2] = 0
	}
	key[3] = codecPenalty(v)
	if p.MaxAudioChannels != nil && v.AudioChannels != nil && *v.AudioChannels > *p.MaxAudioChannels {
		key[4] = *v.AudioChannels - *p.MaxAudioChannels
	}
	if v.BitrateKbps != nil {
		key[5] = *v.BitrateKbps / 1000
	}
	key[6] = v.MediaIndex
	return key
}

func maxPixels(p Policy) int {
	w, h := 0, 0
	if p.MaxSourceWidth != nil {
		w = *p.MaxSourceWidth
	}
	if p.MaxSourceHeight != nil {
		h = *p.MaxSourceHeight
	}
	if w > 0 && h > 0 {
		return w * h
	}
	return 0
}

// codecPenalty prefers widely direct-playable codecs. Unknown codecs
// sort most expensive rather than blocking: eligibility already passed.
func codecPenalty(v Variant) int {
	switch strings.ToLower(strings.TrimSpace(v.VideoCodec)) {
	case "h264", "avc", "mpeg4":
		return 0
	case "hevc", "h265":
		return 1
	case "vp9", "av1":
		return 2
	default:
		return 3
	}
}

func lessKey(a, b [7]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// RenderReason prefixes a rejection for traces: USER_MAX_SOURCE_....
func RenderReason(r Rejection) string {
	return r.Scope + "_" + r.Reason
}
