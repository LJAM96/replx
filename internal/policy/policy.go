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
// tristates defer to the less specific level. Provenance records the
// winning scope ("DEFAULT", "GLOBAL", "USER", "DEVICE") per effective
// field so rejections name the level that actually imposed the
// restriction instead of the most specific configured row.
type Policy struct {
	MaxSourceWidth          *int              `json:"maxSourceWidth"`
	MaxSourceHeight         *int              `json:"maxSourceHeight"`
	MaxSourceBitrateKbps    *int              `json:"maxSourceBitrateKbps"`
	MaxStreamingBitrateKbps *int              `json:"maxStreamingBitrateKbps"`
	Allow4K                 TriState          `json:"allow4K"`
	AllowHDR                TriState          `json:"allowHDR"`
	AllowDolbyVision        TriState          `json:"allowDolbyVision"`
	AllowTranscode          TriState          `json:"allowTranscode"`
	PreferDirectPlay        TriState          `json:"preferDirectPlay"`
	MaxAudioChannels        *int              `json:"maxAudioChannels"`
	UnknownDRBehavior       TriState          `json:"unknownDynamicRangeBehavior"`
	RoutingMode             string            `json:"routingMode"`
	Provenance              map[string]string `json:"provenance,omitempty"`
}

// Provenance scopes.
const (
	ProvDefault = "DEFAULT"
	ProvGlobal  = "GLOBAL"
	ProvUser    = "USER"
	ProvDevice  = "DEVICE"
)

// Defaults approximates normal Plex behaviour: everything allowed,
// routing automatic.
func Defaults() Policy {
	prov := map[string]string{}
	for _, f := range provFields {
		prov[f] = ProvDefault
	}
	return Policy{
		Allow4K: Allow, AllowHDR: Allow, AllowDolbyVision: Allow,
		AllowTranscode: Allow, PreferDirectPlay: Allow,
		UnknownDRBehavior: Allow, RoutingMode: "automatic",
		Provenance: prov,
	}
}

// provFields enumerates effective fields tracked for provenance.
var provFields = []string{
	"maxSourceWidth", "maxSourceHeight", "maxSourceBitrateKbps",
	"maxStreamingBitrateKbps", "allow4K", "allowHDR", "allowDolbyVision",
	"allowTranscode", "preferDirectPlay", "maxAudioChannels",
	"unknownDynamicRangeBehavior", "routingMode",
}

// Merge overlays specific onto base field by field: only configured
// (non-nil, non-inherit) fields replace. Provenance carries over for
// replaced fields only when merging with MergeAs.
func Merge(base, specific Policy) Policy {
	return MergeAs(base, specific, "")
}

// MergeAs is Merge that attributes replaced fields to scope. Empty scope
// leaves existing provenance untouched.
func MergeAs(base, specific Policy, scope string) Policy {
	out := base
	out.Provenance = cloneProv(base.Provenance)
	stamp := func(field string) {
		if scope != "" {
			if out.Provenance == nil {
				out.Provenance = map[string]string{}
			}
			out.Provenance[field] = scope
		}
	}
	if specific.MaxSourceWidth != nil {
		out.MaxSourceWidth = specific.MaxSourceWidth
		stamp("maxSourceWidth")
	}
	if specific.MaxSourceHeight != nil {
		out.MaxSourceHeight = specific.MaxSourceHeight
		stamp("maxSourceHeight")
	}
	if specific.MaxSourceBitrateKbps != nil {
		out.MaxSourceBitrateKbps = specific.MaxSourceBitrateKbps
		stamp("maxSourceBitrateKbps")
	}
	if specific.MaxStreamingBitrateKbps != nil {
		out.MaxStreamingBitrateKbps = specific.MaxStreamingBitrateKbps
		stamp("maxStreamingBitrateKbps")
	}
	if v := specific.Allow4K.Normalize(); v != Inherit {
		out.Allow4K = v
		stamp("allow4K")
	}
	if v := specific.AllowHDR.Normalize(); v != Inherit {
		out.AllowHDR = v
		stamp("allowHDR")
	}
	if v := specific.AllowDolbyVision.Normalize(); v != Inherit {
		out.AllowDolbyVision = v
		stamp("allowDolbyVision")
	}
	if v := specific.AllowTranscode.Normalize(); v != Inherit {
		out.AllowTranscode = v
		stamp("allowTranscode")
	}
	if v := specific.PreferDirectPlay.Normalize(); v != Inherit {
		out.PreferDirectPlay = v
		stamp("preferDirectPlay")
	}
	if specific.MaxAudioChannels != nil {
		out.MaxAudioChannels = specific.MaxAudioChannels
		stamp("maxAudioChannels")
	}
	if v := specific.UnknownDRBehavior.Normalize(); v != Inherit {
		out.UnknownDRBehavior = v
		stamp("unknownDynamicRangeBehavior")
	}
	if specific.RoutingMode != "" && !strings.EqualFold(specific.RoutingMode, "inherit") {
		out.RoutingMode = specific.RoutingMode
		stamp("routingMode")
	}
	return out
}

