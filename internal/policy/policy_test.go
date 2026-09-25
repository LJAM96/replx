package policy

import (
	"reflect"
	"strings"
	"testing"
)

func intp(n int) *int { return &n }

func TestMergePrecedence(t *testing.T) {
	global := Policy{MaxSourceHeight: intp(2160), AllowTranscode: Deny}
	user := Policy{MaxSourceHeight: intp(1080)}
	device := Policy{AllowTranscode: Allow}
	eff := Effective(global, user, device)
	if *eff.MaxSourceHeight != 1080 {
		t.Fatalf("user height must win: %+v", eff.MaxSourceHeight)
	}
	if eff.AllowTranscode.Normalize() != Allow {
		t.Fatal("device transcode must override global deny")
	}
	if eff.Allow4K.Normalize() != Allow {
		t.Fatal("unset fields must inherit defaults")
	}
	// Empty levels never weaken.
	if got, want := stripProv(Effective(Policy{}, Policy{}, Policy{})), stripProv(Defaults()); !reflect.DeepEqual(got, want) {
		t.Fatal("empty levels must equal defaults")
	}
}

// stripProv removes provenance for value comparison; provenance itself is
// covered by TestRejectionProvenance.
func stripProv(p Policy) Policy {
	p.Provenance = nil
	return p
}

// TestRejectionProvenance: a device override of an unrelated field must
// not mislabel an inherited global restriction. Global denies HDR while
// the device configures only audio channels: the HDR rejection reads
// GLOBAL_HDR_DENIED.
func TestRejectionProvenance(t *testing.T) {
	hdr := Variant{MediaIndex: 0, DynamicRange: "HDR10", Playability: DirectPlay, PartAvailable: true}
	sdr := Variant{MediaIndex: 1, DynamicRange: "SDR", Playability: DirectPlay, PartAvailable: true}
	eff := Effective(Policy{AllowHDR: Deny}, Policy{}, Policy{MaxAudioChannels: intp(2)})
	d, err := Evaluate(eff, "DEVICE", []Variant{hdr, sdr})
	if err != nil {
		t.Fatal(err)
	}
	if d.SelectedIndex != 1 {
		t.Fatalf("SDR must win, got %d", d.SelectedIndex)
	}
	if len(d.Rejected) != 1 || RenderReason(d.Rejected[0]) != "GLOBAL_HDR_DENIED" {
		t.Fatalf("provenance must name global: %+v", d.Rejected)
	}
	if eff.Provenance["allowHDR"] != ProvGlobal || eff.Provenance["maxAudioChannels"] != ProvDevice {
		t.Fatalf("provenance map: %+v", eff.Provenance)
	}
}

// TestJodieAcceptance is docs/acceptance_criteria.md: Jodie max 1080p over
// 4K+1080p variants selects 1080p during negotiation.
func TestJodieAcceptance(t *testing.T) {
	v4k := Variant{MediaIndex: 0, Width: intp(3840), Height: intp(2160), BitrateKbps: intp(60000),
		DynamicRange: "HDR10", VideoCodec: "hevc", Playability: DirectPlay, PartAvailable: true}
	v1080 := Variant{MediaIndex: 1, Width: intp(1920), Height: intp(1080), BitrateKbps: intp(15000),
		DynamicRange: "SDR", VideoCodec: "h264", Playability: DirectPlay, PartAvailable: true}
	eff := Effective(Policy{}, Policy{MaxSourceWidth: intp(1920), MaxSourceHeight: intp(1080)}, Policy{})
	d, err := Evaluate(eff, "USER", []Variant{v4k, v1080})
	if err != nil {
		t.Fatal(err)
	}
	if d.SelectedIndex != 1 {
		t.Fatalf("Jodie must select 1080p index 1, got %d", d.SelectedIndex)
	}
	if len(d.Rejected) != 1 || RenderReason(d.Rejected[0]) != "USER_MAX_SOURCE_RESOLUTION" {
		t.Fatalf("rejection: %+v", d.Rejected)
	}
}

func TestResolutionLimitRejectsUnknownDimensions(t *testing.T) {
	unknown := Variant{MediaIndex: 0, Height: intp(1080), Playability: DirectPlay, PartAvailable: true}
	v1080 := Variant{MediaIndex: 1, Width: intp(1920), Height: intp(1080), Playability: DirectPlay, PartAvailable: true}
	eff := Effective(Policy{}, Policy{MaxSourceWidth: intp(1920), MaxSourceHeight: intp(1080)}, Policy{})
	decision, err := Evaluate(eff, "USER", []Variant{unknown, v1080})
	if err != nil || decision.SelectedIndex != 1 {
		t.Fatalf("known 1080p version must win: %+v %v", decision, err)
	}
	if len(decision.Rejected) != 1 || RenderReason(decision.Rejected[0]) != "USER_UNKNOWN_SOURCE_RESOLUTION" {
		t.Fatalf("unknown width must be rejected: %+v", decision.Rejected)
	}
	_, err = Evaluate(eff, "USER", []Variant{unknown})
	if perr, ok := err.(*Error); !ok || perr.Code != NoAllowedVariant {
		t.Fatalf("unknown-only source must not bypass limit: %v", err)
	}
	if _, err := Evaluate(Defaults(), "GLOBAL", []Variant{unknown}); err != nil {
		t.Fatalf("unlimited policy should preserve existing playback: %v", err)
	}
}

