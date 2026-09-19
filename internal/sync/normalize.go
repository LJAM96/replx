package sync

import "strings"

// Dynamic range vocabulary per the playback policy specification.
const (
	DRSDR         = "SDR"
	DRHDR10       = "HDR10"
	DRHDR10Plus   = "HDR10_PLUS"
	DRHLG         = "HLG"
	DRDolbyVision = "DOLBY_VISION"
	DRHDROther    = "HDR_OTHER"
	DRUnknown     = "UNKNOWN"
)

// NormalizeDynamicRange maps origin metadata signals to the canonical
// vocabulary. Matching is case insensitive over the concatenated signals.
// Precedence is deliberate: Dolby Vision markers beat HDR10+ markers, which
// beat HDR10/PQ, then HLG, then generic HDR, then SDR. Anything without an
// explicit signal stays UNKNOWN and is never auto-classified as HDR:
// policy decides unknown handling via unknownDynamicRangeBehavior.
func NormalizeDynamicRange(dynamicRange, hdrFormat, videoCodec, videoProfile string) string {
	text := strings.ToLower(strings.Join([]string{dynamicRange, hdrFormat, videoCodec, videoProfile}, " "))
	has := func(needl ...string) bool {
		for _, n := range needl {
			if strings.Contains(text, n) {
				return true
			}
		}
		return false
	}
	switch {
	case has("dovi", "dvhe", "dvav") || (has("dolby") && has("vision")):
		return DRDolbyVision
	case has("hdr10+", "hdr10 plus", "hdr10plus"):
		return DRHDR10Plus
	case has("hdr10") || (has("pq") && !has("hlg")):
		return DRHDR10
	case has("hlg"):
		return DRHLG
	case has("hdr"):
		return DRHDROther
	case has("sdr"):
		return DRSDR
	default:
		return DRUnknown
	}
}