func cloneProv(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Effective folds defaults < global < user < device.
func Effective(global, user, device Policy) Policy {
	return MergeAs(MergeAs(MergeAs(Defaults(), global, ProvGlobal), user, ProvUser), device, ProvDevice)
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
	RMaxSourceResolution     = "MAX_SOURCE_RESOLUTION"
	RUnknownSourceResolution = "UNKNOWN_SOURCE_RESOLUTION"
	RSource4KDenied          = "SOURCE_4K_DENIED"
	RHDRDenied               = "HDR_DENIED"
	RDolbyVisionDenied       = "DOLBY_VISION_DENIED"
	RSourceBitrateCeil       = "SOURCE_BITRATE_CEILING"
	RPartUnavailable         = "PART_UNAVAILABLE"
	RUnknownDRDenied         = "UNKNOWN_DYNAMIC_RANGE_DENIED"
)

// Evaluate filters, ranks and selects. scope is the fallback provenance
// label; each rejection prefers the per-field winning scope recorded in
// Policy.Provenance so explanations name the level that imposed them.
func Evaluate(p Policy, scope string, variants []Variant) (Decision, error) {
	if scope == "" {
		scope = "GLOBAL"
	}
	prov := func(field string) string {
		if s, ok := p.Provenance[field]; ok && s != "" {
			return s
		}
		return scope
	}
	var eligible []Variant
	var rejected []Rejection
	reject := func(v Variant, reason, field string) {
		s := scope
		if field != "" {
			s = prov(field)
		}
		rejected = append(rejected, Rejection{MediaIndex: v.MediaIndex, Scope: s, Reason: reason})
	}
	for _, v := range variants {
		if !v.PartAvailable {
			reject(v, RPartUnavailable, "")
			continue
		}
		if is4K(v) && p.Allow4K.Normalize() == Deny {
			reject(v, RSource4KDenied, "allow4K")
			continue
		}
		if unknownResolution(v, p) {
			reject(v, RUnknownSourceResolution, resolutionField(v, p))
			continue
		}
		if overResolution(v, p) {
			reject(v, RMaxSourceResolution, resolutionField(v, p))
			continue
		}
		if deniesDR(p, v) {
			if v.DynamicRange == "UNKNOWN" {
				reject(v, RUnknownDRDenied, "unknownDynamicRangeBehavior")
			} else if v.DynamicRange == "DOLBY_VISION" && p.AllowDolbyVision.Normalize() == Deny {
				reject(v, RDolbyVisionDenied, "allowDolbyVision")
			} else {
				reject(v, RHDRDenied, "allowHDR")
			}
			continue
		}
		if p.MaxSourceBitrateKbps != nil && v.BitrateKbps != nil && *v.BitrateKbps > *p.MaxSourceBitrateKbps {
			reject(v, RSourceBitrateCeil, "maxSourceBitrateKbps")
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

// resolutionField names the bound that overResolution fired on, so the
// rejection carries that field's provenance.
func resolutionField(v Variant, p Policy) string {
	if p.MaxSourceHeight != nil && v.Height == nil {
		return "maxSourceHeight"
	}
	if p.MaxSourceWidth != nil && v.Width == nil {
		return "maxSourceWidth"
	}
	if p.MaxSourceHeight != nil && v.Height != nil && *v.Height > *p.MaxSourceHeight {
		return "maxSourceHeight"
	}
	return "maxSourceWidth"
}

func unknownResolution(v Variant, p Policy) bool {
	return (p.MaxSourceWidth != nil && v.Width == nil) ||
		(p.MaxSourceHeight != nil && v.Height == nil)
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