// TestLukeAcceptance: 4K allowed globally selects 4K on a capable client;
// a 1080p-limited client policy still picks 1080p for the same user.
func TestLukeAcceptance(t *testing.T) {
	v4k := Variant{MediaIndex: 0, Width: intp(3840), Height: intp(2160), BitrateKbps: intp(60000),
		DynamicRange: "HDR10", VideoCodec: "hevc", Playability: DirectPlay, PartAvailable: true}
	v1080 := Variant{MediaIndex: 1, Width: intp(1920), Height: intp(1080), BitrateKbps: intp(15000),
		DynamicRange: "SDR", VideoCodec: "h264", Playability: DirectPlay, PartAvailable: true}
	d, err := Evaluate(Effective(Policy{}, Policy{}, Policy{}), "USER", []Variant{v4k, v1080})
	if err != nil || d.SelectedIndex != 0 {
		t.Fatalf("capable client must select 4K: %+v %v", d, err)
	}
	limited, err := Evaluate(Effective(Policy{}, Policy{MaxSourceHeight: intp(1080)}, Policy{}), "USER", []Variant{v4k, v1080})
	if err != nil || limited.SelectedIndex != 1 {
		t.Fatalf("limited client must select 1080p: %+v %v", limited, err)
	}
}

func TestBandwidthCapsOutputNotPermission(t *testing.T) {
	v1080 := Variant{MediaIndex: 1, Width: intp(1920), Height: intp(1080), BitrateKbps: intp(15000),
		DynamicRange: "SDR", VideoCodec: "h264", Playability: DirectStream, PartAvailable: true}
	// 6 Mbps target with transcode allowed: 1080p source, capped output.
	d, err := Evaluate(Effective(Policy{}, Policy{MaxStreamingBitrateKbps: intp(6000)}, Policy{}), "USER", []Variant{v1080})
	if err != nil || d.SelectedIndex != 1 {
		t.Fatalf("select 1080p: %+v %v", d, err)
	}
	if d.OutputBitrateKbps == nil || *d.OutputBitrateKbps != 6000 {
		t.Fatalf("output must cap at 6000: %+v", d.OutputBitrateKbps)
	}
	// Same target with transcode denied and only a transcode path: forbidden.
	vTrans := Variant{MediaIndex: 1, Width: intp(1920), Height: intp(1080), BitrateKbps: intp(15000),
		DynamicRange: "SDR", VideoCodec: "h264", Playability: Transcode, PartAvailable: true}
	_, err = Evaluate(Effective(Policy{AllowTranscode: Deny}, Policy{MaxStreamingBitrateKbps: intp(6000)}, Policy{}),
		"USER", []Variant{vTrans})
	if pe, ok := err.(*Error); !ok || pe.Code != TranscodeForbidden {
		t.Fatalf("want POLICY_TRANSCODE_FORBIDDEN, got %v", err)
	}
}

func TestHDRUnknownBehavior(t *testing.T) {
	unk := Variant{MediaIndex: 0, Width: intp(1920), Height: intp(1080), DynamicRange: "UNKNOWN",
		VideoCodec: "h264", Playability: DirectPlay, PartAvailable: true}
	// Default allows unknown (avoid false blocks).
	if _, err := Evaluate(Defaults(), "GLOBAL", []Variant{unk}); err != nil {
		t.Fatalf("default must allow unknown DR: %v", err)
	}
	_, err := Evaluate(Effective(Policy{UnknownDRBehavior: Deny}, Policy{}, Policy{}), "GLOBAL", []Variant{unk})
	if pe, ok := err.(*Error); !ok || pe.Code != NoAllowedVariant ||
		!strings.Contains(RenderReason(pe.Rejected[0]), "UNKNOWN_DYNAMIC_RANGE_DENIED") {
		t.Fatalf("deny unknown: %+v %v", pe, err)
	}
}

func TestNoEligibleSource(t *testing.T) {
	v := Variant{MediaIndex: 0, Width: intp(3840), Height: intp(2160), DynamicRange: "SDR",
		VideoCodec: "h264", Playability: DirectPlay, PartAvailable: false}
	_, err := Evaluate(Defaults(), "GLOBAL", []Variant{v})
	if pe, ok := err.(*Error); !ok || pe.Code != NoAllowedVariant {
		t.Fatalf("want NO_ALLOWED_MEDIA_VARIANT, got %v", err)
	}
}

func TestRankingDeterministic(t *testing.T) {
	mk := func(idx, w, h, br int, codec string, play Playability) Variant {
		return Variant{MediaIndex: idx, Width: intp(w), Height: intp(h), BitrateKbps: intp(br),
			DynamicRange: "SDR", VideoCodec: codec, Playability: play, PartAvailable: true}
	}
	in := []Variant{
		mk(2, 1920, 1080, 8000, "h264", Transcode),
		mk(0, 1920, 1080, 15000, "h264", DirectPlay),
		mk(1, 1920, 1080, 15000, "h264", DirectPlay),
	}
	d, err := Evaluate(Defaults(), "GLOBAL", in)
	if err != nil || d.SelectedIndex != 0 {
		t.Fatalf("direct play wins, lowest index breaks ties: %+v %v", d, err)
	}
}
